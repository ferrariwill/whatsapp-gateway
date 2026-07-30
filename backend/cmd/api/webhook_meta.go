package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

type outboundWebhookPayload struct {
	SystemID         string           `json:"system_id"`
	ExternalClientID string           `json:"external_client_id"`
	MetaMessageID    string           `json:"meta_message_id,omitempty"`
	PhoneNumber      string           `json:"phone_number"`
	Text             string           `json:"text"`
	EventType        string           `json:"event_type"`
	Action           string           `json:"action,omitempty"`
	Media            *inboundMedia    `json:"media,omitempty"`
	Location         *inboundLocation `json:"location,omitempty"`
	Reaction         *inboundReaction `json:"reaction,omitempty"`
}

type metaMessageStatus struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Timestamp   string `json:"timestamp"`
	RecipientID string `json:"recipient_id"`
	Pricing     *struct {
		Billable     bool   `json:"billable"`
		PricingModel string `json:"pricing_model"`
		Category     string `json:"category"`
	} `json:"pricing"`
}

func (s *server) handleMetaWebhookVerify(w http.ResponseWriter, r *http.Request) {
	phoneNumberID := strings.TrimSpace(r.PathValue("phone_number_id"))
	if phoneNumberID == "" {
		http.Error(w, "phone_number_id is required", http.StatusBadRequest)
		return
	}

	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	expectedToken := strings.TrimSpace(os.Getenv("META_WEBHOOK_VERIFY_TOKEN"))
	if mode != "subscribe" || token == "" || token != expectedToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if _, err := s.repo.FindClientChannelByPhoneNumberID(r.Context(), phoneNumberID); err != nil {
		if errors.Is(err, repository.ErrClientChannelNotFound) {
			http.Error(w, "unknown phone number id", http.StatusNotFound)
			return
		}
		// Erro de transporte não pode virar verificação bem-sucedida: sem esta
		// guarda, uma falha de banco deixava qualquer phone_number_id assinar
		// o webhook na Meta.
		log.Printf("lookup client channel for webhook verify %s: %v", phoneNumberID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(challenge))
}

func (s *server) handleMetaWebhookEvent(w http.ResponseWriter, r *http.Request) {
	phoneNumberID := strings.TrimSpace(r.PathValue("phone_number_id"))
	if phoneNumberID == "" {
		http.Error(w, "phone_number_id is required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	appSecret := strings.TrimSpace(os.Getenv("META_APP_SECRET"))
	signature := r.Header.Get("X-Hub-Signature-256")
	if !security.VerifyMetaSignature(appSecret, body, signature) {
		s.logRejectedWebhook(
			"webhook meta legacy: invalid or missing X-Hub-Signature-256 for phone_number_id %s",
			phoneNumberID,
		)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	channel, err := s.repo.FindClientChannelByPhoneNumberID(r.Context(), phoneNumberID)
	if errors.Is(err, repository.ErrClientChannelNotFound) {
		http.Error(w, "unknown phone number id", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("lookup client channel for phone_number_id %s: %v", phoneNumberID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// O payload legado é o mesmo da Meta; só o phone_number_id vem da rota em
	// vez de value.metadata. Decodificar no tipo unificado deixa um único
	// extrator de eventos e um único caminho de status de entrega.
	var payload unifiedMetaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := s.relay.Submit(func(taskCtx context.Context) {
		s.processLegacyMetaWebhookAsync(taskCtx, phoneNumberID, channel, payload)
	}); err != nil {
		log.Printf("webhook meta legacy: rejecting event for phone_number_id %s: %v", phoneNumberID, err)
		http.Error(w, "gateway busy, retry later", http.StatusServiceUnavailable)
		return
	}

	// Responde imediatamente à Meta (timeout de 3s); processamento em background.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *server) processLegacyMetaWebhookAsync(
	ctx context.Context,
	phoneNumberID string,
	channel *model.ClientChannelWithSystem,
	payload unifiedMetaWebhookPayload,
) {
	// A conexão é resolvida uma única vez e serve tanto ao status de entrega
	// quanto à auditoria do inbound. ErrConnectionNotFound é esperado: o canal
	// legado em client_channels pode não ter linha em whatsapp_connections.
	// Qualquer outro erro é de transporte e não pode virar silêncio — sem log,
	// a linha cairia para connection_id NULL e sairia do escopo de billing por
	// conexão (MarkMessageLogDelivered exige connection_id) sem ninguém ver.
	conn, err := s.repo.FindConnectionByPhoneNumberID(ctx, phoneNumberID)
	if err != nil {
		if !errors.Is(err, repository.ErrConnectionNotFound) {
			log.Printf("lookup connection for legacy phone_number_id %s: %v", phoneNumberID, err)
		}
		conn = nil
	}

	connectionID := ""
	sistemaOrigem := ""
	if conn != nil {
		connectionID = conn.ID
		sistemaOrigem = conn.SistemaOrigem
		s.processDeliveryStatusesFromPayload(ctx, conn, payload)
	}

	targetURL := ""
	if conn != nil {
		targetURL = strings.TrimSpace(conn.WebhookURL)
	}
	if targetURL == "" {
		targetURL = strings.TrimSpace(channel.WebhookURL)
	}

	relay := inboundRelay{
		systemID:         channel.SystemID,
		connectionID:     connectionID,
		sistemaOrigem:    sistemaOrigem,
		externalClientID: channel.ExternalClientID,
		targetURL:        targetURL,
		label:            "legacy " + channel.SystemID + "/" + channel.ExternalClientID,
		buildPayload: func(event inboundEvent) any {
			payload := outboundWebhookPayload{
				SystemID:         channel.SystemID,
				ExternalClientID: channel.ExternalClientID,
			}
			applyInboundEventToLegacyPayload(&payload, event)
			return payload
		},
	}

	s.relayInboundEvents(ctx, relay, extractInboundEventsFromUnified(payload))
}

func parseMetaWebhookTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().UTC()
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(seconds, 0).UTC()
}

func (s *server) createClientChannel(
	ctx context.Context,
	systemID string,
	salonName string,
	externalClientID string,
	phoneNumberID string,
	whatsappPhoneNumber string,
) error {
	channel := &model.ClientChannel{
		SystemID:            systemID,
		SalonName:           salonName,
		ExternalClientID:    externalClientID,
		PhoneNumberID:       phoneNumberID,
		WhatsAppPhoneNumber: whatsappPhoneNumber,
	}

	return s.repo.CreateClientChannel(ctx, channel)
}

type createChannelRequest struct {
	SalonName           string `json:"salon_name"`
	ExternalClientID    string `json:"external_client_id"`
	PhoneNumberID       string `json:"phone_number_id"`
	WhatsAppPhoneNumber string `json:"whatsapp_phone_number"`
}

func (s *server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	var req createChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	req.SalonName = strings.TrimSpace(req.SalonName)
	req.ExternalClientID = strings.TrimSpace(req.ExternalClientID)
	req.PhoneNumberID = strings.TrimSpace(req.PhoneNumberID)
	req.WhatsAppPhoneNumber = strings.TrimSpace(req.WhatsAppPhoneNumber)

	if req.ExternalClientID == "" || req.PhoneNumberID == "" || req.WhatsAppPhoneNumber == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "external_client_id, phone_number_id and whatsapp_phone_number are required"})
		return
	}
	if req.SalonName == "" {
		req.SalonName = req.ExternalClientID
	}

	if err := s.createClientChannel(
		r.Context(),
		system.ID,
		req.SalonName,
		req.ExternalClientID,
		req.PhoneNumberID,
		req.WhatsAppPhoneNumber,
	); err != nil {
		log.Printf("create channel for system %s: %v", system.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to create client channel"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"status":             "created",
		"system_id":          system.ID,
		"external_client_id": req.ExternalClientID,
		"webhook_path":       "/webhooks/meta/" + req.PhoneNumberID,
	})
}
