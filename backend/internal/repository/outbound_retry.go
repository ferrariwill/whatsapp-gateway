package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

// EnqueueOutboundRetry inserts or refreshes a DLQ row for Meta 429/5xx (no sync busy-loop).
func (r *PostgresRepository) EnqueueOutboundRetry(ctx context.Context, item *model.OutboundRetryQueueItem) error {
	const query = `
		INSERT INTO outbound_retry_queue (
			message_log_id, system_id, connection_id, kind, payload_json,
			attempts, next_attempt_at, last_error, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (message_log_id) DO UPDATE SET
			payload_json = EXCLUDED.payload_json,
			attempts = EXCLUDED.attempts,
			next_attempt_at = EXCLUDED.next_attempt_at,
			last_error = EXCLUDED.last_error,
			status = EXCLUDED.status,
			updated_at = NOW()
		RETURNING id, created_at, updated_at
	`
	status := item.Status
	if status == "" {
		status = model.OutboundRetryPending
	}
	err := r.db.QueryRowContext(ctx, query,
		item.MessageLogID, item.SystemID, item.ConnectionID, item.Kind, item.PayloadJSON,
		item.Attempts, item.NextAttemptAt, item.LastError, status,
	).Scan(&item.ID, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return fmt.Errorf("enqueue outbound retry: %w", err)
	}
	return nil
}

// ClaimOutboundRetries claims due rows for the outbound Meta retry sweeper.
func (r *PostgresRepository) ClaimOutboundRetries(
	ctx context.Context, batch int, claimTTL time.Duration,
) ([]model.OutboundRetryQueueItem, error) {
	if batch <= 0 {
		batch = 50
	}
	if claimTTL <= 0 {
		claimTTL = 2 * time.Minute
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	const query = `
		WITH due AS (
			SELECT id FROM outbound_retry_queue
			WHERE (
				status IN ('pending', 'failed') AND next_attempt_at <= NOW()
			) OR (
				status = 'relaying' AND (
					sweep_claimed_at IS NULL OR sweep_claimed_at < NOW() - make_interval(secs => $1::double precision)
				)
			)
			ORDER BY next_attempt_at ASC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE outbound_retry_queue q
		SET status = 'relaying', sweep_claimed_at = NOW(), updated_at = NOW()
		FROM due
		WHERE q.id = due.id
		RETURNING q.id, q.message_log_id, q.system_id, q.connection_id, q.kind, q.payload_json,
			q.attempts, q.next_attempt_at, COALESCE(q.last_error, ''), q.status,
			q.sweep_claimed_at, q.created_at, q.updated_at
	`
	rows, err := tx.QueryContext(ctx, query, claimTTL.Seconds(), batch)
	if err != nil {
		return nil, fmt.Errorf("claim outbound retries: %w", err)
	}
	defer rows.Close()

	items := make([]model.OutboundRetryQueueItem, 0)
	for rows.Next() {
		var it model.OutboundRetryQueueItem
		var claimed sql.NullTime
		if err := rows.Scan(
			&it.ID, &it.MessageLogID, &it.SystemID, &it.ConnectionID, &it.Kind, &it.PayloadJSON,
			&it.Attempts, &it.NextAttemptAt, &it.LastError, &it.Status,
			&claimed, &it.CreatedAt, &it.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan outbound retry: %w", err)
		}
		if claimed.Valid {
			it.SweepClaimedAt = &claimed.Time
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

// MarkOutboundRetrySent marks a successful Meta retry.
func (r *PostgresRepository) MarkOutboundRetrySent(ctx context.Context, id, metaMessageID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var messageLogID string
	err = tx.QueryRowContext(ctx, `
		UPDATE outbound_retry_queue
		SET status = 'sent', last_error = NULL, updated_at = NOW(), sweep_claimed_at = NULL
		WHERE id = $1
		RETURNING message_log_id
	`, id).Scan(&messageLogID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark outbound retry sent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE message_logs
		SET status = $2, meta_message_id = NULLIF($3, ''), failure_code = NULL, failure_reason = NULL
		WHERE id = $1
	`, messageLogID, model.MessageStatusSent, metaMessageID); err != nil {
		return fmt.Errorf("mark message log sent after outbound retry: %w", err)
	}
	return tx.Commit()
}

// MarkOutboundRetryFailure schedules another attempt or exhausts the queue.
func (r *PostgresRepository) MarkOutboundRetryFailure(
	ctx context.Context, id string, attempts int, nextAttemptAt time.Time, lastErr string, exhausted bool,
) error {
	status := model.OutboundRetryFailed
	if exhausted {
		status = model.OutboundRetryExhausted
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE outbound_retry_queue
		SET status = $2, attempts = $3, next_attempt_at = $4, last_error = $5,
		    sweep_claimed_at = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, status, attempts, nextAttemptAt, lastErr)
	if err != nil {
		return fmt.Errorf("mark outbound retry failure: %w", err)
	}
	return nil
}

// UpdateMessageLogFailureCode sets optional failure_code on a message log.
func (r *PostgresRepository) UpdateMessageLogFailureCode(ctx context.Context, id, code, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE message_logs SET failure_code = NULLIF($2, ''), failure_reason = NULLIF($3, '') WHERE id = $1
	`, id, code, reason)
	if err != nil {
		return fmt.Errorf("update message log failure code: %w", err)
	}
	return nil
}
