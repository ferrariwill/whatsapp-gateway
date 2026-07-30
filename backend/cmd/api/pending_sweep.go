package main

// Reconciliação das linhas inbound presas em pending e replay/DLQ de failed.
//
// O sweep claima (pending/failed retryable/orphan relaying → relaying) ANTES do
// POST. Claims órfãos voltam após o TTL do lease. Falhas permanentes não
// reentram; transitórias usam next_attempt_at com backoff+jitter.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

const (
	defaultPendingSweepInterval = 5 * time.Minute
	defaultPendingSweepAge      = 10 * time.Minute
	defaultPendingSweepBatch    = 200
	defaultPendingClaimTTL      = 2 * time.Minute
	defaultDLQMaxAttempts       = 5
	defaultDLQBaseBackoff       = 30 * time.Second
	defaultDLQMaxBackoff        = 15 * time.Minute
)

type pendingSweepConfig struct {
	Interval    time.Duration
	MinAge      time.Duration
	Batch       int
	ClaimTTL    time.Duration
	MaxAttempts int
}

func pendingSweepConfigFromEnv() pendingSweepConfig {
	return pendingSweepConfig{
		Interval:    envDuration("INBOUND_PENDING_SWEEP_INTERVAL", defaultPendingSweepInterval),
		MinAge:      envDuration("INBOUND_PENDING_SWEEP_AGE", defaultPendingSweepAge),
		Batch:       envInt("INBOUND_PENDING_SWEEP_BATCH", defaultPendingSweepBatch),
		ClaimTTL:    envDuration("INBOUND_PENDING_CLAIM_TTL", defaultPendingClaimTTL),
		MaxAttempts: envInt("INBOUND_DLQ_MAX_ATTEMPTS", defaultDLQMaxAttempts),
	}
}

// pendingRelayPayload é o corpo do repasse de reconciliação. Carrega identidade
// multi-tenant, meta_message_id e campos tipados restaurados de inbound_payload.
type pendingRelayPayload struct {
	SystemID         string           `json:"system_id"`
	SistemaOrigem    string           `json:"sistema_origem,omitempty"`
	TenantID         string           `json:"tenant_id,omitempty"`
	ExternalClientID string           `json:"external_client_id,omitempty"`
	MetaMessageID    string           `json:"meta_message_id,omitempty"`
	PhoneNumber      string           `json:"phone_number"`
	Text             string           `json:"text"`
	EventType        string           `json:"event_type"`
	Action           string           `json:"action,omitempty"`
	Media            *inboundMedia    `json:"media,omitempty"`
	Location         *inboundLocation `json:"location,omitempty"`
	Reaction         *inboundReaction `json:"reaction,omitempty"`
	Replay           bool             `json:"replay"`
}

type pendingSweepResult struct {
	Claimed     int
	Relayed     int
	Failed      int
	Retried     int
	AlreadyDone int
}

func (s *server) sweepPendingInbound(
	ctx context.Context,
	minAge time.Duration,
	batch int,
) (pendingSweepResult, error) {
	return s.sweepPendingInboundWithClaimTTL(ctx, minAge, defaultPendingClaimTTL, batch, defaultDLQMaxAttempts)
}

func (s *server) sweepPendingInboundWithClaimTTL(
	ctx context.Context,
	minAge time.Duration,
	claimTTL time.Duration,
	batch int,
	maxAttempts int,
) (pendingSweepResult, error) {
	var result pendingSweepResult

	if batch <= 0 {
		batch = defaultPendingSweepBatch
	}
	if minAge < 0 {
		minAge = 0
	}
	if claimTTL <= 0 {
		claimTTL = defaultPendingClaimTTL
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultDLQMaxAttempts
	}

	now := time.Now().UTC()
	claimed, err := s.repo.ClaimStalePendingInboundLogs(
		ctx,
		now.Add(-minAge),
		now.Add(-claimTTL),
		now,
		maxAttempts,
		batch,
	)
	if err != nil {
		return result, err
	}
	result.Claimed = len(claimed)

	for _, row := range claimed {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		label := row.SistemaOrigem + "/" + row.ExternalClientID
		targetURL := resolveInboundWebhookURL(row.TargetWebhookURL)

		if targetURL == "" {
			log.Printf("pending sweep: %s (log %s) has no webhook_url, marking permanent failed", label, row.ID)
			if s.closeRelayingInbound(ctx, row, repository.CloseRelayingUpdate{
				Status:        model.MessageStatusFailed,
				FailureReason: failureReasonNoWebhook,
				LastError:     "no webhook_url on connection/system",
			}) {
				result.Failed++
			} else {
				result.AlreadyDone++
			}
			continue
		}

		fallback := inboundEvent{
			id:        row.MetaMessageID,
			from:      row.PhoneNumber,
			text:      row.ReceivedContent,
			eventType: row.EventType,
			action:    mapButtonAction(row.ReceivedContent),
		}
		event := inboundEventFromPayloadJSON(row.InboundPayload, fallback)
		if event.auditOnly {
			log.Printf("pending sweep: log %s unknown payload type=%q — permanent", row.ID, event.rawType)
			if s.closeRelayingInbound(ctx, row, repository.CloseRelayingUpdate{
				Status:        model.MessageStatusFailed,
				FailureReason: failureReasonPermanent,
				LastError:     "unknown inbound type " + event.rawType,
			}) {
				result.Failed++
			} else {
				result.AlreadyDone++
			}
			continue
		}

		payload := pendingRelayPayload{
			SystemID:         row.SystemID,
			SistemaOrigem:    row.SistemaOrigem,
			TenantID:         row.ExternalClientID,
			ExternalClientID: row.ExternalClientID,
			MetaMessageID:    event.id,
			PhoneNumber:      event.from,
			Text:             event.text,
			EventType:        event.eventType,
			Action:           event.action,
			Media:            event.media,
			Location:         event.location,
			Reaction:         event.reaction,
			Replay:           true,
		}

		if err := s.forwardWebhookWithRetry(ctx, targetURL, payload); err != nil {
			log.Printf("pending sweep: replay of log %s (%s) to %s failed: %v",
				row.ID, label, targetURL, err)
			closeUpdate := repository.CloseRelayingUpdate{
				Status:        model.MessageStatusFailed,
				FailureReason: relayFailureReason(err),
				LastError:     err.Error(),
			}
			if closeUpdate.FailureReason == failureReasonTransient {
				if row.RelayAttempts >= maxAttempts {
					closeUpdate.FailureReason = failureReasonExhausted
				} else {
					next := time.Now().UTC().Add(dlqBackoffWithJitter(row.RelayAttempts))
					closeUpdate.NextAttemptAt = &next
					result.Retried++
				}
			}
			if s.closeRelayingInbound(ctx, row, closeUpdate) {
				result.Failed++
			} else {
				result.AlreadyDone++
			}
			continue
		}

		if s.closeRelayingInbound(ctx, row, repository.CloseRelayingUpdate{
			Status: model.MessageStatusSent,
		}) {
			result.Relayed++
			log.Printf("pending sweep: log %s (%s) relayed on replay, marked sent", row.ID, label)
		} else {
			result.AlreadyDone++
		}
	}

	return result, nil
}

// resolveInboundWebhookURL usa apenas o destino já resolvido no SQL
// (conexão → system). Sem fallback compartilhado MOTHER_SYSTEM_WEBHOOK_URL.
func resolveInboundWebhookURL(resolvedFromDB string) string {
	return strings.TrimSpace(resolvedFromDB)
}

func (s *server) closeRelayingInbound(
	ctx context.Context,
	row repository.PendingInboundLog,
	update repository.CloseRelayingUpdate,
) bool {
	err := s.repo.UpdateMessageLogStatusFromRelaying(ctx, row.ID, update)
	if err == nil {
		return true
	}
	if errors.Is(err, repository.ErrMessageLogNotFound) {
		return false
	}
	log.Printf("pending sweep: mark log %s as %s: %v", row.ID, update.Status, err)
	return false
}

func (s *server) runPendingSweeper(ctx context.Context, cfg pendingSweepConfig) {
	if cfg.Interval <= 0 {
		log.Printf("inbound pending sweep disabled (INBOUND_PENDING_SWEEP_INTERVAL=0)")
		return
	}

	log.Printf("inbound pending sweep: every %s for rows older than %s (batch %d, claim TTL %s, dlq max %d)",
		cfg.Interval, cfg.MinAge, cfg.Batch, cfg.ClaimTTL, cfg.MaxAttempts)

	s.logSweep(s.sweepPendingInboundWithClaimTTL(ctx, cfg.MinAge, cfg.ClaimTTL, cfg.Batch, cfg.MaxAttempts))

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logSweep(s.sweepPendingInboundWithClaimTTL(ctx, cfg.MinAge, cfg.ClaimTTL, cfg.Batch, cfg.MaxAttempts))
		}
	}
}

func (s *server) logSweep(result pendingSweepResult, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		log.Printf("inbound pending sweep: %v", err)
		return
	}
	if result.Claimed == 0 {
		return
	}
	log.Printf("inbound pending sweep: %d claimed — %d relayed, %d failed, %d scheduled retry, %d already closed",
		result.Claimed, result.Relayed, result.Failed, result.Retried, result.AlreadyDone)
}

func dlqBackoffWithJitter(attempts int) time.Duration {
	base := envDuration("INBOUND_DLQ_BASE_BACKOFF", defaultDLQBaseBackoff)
	max := envDuration("INBOUND_DLQ_MAX_BACKOFF", defaultDLQMaxBackoff)
	if attempts < 1 {
		attempts = 1
	}
	exp := float64(base) * math.Pow(2, float64(attempts-1))
	if exp > float64(max) {
		exp = float64(max)
	}
	// Jitter uniforme em [0.5, 1.5) × delay.
	jitter := 0.5 + float64(randUint32()%1000)/1000.0
	d := time.Duration(exp * jitter)
	if d < base {
		return base
	}
	if d > max {
		return max
	}
	return d
}

func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint32(b[:])
}
