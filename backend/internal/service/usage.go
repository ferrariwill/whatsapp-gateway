package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
)

const defaultMonthlyMessageLimit int64 = 3000

// MetaUtilityCostUSD custo aproximado Meta por mensagem UTILITY entregue (controle interno).
const MetaUtilityCostUSD = 0.0068

// MessageCategory classifica mensagens para auditoria Meta.
type MessageCategory string

const (
	CategoryUtility        MessageCategory = "UTILITY"
	CategoryMarketing      MessageCategory = "MARKETING"
	CategoryAuthentication MessageCategory = "AUTHENTICATION"
	CategoryService        MessageCategory = "SERVICE"
)

// UsageReport resume o volume de disparos de um cliente externo no período.
type UsageReport struct {
	TotalMessagesSent int64   `json:"total_messages_sent"`
	TotalDelivered    int64   `json:"total_delivered"`
	TotalFailed       int64   `json:"total_failed"`
	EstimatedMetaCost float64 `json:"estimated_meta_cost"`
}

// ExternalClientUsageReport agrupa métricas por aplicação (system) e cliente externo.
type ExternalClientUsageReport struct {
	SystemID           string `json:"system_id"`
	SystemName         string `json:"system_name"`
	ExternalClientID   string `json:"external_client_id"`
	UsageReport        `json:",inline"`
	WithinMonthlyLimit bool  `json:"within_monthly_limit"`
	MonthlyLimit       int64 `json:"monthly_limit"`
	BarWidthPercent    int   `json:"-"`
}

// UsageService audita volume de mensagens e detecta abuso.
type UsageService struct {
	repo         *repository.PostgresRepository
	monthlyLimit int64
}

func NewUsageService(repo *repository.PostgresRepository) *UsageService {
	return &UsageService{
		repo:         repo,
		monthlyLimit: loadMonthlyMessageLimit(),
	}
}

func loadMonthlyMessageLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("MONTHLY_MESSAGE_LIMIT"))
	if raw == "" {
		return defaultMonthlyMessageLimit
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return defaultMonthlyMessageLimit
	}
	return limit
}

// MonthlyLimit retorna o limite configurado de mensagens por cliente externo/mês.
func (s *UsageService) MonthlyLimit() int64 {
	return s.monthlyLimit
}

// NormalizeCategory converte a categoria enviada pela Meta para o enum interno.
func NormalizeCategory(raw string) MessageCategory {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MARKETING":
		return CategoryMarketing
	case "AUTHENTICATION":
		return CategoryAuthentication
	case "SERVICE":
		return CategoryService
	default:
		return CategoryUtility
	}
}

// MetaCostForCategory retorna o custo estimado Meta (USD) por mensagem entregue.
func MetaCostForCategory(category MessageCategory) float64 {
	switch category {
	case CategoryMarketing:
		return 0.0625
	case CategoryAuthentication:
		return 0.0068
	case CategoryService:
		return 0.0
	default:
		return MetaUtilityCostUSD
	}
}

// GetMonthlyUsageReport agrega volume de um cliente externo em uma aplicação no mês informado.
func (s *UsageService) GetMonthlyUsageReport(
	ctx context.Context,
	systemID string,
	month, year int,
	externalClientID string,
) (*UsageReport, error) {
	if strings.TrimSpace(systemID) == "" {
		return nil, fmt.Errorf("system id is required")
	}
	if strings.TrimSpace(externalClientID) == "" {
		return nil, fmt.Errorf("external client id is required")
	}
	if month < 1 || month > 12 {
		return nil, fmt.Errorf("month must be between 1 and 12")
	}
	if year < 2000 || year > 9999 {
		return nil, fmt.Errorf("invalid year")
	}

	stats, err := s.repo.AggregateClientUsageStats(ctx, systemID, externalClientID, month, year)
	if err != nil {
		return nil, err
	}

	return buildUsageReport(stats), nil
}

// GetMonthlyVolumeByApplicationAndClient lista volume agrupado por aplicação e cliente externo.
func (s *UsageService) GetMonthlyVolumeByApplicationAndClient(
	ctx context.Context,
	month, year int,
) ([]ExternalClientUsageReport, error) {
	if month < 1 || month > 12 {
		return nil, fmt.Errorf("month must be between 1 and 12")
	}
	if year < 2000 || year > 9999 {
		return nil, fmt.Errorf("invalid year")
	}

	rows, err := s.repo.AggregateUsageVolumeByApplicationAndClient(ctx, month, year)
	if err != nil {
		return nil, err
	}

	reports := make([]ExternalClientUsageReport, 0, len(rows))
	for _, row := range rows {
		report := ExternalClientUsageReport{
			SystemID:         row.SystemID,
			SystemName:       row.SystemName,
			ExternalClientID: row.ExternalClientID,
			UsageReport:      *buildUsageReport(row.Stats),
			MonthlyLimit:     s.monthlyLimit,
		}
		report.WithinMonthlyLimit = report.TotalMessagesSent <= s.monthlyLimit
		report.BarWidthPercent = barWidthAgainstLimit(report.TotalMessagesSent, s.monthlyLimit)
		reports = append(reports, report)
	}

	return reports, nil
}

// CheckMonthlyLimit retorna true se o cliente externo ainda pode enviar (abaixo do limite mensal).
func (s *UsageService) CheckMonthlyLimit(ctx context.Context, systemID, clientID string) (bool, error) {
	if strings.TrimSpace(systemID) == "" || strings.TrimSpace(clientID) == "" {
		return false, fmt.Errorf("system id and client id are required")
	}

	now := time.Now().UTC()
	count, err := s.repo.CountMonthlyMessagesForClient(ctx, systemID, clientID, int(now.Month()), now.Year())
	if err != nil {
		return false, err
	}

	return count < s.monthlyLimit, nil
}

// IsWithinMonthlyLimit indica se o volume acumulado está dentro ou igual ao limite configurado.
func (s *UsageService) IsWithinMonthlyLimit(totalMessages int64) bool {
	return totalMessages <= s.monthlyLimit
}

func barWidthAgainstLimit(total, limit int64) int {
	if limit <= 0 {
		return 0
	}
	percent := int(total * 100 / limit)
	if percent > 100 {
		return 100
	}
	if percent < 2 && total > 0 {
		return 2
	}
	return percent
}

func buildUsageReport(stats repository.ClientUsageStats) *UsageReport {
	utilityDelivered := stats.UtilityDelivered
	if utilityDelivered == 0 {
		utilityDelivered = stats.TotalDelivered
	}

	return &UsageReport{
		TotalMessagesSent: stats.TotalSent,
		TotalDelivered:    stats.TotalDelivered,
		TotalFailed:       stats.TotalFailed,
		EstimatedMetaCost: float64(utilityDelivered) * MetaUtilityCostUSD,
	}
}

// ParseUsagePeriod extrai mês/ano dos query params, usando o mês corrente como padrão.
func ParseUsagePeriod(monthRaw, yearRaw string, now time.Time) (month, year int, err error) {
	if strings.TrimSpace(yearRaw) == "" {
		year = now.Year()
	} else {
		if _, scanErr := fmt.Sscanf(yearRaw, "%d", &year); scanErr != nil {
			return 0, 0, fmt.Errorf("invalid year")
		}
	}

	if strings.TrimSpace(monthRaw) == "" {
		month = int(now.Month())
	} else {
		if _, scanErr := fmt.Sscanf(monthRaw, "%d", &month); scanErr != nil {
			return 0, 0, fmt.Errorf("invalid month")
		}
	}

	if month < 1 || month > 12 {
		return 0, 0, fmt.Errorf("month must be between 1 and 12")
	}
	return month, year, nil
}
