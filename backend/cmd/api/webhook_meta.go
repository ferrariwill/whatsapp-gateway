package main

import (
	"bytes"
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
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/service"
)

type outboundWebhookPayload struct {
	SystemID         string `json:"system_id"`
	ExternalClientID string `json:"external_client_id"`
	PhoneNumber      string `json:"phone_number"`
	Text             string `json:"text"`
	EventType        string `json:"event_type"`
	Action           string `json:"action,omitempty"`
}

type metaWebhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Messages []metaInboundMessage `json:"messages"`
				Statuses []metaMessageStatus  `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
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

type metaInboundMessage struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Type string `json:"type"`
	Text *struct {
		Body string `json:"body"`
	} `json:"text"`
	Button *struct {
		Payload string `json:"payload"`
		Text    string `json:"text"`
	} `json:"button"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply"`
	} `json:"interactive"`
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

	if _, err := s.repo.FindClientChannelByPhoneNumberID(r.Context(), phoneNumberID); errors.Is(err, repository.ErrClientChannelNotFound) {
		http.Error(w, "unknown phone number id", http.StatusNotFound)
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	var payload metaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	targetURL := channel.WebhookURL
	if targetURL == "" {
		targetURL = strings.TrimSpace(os.Getenv("MOTHER_SYSTEM_WEBHOOK_URL"))
	}

	s.processDeliveryStatuses(r.Context(), phoneNumberID, payload)

	events := extractInboundEvents(payload)
	for _, event := range events {
		inboundLog := &model.MessageLog{
			SystemID:         channel.SystemID,
			ExternalClientID: channel.ExternalClientID,
			PhoneNumber:      event.from,
			TemplateName:     event.eventType,
			ReceivedContent:  event.text,
			Direction:        model.MessageDirectionInbound,
			Status:           model.MessageStatusSent,
		}
		if err := s.repo.CreateMessageLog(r.Context(), inboundLog); err != nil {
			log.Printf(
				"audit inbound log for system %s client %s: %v",
				channel.SystemID,
				channel.ExternalClientID,
				err,
			)
		}

		outbound := outboundWebhookPayload{
			SystemID:         channel.SystemID,
			ExternalClientID: channel.ExternalClientID,
			PhoneNumber:      event.from,
			Text:             event.text,
			EventType:        event.eventType,
			Action:           event.action,
		}

		if targetURL == "" {
			log.Printf(
				"skip outbound webhook: system %s channel %s has no webhook_url",
				channel.SystemID,
				channel.ExternalClientID,
			)
			continue
		}

		if err := s.forwardOutboundWebhook(r.Context(), targetURL, outbound); err != nil {
			log.Printf(
				"forward webhook to %s for system %s client %s: %v",
				targetURL,
				channel.SystemID,
				channel.ExternalClientID,
				err,
			)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *server) processDeliveryStatuses(ctx context.Context, phoneNumberID string, payload metaWebhookPayload) {
	conn, err := s.repo.FindConnectionByPhoneNumberID(ctx, phoneNumberID)
	if err != nil {
		if !errors.Is(err, repository.ErrConnectionNotFound) {
			log.Printf("lookup connection for delivery status phone_number_id %s: %v", phoneNumberID, err)
		}
		return
	}

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
						log.Printf("delivery webhook for unknown/unscoped meta_message_id %s", metaMessageID)
						continue
					}
					log.Printf("mark message %s delivered: %v", metaMessageID, err)
				}
			}
		}
	}
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

type inboundEvent struct {
	id        string
	from      string
	text      string
	eventType string
	action    string
}

func extractInboundEvents(payload metaWebhookPayload) []inboundEvent {
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

func parseInboundMessage(message metaInboundMessage) (inboundEvent, bool) {
	from := strings.TrimSpace(message.From)
	if from == "" {
		return inboundEvent{}, false
	}
	messageID := strings.TrimSpace(message.ID)

	switch message.Type {
	case "text":
		if message.Text == nil || strings.TrimSpace(message.Text.Body) == "" {
			return inboundEvent{}, false
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      strings.TrimSpace(message.Text.Body),
			eventType: "text_message",
		}, true

	case "button":
		if message.Button == nil {
			return inboundEvent{}, false
		}
		payload := strings.TrimSpace(message.Button.Payload)
		if payload == "" {
			payload = strings.TrimSpace(message.Button.Text)
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      payload,
			eventType: "button_reply",
			action:    mapButtonAction(payload),
		}, true

	case "interactive":
		if message.Interactive == nil || message.Interactive.ButtonReply == nil {
			return inboundEvent{}, false
		}
		payload := strings.TrimSpace(message.Interactive.ButtonReply.ID)
		if payload == "" {
			payload = strings.TrimSpace(message.Interactive.ButtonReply.Title)
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      payload,
			eventType: "button_reply",
			action:    mapButtonAction(payload),
		}, true
	}

	return inboundEvent{}, false
}

func mapButtonAction(payload string) string {
	switch payload {
	case provider.ButtonPayloadConfirm:
		return "CONFIRM"
	case provider.ButtonPayloadReschedule:
		return "CANCEL"
	default:
		return ""
	}
}

func (s *server) forwardOutboundWebhook(ctx context.Context, targetURL string, payload outboundWebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.metaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		respBody, _ := io.ReadAll(resp.Body)
		return errors.New(strings.TrimSpace(string(respBody)))
	}

	return nil
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
