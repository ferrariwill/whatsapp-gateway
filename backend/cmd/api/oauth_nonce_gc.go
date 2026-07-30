package main

// GC da tabela oauth_state_nonces.
//
// Cada state emitido em POST /v1/embedded-signup/state grava uma linha, que
// continua útil depois de expirar: é ela que faz um replay tardio bater em
// "state já utilizado" em vez de sumir sem rastro. Por isso a remoção acontece
// com folga sobre o expires_at, e não no instante da expiração.

import (
	"context"
	"errors"
	"log"
	"time"
)

const (
	defaultOAuthNonceGCInterval  = 1 * time.Hour
	defaultOAuthNonceGCRetention = 24 * time.Hour
)

type oauthNonceGCConfig struct {
	Interval  time.Duration
	Retention time.Duration
}

func oauthNonceGCConfigFromEnv() oauthNonceGCConfig {
	return oauthNonceGCConfig{
		Interval:  envDuration("OAUTH_STATE_NONCE_GC_INTERVAL", defaultOAuthNonceGCInterval),
		Retention: envDuration("OAUTH_STATE_NONCE_RETENTION", defaultOAuthNonceGCRetention),
	}
}

// collectExpiredOAuthStateNonces remove os nonces vencidos há mais que a
// retenção configurada.
func (s *server) collectExpiredOAuthStateNonces(ctx context.Context, retention time.Duration) (int64, error) {
	if retention < 0 {
		retention = 0
	}
	return s.repo.DeleteExpiredOAuthStateNonces(ctx, time.Now().UTC().Add(-retention))
}

func (s *server) runOAuthNonceGC(ctx context.Context, cfg oauthNonceGCConfig) {
	if cfg.Interval <= 0 {
		log.Printf("oauth state nonce GC disabled (OAUTH_STATE_NONCE_GC_INTERVAL=0)")
		return
	}
	if cfg.Retention < 0 {
		cfg.Retention = defaultOAuthNonceGCRetention
	}

	log.Printf("oauth state nonce GC: every %s for nonces expired more than %s ago",
		cfg.Interval, cfg.Retention)

	s.logOAuthNonceGC(s.collectExpiredOAuthStateNonces(ctx, cfg.Retention))

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logOAuthNonceGC(s.collectExpiredOAuthStateNonces(ctx, cfg.Retention))
		}
	}
}

func (s *server) logOAuthNonceGC(removed int64, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		log.Printf("oauth state nonce GC: %v", err)
		return
	}
	if removed == 0 {
		return
	}
	log.Printf("oauth state nonce GC: %d expired nonces removed", removed)
}
