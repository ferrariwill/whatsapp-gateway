package main

import (
	"context"
	"fmt"
	"log"

	"github.com/whatsappgetway/gateway/internal/model"
)

const failureReasonRateLimited = "rate_limited"

type outboundAttemptAudit struct {
	AppointmentID string
	PhoneNumber   string
	TemplateName  string
	Variables     []string
}

// persistRateLimitedOutbound registra uma tentativa recusada antes de qualquer
// chamada à Meta. O status rejected é deliberadamente excluído das consultas
// de uso/envios aceitos.
func (s *server) persistRateLimitedOutbound(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	audit outboundAttemptAudit,
) error {
	if audit.AppointmentID == "" {
		audit.AppointmentID = "-"
	}
	entry := &model.MessageLog{
		SystemID:         conn.SystemID,
		ConnectionID:     conn.ID,
		SistemaOrigem:    conn.SistemaOrigem,
		ExternalClientID: conn.TenantID,
		AppointmentID:    audit.AppointmentID,
		PhoneNumber:      audit.PhoneNumber,
		TemplateName:     audit.TemplateName,
		SentContent:      formatOutboundSentContent(audit.TemplateName, audit.Variables),
		Direction:        model.MessageDirectionOutbound,
		MessageCategory:  model.MessageCategoryUtility,
		Status:           model.MessageStatusRejected,
		FailureReason:    failureReasonRateLimited,
	}
	if err := s.repo.CreateMessageLog(ctx, entry); err != nil {
		return fmt.Errorf("persist rate-limited outbound attempt: %w", err)
	}
	log.Printf(
		"rate-limited outbound attempt persisted: system=%s connection=%s tenant=%s log=%s",
		conn.SystemID, conn.ID, conn.TenantID, entry.ID,
	)
	return nil
}
