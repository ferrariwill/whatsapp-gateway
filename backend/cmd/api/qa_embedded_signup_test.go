package main

// Testes regressivos de QA para o callback OAuth do Embedded Signup da Meta
// (GET /meta/embedded-signup/callback) e o mint de state single-use
// (POST /v1/embedded-signup/state).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

type qaConnectionRow struct {
	ID            string
	SystemID      string
	SistemaOrigem string
	TenantID      string
	WabaID        string
	PhoneNumberID string
	AccessToken   string
	WebhookURL    string
	Status        string
}

func qaFindConnection(t *testing.T, systemID, tenantID string) (qaConnectionRow, bool) {
	t.Helper()

	var row qaConnectionRow
	err := qaDB.QueryRow(`
		SELECT id::text, system_id::text, sistema_origem, tenant_id, waba_id, phone_number_id,
		       access_token, COALESCE(webhook_url, ''), status
		FROM whatsapp_connections
		WHERE system_id = $1 AND tenant_id = $2
	`, systemID, tenantID).Scan(
		&row.ID, &row.SystemID, &row.SistemaOrigem, &row.TenantID, &row.WabaID,
		&row.PhoneNumberID, &row.AccessToken, &row.WebhookURL, &row.Status,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return qaConnectionRow{}, false
		}
		t.Fatalf("find connection %s/%s: %v", systemID, tenantID, err)
	}
	return row, true
}

func qaCountConnections(t *testing.T) int {
	t.Helper()
	var count int
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM whatsapp_connections`).Scan(&count); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	return count
}

func qaEmbeddedSignupMetaStub(accessToken, wabaID, phoneNumberID, displayPhone string) *qaMetaStub {
	stub := newQAMetaStub()
	stub.respond = func(call qaMetaCall, _ int64) (int, string) {
		switch {
		case strings.HasSuffix(call.Path, "/oauth/access_token"):
			return http.StatusOK, fmt.Sprintf(`{"access_token":%q,"token_type":"bearer","expires_in":5184000}`, accessToken)
		case strings.HasSuffix(call.Path, "/debug_token"):
			return http.StatusOK, fmt.Sprintf(
				`{"data":{"granular_scopes":[{"scope":"whatsapp_business_messaging","target_ids":[%q]}]}}`, wabaID)
		case strings.HasSuffix(call.Path, "/phone_numbers"):
			return http.StatusOK, fmt.Sprintf(
				`{"data":[{"id":%q,"display_phone_number":%q,"verified_name":"QA Salon"}]}`, phoneNumberID, displayPhone)
		case strings.HasSuffix(call.Path, "/subscribed_apps"):
			return http.StatusOK, `{"success":true}`
		default:
			return http.StatusOK, `{}`
		}
	}
	return stub
}

func qaGetEmbeddedSignupCallback(t *testing.T, srv *server, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/meta/embedded-signup/callback?"+query, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	srv.handleEmbeddedSignupCallback(rec, req)
	return rec
}

func qaSetEmbeddedSignupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("META_APP_ID", "qa-app-id")
	t.Setenv("META_APP_SECRET", "qa-app-secret")
	t.Setenv("META_EMBEDDED_SIGNUP_REDIRECT_URI", "https://gateway.qa/meta/embedded-signup/callback")
	t.Setenv("EMBEDDED_SIGNUP_ALLOW_UNSIGNED_STATE", "")
	t.Setenv("OAUTH_STATE_SECRET", "")
}

func qaMintSignedState(t *testing.T, srv *server, apiKey, tenantID string) string {
	t.Helper()

	body := fmt.Sprintf(`{"tenant_id":%q}`, tenantID)
	req := httptest.NewRequest(http.MethodPost, "/v1/embedded-signup/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, apiKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleMintEmbeddedSignupState)).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint state status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var payload mintEmbeddedSignupStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if payload.State == "" {
		t.Fatal("mint response missing state")
	}
	return payload.State
}

func qaCallbackQuery(code, state string) string {
	q := url.Values{}
	q.Set("code", code)
	q.Set("state", state)
	return q.Encode()
}

// TestQAEmbeddedSignupStateParsing fixa o contrato do parser legado (flag on).
func TestQAEmbeddedSignupStateParsing(t *testing.T) {
	cases := []struct {
		state      string
		wantSlug   string
		wantTenant string
		wantErr    bool
	}{
		{state: "beleza_123", wantSlug: "beleza", wantTenant: "123"},
		{state: "beleza_web_789", wantSlug: "beleza_web", wantTenant: "789"},
		{state: "BELEZA_WEB_789", wantSlug: "beleza_web", wantTenant: "789"},
		{state: "clinica_salao-42", wantSlug: "clinica", wantTenant: "salao-42"},
		{state: "beleza_web::salao_1", wantSlug: "beleza_web", wantTenant: "salao_1"},
		{state: "beleza_web_salao_1", wantSlug: "beleza_web_salao", wantTenant: "1"},
		{state: "", wantErr: true},
		{state: "semunderscore", wantErr: true},
		{state: "_123", wantErr: true},
		{state: "beleza_", wantErr: true},
		{state: "::salao", wantErr: true},
		{state: "beleza::", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			slug, tenantID, err := parseEmbeddedSignupState(tc.state)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for state %q, got slug=%q tenant=%q", tc.state, slug, tenantID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for state %q: %v", tc.state, err)
			}
			if slug != tc.wantSlug || tenantID != tc.wantTenant {
				t.Errorf("state %q -> slug=%q tenant=%q, want slug=%q tenant=%q",
					tc.state, slug, tenantID, tc.wantSlug, tc.wantTenant)
			}
		})
	}
}

func TestQAEmbeddedSignupRejectsUnsignedStateByDefault(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	createQASystem(t, "Beleza Web", "beleza_web", "")

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao-1", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unsigned state (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("unsigned state must not reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountConnections(t); count != 0 {
		t.Errorf("unsigned state created %d connections", count)
	}
}

func TestQAEmbeddedSignupRejectsInvalidCallback(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	createQASystem(t, "Beleza Web", "beleza_web", "")

	cases := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{name: "meta denied", query: "error=access_denied&error_description=user+cancelled", wantStatus: http.StatusBadRequest},
		{name: "missing code", query: "state=beleza_web_1", wantStatus: http.StatusBadRequest},
		{name: "missing state", query: "code=abc", wantStatus: http.StatusBadRequest},
		{name: "malformed state", query: "code=abc&state=semunderscore", wantStatus: http.StatusBadRequest},
		{name: "unknown slug signed", query: "", wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := tc.query
			if tc.name == "unknown slug signed" {
				secret := oauthStateSecret()
				state, claims, err := security.SignEmbeddedSignupStateClaims(secret, "slug_inexistente", "1", time.Minute)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				_ = claims
				query = qaCallbackQuery("abc", state)
			}
			rec := qaGetEmbeddedSignupCallback(t, srv, query, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}

	if stub.CallCount() != 0 {
		t.Errorf("invalid callbacks must not reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountConnections(t); count != 0 {
		t.Errorf("invalid callbacks created %d connections", count)
	}
}

func TestQAEmbeddedSignupPersistsTenantCredentialsAndNotifiesSaaS(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("tenant-long-lived-token", "waba-salao-1", "phone-salao-1", "5511988880000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	state := qaMintSignedState(t, srv, beleza.APIKey, "salao-1")

	rec := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("oauth-code-1", state), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	conn, ok := qaFindConnection(t, beleza.ID, "salao-1")
	if !ok {
		t.Fatal("connection was not persisted for beleza_web/salao-1")
	}
	if conn.AccessToken != "tenant-long-lived-token" {
		t.Errorf("access_token = %q, want the token exchanged for this tenant", conn.AccessToken)
	}
	if conn.PhoneNumberID != "phone-salao-1" || conn.WabaID != "waba-salao-1" {
		t.Errorf("wrong Meta assets persisted: %+v", conn)
	}
	if conn.SistemaOrigem != beleza.Slug {
		t.Errorf("sistema_origem = %q, want %q", conn.SistemaOrigem, beleza.Slug)
	}
	if conn.WebhookURL != receiver.URL() {
		t.Errorf("webhook_url = %q, want the system webhook %q", conn.WebhookURL, receiver.URL())
	}
	if conn.Status != "ACTIVE" {
		t.Errorf("status = %q, want ACTIVE", conn.Status)
	}

	if got := receiver.Hits(); got != 1 {
		t.Fatalf("SaaS activation callbacks = %d, want 1", got)
	}
}

func TestQAEmbeddedSignupSignedStateReplayRejected(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-1", "waba-1", "phone-1", "5511988880000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	state := qaMintSignedState(t, srv, beleza.APIKey, "salao-1")

	rec1 := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("code-1", state), nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first callback status = %d (body=%s)", rec1.Code, rec1.Body.String())
	}

	metaCallsAfterFirst := stub.CallCount()
	rec2 := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("code-2", state), nil)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if stub.CallCount() != metaCallsAfterFirst {
		t.Errorf("replay must not call Meta again: calls %d -> %d", metaCallsAfterFirst, stub.CallCount())
	}
	if count := qaCountConnections(t); count != 1 {
		t.Errorf("connections = %d, want 1 after replay", count)
	}
}

func TestQAEmbeddedSignupConcurrentCallbackConsumesOnce(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-race", "waba-race", "phone-race", "5511988880000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	receiver := newQASaaSReceiver(t)
	// Slow SaaS notify keeps the first winner in the Meta+upsert window.
	receiver.delayMs.Store(200)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	state := qaMintSignedState(t, srv, beleza.APIKey, "salao-race")

	var (
		wg       sync.WaitGroup
		statuses [2]int
	)
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery(fmt.Sprintf("code-%d", i), state), nil)
			statuses[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	ok, bad := 0, 0
	for _, st := range statuses {
		switch st {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			bad++
		default:
			t.Fatalf("unexpected status %d among %v", st, statuses)
		}
	}
	if ok != 1 || bad != 1 {
		t.Fatalf("concurrent callbacks statuses = %v, want exactly one 200 and one 400", statuses)
	}
	if count := qaCountConnections(t); count != 1 {
		t.Errorf("connections = %d, want 1", count)
	}
}

func TestQAEmbeddedSignupNonceTenantMismatchRejected(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	state := qaMintSignedState(t, srv, beleza.APIKey, "salao-1")

	claims, ok, err := security.ParseEmbeddedSignupState(oauthStateSecret(), state)
	if err != nil || !ok {
		t.Fatalf("parse minted state: ok=%v err=%v", ok, err)
	}
	// Force a tenant mismatch on the persisted row.
	_, err = qaDB.Exec(`UPDATE oauth_state_nonces SET tenant_id = 'other-tenant' WHERE nonce_hash = $1`, claims.NonceHash)
	if err != nil {
		t.Fatalf("mutate nonce tenant: %v", err)
	}

	rec := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("code-x", state), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on tenant mismatch (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("mismatch must not reach Meta, got %d calls", stub.CallCount())
	}
}

func TestQAEmbeddedSignupUnsignedStateTakesOverExistingTenant(t *testing.T) {
	requireQADB(t)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	legit := createQAConnection(t, beleza, "salao-1", "phone-legitimo", "token-legitimo", receiver.URL())

	stub := qaEmbeddedSignupMetaStub("token-do-atacante", "waba-atacante", "phone-atacante", "5511900000000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=codigo-do-atacante&state=beleza_web_salao-1", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsigned takeover status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	conn, ok := qaFindConnection(t, beleza.ID, "salao-1")
	if !ok {
		t.Fatal("connection disappeared")
	}
	if conn.AccessToken != "token-legitimo" || conn.PhoneNumberID != "phone-legitimo" {
		t.Fatalf("credentials overwritten: %+v (was id=%s)", conn, legit.ID)
	}
	if stub.CallCount() != 0 {
		t.Errorf("unsigned takeover must not call Meta, got %d calls", stub.CallCount())
	}
}

func TestQAEmbeddedSignupUnsignedFlagForbidsCreateAndMutation(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)
	t.Setenv("EMBEDDED_SIGNUP_ALLOW_UNSIGNED_STATE", "true")

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	// Create blocked.
	rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao-new", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned create status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := qaFindConnection(t, beleza.ID, "salao-new"); ok {
		t.Fatal("unsigned flag must not create connection")
	}

	// Mutation blocked on existing tenant.
	createQAConnection(t, beleza, "salao-1", "phone-legitimo", "token-legitimo", "")
	rec2 := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao-1", nil)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("unsigned mutation status = %d, want 403 (body=%s)", rec2.Code, rec2.Body.String())
	}
	conn, ok := qaFindConnection(t, beleza.ID, "salao-1")
	if !ok || conn.AccessToken != "token-legitimo" {
		t.Fatalf("unsigned flag mutated credentials: ok=%v conn=%+v", ok, conn)
	}
}

func TestQAEmbeddedSignupTenantIDWithUnderscoreViaSignedState(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	state := qaMintSignedState(t, srv, beleza.APIKey, "salao_1")

	rec := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("abc", state), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	conn, ok := qaFindConnection(t, beleza.ID, "salao_1")
	if !ok {
		t.Fatal("connection was not persisted for tenant salao_1")
	}
	if conn.PhoneNumberID != "phone-x" {
		t.Errorf("phone_number_id = %q, want phone-x", conn.PhoneNumberID)
	}
}

func TestQAEmbeddedSignupMetaFailureDoesNotPersistConnection(t *testing.T) {
	cases := []struct {
		name       string
		failOn     string
		wantStatus int
	}{
		{name: "token exchange 500", failOn: "/oauth/access_token", wantStatus: http.StatusBadGateway},
		{name: "debug_token 429", failOn: "/debug_token", wantStatus: http.StatusBadGateway},
		{name: "phone_numbers 500", failOn: "/phone_numbers", wantStatus: http.StatusBadGateway},
		{name: "subscribed_apps 500", failOn: "/subscribed_apps", wantStatus: http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireQADB(t)

			stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
			base := stub.respond
			stub.respond = func(call qaMetaCall, seq int64) (int, string) {
				if strings.HasSuffix(call.Path, tc.failOn) {
					return http.StatusInternalServerError, `{"error":{"message":"meta down","code":1}}`
				}
				return base(call, seq)
			}

			srv := newQAServer(t, stub, 100)
			qaSetEmbeddedSignupEnv(t)

			beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
			state := qaMintSignedState(t, srv, beleza.APIKey, "salao-1")

			rec := qaGetEmbeddedSignupCallback(t, srv, qaCallbackQuery("abc", state), nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if _, ok := qaFindConnection(t, beleza.ID, "salao-1"); ok {
				t.Error("a partial connection was persisted despite the Meta failure")
			}
			if strings.Contains(rec.Body.String(), "qa-app-secret") {
				t.Errorf("META_APP_SECRET leaked into the callback response: %s", rec.Body.String())
			}
		})
	}
}

func TestQAEmbeddedSignupRequiresMetaConfiguration(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	t.Setenv("META_APP_ID", "")
	t.Setenv("META_APP_SECRET", "")
	t.Setenv("META_EMBEDDED_SIGNUP_REDIRECT_URI", "")

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao-1", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when Meta credentials are missing (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("must not call Meta without app credentials, got %d calls", stub.CallCount())
	}
	if _, ok := qaFindConnection(t, beleza.ID, "salao-1"); ok {
		t.Error("connection persisted without Meta configuration")
	}
}

func TestQAMintEmbeddedSignupStateRequiresAPIKeyAndRegistersNonce(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embedded-signup/state", strings.NewReader(`{"tenant_id":"t1"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleMintEmbeddedSignupState)).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing api key status = %d, want 401", rec.Code)
	}

	state := qaMintSignedState(t, srv, beleza.APIKey, "t1")
	claims, ok, err := security.ParseEmbeddedSignupState(oauthStateSecret(), state)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}

	err = repository.NewPostgresRepository(qaDB).ConsumeOAuthStateNonce(context.Background(), claims.NonceHash, beleza.ID, "t1")
	if err != nil {
		t.Fatalf("registered nonce should be consumable: %v", err)
	}
	err = repository.NewPostgresRepository(qaDB).ConsumeOAuthStateNonce(context.Background(), claims.NonceHash, beleza.ID, "t1")
	if !errors.Is(err, repository.ErrOAuthStateNotFound) {
		t.Fatalf("second consume = %v, want ErrOAuthStateNotFound", err)
	}
}
