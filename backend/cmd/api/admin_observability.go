package main

import (
	"log"
	"net/http"
	"strconv"
)

type adminAuditLogsResponse struct {
	Logs any `json:"logs"`
}

// handleAdminAuditLogs expõe os mesmos registros exibidos no painel, incluindo
// status=rejected e failure_reason=rate_limited.
func (s *server) handleAdminAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 500 {
		limit = 500
	}

	logs, err := s.repo.ListRecentMessageLogs(r.Context(), limit)
	if err != nil {
		log.Printf("list admin audit logs: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to load audit logs"})
		return
	}
	writeJSON(w, http.StatusOK, adminAuditLogsResponse{Logs: logs})
}

type relayMetricsResponse struct {
	Scope string         `json:"scope"`
	Pool  relayPoolStats `json:"pool"`
}

// handleAdminRelayMetrics expõe métricas operacionais process-global do pool.
// Não inclui identidade de tenant, pois o pool é infraestrutura compartilhada.
func (s *server) handleAdminRelayMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, relayMetricsResponse{
		Scope: "process",
		Pool:  s.relay.Stats(),
	})
}
