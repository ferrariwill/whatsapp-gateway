package main

// Reconciliação das linhas inbound presas em pending.
//
// `pending` era escrito e nunca lido: sem reaper, sem job periódico, sem query
// de reprocessamento. Um deploy ou restart matava os repasses em voo e a linha
// ficava pending para sempre — o SaaS nunca era avisado e nada reconciliava.
// O sweep roda no startup e periodicamente: reprocessa o repasse e, esgotado o
// retry (ou sem webhook de destino), fecha a linha em failed.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

const (
	defaultPendingSweepInterval = 5 * time.Minute
	defaultPendingSweepAge      = 10 * time.Minute
	defaultPendingSweepBatch    = 200
)

type pendingSweepConfig struct {
	Interval time.Duration
	MinAge   time.Duration
	Batch    int
}

func pendingSweepConfigFromEnv() pendingSweepConfig {
	return pendingSweepConfig{
		Interval: envDuration("INBOUND_PENDING_SWEEP_INTERVAL", defaultPendingSweepInterval),
		MinAge:   envDuration("INBOUND_PENDING_SWEEP_AGE", defaultPendingSweepAge),
		Batch:    envInt("INBOUND_PENDING_SWEEP_BATCH", defaultPendingSweepBatch),
	}
}

// pendingRelayPayload é o corpo do repasse de reconciliação. A linha em pending
// não guarda por qual endpoint entrou, então o corpo carrega as duas chaves de
// identidade do tenant (tenant_id do webhook unificado e external_client_id do
// legado) e marca replay para o SaaS distinguir de uma entrega em tempo real.
type pendingRelayPayload struct {
	SystemID         string `json:"system_id"`
	SistemaOrigem    string `json:"sistema_origem,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	ExternalClientID string `json:"external_client_id,omitempty"`
	PhoneNumber      string `json:"phone_number"`
	Text             string `json:"text"`
	EventType        string `json:"event_type"`
	Action           string `json:"action,omitempty"`
	Replay           bool   `json:"replay"`
}

type pendingSweepResult struct {
	Scanned     int
	Relayed     int
	Failed      int
	AlreadyDone int
}

// sweepPendingInbound reprocessa um lote de linhas inbound presas em pending há
// mais de minAge. Nunca sobrescreve o desfecho de um repasse concluído por
// outro worker: o fechamento é condicionado a status = 'pending'.
func (s *server) sweepPendingInbound(
	ctx context.Context,
	minAge time.Duration,
	batch int,
) (pendingSweepResult, error) {
	var result pendingSweepResult

	if batch <= 0 {
		batch = defaultPendingSweepBatch
	}
	if minAge < 0 {
		minAge = 0
	}

	stale, err := s.repo.ListStalePendingInboundLogs(ctx, time.Now().UTC().Add(-minAge), batch)
	if err != nil {
		return result, err
	}
	result.Scanned = len(stale)

	for _, row := range stale {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		label := row.SistemaOrigem + "/" + row.ExternalClientID

		if row.TargetWebhookURL == "" {
			log.Printf("pending sweep: %s (log %s) has no webhook_url, marking failed", label, row.ID)
			if s.closePendingInbound(ctx, row, model.MessageStatusFailed) {
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
			PhoneNumber:      row.PhoneNumber,
			Text:             row.ReceivedContent,
			EventType:        row.EventType,
			Action:           mapButtonAction(row.ReceivedContent),
			Replay:           true,
		}

		if err := s.forwardWebhookWithRetry(ctx, row.TargetWebhookURL, payload); err != nil {
			log.Printf("pending sweep: replay of log %s (%s) to %s failed: %v",
				row.ID, label, row.TargetWebhookURL, err)
			if s.closePendingInbound(ctx, row, model.MessageStatusFailed) {
				result.Failed++
			} else {
				result.AlreadyDone++
			}
			continue
		}

		if s.closePendingInbound(ctx, row, model.MessageStatusSent) {
			result.Relayed++
			log.Printf("pending sweep: log %s (%s) relayed on replay, marked sent", row.ID, label)
		} else {
			result.AlreadyDone++
		}
	}

	return result, nil
}

// closePendingInbound devolve false quando a linha já não estava em pending —
// outro worker fechou primeiro e o resultado do sweep é descartado.
func (s *server) closePendingInbound(
	ctx context.Context,
	row repository.PendingInboundLog,
	status model.MessageStatus,
) bool {
	err := s.repo.UpdateMessageLogStatusFromPending(ctx, row.ID, status)
	if err == nil {
		return true
	}
	if errors.Is(err, repository.ErrMessageLogNotFound) {
		return false
	}
	log.Printf("pending sweep: mark log %s as %s: %v", row.ID, status, err)
	return false
}

// runPendingSweeper executa o sweep no startup e a cada Interval até ctx acabar.
func (s *server) runPendingSweeper(ctx context.Context, cfg pendingSweepConfig) {
	if cfg.Interval <= 0 {
		log.Printf("inbound pending sweep disabled (INBOUND_PENDING_SWEEP_INTERVAL=0)")
		return
	}

	log.Printf("inbound pending sweep: every %s for rows older than %s (batch %d)",
		cfg.Interval, cfg.MinAge, cfg.Batch)

	s.logSweep(s.sweepPendingInbound(ctx, cfg.MinAge, cfg.Batch))

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logSweep(s.sweepPendingInbound(ctx, cfg.MinAge, cfg.Batch))
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
	if result.Scanned == 0 {
		return
	}
	log.Printf("inbound pending sweep: %d stale rows — %d relayed, %d failed, %d already closed",
		result.Scanned, result.Relayed, result.Failed, result.AlreadyDone)
}
