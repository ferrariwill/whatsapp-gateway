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
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

// relayBackoffDelays é a política de retry do repasse ao SaaS: primeira
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
		inboundLog := &model.MessageLog{
			SystemID:         relay.systemID,
			ConnectionID:     relay.connectionID,
			SistemaOrigem:    relay.sistemaOrigem,
			ExternalClientID: relay.externalClientID,
			MetaMessageID:    event.id,
			AppointmentID:    "-",
			PhoneNumber:      event.from,
			TemplateName:     event.eventType,
			ReceivedContent:  event.text,
			Direction:        model.MessageDirectionInbound,
			Status:           model.MessageStatusPending,
		}

		if err := s.repo.CreateMessageLog(ctx, inboundLog); err != nil {
			if errors.Is(err, repository.ErrDuplicateMessageLog) {
				log.Printf("dedup inbound %s meta_message_id=%s — skipping SaaS relay", relay.label, event.id)
				continue
			}
			log.Printf("audit inbound %s: %v", relay.label, err)
			continue
		}

		if relay.targetURL == "" {
			log.Printf("skip saas webhook: %s has no webhook_url", relay.label)
			s.markInboundLog(ctx, inboundLog.ID, model.MessageStatusFailed, relay.label, "no webhook")
			continue
		}

		if err := s.forwardWebhookWithRetry(ctx, relay.targetURL, relay.buildPayload(event)); err != nil {
			log.Printf("forward webhook to %s for %s failed after retries: %v", relay.targetURL, relay.label, err)
			s.markInboundLog(ctx, inboundLog.ID, model.MessageStatusFailed, relay.label, "relay exhausted")
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
				return ctx.Err()
			case <-timer.C:
			}
		}

		lastErr = s.forwardWebhook(ctx, targetURL, payload)
		if lastErr == nil {
			return nil
		}
		log.Printf("saas webhook attempt %d/%d to %s failed: %v",
			attempt+1, len(relayBackoffDelays), targetURL, lastErr)
	}
	return lastErr
}

func (s *server) forwardWebhook(ctx context.Context, targetURL string, payload any) error {
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
