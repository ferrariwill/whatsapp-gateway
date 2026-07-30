package main

// Testes do endpoint legado POST /webhooks/meta/{phone_number_id}:
// deve exigir X-Hub-Signature-256 (mesmo contrato do webhook unificado).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

func createQAClientChannel(
	t *testing.T,
	system qaSystem,
	externalClientID, phoneNumberID string,
) *model.ClientChannel {
	t.Helper()
	channel := &model.ClientChannel{
		SystemID:            system.ID,
		SalonName:           externalClientID,
		ExternalClientID:    externalClientID,
		PhoneNumberID:       phoneNumberID,
		WhatsAppPhoneNumber: "5511999990000",
	}
	if err := repository.NewPostgresRepository(qaDB).CreateClientChannel(context.Background(), channel); err != nil {
		t.Fatalf("create client channel: %v", err)
	}
	return channel
}

func TestQALegacyMetaWebhookRejectsInvalidSignature(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Legacy", "beleza_legacy", receiver.URL())
	createQAClientChannel(t, beleza, "salao-1", "phone-legacy")

	// A rejeição precisa vir de MAC divergente, não de fail-closed por segredo
	// vazio: sem esta asserção o teste passaria pelo motivo errado.
	if qaMetaAppSecret(t) == "" {
		t.Fatal("META_APP_SECRET must be set for this test to prove signature mismatch, not fail-closed")
	}

	payload := qaInboundButtonPayload("phone-legacy", "5511900000000", "APPT_CONFIRM")

	req := httptest.NewRequest(
		http.MethodPost,
		"/webhooks/meta/phone-legacy",
		strings.NewReader(payload),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	req.SetPathValue("phone_number_id", "phone-legacy")

	rec := httptest.NewRecorder()
	srv.handleMetaWebhookEvent(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for invalid X-Hub-Signature-256 (body=%s)", rec.Code, rec.Body.String())
	}

	time.Sleep(300 * time.Millisecond)
	if hits := receiver.Hits(); hits != 0 {
		t.Fatalf("forged legacy webhook must not reach SaaS, got %d deliveries", hits)
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Fatalf("forged legacy webhook wrote %d message_logs rows", count)
	}
}

func TestQALegacyMetaWebhookAcceptsValidSignatureAndRelays(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Legacy", "beleza_legacy", receiver.URL())
	createQAClientChannel(t, beleza, "salao-1", "phone-legacy")
	conn := createQAConnection(t, beleza, "salao-1", "phone-legacy", "token-legacy", receiver.URL())

	payload := qaInboundButtonPayloadWithID("phone-legacy", "5511900000000", "APPT_CONFIRM", "wamid.QA.LEGACY.1")
	secret := qaMetaAppSecret(t)

	req := httptest.NewRequest(
		http.MethodPost,
		"/webhooks/meta/phone-legacy",
		strings.NewReader(payload),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload(secret, []byte(payload)))
	req.SetPathValue("phone_number_id", "phone-legacy")

	rec := httptest.NewRecorder()
	srv.handleMetaWebhookEvent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	qaWaitFor(t, 5*time.Second, "legacy SaaS delivery", func() bool {
		return receiver.Hits() >= 1
	})
	qaWaitFor(t, 5*time.Second, "inbound marked sent", func() bool {
		logs := qaListMessageLogs(t)
		return len(logs) == 1 && logs[0].Status == string(model.MessageStatusSent)
	})

	row := qaListMessageLogs(t)[0]
	if row.MetaMessageID != "wamid.QA.LEGACY.1" {
		t.Errorf("meta_message_id = %q, want wamid.QA.LEGACY.1", row.MetaMessageID)
	}
	if row.Status != string(model.MessageStatusSent) {
		t.Errorf("status = %q, want sent", row.Status)
	}
	if row.TenantID != "salao-1" {
		t.Errorf("external_client_id = %q, want salao-1", row.TenantID)
	}
	// connection_id é o escopo de MarkMessageLogDelivered: sem ele a linha sai
	// do billing por conexão.
	if row.ConnectionID != conn.ID {
		t.Errorf("connection_id = %q, want %q", row.ConnectionID, conn.ID)
	}
	// sistema_origem alimenta idx_message_logs_sistema_origem e os filtros do
	// painel; o path legado não preenchia.
	if row.SistemaOrigem != beleza.Slug {
		t.Errorf("sistema_origem = %q, want %q", row.SistemaOrigem, beleza.Slug)
	}
}

func qaPostLegacyMetaWebhook(t *testing.T, srv *server, phoneNumberID, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/webhooks/meta/"+phoneNumberID,
		strings.NewReader(payload),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload(qaMetaAppSecret(t), []byte(payload)))
	req.SetPathValue("phone_number_id", phoneNumberID)
	rec := httptest.NewRecorder()
	srv.handleMetaWebhookEvent(rec, req)
	return rec
}

func TestQALegacyMetaWebhookForwardsTypedMediaEvents(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	receiver := newQASaaSReceiver(t)
	system := createQASystem(t, "Legacy Media", "legacy_media", receiver.URL())
	createQAClientChannel(t, system, "tenant-media", "phone-legacy-media")
	createQAConnection(t, system, "tenant-media", "phone-legacy-media", "token", receiver.URL())

	messages := []string{
		`{"id":"wamid.LEGACY.IMG","from":"5511900000000","type":"image","image":{"id":"img-legacy","mime_type":"image/jpeg","caption":"foto"}}`,
		`{"id":"wamid.LEGACY.AUDIO","from":"5511900000000","type":"audio","audio":{"id":"audio-legacy","mime_type":"audio/ogg","voice":true}}`,
		`{"id":"wamid.LEGACY.DOC","from":"5511900000000","type":"document","document":{"id":"doc-legacy","filename":"arquivo.pdf"}}`,
		`{"id":"wamid.LEGACY.LOC","from":"5511900000000","type":"location","location":{"latitude":-23.5,"longitude":-46.6,"name":"SP"}}`,
		`{"id":"wamid.LEGACY.REACT","from":"5511900000000","type":"reaction","reaction":{"message_id":"wamid.ORIGINAL","emoji":"👍"}}`,
	}
	for _, message := range messages {
		payload := `{"entry":[{"changes":[{"field":"messages","value":{"messages":[` + message + `]}}]}]}`
		if rec := qaPostLegacyMetaWebhook(t, srv, "phone-legacy-media", payload); rec.Code != http.StatusOK {
			t.Fatalf("legacy media status = %d (body=%s)", rec.Code, rec.Body.String())
		}
	}

	qaWaitFor(t, 5*time.Second, "legacy typed media deliveries", func() bool {
		return receiver.Hits() == len(messages)
	})
	got := receiver.Received()
	if len(got) != len(messages) {
		t.Fatalf("typed deliveries = %d, want %d", len(got), len(messages))
	}
	byID := make(map[string]saasWebhookPayload, len(got))
	for _, payload := range got {
		byID[payload.MetaMessageID] = payload
	}
	if payload := byID["wamid.LEGACY.IMG"]; payload.Media == nil || payload.Media.ID != "img-legacy" {
		t.Fatalf("legacy image payload = %+v", payload)
	}
	if payload := byID["wamid.LEGACY.AUDIO"]; payload.Media == nil || payload.Media.ID != "audio-legacy" || !payload.Media.Voice {
		t.Fatalf("legacy audio payload = %+v", payload)
	}
	if payload := byID["wamid.LEGACY.DOC"]; payload.Media == nil || payload.Media.Filename != "arquivo.pdf" {
		t.Fatalf("legacy document payload = %+v", payload)
	}
	if payload := byID["wamid.LEGACY.LOC"]; payload.Location == nil || payload.Location.Name != "SP" {
		t.Fatalf("legacy location payload = %+v", payload)
	}
	if payload := byID["wamid.LEGACY.REACT"]; payload.Reaction == nil || payload.Reaction.MessageID != "wamid.ORIGINAL" {
		t.Fatalf("legacy reaction payload = %+v", payload)
	}
}

func TestQALegacyMetaWebhookIgnoresSharedFallback(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	fallback := newQASaaSReceiver(t)
	t.Setenv("MOTHER_SYSTEM_WEBHOOK_URL", fallback.URL())
	system := createQASystem(t, "Legacy Isolated", "legacy_isolated", "")
	createQAClientChannel(t, system, "tenant-no-webhook", "phone-legacy-isolated")

	payload := qaInboundTextPayloadWithID(
		"phone-legacy-isolated", "5511900000000", "oi", "wamid.LEGACY.NO.FALLBACK",
	)
	if rec := qaPostLegacyMetaWebhook(t, srv, "phone-legacy-isolated", payload); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	qaWaitFor(t, 5*time.Second, "legacy missing webhook audit", func() bool {
		logs := qaListMessageLogs(t)
		return len(logs) == 1 && logs[0].Status == string(model.MessageStatusFailed)
	})
	if fallback.Hits() != 0 {
		t.Fatalf("shared fallback received %d cross-tenant deliveries", fallback.Hits())
	}
}
