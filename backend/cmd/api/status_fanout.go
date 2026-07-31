package main

// Fan-out de status Meta (sent/delivered/read/failed) para o webhook do produto.
//
// Caminho paralelo ao billing (MarkMessageLogDelivered): persiste em
// message_delivery_events com idempotência (meta_message_id, status, timestamp),
// claima e POST assinado HMAC ao callback do tenant. Sem MOTHER fallback —
// status sem URL fica failed, sem cross-delivery.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	gatewayUserAgent              = "WhatsAppGateway/1.0"
	gatewayEventMessageStatus     = "message.status"
	defaultStatusCallbackMaxAttempts = 8
)

// statusCallbackBackoff delays para next_retry_at após esgotar retries curtos.
var statusCallbackBackoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

type statusCallbackPayload struct {
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	ProductID     string          `json:"product_id"`
	SystemID      string          `json:"system_id"`
	MetaMessageID string          `json:"meta_message_id"`
	Recipient     string          `json:"recipient"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
	Errors        json.RawMessage `json:"errors"`
}

type webhookHTTPError struct {
	StatusCode int
	Body       string
}

func (e *webhookHTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("http %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("http %d", e.StatusCode)
}

func statusCallbackMaxAttempts() int {
	raw := strings.TrimSpace(os.Getenv("STATUS_CALLBACK_MAX_ATTEMPTS"))
	if raw == "" {
		return defaultStatusCallbackMaxAttempts
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultStatusCallbackMaxAttempts
	}
	return n
}

func normalizeDeliveryStatus(raw string) (model.DeliveryStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sent":
		return model.DeliveryStatusSent, true
	case "delivered":
		return model.DeliveryStatusDelivered, true
	case "read":
		return model.DeliveryStatusRead, true
	case "failed":
		return model.DeliveryStatusFailed, true
	default:
		return "", false
	}
}

// processStatusFanOut persiste e dispara o callback de cada status[] aceito.
// Billing delivered continua em processDeliveryStatusesFromPayload (paralelo).
func (s *server) processStatusFanOut(
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
				s.persistAndRelayStatus(ctx, conn, statusUpdate)
			}
		}
	}
}

func (s *server) persistAndRelayStatus(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	statusUpdate metaMessageStatus,
) {
	status, ok := normalizeDeliveryStatus(statusUpdate.Status)
	if !ok {
		log.Printf("status fan-out: ignoring unsupported status %q for meta_message_id=%s",
			statusUpdate.Status, strings.TrimSpace(statusUpdate.ID))
		return
	}

	metaMessageID := strings.TrimSpace(statusUpdate.ID)
	if metaMessageID == "" {
		return
	}

	errorsJSON := marshalMetaStatusErrors(statusUpdate.Errors)
	metaTS := parseMetaWebhookTimestamp(statusUpdate.Timestamp)

	targetURL, webhookSecret := s.resolveStatusCallback(ctx, conn)
	callbackStatus := model.CallbackStatusPending
	lastError := ""
	if targetURL == "" {
		callbackStatus = model.CallbackStatusFailed
		lastError = "no webhook"
	}

	event := &model.MessageDeliveryEvent{
		SystemID:       conn.SystemID,
		ConnectionID:   conn.ID,
		TenantID:       conn.TenantID,
		ProductID:      conn.SistemaOrigem,
		MetaMessageID:  metaMessageID,
		Recipient:      strings.TrimSpace(statusUpdate.RecipientID),
		Status:         status,
		MetaTimestamp:  metaTS,
		ErrorsJSON:     errorsJSON,
		CallbackStatus: callbackStatus,
		LastError:      lastError,
	}

	if err := s.repo.InsertDeliveryEvent(ctx, event); err != nil {
		if errors.Is(err, repository.ErrDuplicateDeliveryEvent) {
			log.Printf("status fan-out: dedup meta_message_id=%s status=%s ts=%s — skip",
				metaMessageID, status, metaTS.Format(time.RFC3339))
			return
		}
		log.Printf("status fan-out: insert event meta_message_id=%s: %v", metaMessageID, err)
		return
	}

	if targetURL == "" {
		log.Printf("status fan-out: %s/%s meta_message_id=%s status=%s — no webhook_url, marked failed",
			conn.SistemaOrigem, conn.TenantID, metaMessageID, status)
		return
	}

	s.relayStatusCallback(ctx, event, targetURL, webhookSecret)
}

func (s *server) resolveStatusCallback(
	ctx context.Context,
	conn *model.WhatsAppConnection,
) (targetURL, webhookSecret string) {
	targetURL = strings.TrimSpace(conn.WebhookURL)
	webhookSecret = strings.TrimSpace(conn.WebhookSecret)

	if targetURL == "" {
		system, err := s.repo.FindSystemByID(ctx, conn.SystemID)
		if err == nil {
			targetURL = strings.TrimSpace(system.WebhookURL)
		} else if !errors.Is(err, repository.ErrSystemNotFound) {
			log.Printf("status fan-out: lookup system %s for webhook_url: %v", conn.SystemID, err)
		}
	}
	// Sem fallback MOTHER_SYSTEM_WEBHOOK_URL — mother é só inbound de mensagem.
	return targetURL, webhookSecret
}

func (s *server) relayStatusCallback(
	ctx context.Context,
	event *model.MessageDeliveryEvent,
	targetURL, webhookSecret string,
) {
	claimed, err := s.repo.ClaimDeliveryEventForRelay(ctx, event.ID)
	if err != nil {
		if errors.Is(err, repository.ErrDeliveryEventNotFound) {
			return
		}
		log.Printf("status fan-out: claim event %s: %v", event.ID, err)
		return
	}

	payload := buildStatusCallbackPayload(claimed)
	httpStatus, fwdErr := s.forwardStatusWebhookWithRetry(ctx, targetURL, webhookSecret, payload)
	if fwdErr == nil {
		if err := s.repo.MarkDeliveryEventCallbackSent(ctx, claimed.ID, httpStatus); err != nil {
			log.Printf("status fan-out: mark sent event %s: %v", claimed.ID, err)
		}
		return
	}

	s.failStatusCallback(ctx, claimed, httpStatus, fwdErr.Error())
}

func buildStatusCallbackPayload(event *model.MessageDeliveryEvent) statusCallbackPayload {
	errorsJSON := event.ErrorsJSON
	if len(errorsJSON) == 0 {
		errorsJSON = []byte("[]")
	}
	return statusCallbackPayload{
		EventType:     gatewayEventMessageStatus,
		TenantID:      event.TenantID,
		ProductID:     event.ProductID,
		SystemID:      event.SystemID,
		MetaMessageID: event.MetaMessageID,
		Recipient:     event.Recipient,
		Status:        string(event.Status),
		Timestamp:     event.MetaTimestamp.UTC().Format(time.RFC3339),
		Errors:        json.RawMessage(errorsJSON),
	}
}

func (s *server) failStatusCallback(
	ctx context.Context,
	event *model.MessageDeliveryEvent,
	httpStatus int,
	errMsg string,
) {
	attempts := event.CallbackAttempts + 1
	maxAttempts := statusCallbackMaxAttempts()

	var statusPtr *int
	if httpStatus > 0 {
		statusPtr = &httpStatus
	}

	// 4xx permanente (exceto 429): cliente bugado — DLQ após 1 tentativa.
	permanentClient := httpStatus >= 400 && httpStatus < 500 && httpStatus != http.StatusTooManyRequests
	toDLQ := attempts >= maxAttempts || permanentClient

	var nextRetry *time.Time
	if !toDLQ {
		delay := statusCallbackRetryDelay(attempts)
		t := time.Now().UTC().Add(delay)
		nextRetry = &t
	}

	if err := s.repo.MarkDeliveryEventCallbackFailed(
		ctx, event.ID, statusPtr, errMsg, attempts, nextRetry, toDLQ,
	); err != nil {
		log.Printf("status fan-out: mark failed event %s: %v", event.ID, err)
		return
	}

	if toDLQ {
		log.Printf("status fan-out: event %s meta_message_id=%s moved to dlq after %d attempts: %s",
			event.ID, event.MetaMessageID, attempts, errMsg)
		return
	}
	log.Printf("status fan-out: event %s scheduled retry in %s (attempt %d): %s",
		event.ID, statusCallbackRetryDelay(attempts), attempts, errMsg)
}

func statusCallbackRetryDelay(attempt int) time.Duration {
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(statusCallbackBackoff) {
		idx = len(statusCallbackBackoff) - 1
	}
	return statusCallbackBackoff[idx]
}

func marshalMetaStatusErrors(errors []json.RawMessage) []byte {
	if len(errors) == 0 {
		return []byte("[]")
	}
	raw, err := json.Marshal(errors)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

// forwardStatusWebhookWithRetry reusa relayBackoffDelays; 4xx (≠429) não retenta.
func (s *server) forwardStatusWebhookWithRetry(
	ctx context.Context,
	targetURL, webhookSecret string,
	payload any,
) (httpStatus int, err error) {
	var lastErr error
	var lastStatus int

	for attempt, delay := range relayBackoffDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				if lastErr != nil {
					return lastStatus, lastErr
				}
				return lastStatus, ctx.Err()
			case <-timer.C:
			}
		}

		lastStatus, lastErr = s.forwardSignedWebhook(ctx, targetURL, webhookSecret, gatewayEventMessageStatus, payload)
		if lastErr == nil {
			return lastStatus, nil
		}

		log.Printf("status callback attempt %d/%d to %s failed: %v",
			attempt+1, len(relayBackoffDelays), targetURL, lastErr)

		var httpErr *webhookHTTPError
		if errors.As(lastErr, &httpErr) {
			if httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != http.StatusTooManyRequests {
				return httpErr.StatusCode, lastErr
			}
		}
	}
	return lastStatus, lastErr
}

func (s *server) forwardSignedWebhook(
	ctx context.Context,
	targetURL, webhookSecret, eventType string,
	payload any,
) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", gatewayUserAgent)
	req.Header.Set("X-Gateway-Event", eventType)
	if strings.TrimSpace(webhookSecret) != "" {
		req.Header.Set("X-Gateway-Signature-256", security.SignHMACSHA256Hex(webhookSecret, body))
	}

	resp, err := s.metaClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, &webhookHTTPError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(respBody)),
		}
	}
	return resp.StatusCode, nil
}
