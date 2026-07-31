package repository

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UsageDelta descreve o incremento atômico a aplicar em usage_counters.
type UsageDelta struct {
	SentText                 int64
	SentTemplate             int64
	SentMedia                int64
	Inbound                  int64
	StatusCallbackDelivered  int64
	Errors4xx                int64
	Errors5xx                int64
}

// UsageCounterRow é uma linha diária de usage_counters.
type UsageCounterRow struct {
	SystemID                 string
	TenantID                 string
	UsageDate                time.Time
	SentText                 int64
	SentTemplate             int64
	SentMedia                int64
	Inbound                  int64
	StatusCallbackDelivered  int64
	Errors4xx                int64
	Errors5xx                int64
}

// IsZero retorna true se nenhum contador da delta é positivo.
func (d UsageDelta) IsZero() bool {
	return d.SentText == 0 && d.SentTemplate == 0 && d.SentMedia == 0 &&
		d.Inbound == 0 && d.StatusCallbackDelivered == 0 &&
		d.Errors4xx == 0 && d.Errors5xx == 0
}

// IncrementUsageCounters faz UPSERT atômico somando a delta no dia UTC.
func (r *PostgresRepository) IncrementUsageCounters(
	ctx context.Context,
	systemID, tenantID string,
	dayUTC time.Time,
	d UsageDelta,
) error {
	if strings.TrimSpace(systemID) == "" || strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("system_id and tenant_id are required")
	}
	if d.IsZero() {
		return nil
	}
	day := dayUTC.UTC().Truncate(24 * time.Hour)

	const query = `
		INSERT INTO usage_counters (
			system_id, tenant_id, usage_date,
			sent_text, sent_template, sent_media,
			inbound, status_callback_delivered,
			errors_4xx, errors_5xx
		) VALUES (
			$1, $2, $3::date,
			$4, $5, $6,
			$7, $8,
			$9, $10
		)
		ON CONFLICT (system_id, tenant_id, usage_date) DO UPDATE SET
			sent_text = usage_counters.sent_text + EXCLUDED.sent_text,
			sent_template = usage_counters.sent_template + EXCLUDED.sent_template,
			sent_media = usage_counters.sent_media + EXCLUDED.sent_media,
			inbound = usage_counters.inbound + EXCLUDED.inbound,
			status_callback_delivered = usage_counters.status_callback_delivered + EXCLUDED.status_callback_delivered,
			errors_4xx = usage_counters.errors_4xx + EXCLUDED.errors_4xx,
			errors_5xx = usage_counters.errors_5xx + EXCLUDED.errors_5xx
	`
	_, err := r.db.ExecContext(ctx, query,
		systemID, tenantID, day,
		d.SentText, d.SentTemplate, d.SentMedia,
		d.Inbound, d.StatusCallbackDelivered,
		d.Errors4xx, d.Errors5xx,
	)
	if err != nil {
		return fmt.Errorf("increment usage counters: %w", err)
	}
	return nil
}

// TryRecordStatusDelivered grava idempotência de status delivered.
// Retorna inserted=true apenas na primeira vez para (system, meta_message_id, delivered).
func (r *PostgresRepository) TryRecordStatusDelivered(
	ctx context.Context,
	systemID, metaMessageID string,
) (inserted bool, err error) {
	if strings.TrimSpace(systemID) == "" || strings.TrimSpace(metaMessageID) == "" {
		return false, fmt.Errorf("system_id and meta_message_id are required")
	}
	const query = `
		INSERT INTO usage_status_dedup (system_id, meta_message_id, status)
		VALUES ($1, $2, 'delivered')
		ON CONFLICT (system_id, meta_message_id, status) DO NOTHING
	`
	result, err := r.db.ExecContext(ctx, query, systemID, metaMessageID)
	if err != nil {
		return false, fmt.Errorf("record usage status dedup: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected usage status dedup: %w", err)
	}
	return affected > 0, nil
}

// QueryUsageRange lista dias com contadores para um product+tenant no intervalo [from, to] inclusivo.
func (r *PostgresRepository) QueryUsageRange(
	ctx context.Context,
	systemID, tenantID string,
	from, to time.Time,
) ([]UsageCounterRow, error) {
	if strings.TrimSpace(systemID) == "" || strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("system_id and tenant_id are required")
	}
	const query = `
		SELECT system_id::text, tenant_id, usage_date,
		       sent_text, sent_template, sent_media,
		       inbound, status_callback_delivered,
		       errors_4xx, errors_5xx
		FROM usage_counters
		WHERE system_id = $1
		  AND tenant_id = $2
		  AND usage_date >= $3::date
		  AND usage_date <= $4::date
		ORDER BY usage_date ASC
	`
	return r.scanUsageCounterRows(ctx, query,
		systemID, tenantID,
		from.UTC().Truncate(24*time.Hour),
		to.UTC().Truncate(24*time.Hour),
	)
}

// QueryUsageRangeAdmin lista contadores com filtros opcionais de system/tenant.
func (r *PostgresRepository) QueryUsageRangeAdmin(
	ctx context.Context,
	systemID, tenantID string,
	from, to time.Time,
) ([]UsageCounterRow, error) {
	fromDay := from.UTC().Truncate(24 * time.Hour)
	toDay := to.UTC().Truncate(24 * time.Hour)

	var (
		b    strings.Builder
		args []any
	)
	b.WriteString(`
		SELECT system_id::text, tenant_id, usage_date,
		       sent_text, sent_template, sent_media,
		       inbound, status_callback_delivered,
		       errors_4xx, errors_5xx
		FROM usage_counters
		WHERE usage_date >= $1::date
		  AND usage_date <= $2::date
	`)
	args = append(args, fromDay, toDay)
	idx := 3
	if sid := strings.TrimSpace(systemID); sid != "" {
		fmt.Fprintf(&b, " AND system_id = $%d", idx)
		args = append(args, sid)
		idx++
	}
	if tid := strings.TrimSpace(tenantID); tid != "" {
		fmt.Fprintf(&b, " AND tenant_id = $%d", idx)
		args = append(args, tid)
	}
	b.WriteString(" ORDER BY usage_date ASC, system_id ASC, tenant_id ASC")

	return r.scanUsageCounterRows(ctx, b.String(), args...)
}

func (r *PostgresRepository) scanUsageCounterRows(
	ctx context.Context,
	query string,
	args ...any,
) ([]UsageCounterRow, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage counters: %w", err)
	}
	defer rows.Close()

	out := make([]UsageCounterRow, 0)
	for rows.Next() {
		var row UsageCounterRow
		if err := rows.Scan(
			&row.SystemID, &row.TenantID, &row.UsageDate,
			&row.SentText, &row.SentTemplate, &row.SentMedia,
			&row.Inbound, &row.StatusCallbackDelivered,
			&row.Errors4xx, &row.Errors5xx,
		); err != nil {
			return nil, fmt.Errorf("scan usage counter: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage counters: %w", err)
	}
	return out, nil
}
