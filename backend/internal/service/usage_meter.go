package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
)

const maxUsageRangeDays = 93

// UsageMeterService é o metering diário paralelo ao UsageService legado (agregação em message_logs).
type UsageMeterService struct {
	repo *repository.PostgresRepository
}

func NewUsageMeterService(repo *repository.PostgresRepository) *UsageMeterService {
	return &UsageMeterService{repo: repo}
}

// Increment aplica a delta best-effort no dia UTC informado.
func (s *UsageMeterService) Increment(
	ctx context.Context,
	systemID, tenantID string,
	dayUTC time.Time,
	d repository.UsageDelta,
) error {
	return s.repo.IncrementUsageCounters(ctx, systemID, tenantID, dayUTC, d)
}

// TryRecordStatusDelivered registra idempotência de delivered.
func (s *UsageMeterService) TryRecordStatusDelivered(
	ctx context.Context,
	systemID, metaMessageID string,
) (bool, error) {
	return s.repo.TryRecordStatusDelivered(ctx, systemID, metaMessageID)
}

// QueryRange retorna dias com uso para um product+tenant. Dias sem linha são omitidos.
func (s *UsageMeterService) QueryRange(
	ctx context.Context,
	systemID, tenantID string,
	from, to time.Time,
) ([]repository.UsageCounterRow, error) {
	if err := ValidateUsageDateRange(from, to); err != nil {
		return nil, err
	}
	return s.repo.QueryUsageRange(ctx, systemID, tenantID, from, to)
}

// QueryRangeAdmin retorna dias com filtros opcionais (export admin).
func (s *UsageMeterService) QueryRangeAdmin(
	ctx context.Context,
	systemID, tenantID string,
	from, to time.Time,
) ([]repository.UsageCounterRow, error) {
	if err := ValidateUsageDateRange(from, to); err != nil {
		return nil, err
	}
	return s.repo.QueryUsageRangeAdmin(ctx, systemID, tenantID, from, to)
}

// ValidateUsageDateRange exige from <= to e no máximo 93 dias inclusivos.
func ValidateUsageDateRange(from, to time.Time) error {
	fromDay := from.UTC().Truncate(24 * time.Hour)
	toDay := to.UTC().Truncate(24 * time.Hour)
	if toDay.Before(fromDay) {
		return fmt.Errorf("from must be less than or equal to to")
	}
	// Inclusivo: 1 dia = from==to; 93 dias = to-from <= 92*24h.
	if toDay.Sub(fromDay) > time.Duration(maxUsageRangeDays-1)*24*time.Hour {
		return fmt.Errorf("date range must be at most %d days", maxUsageRangeDays)
	}
	return nil
}

// ParseUsageDate parseia YYYY-MM-DD como meia-noite UTC.
func ParseUsageDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("date is required")
	}
	day, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q (want YYYY-MM-DD)", raw)
	}
	return day, nil
}

// ClassifyOutbound classifica um envio outbound nas métricas sent_*.
// Template name preenchido → sent_template; msgType media → sent_media;
// interactive (e demais) → sent_text (sem coluna sent_interactive nesta fase).
func ClassifyOutbound(templateName, msgType string) (text, template, media int64) {
	if strings.TrimSpace(templateName) != "" {
		return 0, 1, 0
	}
	switch strings.ToLower(strings.TrimSpace(msgType)) {
	case "image", "audio", "video", "document", "sticker", "media":
		return 0, 0, 1
	default:
		return 1, 0, 0
	}
}

// ClassifySendError mapeia falha Meta/HTTP para errors_4xx ou errors_5xx.
// Status 4xx → 4xx; 5xx / timeout / transporte → 5xx.
func ClassifySendError(err error) (errors4xx, errors5xx int64) {
	if err == nil {
		return 0, 0
	}
	msg := err.Error()
	if status, ok := extractMetaStatusCode(msg); ok {
		if status >= 400 && status < 500 {
			return 1, 0
		}
		if status >= 500 {
			return 0, 1
		}
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "temporary failure") ||
		strings.Contains(lower, "i/o timeout") {
		return 0, 1
	}
	// Default: falha pós-Meta sem status claro → 5xx (Bad Gateway interno).
	return 0, 1
}

func extractMetaStatusCode(msg string) (int, bool) {
	const marker = "status "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return 0, false
	}
	rest := msg[idx+len(marker):]
	n := 0
	digits := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		digits++
		if digits > 3 {
			break
		}
	}
	if digits == 0 {
		return 0, false
	}
	return n, true
}
