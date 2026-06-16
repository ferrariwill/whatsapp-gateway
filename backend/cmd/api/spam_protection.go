package main

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

func (s *server) handleAdminUnblockClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := userIDFromContext(r.Context()); !ok {
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	channelID := strings.TrimSpace(r.PathValue("id"))
	if channelID == "" {
		writeHTML(w, http.StatusBadRequest, channelFormErrorHTML("identificador do cliente inválido"))
		return
	}

	channel, err := s.repo.FindClientChannelByID(r.Context(), channelID)
	if errors.Is(err, repository.ErrClientChannelNotFound) {
		writeHTML(w, http.StatusNotFound, channelFormErrorHTML("cliente não encontrado"))
		return
	}
	if err != nil {
		log.Printf("find client channel %s for unblock: %v", channelID, err)
		writeHTML(w, http.StatusInternalServerError, channelFormErrorHTML("erro ao carregar cliente"))
		return
	}

	s.rateLimiter.Unblock(channel.SystemID, channel.ExternalClientID)

	if err := s.repo.ActivateClientChannel(r.Context(), channelID); err != nil {
		log.Printf("activate client channel %s: %v", channelID, err)
		writeHTML(w, http.StatusInternalServerError, channelFormErrorHTML("erro ao reativar cliente"))
		return
	}

	channel.Status = string(security.ClientChannelStatusActive)
	row := channelRowFromModel(*channel)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "channel_row.html", row); err != nil {
		log.Printf("render channel row after unblock: %v", err)
		writeHTML(w, http.StatusInternalServerError, channelFormErrorHTML("cliente reativado, mas falha ao renderizar linha"))
	}
}

func (s *server) enforceSpamProtection(
	w http.ResponseWriter,
	r *http.Request,
	system model.System,
	channel *model.ClientChannel,
) bool {
	if channel.Status == string(security.ClientChannelStatusSuspendedSpam) ||
		s.rateLimiter.IsBlacklisted(system.ID, channel.ExternalClientID) {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: "client temporarily blocked due to spam protection",
		})
		return false
	}

	result := s.rateLimiter.RecordAttempt(system.ID, channel.ExternalClientID)
	if result.Allowed {
		return true
	}

	if result.TriggerAlert {
		if err := s.repo.SuspendClientChannelForSpam(r.Context(), channel.ID); err != nil {
			log.Printf("suspend client channel %s for spam: %v", channel.ID, err)
		} else {
			channel.Status = string(security.ClientChannelStatusSuspendedSpam)
		}

		metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
		security.NotifyAdminSpamAlert(r.Context(), metaProvider, system.Name, channel.ExternalClientID, result.Count)
	}

	writeJSON(w, http.StatusTooManyRequests, errorResponse{
		Error: security.FormatRateLimitError(result.Count, s.rateLimiter.MaxPerMinute()),
	})
	return false
}

func channelRowFromModel(channel model.ClientChannelWithSystem) channelRowData {
	return newChannelRow(
		channel.SystemName,
		channel.ID,
		channel.SalonName,
		channel.ExternalClientID,
		channel.WhatsAppPhoneNumber,
		channel.PhoneNumberID,
		channel.Status,
		channel.CreatedAt,
	)
}
