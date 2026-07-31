package main

// Sweep periódico dos callbacks de status presos em pending/failed/relaying.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
)

const (
	defaultStatusSweepInterval = 5 * time.Minute
	defaultStatusSweepBatch    = 200
	defaultStatusClaimTTL      = 2 * time.Minute
)

type statusSweepConfig struct {
	Interval time.Duration
	Batch    int
	ClaimTTL time.Duration
}

func statusSweepConfigFromEnv() statusSweepConfig {
	return statusSweepConfig{
		Interval: envDuration("STATUS_CALLBACK_SWEEP_INTERVAL", defaultStatusSweepInterval),
		Batch:    envInt("STATUS_CALLBACK_SWEEP_BATCH", defaultStatusSweepBatch),
		ClaimTTL: envDuration("STATUS_CALLBACK_CLAIM_TTL", defaultStatusClaimTTL),
	}
}

type statusSweepResult struct {
	Claimed     int
	Relayed     int
	Failed      int
	AlreadyDone int
}

func (s *server) sweepStatusCallbacks(ctx context.Context, cfg statusSweepConfig) (statusSweepResult, error) {
	var result statusSweepResult

	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultStatusSweepBatch
	}
	claimTTL := cfg.ClaimTTL
	if claimTTL <= 0 {
		claimTTL = defaultStatusClaimTTL
	}

	orphanBefore := time.Now().UTC().Add(-claimTTL)
	claimed, err := s.repo.ClaimStaleDeliveryEvents(ctx, orphanBefore, batch)
	if err != nil {
		return result, err
	}
	result.Claimed = len(claimed)

	for _, row := range claimed {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		targetURL := row.TargetWebhookURL
		if targetURL == "" {
			log.Printf("status sweep: event %s has no webhook_url, marking failed", row.ID)
			if err := s.repo.MarkDeliveryEventCallbackFailed(
				ctx, row.ID, nil, "no webhook", row.CallbackAttempts+1, nil, true,
			); err != nil {
				if errors.Is(err, repository.ErrDeliveryEventNotFound) {
					result.AlreadyDone++
				} else {
					log.Printf("status sweep: mark no-webhook event %s: %v", row.ID, err)
				}
			} else {
				result.Failed++
			}
			continue
		}

		event := row.MessageDeliveryEvent
		payload := buildStatusCallbackPayload(&event)
		httpStatus, fwdErr := s.forwardStatusWebhookWithRetry(ctx, targetURL, row.WebhookSecret, payload)
		if fwdErr == nil {
			if err := s.repo.MarkDeliveryEventCallbackSent(ctx, row.ID, httpStatus); err != nil {
				if errors.Is(err, repository.ErrDeliveryEventNotFound) {
					result.AlreadyDone++
				} else {
					log.Printf("status sweep: mark sent event %s: %v", row.ID, err)
				}
				continue
			}
			result.Relayed++
			continue
		}

		s.failStatusCallback(ctx, &event, httpStatus, fwdErr.Error())
		result.Failed++
	}

	return result, nil
}

func (s *server) runStatusCallbackSweeper(ctx context.Context, cfg statusSweepConfig) {
	if cfg.Interval <= 0 {
		log.Printf("status callback sweep disabled (STATUS_CALLBACK_SWEEP_INTERVAL=0)")
		return
	}

	log.Printf("status callback sweep: every %s (batch %d, claim TTL %s)",
		cfg.Interval, cfg.Batch, cfg.ClaimTTL)

	s.logStatusSweep(s.sweepStatusCallbacks(ctx, cfg))

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logStatusSweep(s.sweepStatusCallbacks(ctx, cfg))
		}
	}
}

func (s *server) logStatusSweep(result statusSweepResult, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		log.Printf("status callback sweep: %v", err)
		return
	}
	if result.Claimed == 0 {
		return
	}
	log.Printf("status callback sweep: %d claimed — %d relayed, %d failed, %d already closed",
		result.Claimed, result.Relayed, result.Failed, result.AlreadyDone)
}
