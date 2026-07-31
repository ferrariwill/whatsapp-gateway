package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
	"github.com/whatsappgetway/gateway/internal/service"
)

type saasWebhookPayload struct {
	SystemID      string `json:"system_id"`
	SistemaOrigem string `json:"sistema_origem"`
	TenantID      string `json:"tenant_id"`
	MetaMessageID string `json:"meta_message_id,omitempty"`
	PhoneNumber   string `json:"phone_number"`
	Text          string `json:"text"`
	EventType     string `json:"event_type"`
	Action        string `json:"action,omitempty"`
	Replay        bool   `json:"replay,omitempty"`
}

type unifiedMetaWebhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID      string `json:"phone_number_id"`
					DisplayPhoneNumber string `json:"display_phone_number"`
				} `json:"metadata"`
				Messages []metaInboundMessage `json:"messages"`
				Statuses []metaMessageStatus  `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// handleWhatsAppWebhookVerify valida o webhook único cadastrado na Meta.
func (s *server) handleWhatsAppWebhookVerify(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	expectedToken := strings.TrimSpace(os.Getenv("META_WEBHOOK_VERIFY_TOKEN"))
	if mode != "subscribe" || token == "" || token != expectedToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(challenge))
}

// handleWhatsAppWebhookEvent recebe eventos da Meta, responde 200 imediatamente e processa em background.
func (s *server) handleWhatsAppWebhookEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	appSecret := strings.TrimSpace(os.Getenv("META_APP_SECRET"))
	signature := r.Header.Get("X-Hub-Signature-256")
	if !security.VerifyMetaSignature(appSecret, body, signature) {
		s.logRejectedWebhook("webhook whatsapp: invalid or missing X-Hub-Signature-256")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var payload unifiedMetaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	phoneNumberID := extractPhoneNumberIDFromPayload(payload)
	if phoneNumberID == "" {
		log.Printf("webhook whatsapp: phone_number_id not found in payload")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}

	conn, err := s.repo.FindConnectionByPhoneNumberID(r.Context(), phoneNumberID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		log.Printf("webhook whatsapp: unknown phone_number_id %s", phoneNumberID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	if err != nil {
		log.Printf("lookup connection for phone_number_id %s: %v", phoneNumberID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Processamento assíncrono: status de entrega + repasse ao SaaS. A admissão
	// no pool acontece antes do 200 para que uma rajada além da capacidade vire
	// 503 (Meta reentrega) em vez de goroutines sem limite.
	if err := s.relay.Submit(func(taskCtx context.Context) {
		s.processWebhookPayloadAsync(taskCtx, conn, payload)
	}); err != nil {
		log.Printf("webhook whatsapp: rejecting event for phone_number_id %s: %v", phoneNumberID, err)
		http.Error(w, "gateway busy, retry later", http.StatusServiceUnavailable)
		return
	}

	// Responde imediatamente à Meta (timeout de 3s).
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *server) processWebhookPayloadAsync(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	payload unifiedMetaWebhookPayload,
) {
	// Billing (delivered → message_logs) e fan-out de status ao produto são
	// caminhos paralelos; o fan-out não bloqueia o 200 à Meta (já estamos no pool).
	s.processDeliveryStatusesFromPayload(ctx, conn, payload)
	s.processStatusFanOut(ctx, conn, payload)

	targetURL := strings.TrimSpace(conn.WebhookURL)
	if targetURL == "" {
		targetURL = strings.TrimSpace(os.Getenv("MOTHER_SYSTEM_WEBHOOK_URL"))
	}

	relay := inboundRelay{
		systemID:         conn.SystemID,
		connectionID:     conn.ID,
		sistemaOrigem:    conn.SistemaOrigem,
		externalClientID: conn.TenantID,
		targetURL:        targetURL,
		label:            conn.SistemaOrigem + "/" + conn.TenantID,
		buildPayload: func(event inboundEvent) any {
			return saasWebhookPayload{
				SystemID:      conn.SystemID,
				SistemaOrigem: conn.SistemaOrigem,
				TenantID:      conn.TenantID,
				MetaMessageID: event.id,
				PhoneNumber:   event.from,
				Text:          event.text,
				EventType:     event.eventType,
				Action:        event.action,
			}
		},
	}

	s.relayInboundEvents(ctx, relay, extractInboundEventsFromUnified(payload))
}

func extractPhoneNumberIDFromPayload(payload unifiedMetaWebhookPayload) string {
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if id := strings.TrimSpace(change.Value.Metadata.PhoneNumberID); id != "" {
				return id
			}
		}
	}
	return ""
}

func (s *server) processDeliveryStatusesFromPayload(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	payload unifiedMetaWebhookPayload,
) {
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}
			for _, statusUpdate := range change.Value.Statuses {
				if strings.ToLower(strings.TrimSpace(statusUpdate.Status)) != "delivered" {
					continue
				}

				metaMessageID := strings.TrimSpace(statusUpdate.ID)
				if metaMessageID == "" {
					continue
				}

				category := service.CategoryUtility
				if statusUpdate.Pricing != nil && strings.TrimSpace(statusUpdate.Pricing.Category) != "" {
					category = service.NormalizeCategory(statusUpdate.Pricing.Category)
				}

				metaCost := service.MetaCostForCategory(category)
				deliveredAt := parseMetaWebhookTimestamp(statusUpdate.Timestamp)

				if err := s.repo.MarkMessageLogDelivered(
					ctx, metaMessageID, conn.ID, model.MessageCategory(category), metaCost, deliveredAt,
				); err != nil {
					if errors.Is(err, repository.ErrMessageLogNotFound) {
						log.Printf("delivery webhook for unknown/unscoped meta_message_id %s connection=%s",
							metaMessageID, conn.ID)
						continue
					}
					log.Printf("mark message %s delivered: %v", metaMessageID, err)
				}
			}
		}
	}
}

func extractInboundEventsFromUnified(payload unifiedMetaWebhookPayload) []inboundEvent {
	events := make([]inboundEvent, 0)
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}
			for _, message := range change.Value.Messages {
				event, ok := parseInboundMessage(message)
				if ok {
					events = append(events, event)
				}
			}
		}
	}
	return events
}

// O repasse ao SaaS e a política de backoff são únicos e vivem em
// inbound_relay.go (forwardWebhookWithRetry), compartilhados com o endpoint
// legado e com o sweep de reconciliação.
