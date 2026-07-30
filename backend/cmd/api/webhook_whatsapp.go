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
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/service"
)

type saasWebhookPayload struct {
	SystemID      string `json:"system_id"`
	SistemaOrigem string `json:"sistema_origem"`
	TenantID      string `json:"tenant_id"`
	PhoneNumber   string `json:"phone_number"`
	Text          string `json:"text"`
	EventType     string `json:"event_type"`
	Action        string `json:"action,omitempty"`
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

	// Responde imediatamente à Meta (timeout de 3s).
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))

	// Processamento assíncrono: status de entrega + repasse ao SaaS.
	go s.processWebhookPayloadAsync(conn, payload)
}

func (s *server) processWebhookPayloadAsync(conn *model.WhatsAppConnection, payload unifiedMetaWebhookPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.processDeliveryStatusesFromPayload(ctx, payload)

	targetURL := strings.TrimSpace(conn.WebhookURL)
	if targetURL == "" {
		targetURL = strings.TrimSpace(os.Getenv("MOTHER_SYSTEM_WEBHOOK_URL"))
	}

	events := extractInboundEventsFromUnified(payload)
	for _, event := range events {
		inboundLog := &model.MessageLog{
			SystemID:         conn.SystemID,
			ConnectionID:     conn.ID,
			SistemaOrigem:    conn.SistemaOrigem,
			ExternalClientID: conn.TenantID,
			AppointmentID:    "-",
			PhoneNumber:      event.from,
			TemplateName:     event.eventType,
			ReceivedContent:  event.text,
			Direction:        model.MessageDirectionInbound,
			Status:           model.MessageStatusSent,
		}
		if err := s.repo.CreateMessageLog(ctx, inboundLog); err != nil {
			log.Printf("audit inbound %s/%s: %v", conn.SistemaOrigem, conn.TenantID, err)
		}

		if targetURL == "" {
			log.Printf("skip saas webhook: %s/%s has no webhook_url", conn.SistemaOrigem, conn.TenantID)
			continue
		}

		outbound := saasWebhookPayload{
			SystemID:      conn.SystemID,
			SistemaOrigem: conn.SistemaOrigem,
			TenantID:      conn.TenantID,
			PhoneNumber:   event.from,
			Text:          event.text,
			EventType:     event.eventType,
			Action:        event.action,
		}

		if err := s.forwardSaaSWebhook(ctx, targetURL, outbound); err != nil {
			log.Printf("forward webhook to %s for %s/%s: %v", targetURL, conn.SistemaOrigem, conn.TenantID, err)
		}
	}
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

func (s *server) processDeliveryStatusesFromPayload(ctx context.Context, payload unifiedMetaWebhookPayload) {
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

				if err := s.repo.MarkMessageLogDelivered(ctx, metaMessageID, model.MessageCategory(category), metaCost, deliveredAt); err != nil {
					if errors.Is(err, repository.ErrMessageLogNotFound) {
						log.Printf("delivery webhook for unknown meta_message_id %s", metaMessageID)
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

func (s *server) forwardSaaSWebhook(ctx context.Context, targetURL string, payload saasWebhookPayload) error {
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
