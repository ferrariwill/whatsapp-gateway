package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/session"
)

func jpegFixture() []byte {
	return []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
		0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20,
		0x24, 0x2e, 0x27, 0x20, 0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
		0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27, 0x39, 0x3d, 0x38, 0x32,
		0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x14, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x03, 0xff, 0xc4, 0x00, 0x14, 0x10, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00,
		0x7f, 0xff, 0xd9,
	}
}

func postMultipartMedia(
	t *testing.T,
	srv *server,
	apiKey, path, tenantID, phone, caption, filename, mimeType string,
	fileBytes []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("tenant_id", tenantID)
	_ = w.WriteField("phone_number", phone)
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	if filename != "" {
		_ = w.WriteField("filename", filename)
	}
	if mimeType != "" {
		_ = w.WriteField("mime_type", mimeType)
	}
	if len(fileBytes) > 0 {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
		if mimeType != "" {
			h.Set("Content-Type", mimeType)
		}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(fileBytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-API-Key", apiKey)
	rec := httptest.NewRecorder()
	handler := srv.handleSendImage
	if strings.Contains(path, "document") {
		handler = srv.handleSendDocument
	}
	srv.apiKeyMiddleware(http.HandlerFunc(handler)).ServeHTTP(rec, req)
	return rec
}

func TestValidateImageUploadTable(t *testing.T) {
	cases := []struct {
		name string
		mime string
		size int
		code string
	}{
		{name: "jpeg ok", mime: "image/jpeg", size: 100, code: ""},
		{name: "png ok", mime: "image/png", size: 100, code: ""},
		{name: "gif", mime: "image/gif", size: 100, code: "unsupported_type"},
		{name: "too large", mime: "image/jpeg", size: maxImageBytes + 1, code: "media_too_large"},
		{name: "empty", mime: "image/jpeg", size: 0, code: "unsupported_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageUpload(tc.mime, tc.size)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected err=%v", err)
				}
				return
			}
			ve, ok := err.(mediaValidationError)
			if !ok || ve.Code() != tc.code {
				t.Fatalf("err=%v want code %s", err, tc.code)
			}
		})
	}
}

func TestValidateDocumentUploadTable(t *testing.T) {
	if err := validateDocumentUpload("application/pdf", 10); err != nil {
		t.Fatal(err)
	}
	err := validateDocumentUpload("application/zip", 10)
	ve, ok := err.(mediaValidationError)
	if !ok || ve.Code() != "unsupported_type" {
		t.Fatalf("got %v", err)
	}
	err = validateDocumentUpload("application/pdf", maxDocumentBytes+1)
	ve, ok = err.(mediaValidationError)
	if !ok || ve.Code() != "media_too_large" {
		t.Fatalf("got %v", err)
	}
}

func TestQASendImageJPEGOK(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media Img", uniqueSlug(t, "mediaimg"), "")
	createQAConnection(t, sys, "tenant-img", "pn-img", "tok-img", "")

	phone := session.NormalizeWAID("5511999000111")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-img", phone, time.Now().UTC(),
	)

	rec := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-img", phone, "foto", "shot.jpg", "image/jpeg", jpegFixture())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp sendMediaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MetaMessageID == "" || resp.MessageLogID == "" {
		t.Fatalf("response=%+v", resp)
	}
	if stub.CallCount() < 2 {
		t.Fatalf("want upload+send meta calls, got %d", stub.CallCount())
	}

	var n int
	if err := qaDB.QueryRow(
		`SELECT COUNT(*) FROM message_logs WHERE id=$1 AND meta_message_id=$2 AND status='sent'`,
		resp.MessageLogID, resp.MetaMessageID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("message_logs rows=%d", n)
	}
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM media_objects WHERE system_id=$1 AND tenant_id='tenant-img'`, sys.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("media_objects=%d", n)
	}
}

func TestQASendImageOversizedNoMeta(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media Big", uniqueSlug(t, "mediabig"), "")
	createQAConnection(t, sys, "tenant-big", "pn-big", "tok-big", "")

	phone := session.NormalizeWAID("5511999000222")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-big", phone, time.Now().UTC(),
	)

	huge := make([]byte, maxImageBytes+1)
	copy(huge, jpegFixture())
	rec := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-big", phone, "", "big.jpg", "image/jpeg", huge)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "media_too_large") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta must not be called, got %d", stub.CallCount())
	}
}

func TestQASendImageUnsupportedType(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media Bad", uniqueSlug(t, "mediabad"), "")
	createQAConnection(t, sys, "tenant-bad", "pn-bad", "tok-bad", "")

	phone := session.NormalizeWAID("5511999000333")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-bad", phone, time.Now().UTC(),
	)

	rec := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-bad", phone, "", "x.gif", "image/gif", []byte("GIF89a"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported_type") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta calls=%d", stub.CallCount())
	}
}

func TestQASendMediaOutside24h(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media Win", uniqueSlug(t, "mediawin"), "")
	createQAConnection(t, sys, "tenant-win", "pn-mwin", "tok-mwin", "")

	rec := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-win", "5511999000444", "", "a.jpg", "image/jpeg", jpegFixture())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside_24h_window") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta calls=%d", stub.CallCount())
	}
}

func TestQASendMediaRateLimited(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media RL", uniqueSlug(t, "mediarl"), "")
	conn := createQAConnection(t, sys, "tenant-mrl", "pn-mrl", "tok-mrl", "")

	if err := repository.NewPostgresRepository(qaDB).UpdateConnectionRateLimit(
		context.Background(), conn.ID, sys.ID, "tenant-mrl", "", 1, 1, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	srv.invalidateRateLimitCache(sys.ID, "tenant-mrl")

	phone := session.NormalizeWAID("5511999000555")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-mrl", phone, time.Now().UTC(),
	)

	rec1 := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-mrl", phone, "", "a.jpg", "image/jpeg", jpegFixture())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first=%d body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := postMultipartMedia(t, srv, sys.APIKey, "/v1/messages/image",
		"tenant-mrl", phone, "", "b.jpg", "image/jpeg", jpegFixture())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second=%d want 429 body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "rate_limited") {
		t.Fatalf("body=%s", rec2.Body.String())
	}
}

func TestQAHostedMediaCrossTenantNotFound(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sysA := createQASystem(t, "Media A", uniqueSlug(t, "mediaa"), "")
	sysB := createQASystem(t, "Media B", uniqueSlug(t, "mediab"), "")
	createQAConnection(t, sysA, "tenant-a", "pn-ma", "tok-ma", "")
	createQAConnection(t, sysB, "tenant-b", "pn-mb", "tok-mb", "")

	phone := session.NormalizeWAID("5511999000666")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sysA.ID, "tenant-a", phone, time.Now().UTC(),
	)

	rec := postMultipartMedia(t, srv, sysA.APIKey, "/v1/messages/image",
		"tenant-a", phone, "", "a.jpg", "image/jpeg", jpegFixture())
	if rec.Code != http.StatusOK {
		t.Fatalf("send=%d body=%s", rec.Code, rec.Body.String())
	}

	var objectID string
	if err := qaDB.QueryRow(
		`SELECT id::text FROM media_objects WHERE system_id=$1 AND tenant_id='tenant-a' ORDER BY created_at DESC LIMIT 1`,
		sysA.ID,
	).Scan(&objectID); err != nil {
		t.Fatal(err)
	}

	// Wrong system API key → 404
	req := httptest.NewRequest(http.MethodGet, "/v1/media/"+objectID+"?tenant_id=tenant-a", nil)
	req.Header.Set("X-API-Key", sysB.APIKey)
	req.SetPathValue("id", objectID)
	cross := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleGetHostedMedia)).ServeHTTP(cross, req)
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-system status=%d body=%s", cross.Code, cross.Body.String())
	}

	// Same system, wrong tenant → 404
	wrongTenant := httptest.NewRecorder()
	reqWT := httptest.NewRequest(http.MethodGet, "/v1/media/"+objectID+"?tenant_id=tenant-b", nil)
	reqWT.Header.Set("X-API-Key", sysA.APIKey)
	reqWT.SetPathValue("id", objectID)
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleGetHostedMedia)).ServeHTTP(wrongTenant, reqWT)
	if wrongTenant.Code != http.StatusNotFound {
		t.Fatalf("wrong tenant status=%d body=%s", wrongTenant.Code, wrongTenant.Body.String())
	}

	own := httptest.NewRecorder()
	reqOwn := httptest.NewRequest(http.MethodGet, "/v1/media/"+objectID+"?tenant_id=tenant-a", nil)
	reqOwn.Header.Set("X-API-Key", sysA.APIKey)
	reqOwn.SetPathValue("id", objectID)
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleGetHostedMedia)).ServeHTTP(own, reqOwn)
	if own.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", own.Code, own.Body.String())
	}
	body, _ := io.ReadAll(own.Body)
	if len(body) == 0 {
		t.Fatal("empty media body")
	}
}

func TestQASendDocumentLinkHTTPS(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Media Doc", uniqueSlug(t, "mediadoc"), "")
	createQAConnection(t, sys, "tenant-doc", "pn-doc", "tok-doc", "")

	phone := session.NormalizeWAID("5511999000777")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-doc", phone, time.Now().UTC(),
	)

	body := `{"tenant_id":"tenant-doc","phone_number":"` + phone + `","link":"https://example.com/receita.pdf","filename":"receita.pdf"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/document", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sys.APIKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendDocument)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 1 {
		t.Fatalf("link path should only call send (no upload), got %d", stub.CallCount())
	}
}
