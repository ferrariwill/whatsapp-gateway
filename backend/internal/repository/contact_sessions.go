package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/whatsappgetway/gateway/internal/session"
)

// UpsertLastInbound refreshes the 24h customer-care window for a contact.
// Only call after a successful CreateMessageLog for a new inbound (not on dedup skip).
func (r *PostgresRepository) UpsertLastInbound(
	ctx context.Context, systemID, tenantID, waID string, at time.Time,
) error {
	waID = session.NormalizeWAID(waID)
	if systemID == "" || tenantID == "" || waID == "" {
		return fmt.Errorf("upsert last inbound: system_id, tenant_id and wa_id are required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	const query = `
		INSERT INTO contact_sessions (system_id, tenant_id, wa_id, last_inbound_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (system_id, tenant_id, wa_id) DO UPDATE SET
			last_inbound_at = GREATEST(contact_sessions.last_inbound_at, EXCLUDED.last_inbound_at),
			updated_at = NOW()
	`
	if _, err := r.db.ExecContext(ctx, query, systemID, tenantID, waID, at.UTC()); err != nil {
		return fmt.Errorf("upsert contact session: %w", err)
	}
	return nil
}

// GetLastInbound returns the last inbound timestamp for the contact, or nil if never seen.
func (r *PostgresRepository) GetLastInbound(
	ctx context.Context, systemID, tenantID, waID string,
) (*time.Time, error) {
	waID = session.NormalizeWAID(waID)
	const query = `
		SELECT last_inbound_at
		FROM contact_sessions
		WHERE system_id = $1 AND tenant_id = $2 AND wa_id = $3
	`
	var at time.Time
	err := r.db.QueryRowContext(ctx, query, systemID, tenantID, waID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get last inbound: %w", err)
	}
	return &at, nil
}
