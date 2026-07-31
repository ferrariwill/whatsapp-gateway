package main

// Caminho único de persistência e repasse do inbound.
//
// Os dois endpoints (/webhook/whatsapp unificado e /webhooks/meta/{phone_number_id}
// legado) mantinham cópias quase idênticas da máquina de estados
// pending → sent/failed e da política de backoff. Aqui existe uma cópia só: cada
// endpoint apenas descreve para quem repassar e como montar o corpo.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

// relayBackoffDelays é a política de retry síncrono do repasse ao SaaS: primeira
// tentativa imediata e três reentregas com espera crescente.
var relayBackoffDelays = []time.Duration{0, 200 * time.Millisecond, 500 * time.Millisecond, time.Second}

// inboundRelay descreve o destino e a identidade multi-tenant de um lote de
// eventos inbound. buildPayload preserva o contrato de corpo de cada endpoint.
type inboundRelay struct {
	systemID         string
	connectionID     string
	sistemaOrigem    string
	externalClientID string
	targetURL        string
	label            string
	buildPayload     func(inboundEvent) any
}

// relayInboundEvents grava cada evento como pending, repassa ao SaaS e fecha a
// linha em sent ou failed. Retorno de ErrDuplicateMessageLog (índice único em
// meta_message_id) descarta o evento sem repasse: é reentrega da Meta.
func (s *server) relayInboundEvents(ctx context.Context, relay inboundRelay, events []inboundEvent) {
	for _, event := range events {
		payloadJSON, err := marshalInboundEventPayload(event)
		if err != nil {
			log.Printf("audit inbound %s: marshal payload: %v", relay.label, err)
			continue
		}

		inboundLog := &model.MessageLog{
			SystemID:         relay.systemID,
			ConnectionID:     relay.connectionID,
			SistemaOrigem:    relay.sistemaOrigem,
			ExternalClientID: relay.externalClientID,
			MetaMessageID:    event.id,
			AppointmentID:    "-",
			PhoneNumber:      event.from,
			TemplateName:     event.eventType,
			ReceivedContent:  event.displayText(),
			Direction:        model.MessageDirectionInbound,
			Status:           model.MessageStatusPending,
			InboundPayload:   payloadJSON,
		}

		if err := s.repo.CreateMessageLog(ctx, inboundLog); err != nil {
			if errors.Is(err, repository.ErrDuplicateMessageLog) {
				log.Printf("dedup inbound %s meta_message_id=%s — skipping SaaS relay", relay.label, event.id)
				continue
			}
			log.Printf("audit inbound %s: %v", relay.label, err)
			continue
		}

		if err := s.repo.UpsertLastInbound(ctx, relay.systemID, relay.externalClientID, event.from, time.Now().UTC()); err != nil {
			log.Printf("upsert contact session %s: %v", relay.label, err)
		}

		if event.auditOnly {
			log.Printf("inbound %s: unknown type=%q meta_message_id=%s audited, not relayed to SaaS",
				relay.label, event.rawType, event.id)
			s.markInboundPermanentFailure(ctx, inboundLog.ID, relay.label, failureReasonPermanent, "unknown inbound type "+event.rawType)
			continue
		}

		if relay.targetURL == "" {
			log.Printf("skip saas webhook: %s has no webhook_url (connection/system)", relay.label)
			s.markInboundPermanentFailure(ctx, inboundLog.ID, relay.label, failureReasonNoWebhook, "no webhook_url on connection/system")
			continue
		}

		if err := s.forwardWebhookWithRetry(ctx, relay.targetURL, relay.buildPayload(event)); err != nil {
			log.Printf("forward webhook to %s for %s failed after retries: %v", relay.targetURL, relay.label, err)
			s.markInboundRelayFailure(ctx, inboundLog.ID, relay.label, err, 1)
			continue
		}

		s.markInboundLog(ctx, inboundLog.ID, model.MessageStatusSent, relay.label, "saas relay")
	}
}

func (s *server) markInboundLog(
	ctx context.Context,
	logID string,
	status model.MessageStatus,
	label string,
	reason string,
) {
	if err := s.repo.UpdateMessageLogStatus(ctx, logID, status); err != nil {
		log.Printf("mark inbound %s (%s) as %s after %s: %v", logID, label, status, reason, err)
	}
}

func (s *server) markInboundPermanentFailure(ctx context.Context, logID, label, failureReason, lastError string) {
	err := s.repo.UpdateMessageLogRelayFailure(ctx, logID, repository.RelayFailureUpdate{
		Status:        model.MessageStatusFailed,
		FailureReason: failureReason,
		LastError:     lastError,
		RelayAttempts: 1,
	})
	if err != nil {
		log.Printf("mark inbound %s (%s) permanent failure: %v", logID, label, err)
	}
}

func (s *server) markInboundRelayFailure(ctx context.Context, logID, label string, relayErr error, attempts int) {
	reason := relayFailureReason(relayErr)
	update := repository.RelayFailureUpdate{
		Status:        model.MessageStatusFailed,
		FailureReason: reason,
		LastError:     relayErr.Error(),
		RelayAttempts: attempts,
	}
	if reason == failureReasonTransient {
		next := time.Now().UTC().Add(dlqBackoffWithJitter(attempts))
		update.NextAttemptAt = &next
	}
	if err := s.repo.UpdateMessageLogRelayFailure(ctx, logID, update); err != nil {
		log.Printf("mark inbound %s (%s) relay failure: %v", logID, label, err)
	}
}

func (s *server) forwardWebhookWithRetry(ctx context.Context, targetURL string, payload any) error {
	var lastErr error
	for attempt, delay := range relayBackoffDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				if lastErr != nil {
					return lastErr
				}
				return newRelayTransportError(ctx.Err())
			case <-timer.C:
			}
		}

		lastErr = s.forwardWebhook(ctx, targetURL, payload)
		if lastErr == nil {
			return nil
		}
		if isPermanentRelayError(lastErr) {
			log.Printf("saas webhook permanent failure to %s: %v", targetURL, lastErr)
			return lastErr
		}
		log.Printf("saas webhook attempt %d/%d to %s failed: %v",
			attempt+1, len(relayBackoffDelays), targetURL, lastErr)
	}
	return lastErr
}

func (s *server) forwardWebhook(ctx context.Context, targetURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return newRelayHTTPError(http.StatusBadRequest, err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return newRelayTransportError(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.metaClient.Do(req)
	if err != nil {
		return newRelayTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newRelayHTTPError(resp.StatusCode, readRelayErrorBody(resp))
	}
	return nil
}
