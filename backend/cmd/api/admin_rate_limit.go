package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/whatsappgetway/gateway/internal/repository"
)

type rateLimitAdminResponse struct {
	RPS           float64 `json:"rps"`
	Burst         int     `json:"burst"`
	Source        string  `json:"source"`
	ConnectionID  string  `json:"connection_id"`
	TenantID      string  `json:"tenant_id"`
	SystemID      string  `json:"system_id"`
}

type rateLimitAdminRequest struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

const maxRateLimitBurst = 1000

func (s *server) handleAdminGetConnectionRateLimit(w http.ResponseWriter, r *http.Request) {
	s.ensureThrottleFields()
	if _, ok := userIDFromContext(r.Context()); !ok {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	conn, err := s.repo.FindConnectionByID(r.Context(), id)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "connection not found"})
		return
	}
	if err != nil {
		log.Printf("admin get rate limit: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	resolved, err := s.resolveRateLimitCached(r.Context(), conn.SystemID, conn.TenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to resolve rate limit"})
		return
	}
	writeJSON(w, http.StatusOK, rateLimitAdminResponse{
		RPS:          resolved.RPS,
		Burst:        resolved.Burst,
		Source:       resolved.Source,
		ConnectionID: conn.ID,
		TenantID:     conn.TenantID,
		SystemID:     conn.SystemID,
	})
}

func (s *server) handleAdminSetConnectionRateLimit(w http.ResponseWriter, r *http.Request) {
	s.ensureThrottleFields()
	actor, ok := userIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	conn, err := s.repo.FindConnectionByID(r.Context(), id)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "connection not found"})
		return
	}
	if err != nil {
		log.Printf("admin set rate limit lookup: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	var req rateLimitAdminRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if req.RPS <= 0 || req.Burst <= 0 || req.Burst > maxRateLimitBurst {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "rps and burst must be > 0; burst max 1000",
		})
		return
	}

	if err := s.repo.UpdateConnectionRateLimit(
		r.Context(), conn.ID, conn.SystemID, conn.TenantID, actor,
		req.RPS, req.Burst, conn.RateLimitRPS, conn.RateLimitBurst,
	); err != nil {
		log.Printf("admin set rate limit: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to update rate limit"})
		return
	}
	s.invalidateRateLimitCache(conn.SystemID, conn.TenantID)
	writeJSON(w, http.StatusOK, rateLimitAdminResponse{
		RPS:          req.RPS,
		Burst:        req.Burst,
		Source:       "connection",
		ConnectionID: conn.ID,
		TenantID:     conn.TenantID,
		SystemID:     conn.SystemID,
	})
}
