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

const maxMediaFormMemory = 32 << 20

type sendMediaJSONRequest struct {
	TenantID      string `json:"tenant_id"`
	PhoneNumber   string `json:"phone_number"`
	To            string `json:"to"` // alias
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
}

func (s *server) handleSendImage(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, "image")
}

func (s *server) handleSendDocument(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, "document")
}

func (s *server) handleSendMedia(w http.ResponseWriter, r *http.Request, kind string) {
	s.ensureThrottleFields()

	system, ok := systemFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})
		return
	}

	tenantID, phone, caption, filename, mimeType, link, appointmentID, fileBytes, err := parseSendMediaRequest(r, kind)
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
		PhoneNumber:   phone,
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

	lastInbound, err := s.repo.GetLastInbound(r.Context(), conn.SystemID, conn.TenantID, phone)
	if err != nil {
		log.Printf("get last inbound %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to check 24h window"})
		return
	}
	if !session.InWindow(lastInbound, time.Now().UTC()) {
		writeOutside24h(w)
		return
	}

	if len(fileBytes) > 0 {
		var valErr error
		if kind == "image" {
			valErr = validateImageUpload(mimeType, len(fileBytes))
		} else {
			valErr = validateDocumentUpload(mimeType, len(fileBytes))
		}
		if valErr != nil {
			writeMediaValidationError(w, valErr)
			return
		}
	} else if err := validateMediaLink(link); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	var metaMediaID string
	var mediaObjectID string

	if len(fileBytes) > 0 {
		obj, storeErr := s.persistOutboundMedia(r.Context(), conn.SystemID, conn.TenantID, mimeType, filename, fileBytes, defaultMediaTTL)
		if storeErr != nil {
			log.Printf("persist media %s/%s: %v", system.Slug, tenantID, storeErr)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to store media"})
			return
		}
		mediaObjectID = obj.ID

		uploadedID, uploadErr := metaProvider.UploadMedia(
			r.Context(), conn.AccessToken, conn.PhoneNumberID, filename, mimeType, bytes.NewReader(fileBytes),
		)
		if uploadErr != nil {
			log.Printf("upload media %s/%s: %v", system.Slug, tenantID, uploadErr)
			failureCode := ""
			if provider.IsMetaRateLimited(uploadErr) {
				failureCode = "meta_rate_limited"
			}
			s.recordUsageBestEffort(r.Context(), conn.SystemID, conn.TenantID, usageDayUTC(time.Now()),
				outboundErrorDelta(uploadErr), "send-media/upload-error")
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: uploadErr.Error(), Code: failureCode})
			return
		}
		metaMediaID = uploadedID
	}

	var metaMessageID string
	var sendErr error
	if kind == "image" {
		metaMessageID, sendErr = metaProvider.SendImageMessage(
			r.Context(), conn.AccessToken, conn.PhoneNumberID, phone,
			provider.ImageSendOpts{Link: link, MediaID: metaMediaID, Caption: caption},
		)
	} else {
		metaMessageID, sendErr = metaProvider.SendDocumentMessage(
			r.Context(), conn.AccessToken, conn.PhoneNumberID, phone,
			provider.DocumentSendOpts{Link: link, MediaID: metaMediaID, Caption: caption, Filename: filename},
		)
	}

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
		} else if mediaObjectID != "" {
			sentContent = mediaObjectID
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
		PhoneNumber:      phone,
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

	day := usageDayUTC(time.Now())
	if sendErr != nil {
		retryKind := model.OutboundRetryKindImage
		if kind == "document" {
			retryKind = model.OutboundRetryKindDocument
		}
		s.maybeEnqueueOutboundRetry(r.Context(), conn, messageLog, retryKind, map[string]any{
			"phone_number": phone,
			"link":         link,
			"media_id":     metaMediaID,
			"caption":      caption,
			"filename":     filename,
			"kind":         kind,
		}, sendErr)
		s.recordUsageBestEffort(r.Context(), conn.SystemID, conn.TenantID, day, outboundErrorDelta(sendErr), "send-media/meta-error")
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error(), Code: failureCode})
		return
	}

	// TemplateName no log guarda o kind (image/document); metering usa msgType, não o nome.
	s.recordUsageBestEffort(r.Context(), conn.SystemID, conn.TenantID, day, outboundSuccessDelta("", kind), "send-media/sent")

	writeJSON(w, http.StatusOK, sendMediaResponse{
		MessageLogID:  messageLog.ID,
		Status:        status,
		MetaMessageID: metaMessageID,
	})
}

func parseSendMediaRequest(r *http.Request, kind string) (
	tenantID, phone, caption, filename, mimeType, link, appointmentID string,
	fileBytes []byte,
	err error,
) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxMediaFormMemory); err != nil {
			return "", "", "", "", "", "", "", nil, fmt.Errorf("invalid multipart body")
		}
		tenantID = strings.TrimSpace(r.FormValue("tenant_id"))
		phone = firstNonEmpty(r.FormValue("phone_number"), r.FormValue("to"))
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
		phone = firstNonEmpty(req.PhoneNumber, req.To)
		caption = strings.TrimSpace(req.Caption)
		filename = strings.TrimSpace(req.Filename)
		mimeType = strings.TrimSpace(req.MimeType)
		link = strings.TrimSpace(req.Link)
		appointmentID = strings.TrimSpace(req.AppointmentID)
	}

	phone = session.NormalizeWAID(phone)
	if appointmentID == "" {
		appointmentID = "-"
	}
	if tenantID == "" || phone == "" {
		return "", "", "", "", "", "", "", nil, fmt.Errorf("tenant_id and phone_number are required")
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
	return tenantID, phone, caption, filename, mimeType, link, appointmentID, fileBytes, nil
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

func (s *server) handleAdminSendMediaTest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxMediaFormMemory); err != nil {
		http.Error(w, "invalid multipart body", http.StatusBadRequest)
		return
	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))
	tenantID := strings.TrimSpace(r.FormValue("tenant_id"))
	phone := session.NormalizeWAID(firstNonEmpty(r.FormValue("phone_number"), r.FormValue("to")))
	kind := strings.ToLower(strings.TrimSpace(r.FormValue("type")))
	caption := strings.TrimSpace(r.FormValue("caption"))
	filename := strings.TrimSpace(r.FormValue("filename"))
	if kind != "document" {
		kind = "image"
	}
	if systemID == "" || tenantID == "" || phone == "" {
		writeAdminMediaResult(w, false, "system_id, tenant_id e phone_number são obrigatórios", "")
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

	req, err := newAdminMediaAPIRequest(r.Context(), *system, kind, tenantID, phone, caption, filename, mimeType, data)
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
	kind, tenantID, phone, caption, filename, mimeType string,
	data []byte,
) (*http.Request, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("tenant_id", tenantID)
	_ = writer.WriteField("phone_number", phone)
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
