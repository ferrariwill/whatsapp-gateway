package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/session"
)

const maxMediaFormMemory = 32 << 20 // 32 MiB parse buffer; hard caps apply after

type sendMediaJSONRequest struct {
	TenantID      string `json:"tenant_id"`
	To            string `json:"to"`
	PhoneNumber   string `json:"phone_number"`
	Link          string `json:"link"`
	Caption       string `json:"caption"`
	Filename      string `json:"filename"`
	MimeType      string `json:"mime_type"`
	AppointmentID string `json:"appointment_id"`
}

type sendMediaResponse struct {
	MessageLogID  string              `json:"message_log_id"`
	Status        model.MessageStatus `json:"status"`
	MetaMessageID string              `json:"meta_message_id,omitempty"`
	MediaID       string              `json:"media_id,omitempty"`
	HostedURL     string              `json:"hosted_url,omitempty"`
}

func (s *server) handleSendImage(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, "image")
}

func (s *server) handleSendDocument(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, "document")
}

func (s *server) handleSendMedia(w http.ResponseWriter, r *http.Request, kind string) {
	s.ensureThrottleFields()
	s.ensureMediaStore()

	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	tenantID, to, caption, filename, mimeType, link, appointmentID, fileBytes, err := parseSendMediaRequest(r, kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	conn, err := s.repo.FindConnectionBySystemAndTenant(r.Context(), system.ID, tenantID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("whatsapp connection not found for %s/%s", system.Slug, tenantID),
		})
		return
	}
	if err != nil {
		log.Printf("lookup connection %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if !s.enforceConnectionSpamProtection(w, r, conn, outboundAttemptAudit{
		AppointmentID: appointmentID,
		PhoneNumber:   to,
		TemplateName:  kind,
	}) {
		return
	}

	withinLimit, err := s.usage.CheckMonthlyLimit(r.Context(), conn.SystemID, conn.TenantID)
	if err != nil {
		log.Printf("check monthly limit %s/%s: %v", system.Slug, tenantID, err)
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

	lastInbound, err := s.repo.GetLastInbound(r.Context(), conn.SystemID, conn.TenantID, to)
	if err != nil {
		log.Printf("get last inbound %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to check 24h window"})
		return
	}
	if !session.InWindow(lastInbound, time.Now().UTC()) {
		writeOutside24h(w)
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	mediaType := provider.MediaTypeImage
	if kind == "document" {
		mediaType = provider.MediaTypeDocument
	}

	var metaMediaID string
	var hostedURL string

	if len(fileBytes) > 0 {
		if err := validateOutboundMedia(kind, mimeType, len(fileBytes)); err != nil {
			if ve, ok := err.(mediaValidationError); ok {
				writeJSON(w, http.StatusUnprocessableEntity, structuredError{
					Error: ve.message,
					Code:  ve.code,
				})
				return
			}
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
			return
		}

		obj, storeErr := s.mediaStore.Put(conn.SystemID, conn.TenantID, mimeType, filename, fileBytes, defaultMediaTTL)
		if storeErr != nil {
			log.Printf("store media %s/%s: %v", system.Slug, tenantID, storeErr)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to store media"})
			return
		}
		hostedURL = s.signedMediaURL(r, obj)

		uploadedID, uploadErr := metaProvider.UploadMedia(
			r.Context(), conn.AccessToken, conn.PhoneNumberID, mimeType, filename, fileBytes,
		)
		if uploadErr != nil {
			log.Printf("upload media %s/%s: %v", system.Slug, tenantID, uploadErr)
			failureCode := ""
			if provider.IsMetaRateLimited(uploadErr) {
				failureCode = "meta_rate_limited"
			}
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: uploadErr.Error(), Code: failureCode})
			return
		}
		metaMediaID = uploadedID
	} else {
		if err := validateMediaLink(link); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		// Link path: optional mime hint only when provided (Meta fetches the URL).
		if mimeType != "" {
			if err := validateOutboundMedia(kind, mimeType, 1); err != nil {
				if ve, ok := err.(mediaValidationError); ok {
					writeJSON(w, http.StatusUnprocessableEntity, structuredError{
						Error: ve.message,
						Code:  ve.code,
					})
					return
				}
			}
		}
	}

	metaMessageID, sendErr := metaProvider.SendMediaMessage(
		r.Context(),
		conn.AccessToken,
		conn.PhoneNumberID,
		to,
		mediaType,
		metaMediaID,
		link,
		caption,
		filename,
	)

	status := model.MessageStatusSent
	failureCode := ""
	if sendErr != nil {
		status = model.MessageStatusFailed
		log.Printf("send media %s/%s: %v", system.Slug, tenantID, sendErr)
		if provider.IsMetaRateLimited(sendErr) {
			failureCode = "meta_rate_limited"
		}
	}

	sentContent := caption
	if sentContent == "" {
		if filename != "" {
			sentContent = filename
		} else if link != "" {
			sentContent = link
		} else {
			sentContent = kind
		}
	}

	messageLog := &model.MessageLog{
		SystemID:         conn.SystemID,
		ConnectionID:     conn.ID,
		SistemaOrigem:    conn.SistemaOrigem,
		ExternalClientID: conn.TenantID,
		MetaMessageID:    metaMessageID,
		AppointmentID:    appointmentID,
		PhoneNumber:      to,
		TemplateName:     kind,
		SentContent:      sentContent,
		Direction:        model.MessageDirectionOutbound,
		MessageCategory:  model.MessageCategoryService,
		Status:           status,
		FailureCode:      failureCode,
	}
	if err := s.repo.CreateMessageLog(r.Context(), messageLog); err != nil {
		log.Printf("persist message log for %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to persist message log"})
		return
	}

	if sendErr != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error(), Code: failureCode})
		return
	}

	writeJSON(w, http.StatusOK, sendMediaResponse{
		MessageLogID:  messageLog.ID,
		Status:        status,
		MetaMessageID: metaMessageID,
		MediaID:       metaMediaID,
		HostedURL:     hostedURL,
	})
}

// handleGetHostedMedia serves gateway-hosted binaries via signed URL (TTL + system scope).
// GET /v1/media/{id}?system_id=&exp=&sig=
func (s *server) handleGetHostedMedia(w http.ResponseWriter, r *http.Request) {
	s.ensureMediaStore()

	objectID := strings.TrimSpace(r.PathValue("id"))
	if objectID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "media id is required"})
		return
	}

	systemID := strings.TrimSpace(r.URL.Query().Get("system_id"))
	exp := strings.TrimSpace(r.URL.Query().Get("exp"))
	sig := strings.TrimSpace(r.URL.Query().Get("sig"))

	obj, ok := s.mediaStore.Get(objectID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "media not found"})
		return
	}

	// API-key path: product may fetch its own objects without signature.
	if system, hasSystem := systemFromContext(r.Context()); hasSystem {
		if system.ID != obj.SystemID {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden", Code: "cross_tenant"})
			return
		}
		writeMediaBytes(w, obj)
		return
	}

	if systemID == "" || !s.mediaStore.VerifySignature(systemID, objectID, exp, sig) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "invalid or expired media signature"})
		return
	}
	if systemID != obj.SystemID {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden", Code: "cross_tenant"})
		return
	}
	writeMediaBytes(w, obj)
}

func writeMediaBytes(w http.ResponseWriter, obj *mediaObject) {
	if obj.MimeType != "" {
		w.Header().Set("Content-Type", obj.MimeType)
	}
	if obj.Filename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, obj.Filename))
	}
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.Data)
}

func parseSendMediaRequest(r *http.Request, kind string) (
	tenantID, to, caption, filename, mimeType, link, appointmentID string,
	fileBytes []byte,
	err error,
) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxMediaFormMemory); err != nil {
			return "", "", "", "", "", "", "", nil, fmt.Errorf("invalid multipart body")
		}
		tenantID = strings.TrimSpace(r.FormValue("tenant_id"))
		to = firstNonEmpty(r.FormValue("to"), r.FormValue("phone_number"))
		caption = strings.TrimSpace(r.FormValue("caption"))
		filename = strings.TrimSpace(r.FormValue("filename"))
		mimeType = strings.TrimSpace(r.FormValue("mime_type"))
		link = strings.TrimSpace(r.FormValue("link"))
		appointmentID = strings.TrimSpace(r.FormValue("appointment_id"))

		file, header, fileErr := r.FormFile("file")
		if fileErr == nil {
			defer file.Close()
			data, readErr := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
			if readErr != nil {
				return "", "", "", "", "", "", "", nil, fmt.Errorf("failed to read uploaded file")
			}
			fileBytes = data
			if filename == "" && header != nil {
				filename = header.Filename
			}
			if mimeType == "" && header != nil {
				mimeType = header.Header.Get("Content-Type")
			}
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType = sniffMediaMIME(kind, filename, fileBytes)
			}
		}
	} else {
		var req sendMediaJSONRequest
		if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
			return "", "", "", "", "", "", "", nil, fmt.Errorf("invalid request body")
		}
		tenantID = strings.TrimSpace(req.TenantID)
		to = firstNonEmpty(req.To, req.PhoneNumber)
		caption = strings.TrimSpace(req.Caption)
		filename = strings.TrimSpace(req.Filename)
		mimeType = strings.TrimSpace(req.MimeType)
		link = strings.TrimSpace(req.Link)
		appointmentID = strings.TrimSpace(req.AppointmentID)
	}

	to = session.NormalizeWAID(to)
	if appointmentID == "" {
		appointmentID = "-"
	}
	if tenantID == "" || to == "" {
		return "", "", "", "", "", "", "", nil, fmt.Errorf("tenant_id and to (or phone_number) are required")
	}
	if len(fileBytes) == 0 && link == "" {
		return "", "", "", "", "", "", "", nil, fmt.Errorf("provide HTTPS link or multipart file")
	}
	if len(fileBytes) > 0 && link != "" {
		return "", "", "", "", "", "", "", nil, fmt.Errorf("provide either link or file, not both")
	}
	if kind == "document" && filename == "" && len(fileBytes) > 0 {
		filename = "document"
	}
	return tenantID, to, caption, filename, mimeType, link, appointmentID, fileBytes, nil
}

func validateMediaLink(link string) error {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("link must be a valid HTTPS URL")
	}
	return nil
}

func sniffMediaMIME(kind, filename string, data []byte) string {
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	if len(data) >= 8 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4e && data[3] == 0x47 {
		return "image/png"
	}
	if len(data) >= 4 && string(data[:4]) == "%PDF" {
		return "application/pdf"
	}
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	}
	if kind == "image" {
		return "image/jpeg"
	}
	return "application/pdf"
}

func (s *server) ensureMediaStore() {
	if s.mediaStore == nil {
		s.mediaStore = newMediaStore(mediaSigningSecret())
	}
}

func (s *server) signedMediaURL(r *http.Request, obj *mediaObject) string {
	exp := time.Now().UTC().Add(defaultMediaTTL)
	expUnix, sig := s.mediaStore.SignURLQuery(obj.SystemID, obj.ID, exp)
	base := publicBaseURL()
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		base = scheme + "://" + r.Host
	}
	q := url.Values{}
	q.Set("system_id", obj.SystemID)
	q.Set("exp", expUnix)
	q.Set("sig", sig)
	return fmt.Sprintf("%s/v1/media/%s?%s", base, obj.ID, q.Encode())
}

// handleAdminSendMediaTest is a minimal HTMX test form for ops.
func (s *server) handleAdminSendMediaTest(w http.ResponseWriter, r *http.Request) {
	s.ensureMediaStore()

	if err := r.ParseMultipartForm(maxMediaFormMemory); err != nil {
		http.Error(w, "invalid multipart body", http.StatusBadRequest)
		return
	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))
	tenantID := strings.TrimSpace(r.FormValue("tenant_id"))
	to := session.NormalizeWAID(firstNonEmpty(r.FormValue("to"), r.FormValue("phone_number")))
	kind := strings.ToLower(strings.TrimSpace(r.FormValue("type")))
	caption := strings.TrimSpace(r.FormValue("caption"))
	filename := strings.TrimSpace(r.FormValue("filename"))
	if kind != "document" {
		kind = "image"
	}
	if systemID == "" || tenantID == "" || to == "" {
		writeAdminMediaResult(w, false, "system_id, tenant_id e to são obrigatórios", "")
		return
	}

	system, err := s.repo.FindSystemByID(r.Context(), systemID)
	if err != nil || system == nil {
		writeAdminMediaResult(w, false, "aplicação não encontrada", "")
		return
	}

	file, header, fileErr := r.FormFile("file")
	if fileErr != nil {
		writeAdminMediaResult(w, false, "arquivo obrigatório", fileErr.Error())
		return
	}
	defer file.Close()
	data, readErr := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if readErr != nil {
		writeAdminMediaResult(w, false, "falha ao ler arquivo", readErr.Error())
		return
	}
	if filename == "" && header != nil {
		filename = header.Filename
	}
	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = sniffMediaMIME(kind, filename, data)
	}

	req, err := newAdminMediaAPIRequest(r.Context(), *system, kind, tenantID, to, caption, filename, mimeType, data)
	if err != nil {
		writeAdminMediaResult(w, false, "falha ao montar request", err.Error())
		return
	}

	rec := httptest.NewRecorder()
	s.handleSendMedia(rec, req, kind)
	if rec.Code == http.StatusOK {
		writeAdminMediaResult(w, true, "mídia enviada", rec.Body.String())
		return
	}
	writeAdminMediaResult(w, false, fmt.Sprintf("falha HTTP %d", rec.Code), rec.Body.String())
}

func newAdminMediaAPIRequest(
	ctx context.Context,
	system model.System,
	kind, tenantID, to, caption, filename, mimeType string,
	data []byte,
) (*http.Request, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("tenant_id", tenantID)
	_ = writer.WriteField("to", to)
	if caption != "" {
		_ = writer.WriteField("caption", caption)
	}
	if filename != "" {
		_ = writer.WriteField("filename", filename)
	}
	if mimeType != "" {
		_ = writer.WriteField("mime_type", mimeType)
	}
	header := make(textproto.MIMEHeader)
	if filename == "" {
		filename = "upload.bin"
	}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	if mimeType != "" {
		header.Set("Content-Type", mimeType)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	path := "/v1/messages/image"
	if kind == "document" {
		path = "/v1/messages/document"
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req = req.WithContext(context.WithValue(ctx, systemContextKey, system))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, nil
}

func writeAdminMediaResult(w http.ResponseWriter, ok bool, message, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cls := "text-rose-300"
	if ok {
		cls = "text-emerald-300"
	}
	fmt.Fprintf(w, `<div class="rounded-xl border border-slate-700 bg-slate-950 px-4 py-3 text-sm %s"><p class="font-medium">%s</p>`, cls, templateEscape(message))
	if detail != "" {
		fmt.Fprintf(w, `<pre class="mt-2 overflow-x-auto whitespace-pre-wrap text-xs text-slate-400">%s</pre>`, templateEscape(detail))
	}
	fmt.Fprint(w, `</div>`)
}

func templateEscape(s string) string {
	replacer := strings.NewReplacer(
		`&`, "&amp;",
		`<`, "&lt;",
		`>`, "&gt;",
		`"`, "&quot;",
	)
	return replacer.Replace(s)
}
