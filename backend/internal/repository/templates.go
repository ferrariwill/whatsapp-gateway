package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/templateutil"
)

var ErrTemplateNotFound = errors.New("whatsapp template not found")

func (r *PostgresRepository) UpsertWhatsAppTemplate(ctx context.Context, tpl *model.WhatsAppTemplate) error {
	if tpl == nil {
		return fmt.Errorf("template is nil")
	}
	status := templateutil.NormalizeTemplateStatus(tpl.Status)
	if status == "" {
		status = model.TemplateStatusPending
	}
	components := tpl.ComponentsJSON
	if len(components) == 0 {
		components = []byte("[]")
	}
	expected := tpl.ExpectedBodyParams
	if expected == 0 {
		expected = templateutil.ExpectedBodyParamCount(components)
	}

	var connectionID, metaID, category, quality any
	if strings.TrimSpace(tpl.ConnectionID) != "" {
		connectionID = tpl.ConnectionID
	}
	if strings.TrimSpace(tpl.MetaID) != "" {
		metaID = tpl.MetaID
	}
	if strings.TrimSpace(tpl.Category) != "" {
		category = tpl.Category
	}
	if strings.TrimSpace(tpl.QualityScore) != "" {
		quality = tpl.QualityScore
	}
	var syncedAt any
	if tpl.SyncedAt != nil {
		syncedAt = *tpl.SyncedAt
	} else {
		now := time.Now().UTC()
		tpl.SyncedAt = &now
		syncedAt = now
	}

	const query = `
		INSERT INTO whatsapp_templates (
			system_id, tenant_id, connection_id, waba_id, meta_id, name, language,
			category, status, components_json, expected_body_params, quality_score, synced_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10::jsonb, $11, $12, $13
		)
		ON CONFLICT (system_id, tenant_id, name, language) DO UPDATE SET
			connection_id = COALESCE(EXCLUDED.connection_id, whatsapp_templates.connection_id),
			waba_id = EXCLUDED.waba_id,
			meta_id = COALESCE(EXCLUDED.meta_id, whatsapp_templates.meta_id),
			category = COALESCE(EXCLUDED.category, whatsapp_templates.category),
			status = EXCLUDED.status,
			components_json = EXCLUDED.components_json,
			expected_body_params = EXCLUDED.expected_body_params,
			quality_score = COALESCE(EXCLUDED.quality_score, whatsapp_templates.quality_score),
			synced_at = EXCLUDED.synced_at,
			updated_at = NOW()
		RETURNING id, created_at, updated_at, expected_body_params
	`
	err := r.db.QueryRowContext(ctx, query,
		tpl.SystemID, tpl.TenantID, connectionID, tpl.WabaID, metaID, tpl.Name, tpl.Language,
		category, status, string(components), expected, quality, syncedAt,
	).Scan(&tpl.ID, &tpl.CreatedAt, &tpl.UpdatedAt, &tpl.ExpectedBodyParams)
	if err != nil {
		return fmt.Errorf("upsert whatsapp template: %w", err)
	}
	tpl.Status = status
	tpl.ComponentsJSON = components
	return nil
}

func (r *PostgresRepository) ListTemplatesBySystemTenant(
	ctx context.Context, systemID, tenantID string,
) ([]model.WhatsAppTemplate, error) {
	const query = `
		SELECT id, system_id, tenant_id, COALESCE(connection_id::text, ''), waba_id,
		       COALESCE(meta_id, ''), name, language, COALESCE(category, ''), status,
		       components_json, expected_body_params, COALESCE(quality_score, ''),
		       synced_at, created_at, updated_at
		FROM whatsapp_templates
		WHERE system_id = $1 AND tenant_id = $2
		ORDER BY name ASC, language ASC
	`
	rows, err := r.db.QueryContext(ctx, query, systemID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp templates: %w", err)
	}
	defer rows.Close()

	out := make([]model.WhatsAppTemplate, 0)
	for rows.Next() {
		tpl, err := scanWhatsAppTemplateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tpl)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) FindTemplate(
	ctx context.Context, systemID, tenantID, name, language string,
) (*model.WhatsAppTemplate, error) {
	const query = `
		SELECT id, system_id, tenant_id, COALESCE(connection_id::text, ''), waba_id,
		       COALESCE(meta_id, ''), name, language, COALESCE(category, ''), status,
		       components_json, expected_body_params, COALESCE(quality_score, ''),
		       synced_at, created_at, updated_at
		FROM whatsapp_templates
		WHERE system_id = $1 AND tenant_id = $2 AND name = $3 AND language = $4
	`
	return scanWhatsAppTemplate(r.db.QueryRowContext(ctx, query, systemID, tenantID, name, language))
}

func (r *PostgresRepository) UpdateTemplateStatusByMetaID(
	ctx context.Context, systemID, metaID, status, qualityScore string,
) (int64, error) {
	status = templateutil.NormalizeTemplateStatus(status)
	const query = `
		UPDATE whatsapp_templates
		SET status = $3,
		    quality_score = COALESCE(NULLIF($4, ''), quality_score),
		    updated_at = NOW()
		WHERE system_id = $1 AND meta_id = $2
	`
	res, err := r.db.ExecContext(ctx, query, systemID, metaID, status, qualityScore)
	if err != nil {
		return 0, fmt.Errorf("update template status by meta_id: %w", err)
	}
	return res.RowsAffected()
}

func (r *PostgresRepository) UpdateTemplateStatusByNameLang(
	ctx context.Context, systemID, tenantID, name, language, status string, componentsJSON []byte,
) (int64, error) {
	status = templateutil.NormalizeTemplateStatus(status)
	expected := templateutil.ExpectedBodyParamCount(componentsJSON)
	if len(componentsJSON) == 0 {
		const query = `
			UPDATE whatsapp_templates
			SET status = $5, updated_at = NOW()
			WHERE system_id = $1 AND tenant_id = $2 AND name = $3 AND language = $4
		`
		res, err := r.db.ExecContext(ctx, query, systemID, tenantID, name, language, status)
		if err != nil {
			return 0, fmt.Errorf("update template status by name/lang: %w", err)
		}
		return res.RowsAffected()
	}
	const query = `
		UPDATE whatsapp_templates
		SET status = $5,
		    components_json = $6::jsonb,
		    expected_body_params = $7,
		    updated_at = NOW()
		WHERE system_id = $1 AND tenant_id = $2 AND name = $3 AND language = $4
	`
	res, err := r.db.ExecContext(ctx, query, systemID, tenantID, name, language, status, string(componentsJSON), expected)
	if err != nil {
		return 0, fmt.Errorf("update template status/components by name/lang: %w", err)
	}
	return res.RowsAffected()
}

func (r *PostgresRepository) UpdateTemplateQualityByNameLang(
	ctx context.Context, systemID, tenantID, name, language, qualityScore string,
) (int64, error) {
	const query = `
		UPDATE whatsapp_templates
		SET quality_score = $5, updated_at = NOW()
		WHERE system_id = $1 AND tenant_id = $2 AND name = $3 AND language = $4
	`
	res, err := r.db.ExecContext(ctx, query, systemID, tenantID, name, language, qualityScore)
	if err != nil {
		return 0, fmt.Errorf("update template quality: %w", err)
	}
	return res.RowsAffected()
}

func (r *PostgresRepository) TryInsertWebhookDedupe(ctx context.Context, eventKey string) (bool, error) {
	eventKey = strings.TrimSpace(eventKey)
	if eventKey == "" {
		return false, fmt.Errorf("event key is required")
	}
	const query = `
		INSERT INTO webhook_event_dedupe (event_key)
		VALUES ($1)
		ON CONFLICT (event_key) DO NOTHING
	`
	res, err := r.db.ExecContext(ctx, query, eventKey)
	if err != nil {
		return false, fmt.Errorf("insert webhook dedupe: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("webhook dedupe rows affected: %w", err)
	}
	return n > 0, nil
}

func (r *PostgresRepository) UpdateConnectionTemplateSyncAudit(
	ctx context.Context, connID string, syncedAt *time.Time, errMsg, syncedBy string,
) error {
	const query = `
		UPDATE whatsapp_connections
		SET templates_synced_at = $2,
		    templates_sync_error = NULLIF($3, ''),
		    templates_synced_by = NULLIF($4, '')
		WHERE id = $1
	`
	var synced any
	if syncedAt != nil {
		synced = *syncedAt
	}
	res, err := r.db.ExecContext(ctx, query, connID, synced, errMsg, syncedBy)
	if err != nil {
		return fmt.Errorf("update connection template sync audit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update connection template sync audit rows: %w", err)
	}
	if n == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

func (r *PostgresRepository) FindConnectionsByWabaID(
	ctx context.Context, wabaID string,
) ([]model.WhatsAppConnection, error) {
	const query = `
		SELECT id, system_id, sistema_origem, tenant_id, waba_id, phone_number_id,
		       access_token, webhook_url, whatsapp_phone_number, status, created_at
		FROM whatsapp_connections
		WHERE waba_id = $1
	`
	rows, err := r.db.QueryContext(ctx, query, wabaID)
	if err != nil {
		return nil, fmt.Errorf("query connections by waba_id: %w", err)
	}
	defer rows.Close()

	out := make([]model.WhatsAppConnection, 0)
	for rows.Next() {
		conn, err := r.scanConnectionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) GetConnectionTemplateSyncAudit(
	ctx context.Context, connID string,
) (syncedAt *time.Time, syncErr, syncedBy string, err error) {
	const query = `
		SELECT templates_synced_at, COALESCE(templates_sync_error, ''), COALESCE(templates_synced_by, '')
		FROM whatsapp_connections
		WHERE id = $1
	`
	var at sql.NullTime
	err = r.db.QueryRowContext(ctx, query, connID).Scan(&at, &syncErr, &syncedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", ErrConnectionNotFound
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("get connection template sync audit: %w", err)
	}
	if at.Valid {
		t := at.Time
		syncedAt = &t
	}
	return syncedAt, syncErr, syncedBy, nil
}

func scanWhatsAppTemplate(row *sql.Row) (*model.WhatsAppTemplate, error) {
	var tpl model.WhatsAppTemplate
	var syncedAt sql.NullTime
	var components []byte
	err := row.Scan(
		&tpl.ID, &tpl.SystemID, &tpl.TenantID, &tpl.ConnectionID, &tpl.WabaID,
		&tpl.MetaID, &tpl.Name, &tpl.Language, &tpl.Category, &tpl.Status,
		&components, &tpl.ExpectedBodyParams, &tpl.QualityScore,
		&syncedAt, &tpl.CreatedAt, &tpl.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTemplateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan whatsapp template: %w", err)
	}
	tpl.ComponentsJSON = components
	if syncedAt.Valid {
		t := syncedAt.Time
		tpl.SyncedAt = &t
	}
	return &tpl, nil
}

func scanWhatsAppTemplateRow(rows *sql.Rows) (model.WhatsAppTemplate, error) {
	var tpl model.WhatsAppTemplate
	var syncedAt sql.NullTime
	var components []byte
	err := rows.Scan(
		&tpl.ID, &tpl.SystemID, &tpl.TenantID, &tpl.ConnectionID, &tpl.WabaID,
		&tpl.MetaID, &tpl.Name, &tpl.Language, &tpl.Category, &tpl.Status,
		&components, &tpl.ExpectedBodyParams, &tpl.QualityScore,
		&syncedAt, &tpl.CreatedAt, &tpl.UpdatedAt,
	)
	if err != nil {
		return tpl, fmt.Errorf("scan whatsapp template row: %w", err)
	}
	tpl.ComponentsJSON = components
	if syncedAt.Valid {
		t := syncedAt.Time
		tpl.SyncedAt = &t
	}
	return tpl, nil
}
