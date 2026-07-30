package main

// Reconciliação das linhas inbound presas em pending.
//
// `pending` era escrito e nunca lido: sem reaper, sem job periódico, sem query
// de reprocessamento. Um deploy ou restart matava os repasses em voo e a linha
// ficava pending para sempre — o SaaS nunca era avisado e nada reconciliava.
// O sweep roda no startup e periodicamente: claima a linha (pending → relaying)
// ANTES do POST, reprocessa o repasse e fecha em sent ou failed. Claims órfãos
// (processo morto no meio do POST) voltam a ser elegíveis após o TTL do lease.

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

const (
	defaultPendingSweepInterval = 5 * time.Minute
	defaultPendingSweepAge      = 10 * time.Minute
	defaultPendingSweepBatch    = 200
	// defaultPendingClaimTTL é o prazo após o qual um claim em relaying é
	// considerado órfão e pode ser reclaimado por outra instância.
	defaultPendingClaimTTL = 2 * time.Minute
)

type pendingSweepConfig struct {
	Interval time.Duration
	MinAge   time.Duration
	Batch    int
	ClaimTTL time.Duration
}

func pendingSweepConfigFromEnv() pendingSweepConfig {
	return pendingSweepConfig{
		Interval: envDuration("INBOUND_PENDING_SWEEP_INTERVAL", defaultPendingSweepInterval),
		MinAge:   envDuration("INBOUND_PENDING_SWEEP_AGE", defaultPendingSweepAge),
		Batch:    envInt("INBOUND_PENDING_SWEEP_BATCH", defaultPendingSweepBatch),
		ClaimTTL: envDuration("INBOUND_PENDING_CLAIM_TTL", defaultPendingClaimTTL),
	}
}

// pendingRelayPayload é o corpo do repasse de reconciliação. A linha em pending
// não guarda por qual endpoint entrou, então o corpo carrega as duas chaves de
// identidade do tenant (tenant_id do webhook unificado e external_client_id do
// legado), a chave idempotente meta_message_id e marca replay para o SaaS
// distinguir de uma entrega em tempo real.
type pendingRelayPayload struct {
	SystemID         string `json:"system_id"`
	SistemaOrigem    string `json:"sistema_origem,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	ExternalClientID string `json:"external_client_id,omitempty"`
	MetaMessageID    string `json:"meta_message_id,omitempty"`
	PhoneNumber      string `json:"phone_number"`
	Text             string `json:"text"`
	EventType        string `json:"event_type"`
	Action           string `json:"action,omitempty"`
	Replay           bool   `json:"replay"`
}

type pendingSweepResult struct {
	Claimed     int
	Relayed     int
	Failed      int
	AlreadyDone int
}

// sweepPendingInbound claima e reprocessa um lote de linhas inbound presas em
// pending (ou claims órfãos em relaying). O POST ao SaaS só acontece depois do
// claim commitado — duas instâncias não podem repassar a mesma linha.
func (s *server) sweepPendingInbound(
	ctx context.Context,
	minAge time.Duration,
	batch int,
) (pendingSweepResult, error) {
	return s.sweepPendingInboundWithClaimTTL(ctx, minAge, defaultPendingClaimTTL, batch)
}

func (s *server) sweepPendingInboundWithClaimTTL(
	ctx context.Context,
	minAge time.Duration,
	claimTTL time.Duration,
	batch int,
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

	now := time.Now().UTC()
	claimed, err := s.repo.ClaimStalePendingInboundLogs(
		ctx,
		now.Add(-minAge),
		now.Add(-claimTTL),
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
			log.Printf("pending sweep: %s (log %s) has no webhook_url, marking failed", label, row.ID)
			if s.closeRelayingInbound(ctx, row, model.MessageStatusFailed) {
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
			MetaMessageID:    row.MetaMessageID,
			PhoneNumber:      row.PhoneNumber,
			Text:             row.ReceivedContent,
			EventType:        row.EventType,
			Action:           mapButtonAction(row.ReceivedContent),
			Replay:           true,
		}

		if err := s.forwardWebhookWithRetry(ctx, targetURL, payload); err != nil {
			log.Printf("pending sweep: replay of log %s (%s) to %s failed: %v",
				row.ID, label, targetURL, err)
			if s.closeRelayingInbound(ctx, row, model.MessageStatusFailed) {
				result.Failed++
			} else {
				result.AlreadyDone++
			}
			continue
		}

		if s.closeRelayingInbound(ctx, row, model.MessageStatusSent) {
			result.Relayed++
			log.Printf("pending sweep: log %s (%s) relayed on replay, marked sent", row.ID, label)
		} else {
			result.AlreadyDone++
		}
	}

	return result, nil
}

// resolveInboundWebhookURL aplica a mesma resolução do caminho em tempo real:
// webhook do tenant/conexão → webhook do system (já resolvido no SQL) →
// MOTHER_SYSTEM_WEBHOOK_URL.
func resolveInboundWebhookURL(resolvedFromDB string) string {
	if url := strings.TrimSpace(resolvedFromDB); url != "" {
		return url
	}
	return strings.TrimSpace(os.Getenv("MOTHER_SYSTEM_WEBHOOK_URL"))
}

// closeRelayingInbound devolve false quando a linha já não estava em relaying —
// outro worker fechou primeiro (ou um reclaim venceu a corrida) e o resultado
// deste sweep é descartado.
func (s *server) closeRelayingInbound(
	ctx context.Context,
	row repository.PendingInboundLog,
	status model.MessageStatus,
) bool {
	err := s.repo.UpdateMessageLogStatusFromRelaying(ctx, row.ID, status)
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

	log.Printf("inbound pending sweep: every %s for rows older than %s (batch %d, claim TTL %s)",
		cfg.Interval, cfg.MinAge, cfg.Batch, cfg.ClaimTTL)

	s.logSweep(s.sweepPendingInboundWithClaimTTL(ctx, cfg.MinAge, cfg.ClaimTTL, cfg.Batch))

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logSweep(s.sweepPendingInboundWithClaimTTL(ctx, cfg.MinAge, cfg.ClaimTTL, cfg.Batch))
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
	log.Printf("inbound pending sweep: %d claimed — %d relayed, %d failed, %d already closed",
		result.Claimed, result.Relayed, result.Failed, result.AlreadyDone)
}
