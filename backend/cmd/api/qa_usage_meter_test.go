package main

// DEV-167 — metering diário usage_counters + GET /v1/usage + export CSV.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

func qaGetUsage(t *testing.T, srv *server, apiKey, tenantID, from, to string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v1/usage?tenant_id=%s&from=%s&to=%s", tenantID, from, to)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if apiKey != "" {
		req.Header.Set(apiKeyHeader, apiKey)
	}
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleUsageMeter)).ServeHTTP(rec, req)
	return rec
}

func qaGetUsageExport(t *testing.T, srv *server, withAuth bool, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/export.csv?"+query, nil)
	if withAuth {
		token, err := security.GenerateToken("qa-admin", srv.jwtSecret)
		if err != nil {
			t.Fatalf("generate token: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: security.AuthCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	srv.jwtMiddleware(http.HandlerFunc(srv.handleUsageExportCSV)).ServeHTTP(rec, req)
	return rec
}

func qaUsageDayBounds() (from, to string) {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	return day, day
}

func TestQAUsageMeterTemplateSendsAccumulate(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_usage", "")
	createQAConnection(t, sys, "salon-42", "phone-usage", "token-usage", "")

	for i := 0; i < 3; i++ {
		rec := qaPostSendNotification(t, srv, sys.APIKey, map[string]any{
			"tenant_id":     "salon-42",
			"phone_number":  "5511988887777",
			"template_name": "confirma_agendamento",
			"variables":     []string{"Ana", "10:00"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("send %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}

	from, to := qaUsageDayBounds()
	rec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body usageMeterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if body.SystemID != sys.ID || body.TenantID != "salon-42" {
		t.Errorf("scope mismatch: %+v", body)
	}
	if body.Totals.SentTemplate != 3 {
		t.Errorf("sent_template=%d, want 3", body.Totals.SentTemplate)
	}
	if body.Totals.SentText != 0 || body.Totals.SentMedia != 0 {
		t.Errorf("unexpected sent_text/media: %+v", body.Totals)
	}
	if body.Totals.Errors4xx != 0 || body.Totals.Errors5xx != 0 {
		t.Errorf("unexpected errors: %+v", body.Totals)
	}
	if len(body.Days) != 1 {
		t.Fatalf("days=%d, want 1 (empty days omitted)", len(body.Days))
	}
	if body.Days[0].SentTemplate != 3 {
		t.Errorf("day sent_template=%d, want 3", body.Days[0].SentTemplate)
	}
}

func TestQAUsageMeterUnknownTenantReturnsEmpty200(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_empty", "")

	from, to := qaUsageDayBounds()
	rec := qaGetUsage(t, srv, sys.APIKey, "no-such-tenant", from, to)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (no enumeration via 404)", rec.Code)
	}
	var body usageMeterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Totals.SentTemplate != 0 || len(body.Days) != 0 {
		t.Errorf("want empty totals/days, got %+v", body)
	}
}

func TestQAUsageMeterDeliveredIdempotent(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_deliv", "")
	createQAConnection(t, sys, "salon-42", "phone-beleza", "token-beleza", "")

	sendRec := qaPostSendNotification(t, srv, sys.APIKey, map[string]any{
		"tenant_id":     "salon-42",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
	})
	if sendRec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sendRec.Code, sendRec.Body.String())
	}
	logs := qaListMessageLogs(t)
	if len(logs) != 1 || logs[0].MetaMessageID == "" {
		t.Fatalf("expected 1 outbound log with wamid, got %+v", logs)
	}
	wamid := logs[0].MetaMessageID

	deliveredAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	payload := qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "UTILITY", deliveredAt.Unix())

	for i := 0; i < 2; i++ {
		rec := qaPostMetaWebhook(t, srv, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery webhook %d status=%d", i, rec.Code)
		}
	}
	qaWaitFor(t, 5*time.Second, "status_callback_delivered=1", func() bool {
		from, to := qaUsageDayBounds()
		rec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
		if rec.Code != http.StatusOK {
			return false
		}
		var body usageMeterResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return false
		}
		return body.Totals.StatusCallbackDelivered == 1 && body.Totals.SentTemplate == 1
	})

	from, to := qaUsageDayBounds()
	rec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
	var body usageMeterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Totals.StatusCallbackDelivered != 1 {
		t.Errorf("status_callback_delivered=%d, want 1 (idempotent)", body.Totals.StatusCallbackDelivered)
	}
	if body.Totals.SentTemplate != 1 {
		t.Errorf("sent_template=%d, want 1 (status must not re-increment sent)", body.Totals.SentTemplate)
	}
}

func TestQAUsageMeterCrossSystemIsolation(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sysA := createQASystem(t, "System A", "sys_a_usage", "")
	sysB := createQASystem(t, "System B", "sys_b_usage", "")
	createQAConnection(t, sysA, "shared-tenant", "phone-a", "token-a", "")
	createQAConnection(t, sysB, "shared-tenant", "phone-b", "token-b", "")

	if rec := qaPostSendNotification(t, srv, sysA.APIKey, map[string]any{
		"tenant_id": "shared-tenant", "phone_number": "5511988887777", "template_name": "tpl_a",
	}); rec.Code != http.StatusOK {
		t.Fatalf("A send: %d %s", rec.Code, rec.Body.String())
	}
	if rec := qaPostSendNotification(t, srv, sysB.APIKey, map[string]any{
		"tenant_id": "shared-tenant", "phone_number": "5511988887777", "template_name": "tpl_b",
	}); rec.Code != http.StatusOK {
		t.Fatalf("B send: %d %s", rec.Code, rec.Body.String())
	}

	from, to := qaUsageDayBounds()
	recA := qaGetUsage(t, srv, sysA.APIKey, "shared-tenant", from, to)
	recB := qaGetUsage(t, srv, sysB.APIKey, "shared-tenant", from, to)
	var bodyA, bodyB usageMeterResponse
	_ = json.Unmarshal(recA.Body.Bytes(), &bodyA)
	_ = json.Unmarshal(recB.Body.Bytes(), &bodyB)

	if bodyA.SystemID != sysA.ID || bodyA.Totals.SentTemplate != 1 {
		t.Errorf("A must see only own counters: %+v", bodyA)
	}
	if bodyB.SystemID != sysB.ID || bodyB.Totals.SentTemplate != 1 {
		t.Errorf("B must see only own counters: %+v", bodyB)
	}
}

func TestQAUsageExportCSVColumnsAndFilter(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sysA := createQASystem(t, "System A", "sys_a_csv", "")
	sysB := createQASystem(t, "System B", "sys_b_csv", "")
	createQAConnection(t, sysA, "t-a", "phone-a", "token-a", "")
	createQAConnection(t, sysB, "t-b", "phone-b", "token-b", "")

	_ = qaPostSendNotification(t, srv, sysA.APIKey, map[string]any{
		"tenant_id": "t-a", "phone_number": "5511988887777", "template_name": "tpl",
	})
	_ = qaPostSendNotification(t, srv, sysB.APIKey, map[string]any{
		"tenant_id": "t-b", "phone_number": "5511988887777", "template_name": "tpl",
	})

	from, to := qaUsageDayBounds()
	query := fmt.Sprintf("from=%s&to=%s&system_id=%s", from, to, sysA.ID)
	rec := qaGetUsageExport(t, srv, true, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/csv") {
		t.Errorf("Content-Type=%q, want text/csv", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "usage_") || !strings.Contains(cd, ".csv") {
		t.Errorf("Content-Disposition=%q", cd)
	}

	reader := csv.NewReader(strings.NewReader(rec.Body.String()))
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	wantHeader := []string{
		"system_id", "tenant_id", "usage_date",
		"sent_text", "sent_template", "sent_media",
		"inbound", "status_callback_delivered",
		"errors_4xx", "errors_5xx",
	}
	if len(records) < 2 {
		t.Fatalf("csv rows=%d, want header+data", len(records))
	}
	if strings.Join(records[0], ",") != strings.Join(wantHeader, ",") {
		t.Errorf("header=%v, want %v", records[0], wantHeader)
	}
	for i, row := range records[1:] {
		if row[0] != sysA.ID {
			t.Errorf("row %d system_id=%q, want filtered to A (%s)", i, row[0], sysA.ID)
		}
		if row[0] == sysB.ID {
			t.Errorf("row %d leaked system B", i)
		}
	}
}

func TestQAUsageMeterRateLimitIncrementsErrors4xxNotSent(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1)
	sys := createQASystem(t, "Beleza", "beleza_rl", "")
	createQAConnection(t, sys, "salon-42", "phone-rl", "token-rl", "")

	payload := map[string]any{
		"tenant_id": "salon-42", "phone_number": "5511988887777", "template_name": "confirma",
	}
	if rec := qaPostSendNotification(t, srv, sys.APIKey, payload); rec.Code != http.StatusOK {
		t.Fatalf("accepted status=%d", rec.Code)
	}
	if rec := qaPostSendNotification(t, srv, sys.APIKey, payload); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected status=%d, want 429", rec.Code)
	}

	from, to := qaUsageDayBounds()
	rec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
	var body usageMeterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Totals.SentTemplate != 1 {
		t.Errorf("sent_template=%d, want 1", body.Totals.SentTemplate)
	}
	if body.Totals.Errors4xx != 1 {
		t.Errorf("errors_4xx=%d, want 1", body.Totals.Errors4xx)
	}
}

func TestQAUsageMeterMeta5xxIncrementsErrors5xxNotSent(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	stub.respond = func(call qaMetaCall, seq int64) (int, string) {
		return http.StatusInternalServerError, `{"error":{"message":"upstream down","code":2}}`
	}
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_5xx", "")
	createQAConnection(t, sys, "salon-42", "phone-5xx", "token-5xx", "")

	rec := qaPostSendNotification(t, srv, sys.APIKey, map[string]any{
		"tenant_id": "salon-42", "phone_number": "5511988887777", "template_name": "confirma",
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 body=%s", rec.Code, rec.Body.String())
	}

	from, to := qaUsageDayBounds()
	usageRec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
	var body usageMeterResponse
	if err := json.Unmarshal(usageRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Totals.SentTemplate != 0 {
		t.Errorf("sent_template=%d, want 0", body.Totals.SentTemplate)
	}
	if body.Totals.Errors5xx != 1 {
		t.Errorf("errors_5xx=%d, want 1", body.Totals.Errors5xx)
	}
	if body.Totals.Errors4xx != 0 {
		t.Errorf("errors_4xx=%d, want 0", body.Totals.Errors4xx)
	}
}

func TestQAUsageMeterRequiresAPIKeyAndValidQuery(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_auth", "")

	from, to := qaUsageDayBounds()
	if rec := qaGetUsage(t, srv, "", "t", from, to); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing key status=%d, want 401", rec.Code)
	}
	if rec := qaGetUsage(t, srv, sys.APIKey, "", from, to); rec.Code != http.StatusBadRequest {
		t.Errorf("missing tenant status=%d, want 400", rec.Code)
	}
	if rec := qaGetUsage(t, srv, sys.APIKey, "t", "not-a-date", to); rec.Code != http.StatusBadRequest {
		t.Errorf("bad from status=%d, want 400", rec.Code)
	}
}

func TestQAUsageMeterInboundIncrements(t *testing.T) {
	requireQADB(t)

	saas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(saas.Close)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_in", saas.URL)
	createQAConnection(t, sys, "salon-42", "phone-beleza", "token-beleza", saas.URL)

	if rec := qaPostMetaWebhook(t, srv, qaInboundTextPayload("phone-beleza", "5511977776666", "oi")); rec.Code != http.StatusOK {
		t.Fatalf("inbound status=%d", rec.Code)
	}
	qaWaitFor(t, 5*time.Second, "inbound counter", func() bool {
		from, to := qaUsageDayBounds()
		rec := qaGetUsage(t, srv, sys.APIKey, "salon-42", from, to)
		var body usageMeterResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return false
		}
		return body.Totals.Inbound == 1
	})
}

func TestQAUsageExportRequiresAuth(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	from, to := qaUsageDayBounds()
	rec := qaGetUsageExport(t, srv, false, fmt.Sprintf("from=%s&to=%s", from, to))
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth export status=%d, want redirect/401", rec.Code)
	}
}

// Smoke: repository UPSERT + dedup direto (sem HTTP).
func TestQAUsageCounterRepoUpsertAndDedup(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	sys := createQASystem(t, "Beleza", "beleza_repo", "")
	day := time.Now().UTC().Truncate(24 * time.Hour)
	ctx := context.Background()

	delta := repository.UsageDelta{SentTemplate: 2}
	if err := srv.usageMeter.Increment(ctx, sys.ID, "t1", day, delta); err != nil {
		t.Fatalf("increment: %v", err)
	}
	delta.SentTemplate = 3
	if err := srv.usageMeter.Increment(ctx, sys.ID, "t1", day, delta); err != nil {
		t.Fatalf("increment2: %v", err)
	}
	rows, err := srv.usageMeter.QueryRange(ctx, sys.ID, "t1", day, day)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].SentTemplate != 5 {
		t.Fatalf("rows=%+v, want sent_template=5", rows)
	}

	ok1, err := srv.usageMeter.TryRecordStatusDelivered(ctx, sys.ID, "wamid.dup")
	if err != nil || !ok1 {
		t.Fatalf("first dedup inserted=%v err=%v", ok1, err)
	}
	ok2, err := srv.usageMeter.TryRecordStatusDelivered(ctx, sys.ID, "wamid.dup")
	if err != nil || ok2 {
		t.Fatalf("second dedup inserted=%v err=%v (want false)", ok2, err)
	}
}
