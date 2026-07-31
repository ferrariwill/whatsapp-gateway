package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
)

func generateWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func ensureWebhookSecret(existing string) (string, error) {
	if strings.TrimSpace(existing) != "" {
		return existing, nil
	}
	return generateWebhookSecret()
}

func (s *server) handleAdminCreateConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		respondConnectionFormError(w, r, "formulário inválido")
		return
	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))
	system, err := s.repo.FindSystemByID(r.Context(), systemID)
	if errors.Is(err, repository.ErrSystemNotFound) {
		respondConnectionFormError(w, r, "selecione uma aplicação mãe válida")
		return
	}
	if err != nil {
		log.Printf("find system %s: %v", systemID, err)
		respondConnectionFormError(w, r, "erro ao carregar aplicação mãe")
		return
	}

	secret, err := ensureWebhookSecret(strings.TrimSpace(r.FormValue("webhook_secret")))
	if err != nil {
		respondConnectionFormError(w, r, "erro ao gerar webhook_secret")
		return
	}

	conn := &model.WhatsAppConnection{
		SystemID:            system.ID,
		SistemaOrigem:       system.Slug,
		TenantID:            strings.TrimSpace(r.FormValue("tenant_id")),
		WabaID:              strings.TrimSpace(r.FormValue("waba_id")),
		PhoneNumberID:       strings.TrimSpace(r.FormValue("phone_number_id")),
		AccessToken:         strings.TrimSpace(r.FormValue("access_token")),
		WebhookURL:          strings.TrimSpace(r.FormValue("webhook_url")),
		WebhookSecret:       secret,
		WhatsAppPhoneNumber: strings.TrimSpace(r.FormValue("whatsapp_phone_number")),
	}

	if conn.TenantID == "" || conn.WabaID == "" || conn.PhoneNumberID == "" || conn.AccessToken == "" {
		respondConnectionFormError(w, r, "preencha tenant_id, waba_id, phone_number_id e access_token")
		return
	}

	if err := s.repo.CreateWhatsAppConnection(r.Context(), conn); err != nil {
		log.Printf("create connection: %v", err)
		respondConnectionFormError(w, r, "erro ao salvar conexão — verifique duplicatas")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "connection_row.html", newConnectionRow(*conn, system.Name)); err != nil {
		log.Printf("render connection row: %v", err)
		respondConnectionFormError(w, r, "conexão salva, mas falha ao renderizar linha")
	}
}

func (s *server) handleAdminUpdateConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	connID := strings.TrimSpace(r.PathValue("id"))
	if connID == "" {
		respondConnectionFormError(w, r, "identificador inválido")
		return
	}
	if err := r.ParseForm(); err != nil {
		respondConnectionFormError(w, r, "formulário inválido")
		return
	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))
	system, err := s.repo.FindSystemByID(r.Context(), systemID)
	if errors.Is(err, repository.ErrSystemNotFound) {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "selecione uma aplicação mãe válida")
		return
	}
	if err != nil {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "erro ao carregar aplicação mãe")
		return
	}

	existing, err := s.repo.FindConnectionByID(r.Context(), connID)
	if err != nil {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "conexão não encontrada")
		return
	}

	secret := strings.TrimSpace(r.FormValue("webhook_secret"))
	if secret == "" {
		secret = existing.WebhookSecret
	}
	if secret == "" {
		generated, genErr := generateWebhookSecret()
		if genErr != nil {
			respondFormFeedback(w, r, "edit-connection-form-feedback", "erro ao gerar webhook_secret")
			return
		}
		secret = generated
	}

	conn := &model.WhatsAppConnection{
		ID:                  connID,
		SystemID:            system.ID,
		SistemaOrigem:       system.Slug,
		TenantID:            strings.TrimSpace(r.FormValue("tenant_id")),
		WabaID:              strings.TrimSpace(r.FormValue("waba_id")),
		PhoneNumberID:       strings.TrimSpace(r.FormValue("phone_number_id")),
		AccessToken:         strings.TrimSpace(r.FormValue("access_token")),
		WebhookURL:          strings.TrimSpace(r.FormValue("webhook_url")),
		WebhookSecret:       secret,
		WhatsAppPhoneNumber: strings.TrimSpace(r.FormValue("whatsapp_phone_number")),
	}

	if conn.TenantID == "" || conn.WabaID == "" || conn.PhoneNumberID == "" {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "preencha todos os campos obrigatórios")
		return
	}

	if conn.AccessToken == "" {
		conn.AccessToken = existing.AccessToken
	}

	if err := s.repo.UpdateWhatsAppConnection(r.Context(), conn); err != nil {
		if errors.Is(err, repository.ErrConnectionNotFound) {
			respondFormFeedback(w, r, "edit-connection-form-feedback", "conexão não encontrada")
			return
		}
		log.Printf("update connection %s: %v", connID, err)
		respondFormFeedback(w, r, "edit-connection-form-feedback", "erro ao atualizar conexão")
		return
	}

	updated, err := s.repo.FindConnectionByID(r.Context(), connID)
	if err != nil {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "conexão atualizada, mas falha ao recarregar")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "connection_row.html", newConnectionRow(*updated, system.Name)); err != nil {
		respondFormFeedback(w, r, "edit-connection-form-feedback", "conexão atualizada, mas falha ao renderizar")
	}
}

func (s *server) handleAdminDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	connID := strings.TrimSpace(r.PathValue("id"))
	if connID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := s.repo.DeleteWhatsAppConnection(r.Context(), connID); err != nil {
		if errors.Is(err, repository.ErrConnectionNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("delete connection %s: %v", connID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleAdminUnblockConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	connID := strings.TrimSpace(r.PathValue("id"))
	conn, err := s.repo.FindConnectionByID(r.Context(), connID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeHTML(w, http.StatusNotFound, connectionFormErrorHTML("conexão não encontrada"))
		return
	}
	if err != nil {
		writeHTML(w, http.StatusInternalServerError, connectionFormErrorHTML("erro ao carregar conexão"))
		return
	}

	s.rateLimiter.Unblock(conn.SystemID, conn.TenantID)
	if err := s.repo.ActivateConnection(r.Context(), connID); err != nil {
		writeHTML(w, http.StatusInternalServerError, connectionFormErrorHTML("erro ao reativar conexão"))
		return
	}

	systemName := conn.SistemaOrigem
	if system, err := s.repo.FindSystemByID(r.Context(), conn.SystemID); err == nil {
		systemName = system.Name
	}

	conn.Status = model.ConnectionStatusActive
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "connection_row.html", newConnectionRow(*conn, systemName))
}

func respondConnectionFormError(w http.ResponseWriter, r *http.Request, message string) {
	respondFormFeedback(w, r, "connection-form-feedback", message)
}

func connectionFormErrorHTML(message string) string {
	return fmt.Sprintf(`<tr><td colspan="10" class="px-4 py-3 text-sm text-red-300">%s</td></tr>`, template.HTMLEscapeString(message))
}
