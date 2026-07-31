package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/whatsappgetway/gateway/internal/model"
)

var ErrConnectionNotFound = errors.New("whatsapp connection not found")

const connectionColumns = `
	id, system_id, sistema_origem, tenant_id, waba_id, phone_number_id,
	access_token, webhook_url, COALESCE(webhook_secret, ''), whatsapp_phone_number, status, created_at
`

func (r *PostgresRepository) CreateWhatsAppConnection(ctx context.Context, conn *model.WhatsAppConnection) error {
	const query = `
		INSERT INTO whatsapp_connections (
			system_id, sistema_origem, tenant_id, waba_id, phone_number_id,
			access_token, webhook_url, webhook_secret, whatsapp_phone_number, status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10)
		RETURNING id, created_at
	`
	status := conn.Status
	if status == "" {
		status = model.ConnectionStatusActive
	}
	var webhookURL, phone any
	if conn.WebhookURL != "" {
		webhookURL = conn.WebhookURL
	}
	if conn.WhatsAppPhoneNumber != "" {
		phone = conn.WhatsAppPhoneNumber
	}

	err := r.db.QueryRowContext(ctx, query,
		conn.SystemID, conn.SistemaOrigem, conn.TenantID, conn.WabaID, conn.PhoneNumberID,
		conn.AccessToken, webhookURL, conn.WebhookSecret, phone, status,
	).Scan(&conn.ID, &conn.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert whatsapp connection: %w", err)
	}
	return nil
}

func (r *PostgresRepository) FindConnectionBySystemAndTenant(
	ctx context.Context, systemID, tenantID string,
) (*model.WhatsAppConnection, error) {
	query := `SELECT ` + connectionColumns + `
		FROM whatsapp_connections
		WHERE system_id = $1 AND tenant_id = $2
	`
	return r.scanConnection(r.db.QueryRowContext(ctx, query, systemID, tenantID))
}

func (r *PostgresRepository) FindConnectionBySistemaAndTenant(
	ctx context.Context, sistemaOrigem, tenantID string,
) (*model.WhatsAppConnection, error) {
	query := `SELECT ` + connectionColumns + `
		FROM whatsapp_connections
		WHERE sistema_origem = $1 AND tenant_id = $2
	`
	return r.scanConnection(r.db.QueryRowContext(ctx, query, sistemaOrigem, tenantID))
}

func (r *PostgresRepository) FindConnectionByPhoneNumberID(
	ctx context.Context, phoneNumberID string,
) (*model.WhatsAppConnection, error) {
	query := `SELECT ` + connectionColumns + `
		FROM whatsapp_connections
		WHERE phone_number_id = $1
	`
	return r.scanConnection(r.db.QueryRowContext(ctx, query, phoneNumberID))
}

func (r *PostgresRepository) FindConnectionByID(ctx context.Context, id string) (*model.WhatsAppConnection, error) {
	query := `SELECT ` + connectionColumns + `
		FROM whatsapp_connections
		WHERE id = $1
	`
	return r.scanConnection(r.db.QueryRowContext(ctx, query, id))
}

func (r *PostgresRepository) ListWhatsAppConnections(ctx context.Context) ([]model.WhatsAppConnection, error) {
	query := `SELECT ` + connectionColumns + `
		FROM whatsapp_connections
		ORDER BY created_at DESC
	`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query whatsapp connections: %w", err)
	}
	defer rows.Close()

	connections := make([]model.WhatsAppConnection, 0)
	for rows.Next() {
		conn, err := r.scanConnectionRow(rows)
		if err != nil {
			return nil, err
		}
		connections = append(connections, conn)
	}
	return connections, rows.Err()
}

func (r *PostgresRepository) UpsertWhatsAppConnection(ctx context.Context, conn *model.WhatsAppConnection) error {
	const query = `
		INSERT INTO whatsapp_connections (
			system_id, sistema_origem, tenant_id, waba_id, phone_number_id,
			access_token, webhook_url, webhook_secret, whatsapp_phone_number, status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10)
		ON CONFLICT (system_id, tenant_id) DO UPDATE SET
			sistema_origem = EXCLUDED.sistema_origem,
			waba_id = EXCLUDED.waba_id,
			phone_number_id = EXCLUDED.phone_number_id,
			access_token = EXCLUDED.access_token,
			webhook_url = COALESCE(NULLIF(EXCLUDED.webhook_url, ''), whatsapp_connections.webhook_url),
			webhook_secret = COALESCE(NULLIF(EXCLUDED.webhook_secret, ''), whatsapp_connections.webhook_secret),
			whatsapp_phone_number = EXCLUDED.whatsapp_phone_number,
			status = EXCLUDED.status
		RETURNING id, created_at
	`
	status := conn.Status
	if status == "" {
		status = model.ConnectionStatusActive
	}
	var webhookURL, phone any
	if conn.WebhookURL != "" {
		webhookURL = conn.WebhookURL
	}
	if conn.WhatsAppPhoneNumber != "" {
		phone = conn.WhatsAppPhoneNumber
	}

	err := r.db.QueryRowContext(ctx, query,
		conn.SystemID, conn.SistemaOrigem, conn.TenantID, conn.WabaID, conn.PhoneNumberID,
		conn.AccessToken, webhookURL, conn.WebhookSecret, phone, status,
	).Scan(&conn.ID, &conn.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert whatsapp connection: %w", err)
	}
	return nil
}

func (r *PostgresRepository) UpdateWhatsAppConnection(ctx context.Context, conn *model.WhatsAppConnection) error {
	const query = `
		UPDATE whatsapp_connections
		SET system_id = $2, sistema_origem = $3, tenant_id = $4, waba_id = $5, phone_number_id = $6,
		    access_token = $7, webhook_url = NULLIF($8, ''),
		    webhook_secret = COALESCE(NULLIF($9, ''), webhook_secret),
		    whatsapp_phone_number = NULLIF($10, ''), status = $11
		WHERE id = $1
		RETURNING created_at
	`
	status := conn.Status
	if status == "" {
		status = model.ConnectionStatusActive
	}
	err := r.db.QueryRowContext(ctx, query,
		conn.ID, conn.SystemID, conn.SistemaOrigem, conn.TenantID, conn.WabaID, conn.PhoneNumberID,
		conn.AccessToken, conn.WebhookURL, conn.WebhookSecret, conn.WhatsAppPhoneNumber, status,
	).Scan(&conn.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return fmt.Errorf("update whatsapp connection: %w", err)
	}
	return nil
}

func (r *PostgresRepository) DeleteWhatsAppConnection(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM whatsapp_connections WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete whatsapp connection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete whatsapp connection rows affected: %w", err)
	}
	if rows == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

func (r *PostgresRepository) SuspendConnectionForSpam(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE whatsapp_connections SET status = $2 WHERE id = $1`,
		id, model.ConnectionStatusSuspendedSpam,
	)
	if err != nil {
		return fmt.Errorf("suspend connection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("suspend connection rows affected: %w", err)
	}
	if rows == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

func (r *PostgresRepository) ActivateConnection(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE whatsapp_connections SET status = $2 WHERE id = $1`,
		id, model.ConnectionStatusActive,
	)
	if err != nil {
		return fmt.Errorf("activate connection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("activate connection rows affected: %w", err)
	}
	if rows == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

func (r *PostgresRepository) scanConnection(row *sql.Row) (*model.WhatsAppConnection, error) {
	var conn model.WhatsAppConnection
	var webhookURL, phone sql.NullString
	err := row.Scan(
		&conn.ID, &conn.SystemID, &conn.SistemaOrigem, &conn.TenantID, &conn.WabaID, &conn.PhoneNumberID,
		&conn.AccessToken, &webhookURL, &conn.WebhookSecret, &phone, &conn.Status, &conn.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan whatsapp connection: %w", err)
	}
	if webhookURL.Valid {
		conn.WebhookURL = webhookURL.String
	}
	if phone.Valid {
		conn.WhatsAppPhoneNumber = phone.String
	}
	return &conn, nil
}

func (r *PostgresRepository) scanConnectionRow(rows *sql.Rows) (model.WhatsAppConnection, error) {
	var conn model.WhatsAppConnection
	var webhookURL, phone sql.NullString
	err := rows.Scan(
		&conn.ID, &conn.SystemID, &conn.SistemaOrigem, &conn.TenantID, &conn.WabaID, &conn.PhoneNumberID,
		&conn.AccessToken, &webhookURL, &conn.WebhookSecret, &phone, &conn.Status, &conn.CreatedAt,
	)
	if err != nil {
		return conn, fmt.Errorf("scan whatsapp connection row: %w", err)
	}
	if webhookURL.Valid {
		conn.WebhookURL = webhookURL.String
	}
	if phone.Valid {
		conn.WhatsAppPhoneNumber = phone.String
	}
	return conn, nil
}
