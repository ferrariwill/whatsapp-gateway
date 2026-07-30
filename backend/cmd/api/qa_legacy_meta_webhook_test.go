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
	createQAConnection(t, beleza, "salao-1", "phone-legacy", "token-legacy", receiver.URL())

	payload := qaInboundButtonPayloadWithID("phone-legacy", "5511900000000", "APPT_CONFIRM", "wamid.QA.LEGACY.1")
	secret := "qa-app-secret"

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
}
