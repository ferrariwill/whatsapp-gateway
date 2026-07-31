package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/whatsappgetway/gateway/internal/security"
)

const (
	defaultEnvRateLimitRPS   = 5.0
	defaultEnvRateLimitBurst = 10
	rateLimitConfigCacheTTL  = 30 * time.Second
)

// RateLimitResolved is the effective commercial throttle for a tenant+product pair.
type RateLimitResolved struct {
	TenantCfg security.BucketConfig
	ProductCfg security.BucketConfig
	Source     string // connection | system | env
	RPS        float64
	Burst      int
}

// ResolveRateLimitConfig reads connection override → system defaults → env.
// Caller may cache with TTL ≤ 30s; AdminUpdateConnectionRateLimit should invalidate.
func (r *PostgresRepository) ResolveRateLimitConfig(
	ctx context.Context, systemID, tenantID string,
) (RateLimitResolved, error) {
	const query = `
		SELECT
			c.rate_limit_rps,
			c.rate_limit_burst,
			s.default_rate_limit_rps,
			s.default_rate_limit_burst
		FROM systems s
		LEFT JOIN whatsapp_connections c
			ON c.system_id = s.id AND c.tenant_id = $2
		WHERE s.id = $1
	`
	var connRPS, sysRPS sql.NullFloat64
	var connBurst, sysBurst sql.NullInt64
	err := r.db.QueryRowContext(ctx, query, systemID, tenantID).Scan(
		&connRPS, &connBurst, &sysRPS, &sysBurst,
	)
	if err != nil {
		return RateLimitResolved{}, fmt.Errorf("resolve rate limit config: %w", err)
	}

	envRPS, envBurst := envRateLimitDefaults()
	source := "env"
	rps := envRPS
	burst := envBurst

	if sysRPS.Valid && sysRPS.Float64 > 0 && sysBurst.Valid && sysBurst.Int64 > 0 {
		rps = sysRPS.Float64
		burst = int(sysBurst.Int64)
		source = "system"
	}
	if connRPS.Valid && connRPS.Float64 > 0 && connBurst.Valid && connBurst.Int64 > 0 {
		rps = connRPS.Float64
		burst = int(connBurst.Int64)
		source = "connection"
	}

	cfg := security.BucketConfig{RPS: rps, Burst: burst}
	return RateLimitResolved{
		TenantCfg:  cfg,
		ProductCfg: cfg, // product quota uses same numeric defaults (aggregated key differs)
		Source:     source,
		RPS:        rps,
		Burst:      burst,
	}, nil
}

func envRateLimitDefaults() (float64, int) {
	rps := defaultEnvRateLimitRPS
	burst := defaultEnvRateLimitBurst
	if raw := os.Getenv("RATE_LIMIT_DEFAULT_RPS"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			rps = v
		}
	}
	if raw := os.Getenv("RATE_LIMIT_DEFAULT_BURST"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			burst = v
		}
	}
	return rps, burst
}

// UpdateConnectionRateLimit sets per-tenant overrides and writes rate_limit_audit.
func (r *PostgresRepository) UpdateConnectionRateLimit(
	ctx context.Context,
	connectionID, systemID, tenantID, actorUserID string,
	newRPS float64,
	newBurst int,
	oldRPS *float64,
	oldBurst *int,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rate limit update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE whatsapp_connections
		SET rate_limit_rps = $2, rate_limit_burst = $3
		WHERE id = $1
	`, connectionID, newRPS, newBurst)
	if err != nil {
		return fmt.Errorf("update connection rate limit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConnectionNotFound
	}

	var actor any
	if actorUserID != "" {
		actor = actorUserID
	}
	var oldRPSVal, oldBurstVal any
	if oldRPS != nil {
		oldRPSVal = *oldRPS
	}
	if oldBurst != nil {
		oldBurstVal = *oldBurst
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rate_limit_audit (
			system_id, tenant_id, actor_user_id, old_rps, old_burst, new_rps, new_burst
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, systemID, tenantID, actor, oldRPSVal, oldBurstVal, newRPS, newBurst); err != nil {
		return fmt.Errorf("insert rate limit audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rate limit update: %w", err)
	}
	return nil
}

// RateLimitConfigCacheTTL is the max cache TTL for resolved configs (≤30s per TL plan).
func RateLimitConfigCacheTTL() time.Duration {
	return rateLimitConfigCacheTTL
}
