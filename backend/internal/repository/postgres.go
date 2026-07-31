package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

var (
	ErrSystemNotFound        = errors.New("system not found")
	ErrSystemSlugNotFound    = errors.New("system slug not found")
	ErrDuplicateSystemSlug   = errors.New("duplicate system slug")
	ErrUserNotFound          = errors.New("user not found")
	ErrClientChannelNotFound = errors.New("client channel not found")
	ErrMessageLogNotFound    = errors.New("message log not found")
	ErrDuplicateMessageLog   = errors.New("duplicate message log")
)

type PostgresRepository struct {
	db *sql.DB
}

func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

func (r *PostgresRepository) FindSystemByAPIKeyHash(ctx context.Context, apiKeyHash string) (*model.System, error) {
	const query = `
		SELECT id, name, slug, api_key_hash, webhook_url, created_at
		FROM systems WHERE api_key_hash = $1
	`
	return r.scanSystem(r.db.QueryRowContext(ctx, query, apiKeyHash))
}

func (r *PostgresRepository) FindSystemBySlug(ctx context.Context, slug string) (*model.System, error) {
	const query = `
		SELECT id, name, slug, api_key_hash, webhook_url, created_at
		FROM systems WHERE slug = $1
	`
	system, err := r.scanSystem(r.db.QueryRowContext(ctx, query, slug))
	if errors.Is(err, ErrSystemNotFound) {
		return nil, ErrSystemSlugNotFound
	}
	return system, err
}

func (r *PostgresRepository) GetDefaultSystem(ctx context.Context) (*model.System, error) {
	const query = `
		SELECT id, name, slug, api_key_hash, webhook_url, created_at
		FROM systems ORDER BY created_at ASC LIMIT 1
	`
	return r.scanSystem(r.db.QueryRowContext(ctx, query))
}

func (r *PostgresRepository) FindUserByEmail(ctx context.Context, email string) (*model.User, error) {
	const query = `SELECT id, email, password_hash, created_at FROM users WHERE LOWER(email) = LOWER($1)`
	var user model.User
	err := r.db.QueryRowContext(ctx, query, email).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query user by email: %w", err)
	}
	return &user, nil
}

func (r *PostgresRepository) CreateSystem(ctx context.Context, system *model.System) error {
	const query = `
		INSERT INTO systems (name, slug, api_key_hash, webhook_url)
		VALUES ($1, $2, $3, $4) RETURNING id, created_at
	`
	var webhookURL any
	if system.WebhookURL != "" {
		webhookURL = system.WebhookURL
	}
	err := r.db.QueryRowContext(ctx, query, system.Name, system.Slug, system.APIKeyHash, webhookURL).Scan(&system.ID, &system.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateSystemSlug
		}
		return fmt.Errorf("insert system: %w", err)
	}
	return nil
}

func (r *PostgresRepository) ListSystems(ctx context.Context) ([]model.System, error) {
	const query = `
		SELECT id, name, slug, api_key_hash, webhook_url, created_at
		FROM systems ORDER BY created_at DESC
	`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query systems: %w", err)
	}
	defer rows.Close()

	systems := make([]model.System, 0)
	for rows.Next() {
		system, err := r.scanSystemRow(rows)
		if err != nil {
			return nil, err
		}
		systems = append(systems, system)
	}
	return systems, rows.Err()
}

func (r *PostgresRepository) FindSystemByID(ctx context.Context, systemID string) (*model.System, error) {
	const query = `
		SELECT id, name, slug, api_key_hash, webhook_url, created_at
		FROM systems WHERE id = $1
	`
	return r.scanSystem(r.db.QueryRowContext(ctx, query, systemID))
}

func (r *PostgresRepository) UpdateSystem(ctx context.Context, systemID, name, slug, webhookURL string) error {
	const query = `
		UPDATE systems
		SET name = $2, slug = $3, webhook_url = NULLIF($4, '')
		WHERE id = $1
	`
	result, err := r.db.ExecContext(ctx, query, systemID, name, slug, webhookURL)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateSystemSlug
		}
		return fmt.Errorf("update system: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update system rows affected: %w", err)
	}
	if rows == 0 {
		return ErrSystemNotFound
	}
	return nil
}

func (r *PostgresRepository) DeleteSystem(ctx context.Context, systemID string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM systems WHERE id = $1`, systemID)
	if err != nil {
		return fmt.Errorf("delete system: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete system rows affected: %w", err)
	}
	if rows == 0 {
		return ErrSystemNotFound
	}
	return nil
}

func (r *PostgresRepository) UpdateClientChannel(ctx context.Context, channel *model.ClientChannel) error {
	const query = `
		UPDATE client_channels
		SET system_id = $2, salon_name = $3, external_client_id = $4,
		    phone_number_id = $5, whatsapp_phone_number = $6
		WHERE id = $1
		RETURNING status, created_at
	`
	err := r.db.QueryRowContext(ctx, query,
		channel.ID, channel.SystemID, channel.SalonName, channel.ExternalClientID,
		channel.PhoneNumberID, channel.WhatsAppPhoneNumber,
	).Scan(&channel.Status, &channel.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClientChannelNotFound
	}
	if err != nil {
		return fmt.Errorf("update client channel: %w", err)
	}
	return nil
}

func (r *PostgresRepository) DeleteClientChannel(ctx context.Context, channelID string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM client_channels WHERE id = $1`, channelID)
	if err != nil {
		return fmt.Errorf("delete client channel: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete client channel rows affected: %w", err)
	}
	if rows == 0 {
		return ErrClientChannelNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateClientChannel(ctx context.Context, channel *model.ClientChannel) error {
	const query = `
		INSERT INTO client_channels (system_id, salon_name, external_client_id, phone_number_id, whatsapp_phone_number)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, status, created_at
	`
	err := r.db.QueryRowContext(ctx, query,
		channel.SystemID, channel.SalonName, channel.ExternalClientID,
		channel.PhoneNumberID, channel.WhatsAppPhoneNumber,
	).Scan(&channel.ID, &channel.Status, &channel.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert client channel: %w", err)
	}
	return nil
}

func (r *PostgresRepository) FindClientChannelByPhoneNumberID(ctx context.Context, phoneNumberID string) (*model.ClientChannelWithSystem, error) {
	const query = `
		SELECT cc.id, cc.system_id, cc.salon_name, cc.external_client_id, cc.phone_number_id,
		       cc.whatsapp_phone_number, cc.status, cc.created_at, s.name, s.webhook_url
		FROM client_channels cc
		INNER JOIN systems s ON s.id = cc.system_id
		WHERE cc.phone_number_id = $1
	`
	return r.scanClientChannelWithSystem(ctx, query, phoneNumberID)
}

func (r *PostgresRepository) FindClientChannelBySystemAndExternalClientID(
	ctx context.Context, systemID, externalClientID string,
) (*model.ClientChannel, error) {
	const query = `
		SELECT id, system_id, salon_name, external_client_id, phone_number_id, whatsapp_phone_number, status, created_at
		FROM client_channels WHERE system_id = $1 AND external_client_id = $2
	`
	var ch model.ClientChannel
	err := r.db.QueryRowContext(ctx, query, systemID, externalClientID).Scan(
		&ch.ID, &ch.SystemID, &ch.SalonName, &ch.ExternalClientID,
		&ch.PhoneNumberID, &ch.WhatsAppPhoneNumber, &ch.Status, &ch.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClientChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query client channel: %w", err)
	}
	return &ch, nil
}

func (r *PostgresRepository) ListClientChannels(ctx context.Context) ([]model.ClientChannelWithSystem, error) {
	const query = `
		SELECT cc.id, cc.system_id, cc.salon_name, cc.external_client_id, cc.phone_number_id,
		       cc.whatsapp_phone_number, cc.status, cc.created_at, s.name, s.webhook_url
		FROM client_channels cc
		INNER JOIN systems s ON s.id = cc.system_id
		ORDER BY cc.created_at DESC
	`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query client channels: %w", err)
	}
	defer rows.Close()

	channels := make([]model.ClientChannelWithSystem, 0)
	for rows.Next() {
		ch, err := r.scanClientChannelRow(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (r *PostgresRepository) scanClientChannelWithSystem(ctx context.Context, query string, arg any) (*model.ClientChannelWithSystem, error) {
	var ch model.ClientChannelWithSystem
	var webhookURL sql.NullString
	err := r.db.QueryRowContext(ctx, query, arg).Scan(
		&ch.ID, &ch.SystemID, &ch.SalonName, &ch.ExternalClientID, &ch.PhoneNumberID,
		&ch.WhatsAppPhoneNumber, &ch.Status, &ch.CreatedAt, &ch.SystemName, &webhookURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClientChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan client channel: %w", err)
	}
	if webhookURL.Valid {
		ch.WebhookURL = webhookURL.String
	}
	return &ch, nil
}

func (r *PostgresRepository) scanClientChannelRow(rows *sql.Rows) (model.ClientChannelWithSystem, error) {
	var ch model.ClientChannelWithSystem
	var webhookURL sql.NullString
	err := rows.Scan(
		&ch.ID, &ch.SystemID, &ch.SalonName, &ch.ExternalClientID, &ch.PhoneNumberID,
		&ch.WhatsAppPhoneNumber, &ch.Status, &ch.CreatedAt, &ch.SystemName, &webhookURL,
	)
	if err != nil {
		return ch, fmt.Errorf("scan client channel row: %w", err)
	}
	if webhookURL.Valid {
		ch.WebhookURL = webhookURL.String
	}
	return ch, nil
}

func (r *PostgresRepository) FindClientChannelByID(ctx context.Context, channelID string) (*model.ClientChannelWithSystem, error) {
	const query = `
		SELECT cc.id, cc.system_id, cc.salon_name, cc.external_client_id, cc.phone_number_id,
		       cc.whatsapp_phone_number, cc.status, cc.created_at, s.name, s.webhook_url
		FROM client_channels cc
		INNER JOIN systems s ON s.id = cc.system_id
		WHERE cc.id = $1
	`
	return r.scanClientChannelWithSystem(ctx, query, channelID)
}

func (r *PostgresRepository) SuspendClientChannelForSpam(ctx context.Context, channelID string) error {
	const query = `
		UPDATE client_channels
		SET status = 'SUSPENDED_SPAM'
		WHERE id = $1
	`
	result, err := r.db.ExecContext(ctx, query, channelID)
	if err != nil {
		return fmt.Errorf("suspend client channel: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("suspend client channel rows affected: %w", err)
	}
	if rows == 0 {
		return ErrClientChannelNotFound
	}
	return nil
}

func (r *PostgresRepository) ActivateClientChannel(ctx context.Context, channelID string) error {
	const query = `
		UPDATE client_channels
		SET status = 'ACTIVE'
		WHERE id = $1
	`
	result, err := r.db.ExecContext(ctx, query, channelID)
	if err != nil {
		return fmt.Errorf("activate client channel: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("activate client channel rows affected: %w", err)
	}
	if rows == 0 {
		return ErrClientChannelNotFound
	}
	return nil
}

type MessageLogWithSystem struct {
	model.MessageLog
	SystemName string
}

func (r *PostgresRepository) ListRecentMessageLogs(ctx context.Context, limit int) ([]MessageLogWithSystem, error) {
	const query = `
		SELECT ml.id, ml.system_id, ml.external_client_id, ml.meta_message_id, ml.appointment_id,
		       ml.phone_number, ml.template_name, ml.sent_content, ml.received_content, ml.direction,
		       ml.message_category, ml.status, ml.meta_cost, ml.delivered_at, ml.created_at,
		       ml.failure_reason, s.name
		FROM message_logs ml INNER JOIN systems s ON s.id = ml.system_id
		ORDER BY ml.created_at DESC LIMIT $1
	`
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query message logs: %w", err)
	}
	defer rows.Close()

	logs := make([]MessageLogWithSystem, 0)
	for rows.Next() {
		entry, err := scanMessageLogWithSystem(rows)
		if err != nil {
			return nil, err
		}
		logs = append(logs, entry)
	}
	return logs, rows.Err()
}

func scanMessageLogWithSystem(scanner interface {
	Scan(dest ...any) error
}) (MessageLogWithSystem, error) {
	var entry MessageLogWithSystem
	var externalClientID, metaMessageID, messageCategory, failureReason sql.NullString
	var templateName, sentContent, receivedContent, direction sql.NullString
	var deliveredAt sql.NullTime
	err := scanner.Scan(
		&entry.ID, &entry.SystemID, &externalClientID, &metaMessageID, &entry.AppointmentID,
		&entry.PhoneNumber, &templateName, &sentContent, &receivedContent, &direction,
		&messageCategory, &entry.Status, &entry.MetaCost, &deliveredAt, &entry.CreatedAt,
		&failureReason, &entry.SystemName,
	)
	if err != nil {
		return entry, fmt.Errorf("scan message log: %w", err)
	}
	if externalClientID.Valid {
		entry.ExternalClientID = externalClientID.String
	}
	if metaMessageID.Valid {
		entry.MetaMessageID = metaMessageID.String
	}
	if templateName.Valid {
		entry.TemplateName = templateName.String
	}
	if sentContent.Valid {
		entry.SentContent = sentContent.String
	}
	if receivedContent.Valid {
		entry.ReceivedContent = receivedContent.String
	}
	if direction.Valid {
		entry.Direction = model.MessageDirection(direction.String)
	} else {
		entry.Direction = model.MessageDirectionOutbound
	}
	if messageCategory.Valid {
		entry.MessageCategory = model.MessageCategory(messageCategory.String)
	}
	if deliveredAt.Valid {
		entry.DeliveredAt = &deliveredAt.Time
	}
	if failureReason.Valid {
		entry.FailureReason = failureReason.String
	}
	return entry, nil
}

func (r *PostgresRepository) FindHighVolumeClientKeys(
	ctx context.Context,
	windowMinutes int,
	threshold int64,
) (map[string]struct{}, error) {
	const query = `
		SELECT system_id, external_client_id
		FROM message_logs
		WHERE direction = 'OUTBOUND'
		  AND status IN ('sent', 'delivered', 'failed')
		  AND external_client_id IS NOT NULL
		  AND external_client_id <> ''
		  AND created_at >= NOW() AT TIME ZONE 'UTC' - ($1 * INTERVAL '1 minute')
		GROUP BY system_id, external_client_id
		HAVING COUNT(*) > $2
	`
	rows, err := r.db.QueryContext(ctx, query, windowMinutes, threshold)
	if err != nil {
		return nil, fmt.Errorf("find high volume clients: %w", err)
	}
	defer rows.Close()

	keys := make(map[string]struct{})
	for rows.Next() {
		var systemID, externalClientID string
		if err := rows.Scan(&systemID, &externalClientID); err != nil {
			return nil, fmt.Errorf("scan high volume client: %w", err)
		}
		keys[systemID+"|"+externalClientID] = struct{}{}
	}
	return keys, rows.Err()
}

func (r *PostgresRepository) CreateMessageLog(ctx context.Context, log *model.MessageLog) error {
	if log.Direction == "" {
		log.Direction = model.MessageDirectionOutbound
	}
	const query = `
		INSERT INTO message_logs (
			system_id, connection_id, sistema_origem, external_client_id, meta_message_id,
			appointment_id, phone_number, template_name, sent_content, received_content,
			direction, message_category, status, meta_cost, inbound_payload, failure_reason
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id, created_at
	`
	var systemID, connectionID, sistemaOrigem, externalClientID, metaMessageID, messageCategory, templateName, sentContent, receivedContent, inboundPayload, failureReason any
	if log.SystemID != "" {
		systemID = log.SystemID
	}
	if log.ConnectionID != "" {
		connectionID = log.ConnectionID
	}
	if log.SistemaOrigem != "" {
		sistemaOrigem = log.SistemaOrigem
	}
	if log.ExternalClientID != "" {
		externalClientID = log.ExternalClientID
	}
	if log.MetaMessageID != "" {
		metaMessageID = log.MetaMessageID
	}
	if log.TemplateName != "" {
		templateName = log.TemplateName
	}
	if log.SentContent != "" {
		sentContent = log.SentContent
	}
	if log.ReceivedContent != "" {
		receivedContent = log.ReceivedContent
	}
	if log.MessageCategory != "" {
		messageCategory = string(log.MessageCategory)
	}
	if len(log.InboundPayload) > 0 {
		inboundPayload = log.InboundPayload
	}
	if log.FailureReason != "" {
		failureReason = log.FailureReason
	}

	err := r.db.QueryRowContext(ctx, query,
		systemID, connectionID, sistemaOrigem, externalClientID, metaMessageID,
		log.AppointmentID, log.PhoneNumber, templateName, sentContent, receivedContent,
		string(log.Direction), messageCategory, log.Status, log.MetaCost, inboundPayload,
		failureReason,
	).Scan(&log.ID, &log.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateMessageLog
		}
		return fmt.Errorf("insert message log: %w", err)
	}
	return nil
}

type ClientUsageStats struct {
	TotalSent        int64
	TotalDelivered   int64
	TotalFailed      int64
	UtilityDelivered int64
}

type ApplicationClientUsageRow struct {
	SystemID         string
	SystemName       string
	ExternalClientID string
	Stats            ClientUsageStats
}

func (r *PostgresRepository) AggregateUsageVolumeByApplicationAndClient(
	ctx context.Context,
	month, year int,
) ([]ApplicationClientUsageRow, error) {
	const query = `
		SELECT
			s.id AS system_id,
			s.name AS system_name,
			ml.external_client_id,
			COUNT(*) FILTER (WHERE ml.status IN ('sent', 'delivered', 'failed') AND ml.direction = 'OUTBOUND')::bigint,
			COUNT(*) FILTER (WHERE ml.status = 'delivered')::bigint,
			COUNT(*) FILTER (WHERE ml.status = 'failed')::bigint,
			COUNT(*) FILTER (
				WHERE ml.status = 'delivered'
				  AND UPPER(COALESCE(ml.message_category, 'UTILITY')) = 'UTILITY'
			)::bigint
		FROM message_logs ml
		INNER JOIN systems s ON s.id = ml.system_id
		WHERE ml.external_client_id IS NOT NULL
		  AND ml.external_client_id <> ''
		  AND EXTRACT(MONTH FROM ml.created_at AT TIME ZONE 'UTC') = $1
		  AND EXTRACT(YEAR FROM ml.created_at AT TIME ZONE 'UTC') = $2
		GROUP BY s.id, s.name, ml.external_client_id
		ORDER BY s.name ASC, ml.external_client_id ASC
	`

	rows, err := r.db.QueryContext(ctx, query, month, year)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage by application and client: %w", err)
	}
	defer rows.Close()

	results := make([]ApplicationClientUsageRow, 0)
	for rows.Next() {
		var row ApplicationClientUsageRow
		if err := rows.Scan(
			&row.SystemID, &row.SystemName, &row.ExternalClientID,
			&row.Stats.TotalSent, &row.Stats.TotalDelivered, &row.Stats.TotalFailed, &row.Stats.UtilityDelivered,
		); err != nil {
			return nil, fmt.Errorf("scan application client usage row: %w", err)
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func (r *PostgresRepository) AggregateClientUsageStats(
	ctx context.Context,
	systemID, externalClientID string,
	month, year int,
) (ClientUsageStats, error) {
	const query = `
		SELECT
			COUNT(*) FILTER (WHERE status IN ('sent', 'delivered', 'failed') AND direction = 'OUTBOUND')::bigint,
			COUNT(*) FILTER (WHERE status = 'delivered')::bigint,
			COUNT(*) FILTER (WHERE status = 'failed')::bigint,
			COUNT(*) FILTER (
				WHERE status = 'delivered'
				  AND UPPER(COALESCE(message_category, 'UTILITY')) = 'UTILITY'
			)::bigint
		FROM message_logs
		WHERE system_id = $1
		  AND external_client_id = $2
		  AND EXTRACT(MONTH FROM created_at AT TIME ZONE 'UTC') = $3
		  AND EXTRACT(YEAR FROM created_at AT TIME ZONE 'UTC') = $4
	`

	var stats ClientUsageStats
	err := r.db.QueryRowContext(ctx, query, systemID, externalClientID, month, year).Scan(
		&stats.TotalSent, &stats.TotalDelivered, &stats.TotalFailed, &stats.UtilityDelivered,
	)
	if err != nil {
		return ClientUsageStats{}, fmt.Errorf("aggregate client usage stats: %w", err)
	}
	return stats, nil
}

func (r *PostgresRepository) CountMonthlyMessagesForClient(
	ctx context.Context,
	systemID, externalClientID string,
	month, year int,
) (int64, error) {
	const query = `
		SELECT COUNT(*)::bigint
		FROM message_logs
		WHERE system_id = $1
		  AND external_client_id = $2
		  AND direction = 'OUTBOUND'
		  AND status IN ('sent', 'delivered', 'failed')
		  AND EXTRACT(MONTH FROM created_at AT TIME ZONE 'UTC') = $3
		  AND EXTRACT(YEAR FROM created_at AT TIME ZONE 'UTC') = $4
	`

	var count int64
	err := r.db.QueryRowContext(ctx, query, systemID, externalClientID, month, year).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count monthly messages: %w", err)
	}
	return count, nil
}

// PendingInboundLog é uma linha inbound claimada pelo sweep para reprocessar o
// repasse. TargetWebhookURL resolve conexão → aplicação mãe (sem fallback global).
type PendingInboundLog struct {
	ID               string
	SystemID         string
	ConnectionID     string
	SistemaOrigem    string
	ExternalClientID string
	MetaMessageID    string
	PhoneNumber      string
	EventType        string
	ReceivedContent  string
	TargetWebhookURL string
	InboundPayload   []byte
	RelayAttempts    int
	CreatedAt        time.Time
	SweepClaimedAt   time.Time
}

// ClaimStalePendingInboundLogs claima atomicamente um lote de linhas inbound
// elegíveis: pending mais velhas que olderThan, failed retryable com
// next_attempt_at vencido, ou relaying cujo lease expirou (claim órfão).
// O claim grava status=relaying + sweep_claimed_at e incrementa relay_attempts
// ANTES de commit — assim duas instâncias não podem fazer POST concorrente.
//
// maxAttempts limita apenas o ramo de failed retryable. O ramo órfão ignora o
// teto de propósito: filtrar por relay_attempts aqui deixaria a linha presa em
// relaying para sempre, invisível para o DLQ. Quem aplica o teto ao órfão é o
// sweep, que encerra como exhausted a linha reclaimada acima do limite.
func (r *PostgresRepository) ClaimStalePendingInboundLogs(
	ctx context.Context,
	olderThan time.Time,
	orphanBefore time.Time,
	now time.Time,
	maxAttempts int,
	limit int,
) ([]PendingInboundLog, error) {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	const query = `
		WITH candidates AS (
			SELECT ml.id
			FROM message_logs ml
			WHERE ml.direction = 'INBOUND'
			  AND (
			        (ml.status = 'pending' AND ml.created_at < $1)
			     OR (ml.status = 'relaying' AND ml.sweep_claimed_at IS NOT NULL AND ml.sweep_claimed_at < $2)
			     OR (
			            ml.status = 'failed'
			        AND COALESCE(ml.failure_reason, '') NOT IN ('permanent', 'exhausted')
			        AND ml.relay_attempts < $4
			        AND (ml.next_attempt_at IS NULL OR ml.next_attempt_at <= $3)
			     )
			  )
			ORDER BY ml.created_at ASC
			LIMIT $5
			FOR UPDATE SKIP LOCKED
		),
		claimed AS (
			UPDATE message_logs ml
			SET status = 'relaying',
			    sweep_claimed_at = NOW(),
			    relay_attempts = ml.relay_attempts + 1,
			    next_attempt_at = NULL
			FROM candidates
			WHERE ml.id = candidates.id
			RETURNING
				ml.id,
				ml.system_id,
				ml.connection_id,
				ml.sistema_origem,
				ml.external_client_id,
				ml.meta_message_id,
				ml.phone_number,
				ml.template_name,
				ml.received_content,
				ml.inbound_payload,
				ml.relay_attempts,
				ml.created_at,
				ml.sweep_claimed_at
		)
		SELECT
			claimed.id::text,
			COALESCE(claimed.system_id::text, ''),
			COALESCE(claimed.connection_id::text, ''),
			COALESCE(claimed.sistema_origem, ''),
			COALESCE(claimed.external_client_id, ''),
			COALESCE(claimed.meta_message_id, ''),
			claimed.phone_number,
			COALESCE(claimed.template_name, ''),
			COALESCE(claimed.received_content, ''),
			COALESCE(NULLIF(c.webhook_url, ''), NULLIF(s.webhook_url, ''), ''),
			claimed.inbound_payload,
			claimed.relay_attempts,
			claimed.created_at,
			claimed.sweep_claimed_at
		FROM claimed
		LEFT JOIN whatsapp_connections c ON c.id = claimed.connection_id
		LEFT JOIN systems s ON s.id = claimed.system_id
		ORDER BY claimed.created_at ASC
	`

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim pending inbound: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query, olderThan, orphanBefore, now, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("claim stale pending inbound logs: %w", err)
	}
	defer rows.Close()

	pending := make([]PendingInboundLog, 0)
	for rows.Next() {
		var row PendingInboundLog
		var inboundPayload []byte
		if err := rows.Scan(
			&row.ID, &row.SystemID, &row.ConnectionID, &row.SistemaOrigem, &row.ExternalClientID,
			&row.MetaMessageID, &row.PhoneNumber, &row.EventType, &row.ReceivedContent,
			&row.TargetWebhookURL, &inboundPayload, &row.RelayAttempts,
			&row.CreatedAt, &row.SweepClaimedAt,
		); err != nil {
			return nil, fmt.Errorf("scan claimed pending inbound log: %w", err)
		}
		row.InboundPayload = inboundPayload
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed pending inbound logs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim pending inbound: %w", err)
	}
	return pending, nil
}

type RelayFailureUpdate struct {
	Status        model.MessageStatus
	FailureReason string
	LastError     string
	RelayAttempts int
	NextAttemptAt *time.Time
}

func (r *PostgresRepository) UpdateMessageLogRelayFailure(
	ctx context.Context,
	id string,
	update RelayFailureUpdate,
) error {
	const query = `
		UPDATE message_logs
		SET status = $2,
		    failure_reason = NULLIF($3, ''),
		    last_error = NULLIF($4, ''),
		    relay_attempts = GREATEST(relay_attempts, $5),
		    next_attempt_at = $6::timestamptz,
		    sweep_claimed_at = NULL
		WHERE id = $1
	`
	result, err := r.db.ExecContext(ctx, query,
		id, update.Status, update.FailureReason, update.LastError, update.RelayAttempts, update.NextAttemptAt,
	)
	if err != nil {
		return fmt.Errorf("update message log relay failure: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrMessageLogNotFound
	}
	return nil
}

func (r *PostgresRepository) MarkMessageLogDelivered(
	ctx context.Context,
	metaMessageID string,
	connectionID string,
	messageCategory model.MessageCategory,
	metaCost float64,
	deliveredAt time.Time,
) error {
	const query = `
		UPDATE message_logs
		SET status = $3,
		    message_category = $4,
		    meta_cost = $5,
		    delivered_at = $6
		WHERE meta_message_id = $1
		  AND connection_id = $2
		  AND status <> 'delivered'
	`
	result, err := r.db.ExecContext(ctx, query,
		metaMessageID,
		connectionID,
		model.MessageStatusDelivered,
		string(messageCategory),
		metaCost,
		deliveredAt,
	)
	if err != nil {
		return fmt.Errorf("update message log delivered: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrMessageLogNotFound
	}
	return nil
}

func (r *PostgresRepository) UpdateMessageLogStatus(
	ctx context.Context,
	id string,
	status model.MessageStatus,
) error {
	const query = `
		UPDATE message_logs
		SET status = $2
		WHERE id = $1
	`
	result, err := r.db.ExecContext(ctx, query, id, status)
	if err != nil {
		return fmt.Errorf("update message log status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrMessageLogNotFound
	}
	return nil
}

type CloseRelayingUpdate struct {
	Status        model.MessageStatus
	FailureReason string
	LastError     string
	NextAttemptAt *time.Time
}

// UpdateMessageLogStatusFromRelaying fecha uma linha claimada pelo sweep.
// A guarda no status evita que um claim órfão reclaimed sobrescreva o desfecho
// de um repasse que outro worker acabou de concluir.
func (r *PostgresRepository) UpdateMessageLogStatusFromRelaying(
	ctx context.Context,
	id string,
	update CloseRelayingUpdate,
) error {
	const query = `
		UPDATE message_logs
		SET status = $2,
		    sweep_claimed_at = NULL,
		    failure_reason = CASE WHEN $2::text = 'sent' THEN NULL ELSE NULLIF($3, '') END,
		    last_error = CASE WHEN $2::text = 'sent' THEN NULL ELSE NULLIF($4, '') END,
		    next_attempt_at = CASE WHEN $2::text = 'sent' THEN NULL ELSE $5::timestamptz END
		WHERE id = $1
		  AND status = 'relaying'
	`
	result, err := r.db.ExecContext(ctx, query,
		id, update.Status, update.FailureReason, update.LastError, update.NextAttemptAt,
	)
	if err != nil {
		return fmt.Errorf("update relaying message log status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return ErrMessageLogNotFound
	}
	return nil
}

func (r *PostgresRepository) scanSystem(row *sql.Row) (*model.System, error) {
	var system model.System
	var webhookURL sql.NullString
	err := row.Scan(
		&system.ID, &system.Name, &system.Slug, &system.APIKeyHash, &webhookURL, &system.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSystemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan system: %w", err)
	}
	if webhookURL.Valid {
		system.WebhookURL = webhookURL.String
	}
	return &system, nil
}

func (r *PostgresRepository) scanSystemRow(rows *sql.Rows) (model.System, error) {
	var system model.System
	var webhookURL sql.NullString
	err := rows.Scan(
		&system.ID, &system.Name, &system.Slug, &system.APIKeyHash, &webhookURL, &system.CreatedAt,
	)
	if err != nil {
		return system, fmt.Errorf("scan system row: %w", err)
	}
	if webhookURL.Valid {
		system.WebhookURL = webhookURL.String
	}
	return system, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "23505"))
}
