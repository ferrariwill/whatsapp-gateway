package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/service"
)

type usageMeterTotals struct {
	SentText                int64 `json:"sent_text"`
	SentTemplate            int64 `json:"sent_template"`
	SentMedia               int64 `json:"sent_media"`
	Inbound                 int64 `json:"inbound"`
	StatusCallbackDelivered int64 `json:"status_callback_delivered"`
	Errors4xx               int64 `json:"errors_4xx"`
	Errors5xx               int64 `json:"errors_5xx"`
}

type usageMeterDay struct {
	Date                    string `json:"date"`
	SentText                int64  `json:"sent_text"`
	SentTemplate            int64  `json:"sent_template"`
	SentMedia               int64  `json:"sent_media"`
	Inbound                 int64  `json:"inbound"`
	StatusCallbackDelivered int64  `json:"status_callback_delivered"`
	Errors4xx               int64  `json:"errors_4xx"`
	Errors5xx               int64  `json:"errors_5xx"`
}

type usageMeterResponse struct {
	SystemID string           `json:"system_id"`
	TenantID string           `json:"tenant_id"`
	From     string           `json:"from"`
	To       string           `json:"to"`
	Totals   usageMeterTotals `json:"totals"`
	Days     []usageMeterDay  `json:"days"`
}

// handleUsageMeter — GET /v1/usage?tenant_id&from&to (API key, product-scoped).
// Dias sem linha em usage_counters são omitidos. Tenant sem uso no período
// retorna totals zerados e days: [] (200), nunca 404.
func (s *server) handleUsageMeter(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "tenant_id is required"})
		return
	}

	from, to, err := parseUsageMeterRange(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	rows, err := s.usageMeter.QueryRange(r.Context(), system.ID, tenantID, from, to)
	if err != nil {
		log.Printf("usage meter query system=%s tenant=%s: %v", system.ID, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to query usage"})
		return
	}

	resp := usageMeterResponse{
		SystemID: system.ID,
		TenantID: tenantID,
		From:     from.Format("2006-01-02"),
		To:       to.Format("2006-01-02"),
		Days:     make([]usageMeterDay, 0, len(rows)),
	}
	for _, row := range rows {
		day := usageMeterDayFromRow(row)
		resp.Days = append(resp.Days, day)
		resp.Totals.SentText += day.SentText
		resp.Totals.SentTemplate += day.SentTemplate
		resp.Totals.SentMedia += day.SentMedia
		resp.Totals.Inbound += day.Inbound
		resp.Totals.StatusCallbackDelivered += day.StatusCallbackDelivered
		resp.Totals.Errors4xx += day.Errors4xx
		resp.Totals.Errors5xx += day.Errors5xx
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleUsageExportCSV — GET /v1/usage/export.csv (JWT admin).
func (s *server) handleUsageExportCSV(w http.ResponseWriter, r *http.Request) {
	if _, ok := userIDFromContext(r.Context()); !ok {
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	from, to, err := parseUsageMeterRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	systemID := strings.TrimSpace(r.URL.Query().Get("system_id"))
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))

	rows, err := s.usageMeter.QueryRangeAdmin(r.Context(), systemID, tenantID, from, to)
	if err != nil {
		log.Printf("usage export csv: %v", err)
		http.Error(w, "failed to export usage", http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("usage_%s_%s.csv", from.Format("20060102"), to.Format("20060102"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	cw := csv.NewWriter(w)
	header := []string{
		"system_id", "tenant_id", "usage_date",
		"sent_text", "sent_template", "sent_media",
		"inbound", "status_callback_delivered",
		"errors_4xx", "errors_5xx",
	}
	if err := cw.Write(header); err != nil {
		log.Printf("usage export csv header: %v", err)
		return
	}
	for _, row := range rows {
		record := []string{
			row.SystemID,
			row.TenantID,
			row.UsageDate.UTC().Format("2006-01-02"),
			strconv.FormatInt(row.SentText, 10),
			strconv.FormatInt(row.SentTemplate, 10),
			strconv.FormatInt(row.SentMedia, 10),
			strconv.FormatInt(row.Inbound, 10),
			strconv.FormatInt(row.StatusCallbackDelivered, 10),
			strconv.FormatInt(row.Errors4xx, 10),
			strconv.FormatInt(row.Errors5xx, 10),
		}
		if err := cw.Write(record); err != nil {
			log.Printf("usage export csv row: %v", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("usage export csv flush: %v", err)
	}
}

func parseUsageMeterRange(r *http.Request) (from, to time.Time, err error) {
	from, err = service.ParseUsageDate(r.URL.Query().Get("from"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from: %w", err)
	}
	to, err = service.ParseUsageDate(r.URL.Query().Get("to"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to: %w", err)
	}
	if err := service.ValidateUsageDateRange(from, to); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func usageMeterDayFromRow(row repository.UsageCounterRow) usageMeterDay {
	return usageMeterDay{
		Date:                    row.UsageDate.UTC().Format("2006-01-02"),
		SentText:                row.SentText,
		SentTemplate:            row.SentTemplate,
		SentMedia:               row.SentMedia,
		Inbound:                 row.Inbound,
		StatusCallbackDelivered: row.StatusCallbackDelivered,
		Errors4xx:               row.Errors4xx,
		Errors5xx:               row.Errors5xx,
	}
}
