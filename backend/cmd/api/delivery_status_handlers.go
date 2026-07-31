package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

type deliveryStatusItem struct {
	ID            string          `json:"id"`
	MetaMessageID string          `json:"meta_message_id"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
	Recipient     string          `json:"recipient"`
	TenantID      string          `json:"tenant_id"`
	ProductID     string          `json:"product_id"`
	Errors        json.RawMessage `json:"errors"`
	CallbackStatus string         `json:"callback_status"`
}

type deliveryStatusResponse struct {
	MetaMessageID string               `json:"meta_message_id"`
	Events        []deliveryStatusItem `json:"events"`
}

// handleGetMessageStatus GET /v1/messages/{meta_message_id}/status
func (s *server) handleGetMessageStatus(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	metaMessageID := strings.TrimSpace(r.PathValue("meta_message_id"))
	if metaMessageID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "meta_message_id is required"})
		return
	}

	events, err := s.repo.ListDeliveryEventsByMetaMessageID(r.Context(), system.ID, metaMessageID)
	if err != nil {
		log.Printf("list delivery events for %s system %s: %v", metaMessageID, system.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to load delivery status"})
		return
	}

	if len(events) == 0 {
		existsElsewhere, err := s.repo.DeliveryEventExistsAnywhere(r.Context(), metaMessageID)
		if err != nil {
			log.Printf("check delivery event existence for %s: %v", metaMessageID, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to load delivery status"})
			return
		}
		if existsElsewhere {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
			return
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "message status not found"})
		return
	}

	items := make([]deliveryStatusItem, 0, len(events))
	for _, event := range events {
		errorsJSON := event.ErrorsJSON
		if len(errorsJSON) == 0 {
			errorsJSON = []byte("[]")
		}
		items = append(items, deliveryStatusItem{
			ID:             event.ID,
			MetaMessageID:  event.MetaMessageID,
			Status:         string(event.Status),
			Timestamp:      event.MetaTimestamp.UTC().Format(time.RFC3339),
			Recipient:      event.Recipient,
			TenantID:       event.TenantID,
			ProductID:      event.ProductID,
			Errors:         json.RawMessage(errorsJSON),
			CallbackStatus: string(event.CallbackStatus),
		})
	}

	writeJSON(w, http.StatusOK, deliveryStatusResponse{
		MetaMessageID: metaMessageID,
		Events:        items,
	})
}

// handleAdminReprocessDeliveryEvent POST /admin/delivery-events/{id}/reprocess
// Só aceita callback_status=dlq; reseta para pending e dispara fan-out imediato.
func (s *server) handleAdminReprocessDeliveryEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	eventID := strings.TrimSpace(r.PathValue("id"))
	if eventID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id is required"})
		return
	}

	event, err := s.repo.ReprocessDeliveryEventFromDLQ(r.Context(), eventID)
	if errors.Is(err, repository.ErrDeliveryEventNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "delivery event not found or not in dlq"})
		return
	}
	if err != nil {
		log.Printf("reprocess delivery event %s: %v", eventID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to reprocess"})
		return
	}

	conn, err := s.repo.FindConnectionByID(r.Context(), event.ConnectionID)
	if err != nil {
		log.Printf("reprocess: load connection %s for event %s: %v", event.ConnectionID, eventID, err)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":              event.ID,
			"callback_status": event.CallbackStatus,
			"note":            "reset to pending; sweep will retry when connection is available",
		})
		return
	}

	targetURL, webhookSecret := s.resolveStatusCallback(r.Context(), conn)
	if targetURL == "" {
		if claimed, claimErr := s.repo.ClaimDeliveryEventForRelay(r.Context(), event.ID); claimErr == nil {
			_ = s.repo.MarkDeliveryEventCallbackFailed(
				r.Context(), claimed.ID, nil, "no webhook", claimed.CallbackAttempts+1, nil, true,
			)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":              event.ID,
			"callback_status": model.CallbackStatusDLQ,
			"error":           "no webhook",
		})
		return
	}

	// Claim + POST síncrono no request admin (QA precisa ver o resultado).
	s.relayStatusCallback(r.Context(), event, targetURL, webhookSecret)

	updated, err := s.repo.FindDeliveryEventByID(r.Context(), event.ID)
	if err != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":              event.ID,
			"callback_status": "pending",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                updated.ID,
		"callback_status":   updated.CallbackStatus,
		"callback_attempts": updated.CallbackAttempts,
		"last_http_status":  updated.LastHTTPStatus,
		"last_error":        updated.LastError,
	})
}
