package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/whatsappgetway/gateway/internal/repository"
)

// handleGetHostedMedia GET /v1/media/{id}?tenant_id=...
// Requires API Key of the owning system. Cross-tenant → 404 (no existence leak).
func (s *server) handleGetHostedMedia(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	objectID := strings.TrimSpace(r.PathValue("id"))
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantID == "" {
		tenantID = strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	}
	if objectID == "" || tenantID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "media id and tenant_id are required"})
		return
	}

	obj, err := s.repo.FindMediaObject(r.Context(), objectID, system.ID, tenantID)
	if errors.Is(err, repository.ErrMediaObjectNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "media not found"})
		return
	}
	if err != nil {
		log.Printf("find media object %s: %v", objectID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	path := absoluteMediaPath(obj.StoragePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "media not found"})
			return
		}
		log.Printf("read media file %s: %v", objectID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to read media"})
		return
	}

	if obj.MimeType != "" {
		w.Header().Set("Content-Type", obj.MimeType)
	}
	if obj.OriginalName != "" {
		w.Header().Set("Content-Disposition", `inline; filename="`+obj.OriginalName+`"`)
	}
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
