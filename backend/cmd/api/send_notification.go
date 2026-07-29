package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
)

type sendNotificationRequest struct {
	TenantID       string   `json:"tenant_id"`
	PhoneNumber    string   `json:"phone_number"`
	AppointmentID  string   `json:"appointment_id"`
	TemplateName   string   `json:"template_name"`
	Variables      []string `json:"variables"`
	LanguageCode   string   `json:"language_code,omitempty"`
	SimpleTemplate bool     `json:"simple_template,omitempty"`
	// Deprecated: ignorado — a aplicação mãe vem da API Key (X-API-Key).
	SistemaOrigem string `json:"sistema_origem,omitempty"`
}

type sendNotificationResponse struct {
	MessageLogID  string              `json:"message_log_id"`
	Status        model.MessageStatus `json:"status"`
	MetaMessageID string              `json:"meta_message_id,omitempty"`
}

// handleSendNotification dispara template WhatsApp usando credenciais do tenant no banco.
// A aplicação mãe é identificada exclusivamente pela API Key autenticada.
func (s *server) handleSendNotification(w http.ResponseWriter, r *http.Request) {
	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	var req sendNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	req.TenantID = strings.TrimSpace(req.TenantID)
	req.PhoneNumber = strings.TrimSpace(req.PhoneNumber)
	req.AppointmentID = strings.TrimSpace(req.AppointmentID)
	req.TemplateName = strings.TrimSpace(req.TemplateName)
	req.SistemaOrigem = strings.TrimSpace(strings.ToLower(req.SistemaOrigem))

	if req.TenantID == "" || req.PhoneNumber == "" || req.TemplateName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "tenant_id, phone_number and template_name are required",
		})
		return
	}
	if req.SistemaOrigem != "" && req.SistemaOrigem != system.Slug {
		writeJSON(w, http.StatusForbidden, errorResponse{
			Error: fmt.Sprintf("sistema_origem %q does not match authenticated system %q", req.SistemaOrigem, system.Slug),
		})
		return
	}

	if req.AppointmentID == "" {
		req.AppointmentID = "-"
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

	if conn.Status == model.ConnectionStatusSuspendedSpam || s.rateLimiter.IsBlacklisted(conn.SystemID, conn.TenantID) {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: "tenant temporarily blocked due to spam protection",
		})
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

	result := s.rateLimiter.RecordAttempt(conn.SystemID, conn.TenantID)
	if !result.Allowed {
		if result.TriggerAlert {
			if err := s.repo.SuspendConnectionForSpam(r.Context(), conn.ID); err != nil {
				log.Printf("suspend connection for spam %s/%s: %v", system.Slug, req.TenantID, err)
			}
			metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
			go s.notifySpamForConnection(conn, result.Count, metaProvider)
		}
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: fmt.Sprintf("rate limit exceeded: %d messages in the last minute", result.Count),
		})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	var metaMessageID string
	var sendErr error
	if req.SimpleTemplate {
		metaMessageID, sendErr = metaProvider.SendPlainTemplate(
			r.Context(),
			conn.AccessToken,
			conn.PhoneNumberID,
			req.PhoneNumber,
			req.TemplateName,
			req.LanguageCode,
		)
	} else {
		metaMessageID, sendErr = metaProvider.SendAppointmentTemplate(
			r.Context(),
			conn.AccessToken,
			conn.PhoneNumberID,
			req.PhoneNumber,
			req.TemplateName,
			req.Variables,
		)
	}

	status := model.MessageStatusSent
	if sendErr != nil {
		status = model.MessageStatusFailed
		log.Printf("send notification %s/%s: %v", system.Slug, req.TenantID, sendErr)
	}

	messageLog := &model.MessageLog{
		SystemID:         conn.SystemID,
		ConnectionID:     conn.ID,
		SistemaOrigem:    conn.SistemaOrigem,
		ExternalClientID: conn.TenantID,
		MetaMessageID:    metaMessageID,
		AppointmentID:    req.AppointmentID,
		PhoneNumber:      req.PhoneNumber,
		TemplateName:     req.TemplateName,
		SentContent:      formatOutboundSentContent(req.TemplateName, req.Variables),
		Direction:        model.MessageDirectionOutbound,
		MessageCategory:  model.MessageCategoryUtility,
		Status:           status,
	}

	if err := s.repo.CreateMessageLog(r.Context(), messageLog); err != nil {
		log.Printf("persist message log for %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to persist message log"})
		return
	}

	if sendErr != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error()})
		return
	}

	writeJSON(w, http.StatusOK, sendNotificationResponse{
		MessageLogID:  messageLog.ID,
		Status:        status,
		MetaMessageID: metaMessageID,
	})
}

func (s *server) notifySpamForConnection(conn *model.WhatsAppConnection, count int, meta *provider.MetaProvider) {
	adminPhone := strings.TrimSpace(os.Getenv("ADMIN_PHONE_NUMBER"))
	if adminPhone == "" {
		return
	}
	body := fmt.Sprintf(
		"ALERTA SPAM: %s/%s — %d msgs/min",
		conn.SistemaOrigem, conn.TenantID, count,
	)
	_ = meta.SendTextMessage(context.Background(), conn.AccessToken, conn.PhoneNumberID, adminPhone, body)
}
