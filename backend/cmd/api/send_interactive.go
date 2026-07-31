package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/session"
)

type interactiveButtonReq struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type interactiveListRowReq struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type interactiveListSectionReq struct {
	Title string                  `json:"title,omitempty"`
	Rows  []interactiveListRowReq `json:"rows"`
}

type sendInteractiveRequest struct {
	TenantID      string                      `json:"tenant_id"`
	PhoneNumber   string                      `json:"phone_number"`
	Type          string                      `json:"type"`
	BodyText      string                      `json:"body_text"`
	HeaderText    string                      `json:"header_text,omitempty"`
	FooterText    string                      `json:"footer_text,omitempty"`
	Buttons       []interactiveButtonReq      `json:"buttons,omitempty"`
	ListButton    string                      `json:"list_button,omitempty"`
	Sections      []interactiveListSectionReq `json:"sections,omitempty"`
	AppointmentID string                      `json:"appointment_id,omitempty"`
}

type sendInteractiveResponse struct {
	MessageLogID  string              `json:"message_log_id"`
	Status        model.MessageStatus `json:"status"`
	MetaMessageID string              `json:"meta_message_id,omitempty"`
}

type interactiveValidationError struct {
	msg string
}

func (e *interactiveValidationError) Error() string { return e.msg }

func validationErr(msg string) error {
	return &interactiveValidationError{msg: msg}
}

func isInteractiveValidationError(err error) bool {
	var v *interactiveValidationError
	return errors.As(err, &v)
}

// validateInteractiveRequest enforces Cloud API interactive limits locally (before Graph).
func validateInteractiveRequest(req *sendInteractiveRequest) error {
	if req == nil {
		return validationErr("request is required")
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.BodyText = strings.TrimSpace(req.BodyText)
	req.HeaderText = strings.TrimSpace(req.HeaderText)
	req.FooterText = strings.TrimSpace(req.FooterText)
	req.ListButton = strings.TrimSpace(req.ListButton)

	if req.Type != "button" && req.Type != "list" {
		return validationErr("type must be button or list")
	}
	if n := utf8.RuneCountInString(req.BodyText); n < 1 || n > 1024 {
		return validationErr("body_text must be 1–1024 characters")
	}
	if utf8.RuneCountInString(req.HeaderText) > 60 {
		return validationErr("header_text must be ≤60 characters")
	}
	if utf8.RuneCountInString(req.FooterText) > 60 {
		return validationErr("footer_text must be ≤60 characters")
	}

	switch req.Type {
	case "button":
		if len(req.Buttons) < 1 || len(req.Buttons) > 3 {
			return validationErr("button type requires 1–3 buttons")
		}
		seen := make(map[string]struct{}, len(req.Buttons))
		for i := range req.Buttons {
			req.Buttons[i].ID = strings.TrimSpace(req.Buttons[i].ID)
			req.Buttons[i].Title = strings.TrimSpace(req.Buttons[i].Title)
			id, title := req.Buttons[i].ID, req.Buttons[i].Title
			if id == "" || title == "" {
				return validationErr("each button requires non-empty id and title")
			}
			if utf8.RuneCountInString(id) > 256 {
				return validationErr("button id must be ≤256 characters")
			}
			if utf8.RuneCountInString(title) > 20 {
				return validationErr("button title must be ≤20 characters")
			}
			if _, ok := seen[id]; ok {
				return validationErr("button ids must be unique")
			}
			seen[id] = struct{}{}
		}
	case "list":
		if n := utf8.RuneCountInString(req.ListButton); n < 1 || n > 20 {
			return validationErr("list_button must be 1–20 characters")
		}
		if len(req.Sections) < 1 || len(req.Sections) > 10 {
			return validationErr("list type requires 1–10 sections")
		}
		totalRows := 0
		seen := map[string]struct{}{}
		for i := range req.Sections {
			req.Sections[i].Title = strings.TrimSpace(req.Sections[i].Title)
			if utf8.RuneCountInString(req.Sections[i].Title) > 24 {
				return validationErr("section title must be ≤24 characters")
			}
			if len(req.Sections[i].Rows) == 0 {
				return validationErr("each section requires at least one row")
			}
			for j := range req.Sections[i].Rows {
				row := &req.Sections[i].Rows[j]
				row.ID = strings.TrimSpace(row.ID)
				row.Title = strings.TrimSpace(row.Title)
				row.Description = strings.TrimSpace(row.Description)
				if row.ID == "" || row.Title == "" {
					return validationErr("each row requires non-empty id and title")
				}
				if utf8.RuneCountInString(row.ID) > 200 {
					return validationErr("row id must be ≤200 characters")
				}
				if utf8.RuneCountInString(row.Title) > 24 {
					return validationErr("row title must be ≤24 characters")
				}
				if utf8.RuneCountInString(row.Description) > 72 {
					return validationErr("row description must be ≤72 characters")
				}
				if _, ok := seen[row.ID]; ok {
					return validationErr("row ids must be unique across the payload")
				}
				seen[row.ID] = struct{}{}
				totalRows++
			}
		}
		if totalRows > 10 {
			return validationErr("list type allows at most 10 rows in total")
		}
	}
	return nil
}

func interactiveTemplateName(msgType string) string {
	if msgType == "list" {
		return "interactive_list"
	}
	return "interactive_button"
}

func interactiveSentContent(req sendInteractiveRequest) string {
	var b strings.Builder
	b.WriteString(req.BodyText)
	switch req.Type {
	case "button":
		ids := make([]string, 0, len(req.Buttons))
		for _, btn := range req.Buttons {
			ids = append(ids, btn.ID)
		}
		if len(ids) > 0 {
			b.WriteString(" | buttons=")
			b.WriteString(strings.Join(ids, ","))
		}
	case "list":
		ids := make([]string, 0)
		for _, sec := range req.Sections {
			for _, row := range sec.Rows {
				ids = append(ids, row.ID)
			}
		}
		if len(ids) > 0 {
			b.WriteString(" | rows=")
			b.WriteString(strings.Join(ids, ","))
		}
	}
	return b.String()
}

func toProviderInteractivePayload(req sendInteractiveRequest) provider.InteractivePayload {
	out := provider.InteractivePayload{
		Type:       req.Type,
		BodyText:   req.BodyText,
		HeaderText: req.HeaderText,
		FooterText: req.FooterText,
		ListButton: req.ListButton,
	}
	for _, b := range req.Buttons {
		out.Buttons = append(out.Buttons, provider.InteractiveButton{ID: b.ID, Title: b.Title})
	}
	for _, sec := range req.Sections {
		ps := provider.InteractiveListSection{Title: sec.Title}
		for _, row := range sec.Rows {
			ps.Rows = append(ps.Rows, provider.InteractiveListRow{
				ID: row.ID, Title: row.Title, Description: row.Description,
			})
		}
		out.Sections = append(out.Sections, ps)
	}
	return out
}

// handleSendInteractive dispara mensagem interativa (button/list) na janela 24h.
func (s *server) handleSendInteractive(w http.ResponseWriter, r *http.Request) {
	s.ensureThrottleFields()

	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	var req sendInteractiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.PhoneNumber = session.NormalizeWAID(req.PhoneNumber)
	req.AppointmentID = strings.TrimSpace(req.AppointmentID)
	if req.AppointmentID == "" {
		req.AppointmentID = "-"
	}

	if req.TenantID == "" || req.PhoneNumber == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "tenant_id and phone_number are required",
		})
		return
	}

	if err := validateInteractiveRequest(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Error: err.Error(),
			Code:  "validation_error",
		})
		return
	}

	conn, err := s.repo.FindConnectionBySystemAndTenant(r.Context(), system.ID, req.TenantID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("whatsapp connection not found for %s/%s", system.Slug, req.TenantID),
		})
		return
	}
	if err != nil {
		log.Printf("lookup connection %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	templateName := interactiveTemplateName(req.Type)
	if !s.enforceConnectionSpamProtection(w, r, conn, outboundAttemptAudit{
		AppointmentID: req.AppointmentID,
		PhoneNumber:   req.PhoneNumber,
		TemplateName:  templateName,
	}) {
		return
	}

	withinLimit, err := s.usage.CheckMonthlyLimit(r.Context(), conn.SystemID, conn.TenantID)
	if err != nil {
		log.Printf("check monthly limit %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to check monthly message limit"})
		return
	}
	if !withinLimit {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: fmt.Sprintf("monthly message limit of %d exceeded for this tenant", s.usage.MonthlyLimit()),
		})
		return
	}

	if !s.enforceCommercialThrottle(w, r, conn) {
		return
	}

	lastInbound, err := s.repo.GetLastInbound(r.Context(), conn.SystemID, conn.TenantID, req.PhoneNumber)
	if err != nil {
		log.Printf("get last inbound %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to check 24h window"})
		return
	}
	if !session.InWindow(lastInbound, time.Now().UTC()) {
		writeOutside24h(w)
		return
	}

	providerPayload := toProviderInteractivePayload(req)
	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	metaMessageID, sendErr := metaProvider.SendInteractive(
		r.Context(), conn.AccessToken, conn.PhoneNumberID, req.PhoneNumber, providerPayload,
	)

	status := model.MessageStatusSent
	failureCode := ""
	if sendErr != nil {
		status = model.MessageStatusFailed
		log.Printf("send interactive %s/%s: %v", system.Slug, req.TenantID, sendErr)
		if provider.IsMetaRateLimited(sendErr) {
			failureCode = "meta_rate_limited"
		}
	}

	messageLog := &model.MessageLog{
		SystemID:         conn.SystemID,
		ConnectionID:     conn.ID,
		SistemaOrigem:    conn.SistemaOrigem,
		ExternalClientID: conn.TenantID,
		MetaMessageID:    metaMessageID,
		AppointmentID:    req.AppointmentID,
		PhoneNumber:      req.PhoneNumber,
		TemplateName:     templateName,
		SentContent:      interactiveSentContent(req),
		Direction:        model.MessageDirectionOutbound,
		MessageCategory:  model.MessageCategoryService,
		Status:           status,
		FailureCode:      failureCode,
	}
	if err := s.repo.CreateMessageLog(r.Context(), messageLog); err != nil {
		log.Printf("persist message log for %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to persist message log"})
		return
	}

	day := usageDayUTC(time.Now())
	if sendErr != nil {
		retryPayload := map[string]any{
			"phone_number": req.PhoneNumber,
			"interactive":  providerPayload,
		}
		s.maybeEnqueueOutboundRetry(r.Context(), conn, messageLog, model.OutboundRetryKindInteractive, retryPayload, sendErr)
		s.recordUsageBestEffort(r.Context(), conn.SystemID, conn.TenantID, day, outboundErrorDelta(sendErr), "send-interactive/meta-error")
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error(), Code: failureCode})
		return
	}

	// Interactive não é template Meta de catálogo: conta como sent_text (sem coluna sent_interactive).
	s.recordUsageBestEffort(r.Context(), conn.SystemID, conn.TenantID, day, outboundSuccessDelta("", "interactive"), "send-interactive/sent")

	writeJSON(w, http.StatusOK, sendInteractiveResponse{
		MessageLogID:  messageLog.ID,
		Status:        status,
		MetaMessageID: metaMessageID,
	})
}
