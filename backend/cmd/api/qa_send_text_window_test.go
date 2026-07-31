package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
	"github.com/whatsappgetway/gateway/internal/session"
)

func TestQASendTextOutside24hWindow(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Win App", uniqueSlug(t, "win"), "")
	createQAConnection(t, sys, "tenant-a", "pn-win-a", "tok-a", "")

	body := `{"tenant_id":"tenant-a","phone_number":"5511999990001","text":"ola"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-text", strings.NewReader(body))
	req.Header.Set("X-API-Key", sys.APIKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendText)).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside_24h_window") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta must not be called outside window, got %d", stub.CallCount())
	}
}

func TestQASendTextAfterInboundOpensWindow(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Win2", uniqueSlug(t, "win2"), "")
	createQAConnection(t, sys, "tenant-b", "pn-win-b", "tok-b", "")

	phone := "5511999990002"
	if err := repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-b", phone, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}

	body := `{"tenant_id":"tenant-b","phone_number":"` + phone + `","text":"ola"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-text", strings.NewReader(body))
	req.Header.Set("X-API-Key", sys.APIKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendText)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 1 {
		t.Fatalf("meta calls=%d", stub.CallCount())
	}
}

func TestQATokenBucketIsolatesTenants(t *testing.T) {
	l := security.NewTokenBucketLimiter()
	cfg := security.BucketConfig{RPS: 1, Burst: 1}
	if ok, _ := l.Allow(security.TenantBucketKey("s", "a"), cfg); !ok {
		t.Fatal("a1")
	}
	if ok, _ := l.Allow(security.TenantBucketKey("s", "a"), cfg); ok {
		t.Fatal("a2 should deny")
	}
	if ok, _ := l.Allow(security.TenantBucketKey("s", "b"), cfg); !ok {
		t.Fatal("b must allow")
	}
}

func TestQAAdminRateLimitLiveUpdate(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "RL", uniqueSlug(t, "rl"), "")
	conn := createQAConnection(t, sys, "tenant-rl", "pn-rl", "tok-rl", "")

	if err := repository.NewPostgresRepository(qaDB).UpdateConnectionRateLimit(
		context.Background(), conn.ID, sys.ID, "tenant-rl", "", 1, 1, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	srv.invalidateRateLimitCache(sys.ID, "tenant-rl")

	phone := session.NormalizeWAID("5511888777666")
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(context.Background(), sys.ID, "tenant-rl", phone, time.Now().UTC())

	send := func() int {
		body := `{"tenant_id":"tenant-rl","phone_number":"` + phone + `","text":"x"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-text", strings.NewReader(body))
		req.Header.Set("X-API-Key", sys.APIKey)
		rec := httptest.NewRecorder()
		srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendText)).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("first send=%d", code)
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("second send=%d want 429", code)
	}
}

func TestQAMeta429EnqueuesOutboundRetry(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	stub.respond = func(call qaMetaCall, seq int64) (int, string) {
		return http.StatusTooManyRequests, `{"error":{"message":"Rate limit hit","code":130429}}`
	}
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "M429", uniqueSlug(t, "m429"), "")
	createQAConnection(t, sys, "tenant-m", "pn-m", "tok-m", "")
	phone := "5511777666555"
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(context.Background(), sys.ID, "tenant-m", phone, time.Now().UTC())

	body := `{"tenant_id":"tenant-m","phone_number":"` + phone + `","text":"retry-me"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-text", strings.NewReader(body))
	req.Header.Set("X-API-Key", sys.APIKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendText)).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var n int
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM outbound_retry_queue`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected outbound_retry_queue row")
	}
	if stub.CallCount() != 1 {
		t.Fatalf("meta calls=%d want 1 (no busy-loop)", stub.CallCount())
	}
}

func uniqueSlug(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "_" + strings.ReplaceAll(t.Name(), "/", "_")
}
