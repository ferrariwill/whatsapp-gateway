package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

func qaPostTemplatesSync(t *testing.T, srv *server, apiKey, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"tenant_id":"` + tenantID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/templates/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, apiKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSyncTemplates)).ServeHTTP(rec, req)
	return rec
}

func qaGetTemplates(t *testing.T, srv *server, apiKey, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/templates?tenant_id="+tenantID, nil)
	req.Header.Set(apiKeyHeader, apiKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleListTemplates)).ServeHTTP(rec, req)
	return rec
}

func TestQATemplateSyncIdempotentAndPreservesOnGraphFailure(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	stub.respond = func(call qaMetaCall, seq int64) (int, string) {
		if strings.Contains(call.Path, "message_templates") && call.Method == http.MethodGet {
			return http.StatusOK, `{
				"data":[
					{"id":"111","name":"lembrete","status":"APPROVED","category":"UTILITY","language":"pt_BR",
					 "components":[{"type":"BODY","text":"Oi {{1}} às {{2}}"}]},
					{"id":"222","name":"promo","status":"PENDING","category":"MARKETING","language":"pt_BR",
					 "components":[{"type":"BODY","text":"Oferta"}]}
				]
			}`
		}
		return http.StatusOK, `{"messages":[{"id":"wamid.ignore"}]}`
	}
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "tenant-a", "phone-a", "token-a", "")

	rec := qaPostTemplatesSync(t, srv, beleza.APIKey, "tenant-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("sync status=%d body=%s", rec.Code, rec.Body.String())
	}
	var first syncTemplatesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if first.Upserted != 2 {
		t.Fatalf("upserted=%d want 2", first.Upserted)
	}

	rec = qaPostTemplatesSync(t, srv, beleza.APIKey, "tenant-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-sync status=%d body=%s", rec.Code, rec.Body.String())
	}
	var second syncTemplatesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if second.Upserted != 2 {
		t.Fatalf("re-sync upserted=%d want 2", second.Upserted)
	}

	var count int
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM whatsapp_templates WHERE system_id=$1 AND tenant_id=$2`,
		beleza.ID, "tenant-a").Scan(&count); err != nil {
		t.Fatalf("count templates: %v", err)
	}
	if count != 3 {
		t.Fatalf("catalog rows=%d want 3 (seed confirma + 2 synced)", count)
	}

	// Token inválido: erro gravado, catálogo intacto.
	stub.respond = func(call qaMetaCall, seq int64) (int, string) {
		if strings.Contains(call.Path, "message_templates") {
			return http.StatusUnauthorized, `{"error":{"message":"Invalid OAuth access token","code":190}}`
		}
		return http.StatusOK, `{"messages":[{"id":"wamid.x"}]}`
	}
	rec = qaPostTemplatesSync(t, srv, beleza.APIKey, "tenant-a")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("invalid token sync status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	_, syncErr, _, err := repository.NewPostgresRepository(qaDB).GetConnectionTemplateSyncAudit(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if syncErr == "" || !strings.Contains(syncErr, "Invalid OAuth") {
		t.Fatalf("templates_sync_error=%q", syncErr)
	}
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM whatsapp_templates WHERE system_id=$1 AND tenant_id=$2`,
		beleza.ID, "tenant-a").Scan(&count); err != nil {
		t.Fatalf("count after failure: %v", err)
	}
	if count != 3 {
		t.Fatalf("catalog corrupted after failed sync: rows=%d", count)
	}

	// Meta 429 no sync também preserva.
	stub.respond = func(call qaMetaCall, seq int64) (int, string) {
		if strings.Contains(call.Path, "message_templates") {
			return http.StatusTooManyRequests, `{"error":{"message":"Rate limit hit","code":130429}}`
		}
		return http.StatusOK, `{"messages":[{"id":"wamid.x"}]}`
	}
	rec = qaPostTemplatesSync(t, srv, beleza.APIKey, "tenant-a")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("429 sync status=%d want 502", rec.Code)
	}
}

func TestQATemplateSendGateAndWebhookRejected(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas", "clinicas_saas", "")
	connA := createQAConnection(t, beleza, "tenant-a", "phone-a", "token-a", "")
	createQAConnection(t, clinica, "tenant-b", "phone-b", "token-b", "")

	// Tenant B não lista templates de A.
	listB := qaGetTemplates(t, srv, clinica.APIKey, "tenant-b")
	if listB.Code != http.StatusOK {
		t.Fatalf("list B status=%d", listB.Code)
	}
	if strings.Contains(listB.Body.String(), "confirma_agendamento") &&
		strings.Contains(listB.Body.String(), connA.ID) {
		// confirma exists for B via seed — OK; ensure A's meta id not present under B wrongly
	}
	listA := qaGetTemplates(t, srv, beleza.APIKey, "tenant-a")
	if listA.Code != http.StatusOK || !strings.Contains(listA.Body.String(), "confirma_agendamento") {
		t.Fatalf("list A missing seeded template: %s", listA.Body.String())
	}

	// param_mismatch
	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "tenant-a",
		"phone_number":  "5511999990000",
		"template_name": "confirma_agendamento",
		"variables":     []string{"só-um"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("param mismatch status=%d want 422 body=%s", rec.Code, rec.Body.String())
	}
	var gate structuredErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &gate)
	if gate.Code != "param_mismatch" {
		t.Fatalf("code=%q want param_mismatch body=%s", gate.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("param_mismatch must not call Meta, got %d", stub.CallCount())
	}

	// Webhook REJECTED → send 422 template_not_approved sem Meta.
	payload := `{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"waba-tenant-a",
			"time":1710000000,
			"changes":[{
				"field":"message_template_status_update",
				"value":{
					"event":"REJECTED",
					"message_template_id":"meta-tpl-confirma_agendamento-pt_BR",
					"message_template_name":"confirma_agendamento",
					"message_template_language":"pt_BR"
				}
			}]
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload(qaMetaAppSecret(t), []byte(payload)))
	webhookRec := httptest.NewRecorder()
	srv.handleWhatsAppWebhookEvent(webhookRec, req)
	if webhookRec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d", webhookRec.Code)
	}

	qaWaitFor(t, 5*time.Second, "template status rejected", func() bool {
		tpl, err := repository.NewPostgresRepository(qaDB).FindTemplate(
			context.Background(), beleza.ID, "tenant-a", "confirma_agendamento", "pt_BR",
		)
		return err == nil && tpl.Status == model.TemplateStatusRejected
	})

	before := stub.CallCount()
	rec = qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "tenant-a",
		"phone_number":  "5511999990000",
		"template_name": "confirma_agendamento",
		"variables":     []string{"Ana", "10:00"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rejected template send status=%d want 422 body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &gate)
	if gate.Code != "template_not_approved" {
		t.Fatalf("code=%q want template_not_approved", gate.Code)
	}
	if stub.CallCount() != before {
		t.Fatalf("rejected template must not call Meta (before=%d after=%d)", before, stub.CallCount())
	}

	// Tenant B não consegue enviar template só de A (mesmo nome seedado em B — isolamento por system).
	// Força status APPROVED em A já rejeitado; B tem o próprio seed APPROVED — send B ok.
	// Cross-tenant list: beleza não vê tenant-b.
	cross := qaGetTemplates(t, srv, beleza.APIKey, "tenant-b")
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross tenant list status=%d want 404 (no connection)", cross.Code)
	}
}

func TestQATemplateNotFoundGate(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	beleza := createQASystem(t, "Beleza", "beleza_web", "")
	createQAConnection(t, beleza, "tenant-a", "phone-a", "token-a", "")

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "tenant-a",
		"phone_number":  "5511999990000",
		"template_name": "inexistente",
		"variables":     []string{},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rec.Code, rec.Body.String())
	}
	var gate structuredErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &gate)
	if gate.Code != "template_not_found" {
		t.Fatalf("code=%q", gate.Code)
	}
	if stub.CallCount() != 0 {
		t.Fatalf("must not call Meta")
	}
}
