package main

// Testes regressivos de QA para POST /send-notification: isolamento
// multi-tenant por system_id, integridade de message_logs sob falha da Meta,
// proteção anti-spam e carga concorrente.

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/whatsappgetway/gateway/internal/model"
)

func TestQASendNotificationRoutesToTenantCredentials(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas SaaS", "clinicas_saas", "")

	// Mesmo tenant_id nas duas aplicações mãe — cenário clássico de vazamento.
	connBeleza := createQAConnection(t, beleza, "tenant-x", "phone-beleza", "token-beleza", "")
	connClinica := createQAConnection(t, clinica, "tenant-x", "phone-clinica", "token-clinica", "")

	cases := []struct {
		system qaSystem
		conn   *model.WhatsAppConnection
	}{
		{system: beleza, conn: connBeleza},
		{system: clinica, conn: connClinica},
	}

	for _, tc := range cases {
		t.Run(tc.system.Slug, func(t *testing.T) {
			rec := qaPostSendNotification(t, srv, tc.system.APIKey, map[string]any{
				"tenant_id":     "tenant-x",
				"phone_number":  "5511988887777",
				"template_name": "confirma_agendamento",
				"variables":     []string{"Ana", "10:00"},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	calls := stub.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 Meta calls, got %d", len(calls))
	}
	for _, call := range calls {
		switch call.AccessToken {
		case "token-beleza":
			if call.PhoneNumberID != "phone-beleza" {
				t.Errorf("credential leak: token-beleza used with phone_number_id %q", call.PhoneNumberID)
			}
		case "token-clinica":
			if call.PhoneNumberID != "phone-clinica" {
				t.Errorf("credential leak: token-clinica used with phone_number_id %q", call.PhoneNumberID)
			}
		default:
			t.Errorf("unexpected access token on the wire: %q", call.AccessToken)
		}
	}

	logs := qaListMessageLogs(t)
	if len(logs) != 2 {
		t.Fatalf("expected 2 message_logs rows, got %d", len(logs))
	}
	bySystem := map[string]qaMessageLogRow{}
	for _, row := range logs {
		bySystem[row.SystemID] = row
	}
	for _, tc := range cases {
		row, ok := bySystem[tc.system.ID]
		if !ok {
			t.Fatalf("no message_logs row for system %s", tc.system.Slug)
		}
		if row.ConnectionID != tc.conn.ID {
			t.Errorf("%s: connection_id = %q, want %q", tc.system.Slug, row.ConnectionID, tc.conn.ID)
		}
		if row.SistemaOrigem != tc.system.Slug {
			t.Errorf("%s: sistema_origem = %q", tc.system.Slug, row.SistemaOrigem)
		}
		if row.TenantID != "tenant-x" {
			t.Errorf("%s: external_client_id = %q, want tenant-x", tc.system.Slug, row.TenantID)
		}
		if row.Direction != string(model.MessageDirectionOutbound) {
			t.Errorf("%s: direction = %q", tc.system.Slug, row.Direction)
		}
		if row.Status != string(model.MessageStatusSent) {
			t.Errorf("%s: status = %q, want sent", tc.system.Slug, row.Status)
		}
		if row.MetaMessageID == "" {
			t.Errorf("%s: meta_message_id must be persisted for delivery reconciliation", tc.system.Slug)
		}
	}
}

func TestQASendNotificationRejectsCrossSystemTenant(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas SaaS", "clinicas_saas", "")
	createQAConnection(t, clinica, "tenant-only-clinica", "phone-clinica", "token-clinica", "")

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "tenant-only-clinica",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
	})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — system A must not reach a tenant of system B (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("no Meta call should happen, got %d", stub.CallCount())
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("no message_logs row should be written, got %d", count)
	}
}

func TestQASendNotificationRequiresValidAPIKey(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

	payload := map[string]any{
		"tenant_id":     "tenant-1",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
	}

	cases := []struct {
		name   string
		apiKey string
	}{
		{name: "missing api key", apiKey: ""},
		{name: "unknown api key", apiKey: "sk_live_not_registered"},
		{name: "api key with wrong case", apiKey: strings.ToUpper(beleza.APIKey)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := qaPostSendNotification(t, srv, tc.apiKey, payload)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}

	if stub.CallCount() != 0 {
		t.Errorf("unauthenticated requests must never reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("unauthenticated requests must not write message_logs, got %d rows", count)
	}
}

func TestQASendNotificationRejectsSistemaOrigemMismatch(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":      "tenant-1",
		"phone_number":   "5511988887777",
		"template_name":  "confirma_agendamento",
		"sistema_origem": "clinicas_saas",
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("mismatched sistema_origem must not reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("mismatched sistema_origem must not write message_logs, got %d rows", count)
	}
}

func TestQASendNotificationValidatesPayload(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{name: "missing tenant_id", payload: map[string]any{"phone_number": "5511988887777", "template_name": "t"}},
		{name: "missing phone_number", payload: map[string]any{"tenant_id": "tenant-1", "template_name": "t"}},
		{name: "missing template_name", payload: map[string]any{"tenant_id": "tenant-1", "phone_number": "5511988887777"}},
		{name: "blank tenant_id", payload: map[string]any{"tenant_id": "   ", "phone_number": "5511988887777", "template_name": "t"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := qaPostSendNotification(t, srv, beleza.APIKey, tc.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}

	if stub.CallCount() != 0 {
		t.Errorf("invalid payloads must not reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("invalid payloads must not write message_logs, got %d rows", count)
	}
}

// TestQASendNotificationMetaFailuresAreAudited garante que queda da Meta
// (500/429/401/503) não perde a mensagem: o gateway responde 502 e registra o
// log como failed, atribuído ao system/conexão correto.
func TestQASendNotificationMetaFailuresAreAudited(t *testing.T) {
	cases := []struct {
		name       string
		metaStatus int
		metaBody   string
		wantInBody string
	}{
		{
			name:       "meta 500",
			metaStatus: http.StatusInternalServerError,
			metaBody:   `{"error":{"message":"An unknown error occurred","code":1}}`,
			wantInBody: "status 500",
		},
		{
			name:       "meta 429",
			metaStatus: http.StatusTooManyRequests,
			metaBody:   `{"error":{"message":"Rate limit hit","code":130429}}`,
			wantInBody: "status 429",
		},
		{
			name:       "meta 401 invalid tenant token",
			metaStatus: http.StatusUnauthorized,
			metaBody:   `{"error":{"message":"Invalid OAuth access token","code":190}}`,
			wantInBody: "status 401",
		},
		{
			name:       "meta 503 empty body",
			metaStatus: http.StatusServiceUnavailable,
			metaBody:   ``,
			wantInBody: "status 503",
		},
		{
			name:       "meta 200 without wamid",
			metaStatus: http.StatusOK,
			metaBody:   `{"messages":[]}`,
			wantInBody: "empty message id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireQADB(t)

			stub := newQAMetaStub()
			stub.respond = func(qaMetaCall, int64) (int, string) { return tc.metaStatus, tc.metaBody }
			srv := newQAServer(t, stub, 100)

			beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
			conn := createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

			rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
				"tenant_id":     "tenant-1",
				"phone_number":  "5511988887777",
				"template_name": "confirma_agendamento",
				"variables":     []string{"Ana"},
			})

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := qaDecodeError(t, rec); !strings.Contains(got, tc.wantInBody) {
				t.Errorf("error body = %q, want it to mention %q", got, tc.wantInBody)
			}
			if strings.Contains(rec.Body.String(), "token-beleza") {
				t.Errorf("tenant access token leaked in the API response: %s", rec.Body.String())
			}

			logs := qaListMessageLogs(t)
			if len(logs) != 1 {
				t.Fatalf("message lost: expected exactly 1 message_logs row, got %d", len(logs))
			}
			row := logs[0]
			if row.Status != string(model.MessageStatusFailed) {
				t.Errorf("status = %q, want failed", row.Status)
			}
			if row.SystemID != beleza.ID || row.ConnectionID != conn.ID {
				t.Errorf("wrong attribution: system_id=%q connection_id=%q", row.SystemID, row.ConnectionID)
			}
			if row.MetaMessageID != "" {
				t.Errorf("meta_message_id = %q, want empty on failure", row.MetaMessageID)
			}
			if row.DeliveredAt != nil {
				t.Errorf("delivered_at must stay NULL on failure, got %v", row.DeliveredAt)
			}
		})
	}
}

// TestQASendNotificationSpamSuspensionIsolatesTenants valida a suspensão
// automática por spam e garante que ela não contamina outro tenant/system.
func TestQASendNotificationSpamSuspensionIsolatesTenants(t *testing.T) {
	requireQADB(t)

	const maxPerMinute = 3

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, maxPerMinute)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas SaaS", "clinicas_saas", "")
	abuser := createQAConnection(t, beleza, "tenant-abuser", "phone-abuser", "token-abuser", "")
	neighbour := createQAConnection(t, beleza, "tenant-neighbour", "phone-neighbour", "token-neighbour", "")
	otherSystem := createQAConnection(t, clinica, "tenant-abuser", "phone-clinica", "token-clinica", "")

	payload := func(tenantID string) map[string]any {
		return map[string]any{
			"tenant_id":     tenantID,
			"phone_number":  "5511988887777",
			"template_name": "confirma_agendamento",
		}
	}

	for i := 1; i <= maxPerMinute; i++ {
		rec := qaPostSendNotification(t, srv, beleza.APIKey, payload("tenant-abuser"))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}

	rec := qaPostSendNotification(t, srv, beleza.APIKey, payload("tenant-abuser"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want 429 (body=%s)", maxPerMinute+1, rec.Code, rec.Body.String())
	}

	if status := qaConnectionStatus(t, abuser.ID); status != model.ConnectionStatusSuspendedSpam {
		t.Errorf("abuser connection status = %q, want %q", status, model.ConnectionStatusSuspendedSpam)
	}

	// Requisição seguinte deve ser barrada pelo estado persistido.
	rec = qaPostSendNotification(t, srv, beleza.APIKey, payload("tenant-abuser"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("suspended tenant: status = %d, want 429", rec.Code)
	}
	if got := qaDecodeError(t, rec); !strings.Contains(got, "spam") {
		t.Errorf("suspended tenant error = %q, want a spam protection message", got)
	}

	// Vizinhos não podem ser afetados.
	if rec := qaPostSendNotification(t, srv, beleza.APIKey, payload("tenant-neighbour")); rec.Code != http.StatusOK {
		t.Errorf("neighbour tenant of the same system: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := qaPostSendNotification(t, srv, clinica.APIKey, payload("tenant-abuser")); rec.Code != http.StatusOK {
		t.Errorf("same tenant_id under another system: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if status := qaConnectionStatus(t, neighbour.ID); status != model.ConnectionStatusActive {
		t.Errorf("neighbour connection status = %q, want ACTIVE", status)
	}
	if status := qaConnectionStatus(t, otherSystem.ID); status != model.ConnectionStatusActive {
		t.Errorf("other system connection status = %q, want ACTIVE", status)
	}

	// Somente as tentativas aceitas viram log; as 429 não geram registro.
	logs := qaListMessageLogs(t)
	if len(logs) != maxPerMinute+2 {
		t.Errorf("expected %d message_logs rows (only accepted attempts), got %d", maxPerMinute+2, len(logs))
	}
	if stub.CallCount() != maxPerMinute+2 {
		t.Errorf("expected %d Meta calls, got %d — blocked attempts must not reach Meta", maxPerMinute+2, stub.CallCount())
	}
	t.Logf(
		"observability gap: %d blocked-by-rate-limit attempts produced no message_logs row (only application log lines)",
		2,
	)
}

// TestQASendNotificationConcurrentLoadKeepsTenantIsolation dispara carga
// concorrente em 2 aplicações mãe x 3 tenants e verifica ausência de
// vazamento de credenciais e de perda/duplicação de logs.
func TestQASendNotificationConcurrentLoadKeepsTenantIsolation(t *testing.T) {
	requireQADB(t)

	const (
		tenantsPerSystem    = 3
		requestsPerTenant   = 30
		expectedTotalPerRun = 2 * tenantsPerSystem * requestsPerTenant
	)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)

	systems := []qaSystem{
		createQASystem(t, "Beleza Web", "beleza_web", ""),
		createQASystem(t, "Clinicas SaaS", "clinicas_saas", ""),
	}

	type tenantFixture struct {
		system qaSystem
		conn   *model.WhatsAppConnection
	}

	fixtures := make([]tenantFixture, 0, 2*tenantsPerSystem)
	for _, system := range systems {
		for i := range tenantsPerSystem {
			tenantID := fmt.Sprintf("tenant-%d", i)
			conn := createQAConnection(
				t, system, tenantID,
				fmt.Sprintf("phone-%s-%d", system.Slug, i),
				fmt.Sprintf("token-%s-%d", system.Slug, i),
				"",
			)
			fixtures = append(fixtures, tenantFixture{system: system, conn: conn})
		}
	}

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		statuses = map[int]int{}
	)

	for _, fixture := range fixtures {
		for range requestsPerTenant {
			wg.Add(1)
			go func(fixture tenantFixture) {
				defer wg.Done()
				<-start
				rec := qaPostSendNotification(t, srv, fixture.system.APIKey, map[string]any{
					"tenant_id":      fixture.conn.TenantID,
					"phone_number":   "5511988887777",
					"appointment_id": "appt-" + fixture.conn.TenantID,
					"template_name":  "confirma_agendamento",
					"variables":      []string{"Ana", "10:00"},
				})
				mu.Lock()
				statuses[rec.Code]++
				mu.Unlock()
			}(fixture)
		}
	}

	close(start)
	wg.Wait()

	if got := statuses[http.StatusOK]; got != expectedTotalPerRun {
		t.Errorf("200 responses = %d, want %d (full status map: %v)", got, expectedTotalPerRun, statuses)
	}

	logs := qaListMessageLogs(t)
	if len(logs) != expectedTotalPerRun {
		t.Errorf("message_logs rows = %d, want %d (lost or duplicated audit rows)", len(logs), expectedTotalPerRun)
	}

	perConnection := map[string]int{}
	wamids := map[string]int{}
	for _, row := range logs {
		perConnection[row.ConnectionID]++
		wamids[row.MetaMessageID]++
		if row.Status != string(model.MessageStatusSent) {
			t.Errorf("connection %s: unexpected status %q under load", row.ConnectionID, row.Status)
		}
	}
	for _, fixture := range fixtures {
		if got := perConnection[fixture.conn.ID]; got != requestsPerTenant {
			t.Errorf(
				"%s/%s: message_logs rows = %d, want %d",
				fixture.system.Slug, fixture.conn.TenantID, got, requestsPerTenant,
			)
		}
	}
	for wamid, count := range wamids {
		if count != 1 {
			t.Errorf("meta_message_id %q appears %d times in message_logs", wamid, count)
		}
	}

	// Cada chamada à Meta deve casar token <-> phone_number_id do tenant.
	calls := stub.Calls()
	if len(calls) != expectedTotalPerRun {
		t.Errorf("Meta calls = %d, want %d", len(calls), expectedTotalPerRun)
	}
	validPairs := map[string]string{}
	for _, fixture := range fixtures {
		validPairs[fixture.conn.AccessToken] = fixture.conn.PhoneNumberID
	}
	for _, call := range calls {
		want, ok := validPairs[call.AccessToken]
		if !ok {
			t.Errorf("unknown access token reached Meta: %q", call.AccessToken)
			continue
		}
		if call.PhoneNumberID != want {
			t.Errorf("credential leak under load: token %q sent through phone_number_id %q, want %q",
				call.AccessToken, call.PhoneNumberID, want)
		}
	}
}

// TestQASendNotificationBlocksSuspendedConnectionFromDatabase garante que o
// estado persistido é respeitado mesmo após restart do processo (rate limiter
// em memória vazio).
func TestQASendNotificationBlocksSuspendedConnectionFromDatabase(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

	if _, err := qaDB.Exec(
		`UPDATE whatsapp_connections SET status = $2 WHERE id = $1`,
		conn.ID, model.ConnectionStatusSuspendedSpam,
	); err != nil {
		t.Fatalf("suspend connection: %v", err)
	}

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "tenant-1",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
	})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 for a suspended connection (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Errorf("suspended connection must not reach Meta, got %d calls", stub.CallCount())
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("suspended connection wrote %d message_logs rows", count)
	}
}

// TestQASendNotificationEnforcesMonthlyLimit garante que /send-notification
// respeita MONTHLY_MESSAGE_LIMIT por tenant (mesmo critério de /v1/messages/send-template).
func TestQASendNotificationEnforcesMonthlyLimit(t *testing.T) {
	requireQADB(t)

	t.Setenv("MONTHLY_MESSAGE_LIMIT", "2")

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	if srv.usage.MonthlyLimit() != 2 {
		t.Fatalf("monthly limit fixture not applied, got %d", srv.usage.MonthlyLimit())
	}

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "tenant-1", "phone-beleza", "token-beleza", "")

	const attempts = 5
	accepted := 0
	rejected := 0
	for i := range attempts {
		rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
			"tenant_id":      "tenant-1",
			"phone_number":   "5511988887777",
			"appointment_id": fmt.Sprintf("appt-%d", i),
			"template_name":  "confirma_agendamento",
		})
		switch rec.Code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Fatalf("unexpected status %d on attempt %d (body=%s)", rec.Code, i, rec.Body.String())
		}
	}

	if accepted != 2 {
		t.Fatalf("accepted = %d, want 2 under MONTHLY_MESSAGE_LIMIT=2", accepted)
	}
	if rejected != 3 {
		t.Fatalf("rejected = %d, want 3", rejected)
	}
}
