package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
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

// auditRateLimitedOutbound persiste a tentativa recusada sem alterar a resposta
// ao cliente.
//
// Política: a recusa continua 429 mesmo quando a auditoria falha. Devolver 500
// transformaria uma indisponibilidade momentânea do banco em sinal de erro do
// gateway para um cliente que na verdade está sendo barrado — e cliente que
// trata 5xx com retry agressivo amplificaria exatamente o volume que o rate
// limit existe para conter. A perda de auditoria fica no log, com o contexto de
// onde ocorreu, para reconciliação posterior.
func (s *server) auditRateLimitedOutbound(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	audit outboundAttemptAudit,
	origin string,
) {
	if err := s.persistRateLimitedOutbound(ctx, conn, audit); err != nil {
		log.Printf(
			"audit lost for rate-limited outbound (%s): system=%s connection=%s tenant=%s: %v — still responding 429",
			origin, conn.SystemID, conn.ID, conn.TenantID, err,
		)
		return
	}
	s.recordUsageBestEffort(ctx, conn.SystemID, conn.TenantID, usageDayUTC(time.Now()),
		repository.UsageDelta{Errors4xx: 1}, "rate-limit/"+origin)
}
