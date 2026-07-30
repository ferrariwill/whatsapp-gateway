package main

// Testes regressivos de QA para o callback OAuth do Embedded Signup da Meta
// (GET /meta/embedded-signup/callback) — o ponto de entrada que cria/atualiza
// credenciais de tenant no banco.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// qaEmbeddedSignupMetaStub responde ao fluxo OAuth completo do Embedded Signup.
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
}

// TestQAEmbeddedSignupStateParsing fixa o contrato do state {slug}_{tenant_id}.
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
		// Ambiguidade: o tenant_id fica sempre após o ÚLTIMO underscore, então um
		// tenant_id com "_" é interpretado como parte do slug.
		{state: "beleza_web_salao_1", wantSlug: "beleza_web_salao", wantTenant: "1"},
		{state: "", wantErr: true},
		{state: "semunderscore", wantErr: true},
		{state: "_123", wantErr: true},
		{state: "beleza_", wantErr: true},
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
		{name: "unknown slug", query: "code=abc&state=slug_inexistente_1", wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := qaGetEmbeddedSignupCallback(t, srv, tc.query, nil)
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

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=oauth-code-1&state=beleza_web_salao-1", nil)
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

// TestQAEmbeddedSignupUnsignedStateTakesOverExistingTenant demonstra que o
// state do OAuth não é assinado nem validado contra nonce: quem controla o
// state escolhe em qual system/tenant o UPSERT vai gravar, sobrescrevendo as
// credenciais Meta de um tenant já conectado.
func TestQAEmbeddedSignupUnsignedStateTakesOverExistingTenant(t *testing.T) {
	requireQADB(t)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	legit := createQAConnection(t, beleza, "salao-1", "phone-legitimo", "token-legitimo", receiver.URL())

	stub := qaEmbeddedSignupMetaStub("token-do-atacante", "waba-atacante", "phone-atacante", "5511900000000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	// state escolhido livremente por quem inicia o fluxo.
	rec := qaGetEmbeddedSignupCallback(t, srv, "code=codigo-do-atacante&state=beleza_web_salao-1", nil)

	conn, ok := qaFindConnection(t, beleza.ID, "salao-1")
	if !ok {
		t.Fatal("connection disappeared")
	}

	if conn.AccessToken == "token-legitimo" && conn.PhoneNumberID == "phone-legitimo" {
		t.Logf("behaviour changed: callback (HTTP %d) no longer overwrote the tenant credentials — state validation seems to be in place, update the QA report", rec.Code)
		return
	}

	if conn.ID != legit.ID {
		t.Errorf("expected the same connection row to be updated, got %s vs %s", conn.ID, legit.ID)
	}
	t.Logf(
		"finding: callback with an attacker-chosen state (HTTP %d) replaced the credentials of beleza_web/salao-1 — access_token %q -> %q, phone_number_id %q -> %q. Outbound messages of that tenant now leave through the injected number.",
		rec.Code, "token-legitimo", conn.AccessToken, "phone-legitimo", conn.PhoneNumberID,
	)
}

// TestQAEmbeddedSignupTenantIDWithUnderscoreIsUnreachable documenta que tenants
// cujo id contém "_" não conseguem concluir o onboarding.
func TestQAEmbeddedSignupTenantIDWithUnderscoreIsUnreachable(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao_1", nil)
	if rec.Code == http.StatusOK {
		if conn, ok := qaFindConnection(t, beleza.ID, "salao_1"); ok {
			t.Logf("behaviour changed: tenant with underscore was bound correctly (%+v) — update the QA report", conn)
			return
		}
		t.Fatalf("callback returned 200 but no connection for tenant salao_1")
	}

	if _, ok := qaFindConnection(t, beleza.ID, "salao_1"); ok {
		t.Fatal("unexpected connection created for tenant salao_1")
	}
	t.Logf(
		"finding: state \"beleza_web_salao_1\" was parsed as slug \"beleza_web_salao\" and rejected with HTTP %d — any tenant_id containing \"_\" cannot be onboarded",
		rec.Code,
	)
}

// TestQAEmbeddedSignupIgnoresAcceptJSON documenta que a negociação de conteúdo
// lê o header Accept da RESPOSTA (w.Header()), nunca da requisição: o ramo JSON
// é inalcançável e o SaaS sempre recebe HTML.
func TestQAEmbeddedSignupIgnoresAcceptJSON(t *testing.T) {
	requireQADB(t)

	stub := qaEmbeddedSignupMetaStub("token-x", "waba-x", "phone-x", "5511999990000")
	srv := newQAServer(t, stub, 100)
	qaSetEmbeddedSignupEnv(t)

	createQASystem(t, "Beleza Web", "beleza_web", "")

	rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=slug_inexistente_1", map[string]string{
		"Accept": "application/json",
	})

	contentType := rec.Header().Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		t.Logf("behaviour changed: Accept: application/json is honoured now (content-type %q) — update the QA report", contentType)
		return
	}
	t.Logf(
		"finding: request with Accept: application/json got Content-Type %q — writeEmbeddedSignupResult reads the response header instead of the request, so the JSON branch is dead code",
		contentType,
	)
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

			rec := qaGetEmbeddedSignupCallback(t, srv, "code=abc&state=beleza_web_salao-1", nil)
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
