package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
	"github.com/whatsappgetway/gateway/internal/slug"
)

func (s *server) handleAdminCreateSystem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		respondSystemFormError(w, r, http.StatusBadRequest, "formulário inválido")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	webhookURL := strings.TrimSpace(r.FormValue("webhook_url"))
	systemSlug := strings.TrimSpace(strings.ToLower(r.FormValue("slug")))
	if name == "" {
		respondSystemFormError(w, r, http.StatusBadRequest, "informe o nome da aplicação")
		return
	}
	if systemSlug == "" {
		systemSlug = slug.Normalize(name)
	}
	if !slug.IsValid(systemSlug) {
		respondSystemFormError(w, r, http.StatusBadRequest, "slug inválido — use letras minúsculas, números e underscore (ex.: beleza, beleza_web, clinica)")
		return
	}

	apiKey, err := security.GenerateAPIKey()
	if err != nil {
		log.Printf("generate api key: %v", err)
		respondSystemFormError(w, r, http.StatusInternalServerError, "erro ao gerar chave de API")
		return
	}

	system := &model.System{
		Name:       name,
		Slug:       systemSlug,
		APIKeyHash: security.HashAPIKey(apiKey),
		WebhookURL: webhookURL,
	}
	if err := s.repo.CreateSystem(r.Context(), system); err != nil {
		if errors.Is(err, repository.ErrDuplicateSystemSlug) {
			respondSystemFormError(w, r, http.StatusBadRequest, fmt.Sprintf("slug %q já está em uso — escolha outro", systemSlug))
			return
		}
		log.Printf("create system: %v", err)
		respondSystemFormError(w, r, http.StatusInternalServerError, "erro ao salvar aplicação")
		return
	}

	row := newSystemRow(*system)
	keyReveal := systemAPIKeyRevealHTML(apiKey)
	systemSelectOOB := systemSelectOptionOOB(system.ID, system.Name, system.Slug)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "system_row.html", row); err != nil {
		log.Printf("render system row: %v", err)
		respondSystemFormError(w, r, http.StatusInternalServerError, "aplicação salva, mas falha ao renderizar linha")
		return
	}
	_, _ = w.Write([]byte(keyReveal + systemSelectOOB))
}

func (s *server) handleAdminUpdateSystem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	systemID := strings.TrimSpace(r.PathValue("id"))
	if systemID == "" {
		respondSystemFormError(w, r, http.StatusBadRequest, "identificador da aplicação inválido")
		return
	}

	if err := r.ParseForm(); err != nil {
		respondSystemFormError(w, r, http.StatusBadRequest, "formulário inválido")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	webhookURL := strings.TrimSpace(r.FormValue("webhook_url"))
	systemSlug := strings.TrimSpace(strings.ToLower(r.FormValue("slug")))
	if name == "" {
		respondFormFeedback(w, r, "edit-system-form-feedback", "informe o nome da aplicação")
		return
	}

	existing, err := s.repo.FindSystemByID(r.Context(), systemID)
	if err != nil {
		respondFormFeedback(w, r, "edit-system-form-feedback", "aplicação não encontrada")
		return
	}
	if systemSlug == "" {
		systemSlug = existing.Slug
	}
	if !slug.IsValid(systemSlug) {
		respondFormFeedback(w, r, "edit-system-form-feedback", "slug inválido — use letras minúsculas, números e underscore")
		return
	}

	if err := s.repo.UpdateSystem(r.Context(), systemID, name, systemSlug, webhookURL); err != nil {
		if errors.Is(err, repository.ErrDuplicateSystemSlug) {
			respondFormFeedback(w, r, "edit-system-form-feedback", fmt.Sprintf("slug %q já está em uso", systemSlug))
			return
		}
		if errors.Is(err, repository.ErrSystemNotFound) {
			respondFormFeedback(w, r, "edit-system-form-feedback", "aplicação não encontrada")
			return
		}
		log.Printf("update system %s: %v", systemID, err)
		respondFormFeedback(w, r, "edit-system-form-feedback", "erro ao atualizar aplicação")
		return
	}

	system, err := s.repo.FindSystemByID(r.Context(), systemID)
	if err != nil {
		log.Printf("find system %s after update: %v", systemID, err)
		respondFormFeedback(w, r, "edit-system-form-feedback", "aplicação atualizada, mas falha ao recarregar")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "system_row.html", newSystemRow(*system)); err != nil {
		log.Printf("render system row after update: %v", err)
		respondFormFeedback(w, r, "edit-system-form-feedback", "aplicação atualizada, mas falha ao renderizar linha")
	}
}

func (s *server) handleAdminDeleteSystem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	systemID := strings.TrimSpace(r.PathValue("id"))
	if systemID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := s.repo.DeleteSystem(r.Context(), systemID); err != nil {
		if errors.Is(err, repository.ErrSystemNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("delete system %s: %v", systemID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleAdminUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	channelID := strings.TrimSpace(r.PathValue("id"))
	if channelID == "" {
		respondChannelFormError(w, r, http.StatusBadRequest, "identificador do canal inválido")
		return
	}

	if err := r.ParseForm(); err != nil {
		respondChannelFormError(w, r, http.StatusBadRequest, "formulário inválido")
		return
	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))
	salonName := strings.TrimSpace(r.FormValue("salon_name"))
	externalClientID := strings.TrimSpace(r.FormValue("external_client_id"))
	whatsappPhoneNumber := strings.TrimSpace(r.FormValue("whatsapp_phone_number"))
	phoneNumberID := strings.TrimSpace(r.FormValue("phone_number_id"))

	if systemID == "" || salonName == "" || externalClientID == "" || whatsappPhoneNumber == "" || phoneNumberID == "" {
		respondFormFeedback(w, r, "edit-channel-form-feedback", "preencha todos os campos obrigatórios")
		return
	}

	if _, err := s.repo.FindSystemByID(r.Context(), systemID); err != nil {
		if errors.Is(err, repository.ErrSystemNotFound) {
			respondFormFeedback(w, r, "edit-channel-form-feedback", "aplicação selecionada não encontrada")
			return
		}
		log.Printf("find system %s for channel update: %v", systemID, err)
		respondFormFeedback(w, r, "edit-channel-form-feedback", "erro ao validar aplicação")
		return
	}

	channel := &model.ClientChannel{
		ID:                  channelID,
		SystemID:            systemID,
		SalonName:           salonName,
		ExternalClientID:    externalClientID,
		PhoneNumberID:       phoneNumberID,
		WhatsAppPhoneNumber: whatsappPhoneNumber,
	}

	if err := s.repo.UpdateClientChannel(r.Context(), channel); err != nil {
		if errors.Is(err, repository.ErrClientChannelNotFound) {
			respondFormFeedback(w, r, "edit-channel-form-feedback", "canal não encontrado")
			return
		}
		log.Printf("update client channel %s: %v", channelID, err)
		respondFormFeedback(w, r, "edit-channel-form-feedback", "erro ao atualizar canal — verifique IDs duplicados")
		return
	}

	updated, err := s.repo.FindClientChannelByID(r.Context(), channelID)
	if err != nil {
		log.Printf("find client channel %s after update: %v", channelID, err)
		respondFormFeedback(w, r, "edit-channel-form-feedback", "canal atualizado, mas falha ao recarregar")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "channel_row.html", channelRowFromModel(*updated)); err != nil {
		log.Printf("render channel row after update: %v", err)
		respondFormFeedback(w, r, "edit-channel-form-feedback", "canal atualizado, mas falha ao renderizar linha")
	}
}

func (s *server) handleAdminDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminUser(w, r) {
		return
	}

	channelID := strings.TrimSpace(r.PathValue("id"))
	if channelID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := s.repo.DeleteClientChannel(r.Context(), channelID); err != nil {
		if errors.Is(err, repository.ErrClientChannelNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("delete client channel %s: %v", channelID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *server) requireAdminUser(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := userIDFromContext(r.Context()); ok {
		return true
	}
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return false
}
