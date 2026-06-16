package main

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/service"
)

func (s *server) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	month, year, externalClientID, err := parseUsageQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if externalClientID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "external_client_id is required"})
		return
	}

	report, err := s.usage.GetMonthlyUsageReport(r.Context(), system.ID, month, year, externalClientID)
	if err != nil {
		log.Printf("usage report for system %s client %s: %v", system.ID, externalClientID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to build usage report"})
		return
	}

	withinLimit := s.usage.IsWithinMonthlyLimit(report.TotalMessagesSent)

	writeJSON(w, http.StatusOK, map[string]any{
		"system_id":            system.ID,
		"system_name":          system.Name,
		"external_client_id":   externalClientID,
		"month":                month,
		"year":                 year,
		"within_monthly_limit": withinLimit,
		"monthly_limit":        s.usage.MonthlyLimit(),
		"usage":                report,
	})
}

func (s *server) handleAdminUsageVolume(w http.ResponseWriter, r *http.Request) {
	if _, ok := userIDFromContext(r.Context()); !ok {
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	month, year, _, err := parseUsageQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reports, err := s.usage.GetMonthlyVolumeByApplicationAndClient(r.Context(), month, year)
	if err != nil {
		log.Printf("admin usage volume: %v", err)
		http.Error(w, "failed to load usage volume", http.StatusInternalServerError)
		return
	}

	renderUsageVolumePartial(w, s.templates, buildUsageVolumeView(reports, month, year, s.usage.MonthlyLimit()))
}

func parseUsageQuery(r *http.Request) (month, year int, externalClientID string, err error) {
	externalClientID = strings.TrimSpace(r.URL.Query().Get("external_client_id"))
	month, year, err = service.ParseUsagePeriod(
		r.URL.Query().Get("month"),
		r.URL.Query().Get("year"),
		time.Now(),
	)
	if err != nil {
		return 0, 0, "", err
	}
	return month, year, externalClientID, nil
}
