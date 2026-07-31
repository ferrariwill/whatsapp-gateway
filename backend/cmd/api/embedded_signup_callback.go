package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

type embeddedSignupConnectionPayload struct {
	Event               string `json:"event"`
	Message             string `json:"message"`
	SystemID            string `json:"system_id"`
	SistemaOrigem       string `json:"sistema_origem"`
	TenantID            string `json:"tenant_id"`
	SalonID             string `json:"salon_id"`
	WabaID              string `json:"waba_id"`
	PhoneNumberID       string `json:"phone_number_id"`
	WhatsAppPhoneNumber string `json:"whatsapp_phone_number,omitempty"`
	Status              string `json:"status"`
}

// handleEmbeddedSignupCallback recebe o redirect OAuth do Embedded Signup da Meta.
// state aceito:
//   - assinado: security.SignEmbeddedSignupState (recomendado)
//   - legado: "{slug}_{tenant_id}" ou "{slug}::{tenant_id}" (tenant com "_" só via "::" ou assinado)
func (s *server) handleEmbeddedSignupCallback(w http.ResponseWriter, r *http.Request) {
	if errMsg := strings.TrimSpace(r.URL.Query().Get("error")); errMsg != "" {
		reason := strings.TrimSpace(r.URL.Query().Get("error_description"))
		if reason == "" {
			reason = strings.TrimSpace(r.URL.Query().Get("error_reason"))
		}
		log.Printf("embedded signup denied: %s (%s)", errMsg, reason)
		writeEmbeddedSignupResult(w, r, http.StatusBadRequest, false, "cadastro cancelado ou negado pela Meta", reason)
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" {
		writeEmbeddedSignupResult(w, r, http.StatusBadRequest, false, "code ausente no callback", "")
		return
	}
	if state == "" {
		writeEmbeddedSignupResult(w, r, http.StatusBadRequest, false, "state ausente no callback", "")
		return
	}

	appID := strings.TrimSpace(os.Getenv("META_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("META_APP_SECRET"))
	redirectURI := strings.TrimSpace(os.Getenv("META_EMBEDDED_SIGNUP_REDIRECT_URI"))
	if appID == "" || appSecret == "" || redirectURI == "" {
		log.Printf("embedded signup missing META_APP_ID/SECRET/REDIRECT_URI")
		writeEmbeddedSignupResult(w, r, http.StatusInternalServerError, false, "configuração Meta incompleta no gateway", "")
		return
	}

	slug, tenantID, stateSigned, err := resolveEmbeddedSignupState(appSecret, state)
	if err != nil {
		writeEmbeddedSignupResult(w, r, http.StatusBadRequest, false, err.Error(), "")
		return
	}

	system, err := s.repo.FindSystemBySlug(r.Context(), slug)
	if errors.Is(err, repository.ErrSystemSlugNotFound) {
		writeEmbeddedSignupResult(w, r, http.StatusBadRequest, false, fmt.Sprintf("aplicação mãe desconhecida: slug %q não cadastrado no gateway", slug), "")
		return
	}
	if err != nil {
		log.Printf("embedded signup lookup system %s: %v", slug, err)
		writeEmbeddedSignupResult(w, r, http.StatusInternalServerError, false, "erro ao identificar aplicação mãe", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	accessToken, err := metaProvider.ExchangeOAuthCode(ctx, appID, appSecret, code, redirectURI)
	if err != nil {
		log.Printf("embedded signup token exchange %s/%s: %v", slug, tenantID, err)
		writeEmbeddedSignupResult(w, r, http.StatusBadGateway, false, "falha ao trocar code por access token", err.Error())
		return
	}

	assets, err := metaProvider.ResolveEmbeddedSignupAssets(ctx, appID, appSecret, accessToken)
	if err != nil {
		log.Printf("embedded signup resolve assets %s/%s: %v", slug, tenantID, err)
		writeEmbeddedSignupResult(w, r, http.StatusBadGateway, false, "falha ao obter WABA e phone_number_id", err.Error())
		return
	}

	existing, findErr := s.repo.FindConnectionBySystemAndTenant(ctx, system.ID, tenantID)
	if findErr != nil && !errors.Is(findErr, repository.ErrConnectionNotFound) {
		log.Printf("embedded signup lookup existing %s/%s: %v", slug, tenantID, findErr)
		writeEmbeddedSignupResult(w, r, http.StatusInternalServerError, false, "erro ao verificar conexão existente", "")
		return
	}
	if existing != nil {
		identityChange := existing.PhoneNumberID != assets.PhoneNumberID || existing.WabaID != assets.WabaID
		if identityChange && !stateSigned {
			log.Printf(
				"embedded signup blocked identity takeover %s/%s: existing phone=%s waba=%s, incoming phone=%s waba=%s",
				slug, tenantID, existing.PhoneNumberID, existing.WabaID, assets.PhoneNumberID, assets.WabaID,
			)
			writeEmbeddedSignupResult(w, r, http.StatusConflict, false,
				"conexão já ativa com outro número WhatsApp; reconectar exige state OAuth assinado", "")
			return
		}
	}

	webhookURL := strings.TrimSpace(system.WebhookURL)
	if webhookURL == "" {
		webhookURL = saasWebhookURLForSistema(system.Slug)
	}

	secret, err := generateWebhookSecret()
	if err != nil {
		log.Printf("embedded signup generate webhook_secret %s/%s: %v", slug, tenantID, err)
		writeEmbeddedSignupResult(w, r, http.StatusInternalServerError, false, "falha ao gerar webhook_secret", err.Error())
		return
	}

	conn := &model.WhatsAppConnection{
		SystemID:            system.ID,
		SistemaOrigem:       system.Slug,
		TenantID:            tenantID,
		WabaID:              assets.WabaID,
		PhoneNumberID:       assets.PhoneNumberID,
		AccessToken:         assets.AccessToken,
		WebhookURL:          webhookURL,
		WebhookSecret:       secret,
		WhatsAppPhoneNumber: assets.WhatsAppPhoneNumber,
		Status:              model.ConnectionStatusActive,
	}

	if err := s.repo.UpsertWhatsAppConnection(ctx, conn); err != nil {
		log.Printf("embedded signup upsert %s/%s: %v", slug, tenantID, err)
		writeEmbeddedSignupResult(w, r, http.StatusInternalServerError, false, "falha ao salvar conexão no banco", err.Error())
		return
	}

	if err := s.notifySaaSWhatsAppActive(ctx, conn); err != nil {
		log.Printf("embedded signup notify saas %s/%s: %v", slug, tenantID, err)
		writeEmbeddedSignupResult(w, r, http.StatusBadGateway, false, "conexão salva, mas falha ao notificar SaaS", err.Error())
		return
	}

	log.Printf("embedded signup completed: system=%s tenant=%s waba=%s phone=%s signed_state=%v", slug, tenantID, conn.WabaID, conn.PhoneNumberID, stateSigned)
	writeEmbeddedSignupResult(w, r, http.StatusOK, true, fmt.Sprintf("O tenant %s (%s) agora está com o WhatsApp ativo", tenantID, system.Name), "")
}

// resolveEmbeddedSignupState prefere state HMAC assinado; fallback legado para compatibilidade.
func resolveEmbeddedSignupState(appSecret, state string) (slug, tenantID string, signed bool, err error) {
	slug, tenantID, signed, err = security.VerifyEmbeddedSignupState(appSecret, state)
	if err != nil {
		return "", "", false, err
	}
	if signed {
		return slug, tenantID, true, nil
	}
	slug, tenantID, err = parseEmbeddedSignupState(state)
	return slug, tenantID, false, err
}

// parseEmbeddedSignupState extrai slug e tenant_id.
// Preferir "{slug}::{tenant_id}" quando o tenant_id contém "_".
// Legado "{slug}_{tenant_id}" usa o último underscore (ambíguo com "_" no tenant).
func parseEmbeddedSignupState(state string) (slug, tenantID string, err error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return "", "", errors.New("state ausente no callback")
	}

	if idx := strings.Index(state, "::"); idx > 0 && idx < len(state)-2 {
		slug = strings.TrimSpace(strings.ToLower(state[:idx]))
		tenantID = strings.TrimSpace(state[idx+2:])
		if slug == "" || tenantID == "" {
			return "", "", fmt.Errorf("state inválido: esperado {slug}::{tenant_id}")
		}
		return slug, tenantID, nil
	}

	idx := strings.LastIndex(state, "_")
	if idx <= 0 || idx >= len(state)-1 {
		return "", "", fmt.Errorf("state inválido: esperado {slug}_{tenant_id} ou {slug}::{tenant_id} (ex.: beleza_123, beleza_web::salao_1)")
	}

	slug = strings.TrimSpace(strings.ToLower(state[:idx]))
	tenantID = strings.TrimSpace(state[idx+1:])
	if slug == "" || tenantID == "" {
		return "", "", fmt.Errorf("state inválido: slug e tenant_id são obrigatórios")
	}

	return slug, tenantID, nil
}

func saasWebhookURLForSistema(sistemaOrigem string) string {
	sistemaOrigem = strings.ToLower(strings.TrimSpace(sistemaOrigem))
	if sistemaOrigem == "" {
		return ""
	}
	envKey := strings.ToUpper(strings.ReplaceAll(sistemaOrigem, "-", "_")) + "_SAAS_WEBHOOK_URL"
	if url := strings.TrimSpace(os.Getenv(envKey)); url != "" {
		return url
	}
	return strings.TrimSpace(os.Getenv("MOTHER_SYSTEM_WEBHOOK_URL"))
}

func (s *server) notifySaaSWhatsAppActive(ctx context.Context, conn *model.WhatsAppConnection) error {
	targetURL := strings.TrimSpace(conn.WebhookURL)
	if targetURL == "" {
		targetURL = saasWebhookURLForSistema(conn.SistemaOrigem)
	}
	if targetURL == "" {
		return fmt.Errorf("webhook SaaS não configurado para %s", conn.SistemaOrigem)
	}

	payload := embeddedSignupConnectionPayload{
		Event:               "whatsapp_connection_completed",
		Message:             fmt.Sprintf("O tenant %s agora está com o WhatsApp ativo", conn.TenantID),
		SystemID:            conn.SystemID,
		SistemaOrigem:       conn.SistemaOrigem,
		TenantID:            conn.TenantID,
		SalonID:             conn.TenantID,
		WabaID:              conn.WabaID,
		PhoneNumberID:       conn.PhoneNumberID,
		WhatsAppPhoneNumber: conn.WhatsAppPhoneNumber,
		Status:              "connected",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.metaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("saas webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func writeEmbeddedSignupResult(w http.ResponseWriter, r *http.Request, status int, success bool, message, detail string) {
	accept := ""
	if r != nil {
		accept = strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	}
	if strings.Contains(accept, "application/json") {
		writeJSON(w, status, map[string]any{
			"success": success,
			"message": message,
			"detail":  detail,
		})
		return
	}

	title := "Conexão WhatsApp"
	color := "#059669"
	if !success {
		title = "Falha na conexão WhatsApp"
		color = "#dc2626"
	}

	escapedMessage := htmlEscape(message)
	escapedDetail := htmlEscape(detail)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="pt-BR"><head><meta charset="utf-8"><title>%s</title></head><body style="font-family:system-ui,sans-serif;padding:2rem;background:#0f172a;color:#e2e8f0"><div style="max-width:32rem;margin:auto;border:1px solid %s;border-radius:1rem;padding:1.5rem;background:#1e293b"><h1 style="color:%s;margin-top:0">%s</h1><p>%s</p>%s</div></body></html>`,
		title, color, color, title, escapedMessage, detailBlock(escapedDetail))
}

func detailBlock(detail string) string {
	if detail == "" {
		return ""
	}
	return fmt.Sprintf(`<p style="color:#94a3b8;font-size:.9rem">%s</p>`, detail)
}

func htmlEscape(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(value)
}
