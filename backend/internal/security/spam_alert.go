package security

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// TextMessageSender envia mensagens de texto via WhatsApp Cloud API.
type TextMessageSender interface {
	SendTextMessage(ctx context.Context, accessToken, phoneNumberID, to, body string) error
	SendUtilityTemplate(ctx context.Context, accessToken, phoneNumberID, to, templateName string, bodyParams []string) error
}

const adminAlertTimeout = 30 * time.Second

// NotifyAdminSpamAlert dispara assincronamente um alerta de segurança para o administrador via WhatsApp.
func NotifyAdminSpamAlert(ctx context.Context, sender TextMessageSender, systemName, clientID string, messageCount int) {
	if sender == nil {
		log.Printf("spam alert skipped: text message sender is nil")
		return
	}

	adminPhone := strings.TrimSpace(os.Getenv("ADMIN_PHONE_NUMBER"))
	if adminPhone == "" {
		log.Printf("spam alert skipped: ADMIN_PHONE_NUMBER is not configured")
		return
	}

	accessToken := strings.TrimSpace(os.Getenv("ADMIN_ALERT_ACCESS_TOKEN"))
	phoneNumberID := strings.TrimSpace(os.Getenv("ADMIN_ALERT_PHONE_NUMBER_ID"))
	if accessToken == "" || phoneNumberID == "" {
		log.Printf("spam alert skipped: ADMIN_ALERT_ACCESS_TOKEN and ADMIN_ALERT_PHONE_NUMBER_ID are required")
		return
	}

	body := fmt.Sprintf(
		"⚠️ *ALERTA DE SEGURANÇA - GATEWAY WHATSAPP* ⚠️\n\n"+
			"Uma possível suspeita de SPAM foi detectada e o cliente foi bloqueado temporariamente por 15 minutos.\n\n"+
			"• *Aplicação:* %s\n"+
			"• *ID do Cliente:* %s\n"+
			"• *Disparos no último minuto:* %d\n\n"+
			"Acesse o painel administrativo para avaliar e restabelecer a ligação manualmente se necessário.",
		systemName,
		clientID,
		messageCount,
	)

	templateName := strings.TrimSpace(os.Getenv("ADMIN_ALERT_TEMPLATE_NAME"))

	go func() {
		alertCtx, cancel := context.WithTimeout(context.Background(), adminAlertTimeout)
		defer cancel()

		var err error
		if templateName != "" {
			err = sender.SendUtilityTemplate(alertCtx, accessToken, phoneNumberID, adminPhone, templateName, []string{
				systemName,
				clientID,
				fmt.Sprintf("%d", messageCount),
			})
		} else {
			err = sender.SendTextMessage(alertCtx, accessToken, phoneNumberID, adminPhone, body)
		}
		if err != nil {
			log.Printf("admin spam alert to %s failed: %v", adminPhone, err)
			return
		}
		log.Printf("admin spam alert sent for system %s client %s (%d msgs/min)", systemName, clientID, messageCount)
	}()

	_ = ctx
}
