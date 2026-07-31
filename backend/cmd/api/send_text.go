package main

import (
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
	"github.com/whatsappgetway/gateway/internal/session"
)

type sendTextRequest struct {
	TenantID      string `json:"tenant_id"`
	PhoneNumber   string `json:"phone_number"`
	Text          string `json:"text"`
	AppointmentID string `json:"appointment_id"`
}

type sendTextResponse struct {
	MessageLogID  string              `json:"message_log_id"`
	Status        model.MessageStatus `json:"status"`
	MetaMessageID string              `json:"meta_message_id,omitempty"`
}

func (s *server) handleSendText(w http.ResponseWriter, r *http.Request) {
	s.ensureThrottleFields()

	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	var req sendTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.PhoneNumber = session.NormalizeWAID(req.PhoneNumber)
	req.Text = strings.TrimSpace(req.Text)
	req.AppointmentID = strings.TrimSpace(req.AppointmentID)
	if req.AppointmentID == "" {
		req.AppointmentID = "-"
	}
	if req.TenantID == "" || req.PhoneNumber == "" || req.Text == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "tenant_id, phone_number and text are required",
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

	if !s.enforceConnectionSpamProtection(w, r, conn, outboundAttemptAudit{
		AppointmentID: req.AppointmentID,
		PhoneNumber:   req.PhoneNumber,
		TemplateName:  "text",
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

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	metaMessageID, sendErr := metaProvider.SendTextMessage(
		r.Context(), conn.AccessToken, conn.PhoneNumberID, req.PhoneNumber, req.Text,
	)

	status := model.MessageStatusSent
	failureCode := ""
	if sendErr != nil {
		status = model.MessageStatusFailed
		log.Printf("send text %s/%s: %v", system.Slug, req.TenantID, sendErr)
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
		SentContent:      req.Text,
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

	if sendErr != nil {
		s.maybeEnqueueOutboundRetry(r.Context(), conn, messageLog, model.OutboundRetryKindText, map[string]any{
			"phone_number": req.PhoneNumber,
			"text":         req.Text,
		}, sendErr)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error(), Code: failureCode})
		return
	}

	writeJSON(w, http.StatusOK, sendTextResponse{
		MessageLogID:  messageLog.ID,
		Status:        status,
		MetaMessageID: metaMessageID,
	})
}
