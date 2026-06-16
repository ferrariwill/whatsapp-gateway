package main

import (
	"fmt"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
)

func formatOutboundSentContent(templateName string, variables []string) string {
	templateName = strings.TrimSpace(templateName)
	if len(variables) == 0 {
		return fmt.Sprintf("Template: %s", templateName)
	}
	return fmt.Sprintf("Template: %s | Variáveis: %s", templateName, strings.Join(variables, ", "))
}

func auditDisplayContent(log model.MessageLog) string {
	if log.Direction == model.MessageDirectionInbound {
		return strings.TrimSpace(log.ReceivedContent)
	}
	return strings.TrimSpace(log.SentContent)
}

func auditTemplateLabel(log model.MessageLog) string {
	if log.Direction == model.MessageDirectionInbound {
		if strings.TrimSpace(log.TemplateName) != "" {
			return log.TemplateName
		}
		return "Resposta do cliente"
	}
	if strings.TrimSpace(log.TemplateName) != "" {
		return log.TemplateName
	}
	return "—"
}

func clientAuditKey(systemID, externalClientID string) string {
	return systemID + "|" + strings.TrimSpace(externalClientID)
}
