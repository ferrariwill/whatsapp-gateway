package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/templateutil"
)

const defaultTemplateLanguage = "pt_BR"

type structuredErrorResponse struct {
	Error    string `json:"error"`
	Code     string `json:"code,omitempty"`
	Expected *int   `json:"expected,omitempty"`
	Got      *int   `json:"got,omitempty"`
}

type syncTemplatesRequest struct {
	TenantID string `json:"tenant_id"`
}

type syncTemplatesResponse struct {
	Synced              int        `json:"synced"`
	Upserted            int        `json:"upserted"`
	TemplatesSyncedAt   *time.Time `json:"templates_synced_at,omitempty"`
	TemplatesSyncError  string     `json:"templates_sync_error,omitempty"`
	TemplatesSyncedBy   string     `json:"templates_synced_by,omitempty"`
}

type catalogTemplateResponse struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Language           string          `json:"language"`
	Category           string          `json:"category,omitempty"`
	Status             string          `json:"status"`
	MetaID             string          `json:"meta_id,omitempty"`
	ExpectedBodyParams int             `json:"expected_body_params"`
	QualityScore       string          `json:"quality_score,omitempty"`
	Components         json.RawMessage `json:"components,omitempty"`
	SyncedAt           *time.Time      `json:"synced_at,omitempty"`
}

type listCatalogTemplatesResponse struct {
	Templates          []catalogTemplateResponse `json:"templates"`
	TemplatesSyncedAt  *time.Time                `json:"templates_synced_at,omitempty"`
	TemplatesSyncError string                    `json:"templates_sync_error,omitempty"`
	TemplatesSyncedBy  string                    `json:"templates_synced_by,omitempty"`
}

type templateGateError struct {
	code     string
	message  string
	expected int
	got      int
	hasCount bool
}

func (e *templateGateError) Error() string {
	if e == nil {
		return "template gate error"
	}
	return e.message
}

func (s *server) validateOutboundTemplate(
	ctx context.Context,
	systemID, tenantID, templateName, language string,
	variables []string,
	simpleTemplate bool,
) error {
	language = strings.TrimSpace(language)
	if language == "" {
		language = defaultTemplateLanguage
	}
	templateName = strings.TrimSpace(templateName)
	tenantID = strings.TrimSpace(tenantID)
	if templateName == "" {
		return &templateGateError{code: "template_not_found", message: "template_name is required"}
	}

	tpl, err := s.repo.FindTemplate(ctx, systemID, tenantID, templateName, language)
	if errors.Is(err, repository.ErrTemplateNotFound) {
		return &templateGateError{
			code:    "template_not_found",
			message: fmt.Sprintf("template %q (%s) not found in local catalog — sync with Meta first", templateName, language),
		}
	}
	if err != nil {
		return err
	}

	if tpl.Status != model.TemplateStatusApproved {
		return &templateGateError{
			code: "template_not_approved",
			message: fmt.Sprintf(
				"template %q (%s) status is %s; only APPROVED templates can be sent",
				templateName, language, tpl.Status,
			),
		}
	}

	if simpleTemplate {
		return nil
	}
	got := len(variables)
	expected := tpl.ExpectedBodyParams
	if got != expected {
		return &templateGateError{
			code:     "param_mismatch",
			message:  fmt.Sprintf("template %q expects %d body parameter(s), got %d", templateName, expected, got),
			expected: expected,
			got:      got,
			hasCount: true,
		}
	}
	return nil
}

func writeTemplateGateError(w http.ResponseWriter, err error) bool {
	var gate *templateGateError
	if !errors.As(err, &gate) {
		return false
	}
	payload := structuredErrorResponse{Error: gate.message, Code: gate.code}
	if gate.hasCount {
		payload.Expected = &gate.expected
		payload.Got = &gate.got
	}
	writeJSON(w, http.StatusUnprocessableEntity, payload)
	return true
}

func (s *server) handleSyncTemplates(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantID == "" {
		var req syncTemplatesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			tenantID = strings.TrimSpace(req.TenantID)
		}
	}
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "tenant_id is required"})
		return
	}

	s.runTemplateSync(w, r, system, tenantID, "api:"+system.Slug)
}

func (s *server) handleAdminSyncTemplates(w http.ResponseWriter, r *http.Request) {
	userID, _ := userIDFromContext(r.Context())
	tenantID := strings.TrimSpace(r.FormValue("tenant_id"))
	if tenantID == "" {
		tenantID = strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	}
	systemID := strings.TrimSpace(r.FormValue("system_id"))
	if systemID == "" {
		systemID = strings.TrimSpace(r.URL.Query().Get("system_id"))
	}
	if tenantID == "" || systemID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "system_id and tenant_id are required"})
		return
	}
	system, err := s.repo.FindSystemByID(r.Context(), systemID)
	if errors.Is(err, repository.ErrSystemNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "system not found"})
		return
	}
	if err != nil {
		log.Printf("admin template sync lookup system: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	syncedBy := "admin:" + userID
	s.runTemplateSync(w, r, *system, tenantID, syncedBy)
}

func (s *server) runTemplateSync(
	w http.ResponseWriter,
	r *http.Request,
	system model.System,
	tenantID, syncedBy string,
) {
	conn, err := s.repo.FindConnectionBySystemAndTenant(r.Context(), system.ID, tenantID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "whatsapp connection not found"})
		return
	}
	if err != nil {
		log.Printf("template sync lookup connection %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	remote, err := metaProvider.GetTemplatesStatus(r.Context(), conn.AccessToken, conn.WabaID)
	if err != nil {
		errMsg := err.Error()
		_ = s.repo.UpdateConnectionTemplateSyncAudit(r.Context(), conn.ID, nil, errMsg, syncedBy)
		log.Printf("template sync graph failure %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusBadGateway, syncTemplatesResponse{
			TemplatesSyncError: errMsg,
			TemplatesSyncedBy:  syncedBy,
		})
		return
	}

	now := time.Now().UTC()
	upserted := 0
	for _, item := range remote {
		components := item.Components
		if len(components) == 0 {
			components = json.RawMessage("[]")
		}
		tpl := &model.WhatsAppTemplate{
			SystemID:           system.ID,
			TenantID:           tenantID,
			ConnectionID:       conn.ID,
			WabaID:             conn.WabaID,
			MetaID:             item.ID,
			Name:               strings.TrimSpace(item.Name),
			Language:           strings.TrimSpace(item.Language),
			Category:           strings.TrimSpace(item.Category),
			Status:             templateutil.NormalizeTemplateStatus(item.Status),
			ComponentsJSON:     []byte(components),
			ExpectedBodyParams: templateutil.ExpectedBodyParamCount(components),
			QualityScore:       item.QualityScore,
			SyncedAt:           &now,
		}
		if tpl.Name == "" || tpl.Language == "" {
			continue
		}
		if tpl.Status == "" {
			tpl.Status = model.TemplateStatusPending
		}
		if err := s.repo.UpsertWhatsAppTemplate(r.Context(), tpl); err != nil {
			log.Printf("upsert template %s/%s %s: %v", system.Slug, tenantID, tpl.Name, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to persist templates"})
			return
		}
		upserted++
	}

	if err := s.repo.UpdateConnectionTemplateSyncAudit(r.Context(), conn.ID, &now, "", syncedBy); err != nil {
		log.Printf("template sync audit %s/%s: %v", system.Slug, tenantID, err)
	}

	writeJSON(w, http.StatusOK, syncTemplatesResponse{
		Synced:            len(remote),
		Upserted:          upserted,
		TemplatesSyncedAt: &now,
		TemplatesSyncedBy: syncedBy,
	})
}

func (s *server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	language := strings.TrimSpace(r.URL.Query().Get("language"))
	if language == "" {
		language = defaultTemplateLanguage
	}
	if name == "" || tenantID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "name path and tenant_id query are required"})
		return
	}

	tpl, err := s.repo.FindTemplate(r.Context(), system.ID, tenantID, name, language)
	if errors.Is(err, repository.ErrTemplateNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "template not found"})
		return
	}
	if err != nil {
		log.Printf("get template %s/%s/%s: %v", system.Slug, tenantID, name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, toCatalogTemplateResponse(tpl))
}

func toCatalogTemplateResponse(tpl *model.WhatsAppTemplate) catalogTemplateResponse {
	components := json.RawMessage(tpl.ComponentsJSON)
	if len(components) == 0 {
		components = json.RawMessage("[]")
	}
	return catalogTemplateResponse{
		ID:                 tpl.ID,
		Name:               tpl.Name,
		Language:           tpl.Language,
		Category:           tpl.Category,
		Status:             tpl.Status,
		MetaID:             tpl.MetaID,
		ExpectedBodyParams: tpl.ExpectedBodyParams,
		QualityScore:       tpl.QualityScore,
		Components:         components,
		SyncedAt:           tpl.SyncedAt,
	}
}
