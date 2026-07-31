package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

var (
	ErrDuplicateDeliveryEvent = errors.New("duplicate delivery event")
	ErrDeliveryEventNotFound  = errors.New("delivery event not found")
)

// InsertDeliveryEvent grava um status Meta. Conflito na chave de idempotência
// (meta_message_id, status, meta_timestamp) devolve ErrDuplicateDeliveryEvent
// sem sobrescrever — reentrega Meta não deve reabrir fan-out.
func (r *PostgresRepository) InsertDeliveryEvent(ctx context.Context, event *model.MessageDeliveryEvent) error {
	const query = `
		INSERT INTO message_delivery_events (
			system_id, connection_id, tenant_id, product_id, meta_message_id,
			recipient, status, meta_timestamp, errors_json, callback_status, last_error
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, NULLIF($11, ''))
		ON CONFLICT (meta_message_id, status, meta_timestamp) DO NOTHING
		RETURNING id, created_at
	`
	errorsJSON := event.ErrorsJSON
	if len(errorsJSON) == 0 {
		errorsJSON = []byte("[]")
	}
	callbackStatus := event.CallbackStatus
	if callbackStatus == "" {
		callbackStatus = model.CallbackStatusPending
	}

	err := r.db.QueryRowContext(ctx, query,
		event.SystemID, event.ConnectionID, event.TenantID, event.ProductID, event.MetaMessageID,
		event.Recipient, string(event.Status), event.MetaTimestamp, string(errorsJSON),
		string(callbackStatus), event.LastError,
	).Scan(&event.ID, &event.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDuplicateDeliveryEvent
	}
	if err != nil {
		return fmt.Errorf("insert delivery event: %w", err)
	}
	event.CallbackStatus = callbackStatus
	event.ErrorsJSON = errorsJSON
	return nil
}

// ClaimDeliveryEventForRelay move pending|failed → relaying com lease.
func (r *PostgresRepository) ClaimDeliveryEventForRelay(ctx context.Context, id string) (*model.MessageDeliveryEvent, error) {
	const query = `
		UPDATE message_delivery_events
		SET callback_status = 'relaying',
		    sweep_claimed_at = NOW()
		WHERE id = $1
		  AND callback_status IN ('pending', 'failed')
		RETURNING
			id, system_id, connection_id, tenant_id, product_id, meta_message_id,
			recipient, status, meta_timestamp, errors_json, callback_status,
			callback_attempts, last_http_status, COALESCE(last_error, ''),
			next_retry_at, sweep_claimed_at, created_at
	`
	return r.scanDeliveryEvent(r.db.QueryRowContext(ctx, query, id))
}

// MarkDeliveryEventCallbackSent fecha o fan-out com sucesso.
func (r *PostgresRepository) MarkDeliveryEventCallbackSent(ctx context.Context, id string, httpStatus int) error {
	const query = `
		UPDATE message_delivery_events
		SET callback_status = 'sent',
		    last_http_status = $2,
		    last_error = NULL,
		    next_retry_at = NULL,
		    sweep_claimed_at = NULL
		WHERE id = $1
		  AND callback_status = 'relaying'
	`
	result, err := r.db.ExecContext(ctx, query, id, httpStatus)
	if err != nil {
		return fmt.Errorf("mark delivery event sent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark delivery event sent rows: %w", err)
	}
	if affected == 0 {
		return ErrDeliveryEventNotFound
	}
	return nil
}

// MarkDeliveryEventCallbackFailed agenda retry ou marca DLQ após N tentativas.
func (r *PostgresRepository) MarkDeliveryEventCallbackFailed(
	ctx context.Context,
	id string,
	httpStatus *int,
	lastError string,
	attempts int,
	nextRetryAt *time.Time,
	toDLQ bool,
) error {
	status := model.CallbackStatusFailed
	if toDLQ {
		status = model.CallbackStatusDLQ
	}
	const query = `
		UPDATE message_delivery_events
		SET callback_status = $2,
		    callback_attempts = $3,
		    last_http_status = $4,
		    last_error = NULLIF($5, ''),
		    next_retry_at = $6,
		    sweep_claimed_at = NULL
		WHERE id = $1
		  AND callback_status = 'relaying'
	`
	result, err := r.db.ExecContext(ctx, query,
		id, string(status), attempts, httpStatus, lastError, nextRetryAt,
	)
	if err != nil {
		return fmt.Errorf("mark delivery event failed: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark delivery event failed rows: %w", err)
	}
	if affected == 0 {
		return ErrDeliveryEventNotFound
	}
	return nil
}

// ClaimStaleDeliveryEvents claima pending/failed elegíveis e órfãos relaying.
type ClaimedDeliveryEvent struct {
	model.MessageDeliveryEvent
	TargetWebhookURL string
	WebhookSecret    string
}

func (r *PostgresRepository) ClaimStaleDeliveryEvents(
	ctx context.Context,
	orphanBefore time.Time,
	batch int,
) ([]ClaimedDeliveryEvent, error) {
	const query = `
		WITH candidates AS (
			SELECT e.id
			FROM message_delivery_events e
			WHERE (
			        (e.callback_status = 'pending')
			     OR (e.callback_status = 'failed' AND e.next_retry_at IS NOT NULL AND e.next_retry_at <= NOW())
			     OR (e.callback_status = 'relaying' AND e.sweep_claimed_at IS NOT NULL AND e.sweep_claimed_at < $1)
			)
			ORDER BY COALESCE(e.next_retry_at, e.created_at) ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		),
		claimed AS (
			UPDATE message_delivery_events e
			SET callback_status = 'relaying',
			    sweep_claimed_at = NOW()
			FROM candidates
			WHERE e.id = candidates.id
			RETURNING
				e.id, e.system_id, e.connection_id, e.tenant_id, e.product_id,
				e.meta_message_id, e.recipient, e.status, e.meta_timestamp,
				e.errors_json, e.callback_status, e.callback_attempts,
				e.last_http_status, COALESCE(e.last_error, ''), e.next_retry_at,
				e.sweep_claimed_at, e.created_at
		)
		SELECT
			claimed.id::text,
			claimed.system_id::text,
			claimed.connection_id::text,
			claimed.tenant_id,
			claimed.product_id,
			claimed.meta_message_id,
			claimed.recipient,
			claimed.status,
			claimed.meta_timestamp,
			claimed.errors_json,
			claimed.callback_status,
			claimed.callback_attempts,
			claimed.last_http_status,
			claimed.last_error,
			claimed.next_retry_at,
			claimed.sweep_claimed_at,
			claimed.created_at,
			COALESCE(NULLIF(c.webhook_url, ''), NULLIF(s.webhook_url, ''), ''),
			COALESCE(c.webhook_secret, '')
		FROM claimed
		LEFT JOIN whatsapp_connections c ON c.id = claimed.connection_id
		LEFT JOIN systems s ON s.id = claimed.system_id
		ORDER BY claimed.created_at ASC
	`

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim delivery events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query, orphanBefore, batch)
	if err != nil {
		return nil, fmt.Errorf("claim stale delivery events: %w", err)
	}
	defer rows.Close()

	claimed := make([]ClaimedDeliveryEvent, 0)
	for rows.Next() {
		var row ClaimedDeliveryEvent
		var lastHTTP sql.NullInt64
		var nextRetry, sweepClaimed sql.NullTime
		var errorsJSON []byte
		if err := rows.Scan(
			&row.ID, &row.SystemID, &row.ConnectionID, &row.TenantID, &row.ProductID,
			&row.MetaMessageID, &row.Recipient, &row.Status, &row.MetaTimestamp,
			&errorsJSON, &row.CallbackStatus, &row.CallbackAttempts,
			&lastHTTP, &row.LastError, &nextRetry, &sweepClaimed, &row.CreatedAt,
			&row.TargetWebhookURL, &row.WebhookSecret,
		); err != nil {
			return nil, fmt.Errorf("scan claimed delivery event: %w", err)
		}
		row.ErrorsJSON = errorsJSON
		if lastHTTP.Valid {
			v := int(lastHTTP.Int64)
			row.LastHTTPStatus = &v
		}
		if nextRetry.Valid {
			t := nextRetry.Time
			row.NextRetryAt = &t
		}
		if sweepClaimed.Valid {
			t := sweepClaimed.Time
			row.SweepClaimedAt = &t
		}
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed delivery events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim delivery events: %w", err)
	}
	return claimed, nil
}

// ListDeliveryEventsByMetaMessageID retorna eventos do system ordenados por timestamp Meta.
func (r *PostgresRepository) ListDeliveryEventsByMetaMessageID(
	ctx context.Context,
	systemID, metaMessageID string,
) ([]model.MessageDeliveryEvent, error) {
	const query = `
		SELECT id, system_id, connection_id, tenant_id, product_id, meta_message_id,
		       recipient, status, meta_timestamp, errors_json, callback_status,
		       callback_attempts, last_http_status, COALESCE(last_error, ''),
		       next_retry_at, sweep_claimed_at, created_at
		FROM message_delivery_events
		WHERE system_id = $1 AND meta_message_id = $2
		ORDER BY meta_timestamp ASC, created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, query, systemID, metaMessageID)
	if err != nil {
		return nil, fmt.Errorf("list delivery events: %w", err)
	}
	defer rows.Close()

	events := make([]model.MessageDeliveryEvent, 0)
	for rows.Next() {
		event, err := scanDeliveryEventRow(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// DeliveryEventExistsAnywhere indica se o meta_message_id existe em qualquer system.
func (r *PostgresRepository) DeliveryEventExistsAnywhere(ctx context.Context, metaMessageID string) (bool, error) {
	const query = `
		SELECT EXISTS(
			SELECT 1 FROM message_delivery_events WHERE meta_message_id = $1
		)
	`
	var exists bool
	if err := r.db.QueryRowContext(ctx, query, metaMessageID).Scan(&exists); err != nil {
		return false, fmt.Errorf("delivery event exists: %w", err)
	}
	return exists, nil
}

// FindDeliveryEventByID carrega um evento pelo UUID.
func (r *PostgresRepository) FindDeliveryEventByID(ctx context.Context, id string) (*model.MessageDeliveryEvent, error) {
	const query = `
		SELECT id, system_id, connection_id, tenant_id, product_id, meta_message_id,
		       recipient, status, meta_timestamp, errors_json, callback_status,
		       callback_attempts, last_http_status, COALESCE(last_error, ''),
		       next_retry_at, sweep_claimed_at, created_at
		FROM message_delivery_events
		WHERE id = $1
	`
	return r.scanDeliveryEvent(r.db.QueryRowContext(ctx, query, id))
}

// ReprocessDeliveryEventFromDLQ reseta dlq → pending e zera claim para reenvio.
func (r *PostgresRepository) ReprocessDeliveryEventFromDLQ(ctx context.Context, id string) (*model.MessageDeliveryEvent, error) {
	const query = `
		UPDATE message_delivery_events
		SET callback_status = 'pending',
		    next_retry_at = NULL,
		    sweep_claimed_at = NULL,
		    last_error = NULL
		WHERE id = $1
		  AND callback_status = 'dlq'
		RETURNING
			id, system_id, connection_id, tenant_id, product_id, meta_message_id,
			recipient, status, meta_timestamp, errors_json, callback_status,
			callback_attempts, last_http_status, COALESCE(last_error, ''),
			next_retry_at, sweep_claimed_at, created_at
	`
	return r.scanDeliveryEvent(r.db.QueryRowContext(ctx, query, id))
}

func (r *PostgresRepository) scanDeliveryEvent(row *sql.Row) (*model.MessageDeliveryEvent, error) {
	var event model.MessageDeliveryEvent
	var lastHTTP sql.NullInt64
	var nextRetry, sweepClaimed sql.NullTime
	var errorsJSON []byte
	err := row.Scan(
		&event.ID, &event.SystemID, &event.ConnectionID, &event.TenantID, &event.ProductID,
		&event.MetaMessageID, &event.Recipient, &event.Status, &event.MetaTimestamp,
		&errorsJSON, &event.CallbackStatus, &event.CallbackAttempts,
		&lastHTTP, &event.LastError, &nextRetry, &sweepClaimed, &event.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeliveryEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan delivery event: %w", err)
	}
	event.ErrorsJSON = errorsJSON
	if lastHTTP.Valid {
		v := int(lastHTTP.Int64)
		event.LastHTTPStatus = &v
	}
	if nextRetry.Valid {
		t := nextRetry.Time
		event.NextRetryAt = &t
	}
	if sweepClaimed.Valid {
		t := sweepClaimed.Time
		event.SweepClaimedAt = &t
	}
	return &event, nil
}

func scanDeliveryEventRow(rows *sql.Rows) (model.MessageDeliveryEvent, error) {
	var event model.MessageDeliveryEvent
	var lastHTTP sql.NullInt64
	var nextRetry, sweepClaimed sql.NullTime
	var errorsJSON []byte
	err := rows.Scan(
		&event.ID, &event.SystemID, &event.ConnectionID, &event.TenantID, &event.ProductID,
		&event.MetaMessageID, &event.Recipient, &event.Status, &event.MetaTimestamp,
		&errorsJSON, &event.CallbackStatus, &event.CallbackAttempts,
		&lastHTTP, &event.LastError, &nextRetry, &sweepClaimed, &event.CreatedAt,
	)
	if err != nil {
		return event, fmt.Errorf("scan delivery event row: %w", err)
	}
	event.ErrorsJSON = errorsJSON
	if lastHTTP.Valid {
		v := int(lastHTTP.Int64)
		event.LastHTTPStatus = &v
	}
	if nextRetry.Valid {
		t := nextRetry.Time
		event.NextRetryAt = &t
	}
	if sweepClaimed.Valid {
		t := sweepClaimed.Time
		event.SweepClaimedAt = &t
	}
	return event, nil
}
