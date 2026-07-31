package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/security"
)

func qaPostSendTemplate(t *testing.T, srv *server, apiKey string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal send-template: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-template", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, apiKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendTemplate)).ServeHTTP(rec, req)
	return rec
}

func TestQASendTemplatePersistsRateLimitedAttemptPerTenant(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1)

	systemA := createQASystem(t, "System A", "system_a", "")
	systemB := createQASystem(t, "System B", "system_b", "")
	connA := createQAConnection(t, systemA, "shared-tenant", "phone-a", "token-a", "")
	connB := createQAConnection(t, systemB, "shared-tenant", "phone-b", "token-b", "")
	createQAClientChannel(t, systemA, "shared-tenant", "phone-a")
	createQAClientChannel(t, systemB, "shared-tenant", "phone-b")

	payload := map[string]any{
		"external_client_id": "shared-tenant",
		"phone_number":       "5511988887777",
		"appointment_id":     "appt-1",
		"template_name":      "confirma_agendamento",
		"variables":          []string{"Ana", "10:00"},
	}

	if rec := qaPostSendTemplate(t, srv, systemA.APIKey, payload); rec.Code != http.StatusOK {
		t.Fatalf("system A accepted status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := qaPostSendTemplate(t, srv, systemB.APIKey, payload); rec.Code != http.StatusOK {
		t.Fatalf("system B accepted status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := qaPostSendTemplate(t, srv, systemA.APIKey, payload); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("system A rejected status=%d, want 429 body=%s", rec.Code, rec.Body.String())
	}

	logs := qaListMessageLogs(t)
	if len(logs) != 3 {
		t.Fatalf("message_logs=%d, want 3 (2 sent + 1 rejected)", len(logs))
	}
	rejected := 0
	for _, row := range logs {
		if row.Status != string(model.MessageStatusRejected) {
			continue
		}
		rejected++
		if row.SystemID != systemA.ID || row.ConnectionID != connA.ID || row.TenantID != "shared-tenant" {
			t.Errorf("rejected row leaked tenant scope: %+v", row)
		}
		if row.FailureReason != failureReasonRateLimited {
			t.Errorf("failure_reason=%q, want %q", row.FailureReason, failureReasonRateLimited)
		}
	}
	if rejected != 1 {
		t.Fatalf("rejected rows=%d, want 1", rejected)
	}

	month, year := int(time.Now().UTC().Month()), time.Now().UTC().Year()
	countA, err := srv.repo.CountMonthlyMessagesForClient(context.Background(), systemA.ID, "shared-tenant", month, year)
	if err != nil {
		t.Fatalf("count system A usage: %v", err)
	}
	countB, err := srv.repo.CountMonthlyMessagesForClient(context.Background(), systemB.ID, "shared-tenant", month, year)
	if err != nil {
		t.Fatalf("count system B usage: %v", err)
	}
	if countA != 1 || countB != 1 {
		t.Errorf("accepted usage A/B=%d/%d, want 1/1", countA, countB)
	}

	highVolume, err := srv.repo.FindHighVolumeClientKeys(context.Background(), 10, 1)
	if err != nil {
		t.Fatalf("high-volume keys: %v", err)
	}
	if _, exists := highVolume[systemA.ID+"|shared-tenant"]; exists {
		t.Error("rejected attempt incorrectly counted as accepted high-volume send")
	}
	if connB.ID == connA.ID {
		t.Fatal("test fixture connections unexpectedly identical")
	}
}

func TestQAAdminAuditAPIIncludesRateLimitedReason(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1)
	system := createQASystem(t, "System A", "system_a", "")
	createQAConnection(t, system, "tenant-a", "phone-a", "token-a", "")

	payload := map[string]any{
		"tenant_id":     "tenant-a",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
		"variables":     []string{"Ana", "10:00"},
	}
	if rec := qaPostSendNotification(t, srv, system.APIKey, payload); rec.Code != http.StatusOK {
		t.Fatalf("accepted status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := qaPostSendNotification(t, srv, system.APIKey, payload); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected status=%d body=%s", rec.Code, rec.Body.String())
	}

	token, err := security.GenerateToken("qa-admin", srv.jwtSecret)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/audit/logs?limit=10", nil)
	req.AddCookie(&http.Cookie{Name: security.AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminAuditLogs)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit API status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"status":"rejected"`) ||
		!strings.Contains(body, `"failure_reason":"rate_limited"`) {
		t.Fatalf("audit API missing rejected reason: %s", body)
	}
}

func TestRelayPoolOperationalMetricsAndAuth(t *testing.T) {
	pool := newRelayPool(relayPoolConfig{
		Workers:       1,
		QueueSize:     1,
		SubmitTimeout: 20 * time.Millisecond,
		TaskTimeout:   5 * time.Second,
	})

	started := make(chan struct{})
	release := make(chan struct{})
	if err := pool.Submit(func(context.Context) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("submit running task: %v", err)
	}
	<-started
	if err := pool.Submit(func(context.Context) {}); err != nil {
		t.Fatalf("submit queued task: %v", err)
	}
	if err := pool.Submit(func(context.Context) {}); !errors.Is(err, ErrRelayPoolSaturated) {
		t.Fatalf("saturated submit=%v, want ErrRelayPoolSaturated", err)
	}

	stats := pool.Stats()
	if stats.QueueDepth != 1 || stats.QueueCapacity != 1 || stats.Workers != 1 || stats.WorkersInFlight != 1 {
		t.Fatalf("unexpected live pool stats: %+v", stats)
	}
	if stats.RejectedSaturated != 1 || stats.RejectedClosed != 0 {
		t.Fatalf("unexpected rejection stats: %+v", stats)
	}

	srv := &server{
		jwtSecret: []byte("qa-jwt-secret-with-at-least-32-chars!!"),
		relay:     pool,
	}
	unauthorized := httptest.NewRecorder()
	srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminRelayMetrics)).ServeHTTP(
		unauthorized,
		httptest.NewRequest(http.MethodGet, "/admin/metrics/relay", nil),
	)
	if unauthorized.Code == http.StatusOK {
		t.Fatal("relay metrics endpoint must require admin authentication")
	}

	token, err := security.GenerateToken("qa-admin", srv.jwtSecret)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	callMetrics := func(query string) string {
		req := httptest.NewRequest(http.MethodGet, "/admin/metrics/relay?"+query, nil)
		req.AddCookie(&http.Cookie{Name: security.AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminRelayMetrics)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	bodyA := callMetrics("system_id=system-a&tenant_id=tenant-a")
	bodyB := callMetrics("system_id=system-b&tenant_id=tenant-b")
	if bodyA != bodyB {
		t.Errorf("process-global metrics changed with tenant query: A=%s B=%s", bodyA, bodyB)
	}
	for _, identity := range []string{"system-a", "tenant-a", "system-b", "tenant-b"} {
		if strings.Contains(bodyA, identity) || strings.Contains(bodyB, identity) {
			t.Errorf("metrics leaked identity %q", identity)
		}
	}
	if !strings.Contains(bodyA, `"scope":"process"`) ||
		!strings.Contains(bodyA, `"queue_capacity":1`) ||
		!strings.Contains(bodyA, `"workers_in_flight":1`) ||
		!strings.Contains(bodyA, `"rejected_saturated":1`) {
		t.Fatalf("metrics payload missing required fields: %s", bodyA)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := pool.Submit(func(context.Context) {}); !errors.Is(err, ErrRelayPoolClosed) {
		t.Fatalf("closed submit=%v, want ErrRelayPoolClosed", err)
	}
	final := pool.Stats()
	if final.WorkersInFlight != 0 || final.QueueDepth != 0 ||
		final.RejectedSaturated != 1 || final.RejectedClosed != 1 {
		t.Fatalf("unexpected final pool stats: %+v", final)
	}
}
