package main

// Testes regressivos de QA para o webhook único da Meta (/webhook/whatsapp):
// roteamento por phone_number_id, isolamento multi-tenant sob concorrência,
// comportamento diante de falha do SaaS de destino e integridade dos status de
// entrega em message_logs.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

// ----------------------------------------------------------------------------
// Receptor de webhook do SaaS
// ----------------------------------------------------------------------------

type qaSaaSReceiver struct {
	server *httptest.Server

	mu       sync.Mutex
	received []saasWebhookPayload

	status  atomic.Int64
	hits    atomic.Int64
	delayMs atomic.Int64
}

func newQASaaSReceiver(t *testing.T) *qaSaaSReceiver {
	t.Helper()

	receiver := &qaSaaSReceiver{}
	receiver.status.Store(http.StatusOK)
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiver.hits.Add(1)

		if delay := receiver.delayMs.Load(); delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}

		var payload saasWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			receiver.mu.Lock()
			receiver.received = append(receiver.received, payload)
			receiver.mu.Unlock()
		}

		status := int(receiver.status.Load())
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":"saas unavailable"}`))
		}
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *qaSaaSReceiver) URL() string { return r.server.URL }

func (r *qaSaaSReceiver) Received() []saasWebhookPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]saasWebhookPayload, len(r.received))
	copy(out, r.received)
	return out
}

func (r *qaSaaSReceiver) Hits() int { return int(r.hits.Load()) }

// ----------------------------------------------------------------------------
// Payloads da Meta
// ----------------------------------------------------------------------------

var qaInboundMsgSeq atomic.Int64

func qaInboundTextPayload(phoneNumberID, from, text string) string {
	id := fmt.Sprintf("wamid.QA.IN.%d", qaInboundMsgSeq.Add(1))
	return qaInboundTextPayloadWithID(phoneNumberID, from, text, id)
}

func qaInboundTextPayloadWithID(phoneNumberID, from, text, messageID string) string {
	return fmt.Sprintf(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": %q, "display_phone_number": "5511999990000"},
					"messages": [{"id": %q, "from": %q, "type": "text", "text": {"body": %q}}]
				}
			}]
		}]
	}`, phoneNumberID, messageID, from, text)
}

func qaInboundButtonPayload(phoneNumberID, from, payload string) string {
	id := fmt.Sprintf("wamid.QA.BTN.%d", qaInboundMsgSeq.Add(1))
	return qaInboundButtonPayloadWithID(phoneNumberID, from, payload, id)
}

func qaInboundButtonPayloadWithID(phoneNumberID, from, payload, messageID string) string {
	return fmt.Sprintf(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": %q},
					"messages": [{"id": %q, "from": %q, "type": "button", "button": {"payload": %q, "text": "Confirmar"}}]
				}
			}]
		}]
	}`, phoneNumberID, messageID, from, payload)
}

func qaDeliveryStatusPayload(phoneNumberID, metaMessageID, status, category string, timestamp int64) string {
	return fmt.Sprintf(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": %q},
					"statuses": [{
						"id": %q,
						"status": %q,
						"timestamp": "%d",
						"recipient_id": "5511988887777",
						"pricing": {"billable": true, "pricing_model": "CBP", "category": %q}
					}]
				}
			}]
		}]
	}`, phoneNumberID, metaMessageID, status, timestamp, category)
}

// ----------------------------------------------------------------------------
// Verificação do webhook
// ----------------------------------------------------------------------------

func TestQAWebhookVerifyRejectsWrongToken(t *testing.T) {
	srv := &server{}
	t.Setenv("META_WEBHOOK_VERIFY_TOKEN", "qa-verify-token")

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "valid subscribe",
			query:      "hub.mode=subscribe&hub.verify_token=qa-verify-token&hub.challenge=1234",
			wantStatus: http.StatusOK,
			wantBody:   "1234",
		},
		{
			name:       "wrong token",
			query:      "hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=1234",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "empty token",
			query:      "hub.mode=subscribe&hub.verify_token=&hub.challenge=1234",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong mode",
			query:      "hub.mode=unsubscribe&hub.verify_token=qa-verify-token&hub.challenge=1234",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no query at all",
			query:      "",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/webhook/whatsapp?"+tc.query, nil)
			rec := httptest.NewRecorder()
			srv.handleWhatsAppWebhookVerify(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && strings.TrimSpace(rec.Body.String()) != tc.wantBody {
				t.Errorf("body = %q, want the hub.challenge echoed back (%q)", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Roteamento e isolamento
// ----------------------------------------------------------------------------

func TestQAWebhookRoutesInboundToOwningTenantOnly(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas SaaS", "clinicas_saas", "")

	receiverBeleza := newQASaaSReceiver(t)
	receiverClinica := newQASaaSReceiver(t)

	connBeleza := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiverBeleza.URL())
	createQAConnection(t, clinica, "salao-1", "phone-clinica", "token-clinica", receiverClinica.URL())

	rec := qaPostMetaWebhook(t, srv, qaInboundTextPayload("phone-beleza", "5511977776666", "quero remarcar"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 immediately (Meta 3s timeout) — body=%s", rec.Code, rec.Body.String())
	}

	qaWaitFor(t, 5*time.Second, "beleza SaaS webhook delivery", func() bool {
		return len(receiverBeleza.Received()) == 1
	})
	qaWaitFor(t, 5*time.Second, "inbound message_logs row", func() bool {
		return qaCountMessageLogs(t) == 1
	})

	// O tenant homônimo da outra aplicação mãe não pode receber nada.
	time.Sleep(200 * time.Millisecond)
	if got := len(receiverClinica.Received()); got != 0 {
		t.Errorf("cross-tenant leak: clinicas_saas receiver got %d payloads", got)
	}

	forwarded := receiverBeleza.Received()[0]
	if forwarded.SystemID != beleza.ID {
		t.Errorf("forwarded system_id = %q, want %q", forwarded.SystemID, beleza.ID)
	}
	if forwarded.SistemaOrigem != beleza.Slug {
		t.Errorf("forwarded sistema_origem = %q, want %q", forwarded.SistemaOrigem, beleza.Slug)
	}
	if forwarded.TenantID != "salao-1" {
		t.Errorf("forwarded tenant_id = %q, want salao-1", forwarded.TenantID)
	}
	if forwarded.PhoneNumber != "5511977776666" || forwarded.Text != "quero remarcar" {
		t.Errorf("forwarded content mismatch: %+v", forwarded)
	}
	if forwarded.EventType != "text_message" {
		t.Errorf("forwarded event_type = %q, want text_message", forwarded.EventType)
	}

	logs := qaListMessageLogs(t)
	row := logs[0]
	if row.SystemID != beleza.ID || row.ConnectionID != connBeleza.ID {
		t.Errorf("inbound log misattributed: system_id=%q connection_id=%q", row.SystemID, row.ConnectionID)
	}
	if row.Direction != string(model.MessageDirectionInbound) {
		t.Errorf("direction = %q, want INBOUND", row.Direction)
	}
	if row.ReceivedContent != "quero remarcar" {
		t.Errorf("received_content = %q", row.ReceivedContent)
	}
	if row.TenantID != "salao-1" {
		t.Errorf("external_client_id = %q, want salao-1", row.TenantID)
	}
}

func TestQAWebhookIgnoresUnknownPhoneNumberID(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	rec := qaPostMetaWebhook(t, srv, qaInboundTextPayload("phone-desconhecido", "5511977776666", "oi"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Meta does not retry forever", rec.Code)
	}

	time.Sleep(300 * time.Millisecond)
	if got := receiver.Hits(); got != 0 {
		t.Errorf("unknown phone_number_id must not be forwarded, got %d deliveries", got)
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("unknown phone_number_id wrote %d message_logs rows", count)
	}
}

func TestQAWebhookMalformedPayloads(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	cases := []struct {
		name       string
		payload    string
		wantStatus int
	}{
		{name: "invalid json", payload: `{"entry":`, wantStatus: http.StatusBadRequest},
		{name: "empty body", payload: ``, wantStatus: http.StatusBadRequest},
		{name: "no entry", payload: `{"object":"whatsapp_business_account"}`, wantStatus: http.StatusOK},
		{name: "entry without metadata", payload: `{"entry":[{"changes":[{"field":"messages","value":{}}]}]}`, wantStatus: http.StatusOK},
		{
			name:       "unknown field type",
			payload:    `{"entry":[{"changes":[{"field":"account_review_update","value":{"metadata":{"phone_number_id":"phone-beleza"}}}]}]}`,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := qaPostMetaWebhook(t, srv, tc.payload)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}

	time.Sleep(300 * time.Millisecond)
	if got := receiver.Hits(); got != 0 {
		t.Errorf("malformed payloads must not be forwarded, got %d deliveries", got)
	}
	if count := qaCountMessageLogs(t); count != 0 {
		t.Errorf("malformed payloads wrote %d message_logs rows", count)
	}
}

// TestQAWebhookConcurrentLoadKeepsTenantIsolation bombardeia o webhook único
// com eventos simultâneos de 4 conexões distintas e verifica que cada SaaS
// recebeu somente o que é seu e que message_logs não perdeu nem duplicou nada.
func TestQAWebhookConcurrentLoadKeepsTenantIsolation(t *testing.T) {
	requireQADB(t)

	const eventsPerTenant = 25

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	systems := []qaSystem{
		createQASystem(t, "Beleza Web", "beleza_web", ""),
		createQASystem(t, "Clinicas SaaS", "clinicas_saas", ""),
	}

	type tenantFixture struct {
		system        qaSystem
		tenantID      string
		phoneNumberID string
		receiver      *qaSaaSReceiver
		conn          *model.WhatsAppConnection
	}

	fixtures := make([]tenantFixture, 0, 4)
	for _, system := range systems {
		for i := range 2 {
			receiver := newQASaaSReceiver(t)
			tenantID := fmt.Sprintf("salao-%d", i)
			phoneNumberID := fmt.Sprintf("phone-%s-%d", system.Slug, i)
			conn := createQAConnection(t, system, tenantID, phoneNumberID,
				fmt.Sprintf("token-%s-%d", system.Slug, i), receiver.URL())
			fixtures = append(fixtures, tenantFixture{
				system: system, tenantID: tenantID, phoneNumberID: phoneNumberID,
				receiver: receiver, conn: conn,
			})
		}
	}

	expectedTotal := len(fixtures) * eventsPerTenant

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fixture := range fixtures {
		for i := range eventsPerTenant {
			wg.Add(1)
			go func(fixture tenantFixture, i int) {
				defer wg.Done()
				<-start
				text := fmt.Sprintf("%s|%s|%d", fixture.system.Slug, fixture.tenantID, i)
				rec := qaPostMetaWebhook(t, srv,
					qaInboundTextPayload(fixture.phoneNumberID, "5511977776666", text))
				if rec.Code != http.StatusOK {
					t.Errorf("%s/%s event %d: status = %d", fixture.system.Slug, fixture.tenantID, i, rec.Code)
				}
			}(fixture, i)
		}
	}
	close(start)
	wg.Wait()

	qaWaitFor(t, 20*time.Second, fmt.Sprintf("%d inbound message_logs rows", expectedTotal), func() bool {
		return qaCountMessageLogs(t) >= expectedTotal
	})
	qaWaitFor(t, 20*time.Second, "all SaaS deliveries", func() bool {
		for _, fixture := range fixtures {
			if len(fixture.receiver.Received()) < eventsPerTenant {
				return false
			}
		}
		return true
	})
	time.Sleep(300 * time.Millisecond)

	if got := qaCountMessageLogs(t); got != expectedTotal {
		t.Errorf("message_logs rows = %d, want %d", got, expectedTotal)
	}

	for _, fixture := range fixtures {
		payloads := fixture.receiver.Received()
		if len(payloads) != eventsPerTenant {
			t.Errorf("%s/%s: SaaS received %d payloads, want %d",
				fixture.system.Slug, fixture.tenantID, len(payloads), eventsPerTenant)
		}
		seen := map[string]int{}
		for _, payload := range payloads {
			if payload.SystemID != fixture.system.ID || payload.TenantID != fixture.tenantID {
				t.Errorf("cross-tenant leak on %s/%s receiver: got system_id=%q tenant_id=%q",
					fixture.system.Slug, fixture.tenantID, payload.SystemID, payload.TenantID)
			}
			wantPrefix := fmt.Sprintf("%s|%s|", fixture.system.Slug, fixture.tenantID)
			if !strings.HasPrefix(payload.Text, wantPrefix) {
				t.Errorf("cross-tenant content leak: %s/%s receiver got text %q",
					fixture.system.Slug, fixture.tenantID, payload.Text)
			}
			seen[payload.Text]++
		}
		for text, count := range seen {
			if count != 1 {
				t.Errorf("%s/%s: event %q delivered %d times", fixture.system.Slug, fixture.tenantID, text, count)
			}
		}
	}

	perConnection := map[string]int{}
	for _, row := range qaListMessageLogs(t) {
		if row.Direction != string(model.MessageDirectionInbound) {
			t.Errorf("unexpected direction %q under inbound load", row.Direction)
		}
		perConnection[row.ConnectionID]++
	}
	for _, fixture := range fixtures {
		if got := perConnection[fixture.conn.ID]; got != eventsPerTenant {
			t.Errorf("%s/%s: message_logs rows = %d, want %d",
				fixture.system.Slug, fixture.tenantID, got, eventsPerTenant)
		}
	}
}

// TestQAWebhookSpawnsUnboundedGoroutinesPerEvent mede o custo de concorrência
// do webhook: cada POST cria uma goroutine sem limite nem backpressure. Com o
// SaaS lento, uma rajada acumula uma goroutine por evento em voo — e o endpoint
// é público (sem API Key e sem validação de assinatura).
func TestQAWebhookSpawnsUnboundedGoroutinesPerEvent(t *testing.T) {
	requireQADB(t)

	const burst = 300

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	receiver.delayMs.Store(400)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	baseline := runtime.NumGoroutine()

	for i := range burst {
		rec := qaPostMetaWebhook(t, srv,
			qaInboundTextPayload("phone-beleza", "5511977776666", fmt.Sprintf("burst-%d", i)))
		if rec.Code != http.StatusOK {
			t.Fatalf("event %d: status = %d", i, rec.Code)
		}
	}

	peak := runtime.NumGoroutine()
	inFlight := peak - baseline

	// Drena antes de encerrar para não vazar processamento no teste seguinte.
	qaWaitFor(t, 60*time.Second, fmt.Sprintf("%d inbound rows", burst), func() bool {
		return qaCountMessageLogs(t) >= burst
	})
	qaWaitFor(t, 60*time.Second, "all SaaS deliveries", func() bool {
		return receiver.Hits() >= burst
	})
	time.Sleep(time.Second)

	if got := qaCountMessageLogs(t); got != burst {
		t.Errorf("message_logs rows = %d, want %d — burst lost or duplicated audit rows", got, burst)
	}
	if got := receiver.Hits(); got != burst {
		t.Errorf("SaaS deliveries = %d, want %d", got, burst)
	}

	t.Logf(
		"load: %d events in one burst -> %d extra goroutines in flight (baseline %d, peak %d); %d audit rows and %d SaaS deliveries, no loss",
		burst, inFlight, baseline, peak, qaCountMessageLogs(t), receiver.Hits(),
	)
	if inFlight < burst/4 {
		t.Logf("behaviour changed: goroutine growth was bounded (%d for %d events) — a worker pool seems to be in place, update the QA report", inFlight, burst)
	}
}

// TestQAWebhookSaaSFailureRetriesAndMarksFailed verifica retry com backoff e
// marcação de falha em message_logs quando o SaaS de destino responde 5xx/429.
// A Meta continua recebendo 200 imediatamente.
func TestQAWebhookSaaSFailureRetriesAndMarksFailed(t *testing.T) {
	cases := []struct {
		name       string
		saasStatus int
	}{
		{name: "saas 500", saasStatus: http.StatusInternalServerError},
		{name: "saas 429", saasStatus: http.StatusTooManyRequests},
		{name: "saas 502", saasStatus: http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireQADB(t)

			stub := newQAMetaStub()
			srv := newQAServer(t, stub, 100)

			beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
			receiver := newQASaaSReceiver(t)
			receiver.status.Store(int64(tc.saasStatus))
			conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

			rec := qaPostMetaWebhook(t, srv, qaInboundTextPayload("phone-beleza", "5511977776666", "oi"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — SaaS failure must not be pushed back to Meta", rec.Code)
			}

			qaWaitFor(t, 10*time.Second, "inbound audit row", func() bool {
				return qaCountMessageLogs(t) == 1
			})
			qaWaitFor(t, 10*time.Second, "SaaS retries completed", func() bool {
				return receiver.Hits() >= 2
			})
			qaWaitFor(t, 10*time.Second, "inbound marked failed", func() bool {
				logs := qaListMessageLogs(t)
				return len(logs) == 1 && logs[0].Status == string(model.MessageStatusFailed)
			})

			logs := qaListMessageLogs(t)
			if len(logs) != 1 {
				t.Fatalf("expected exactly 1 inbound audit row, got %d", len(logs))
			}
			row := logs[0]
			if row.ConnectionID != conn.ID || row.Direction != string(model.MessageDirectionInbound) {
				t.Errorf("inbound audit row misattributed: %+v", row)
			}
			if row.Status != string(model.MessageStatusFailed) {
				t.Errorf("status = %q, want failed after SaaS relay exhaustion", row.Status)
			}

			attempts := receiver.Hits()
			if attempts < 2 {
				t.Errorf("SaaS delivery attempts = %d, want at least 2 retries", attempts)
			}
			t.Logf(
				"SaaS returned %d; gateway made %d attempt(s) and marked inbound as failed",
				tc.saasStatus, attempts,
			)
		})
	}
}

// TestQAWebhookDeliveryStatusUpdatesBilling valida a reconciliação de entrega e
// custo Meta, incluindo idempotência diante do retry da Meta.
func TestQAWebhookDeliveryStatusUpdatesBilling(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", "")

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "salao-1",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
		"variables":     []string{"Ana", "10:00"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	logs := qaListMessageLogs(t)
	if len(logs) != 1 || logs[0].MetaMessageID == "" {
		t.Fatalf("expected 1 outbound row with meta_message_id, got %+v", logs)
	}
	wamid := logs[0].MetaMessageID
	deliveredAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	statusRec := qaPostMetaWebhook(t, srv,
		qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "MARKETING", deliveredAt.Unix()))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status webhook = %d", statusRec.Code)
	}

	qaWaitFor(t, 5*time.Second, "message marked as delivered", func() bool {
		rows := qaListMessageLogs(t)
		return len(rows) == 1 && rows[0].Status == string(model.MessageStatusDelivered)
	})

	row := qaListMessageLogs(t)[0]
	if row.Category != string(model.MessageCategoryMarketing) {
		t.Errorf("message_category = %q, want MARKETING from the Meta pricing block", row.Category)
	}
	if row.MetaCost <= 0 {
		t.Errorf("meta_cost = %v, want the marketing cost", row.MetaCost)
	}
	if row.DeliveredAt == nil {
		t.Fatal("delivered_at must be set")
	}
	if diff := row.DeliveredAt.UTC().Sub(deliveredAt); diff > time.Second || diff < -time.Second {
		t.Errorf("delivered_at = %v, want %v (from the Meta timestamp)", row.DeliveredAt.UTC(), deliveredAt)
	}

	// Retry da Meta com o mesmo status: precisa ser idempotente.
	firstCost := row.MetaCost
	dupRec := qaPostMetaWebhook(t, srv,
		qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "MARKETING", deliveredAt.Unix()))
	if dupRec.Code != http.StatusOK {
		t.Fatalf("duplicate status webhook = %d", dupRec.Code)
	}
	time.Sleep(500 * time.Millisecond)

	rows := qaListMessageLogs(t)
	if len(rows) != 1 {
		t.Fatalf("duplicate delivery webhook created %d rows", len(rows))
	}
	if rows[0].MetaCost != firstCost {
		t.Errorf("meta_cost changed on duplicate webhook: %v -> %v (double billing)", firstCost, rows[0].MetaCost)
	}
}

// TestQAWebhookDeliveryStatusIsNotScopedToConnection expõe que
// MarkMessageLogDelivered busca apenas por meta_message_id: um status recebido
// no phone_number_id de outra conexão marca a mensagem de um system alheio.
// Combinado com a ausência de validação de assinatura (X-Hub-Signature-256),
// permite forjar entrega/custo de outro tenant.
func TestQAWebhookDeliveryStatusIsNotScopedToConnection(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	clinica := createQASystem(t, "Clinicas SaaS", "clinicas_saas", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", "")
	createQAConnection(t, clinica, "clinica-1", "phone-clinica", "token-clinica", "")

	rec := qaPostSendNotification(t, srv, beleza.APIKey, map[string]any{
		"tenant_id":     "salao-1",
		"phone_number":  "5511988887777",
		"template_name": "confirma_agendamento",
		"variables":     []string{"Ana", "10:00"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	wamid := qaListMessageLogs(t)[0].MetaMessageID

	// Status chega pelo phone_number_id da OUTRA aplicação mãe.
	statusRec := qaPostMetaWebhook(t, srv,
		qaDeliveryStatusPayload("phone-clinica", wamid, "delivered", "MARKETING", time.Now().Unix()))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status webhook = %d", statusRec.Code)
	}

	time.Sleep(700 * time.Millisecond)

	row := qaListMessageLogs(t)[0]
	if row.Status == string(model.MessageStatusDelivered) {
		t.Logf(
			"finding: message %s of system %s (beleza_web) was marked delivered with category %s and meta_cost %v by a status event received on the phone_number_id of clinicas_saas — delivery reconciliation is not scoped by connection/system",
			wamid, row.SystemID, row.Category, row.MetaCost,
		)
		return
	}
	t.Logf("behaviour changed: cross-connection status no longer updates the log (status=%q) — update the QA report", row.Status)
}

// TestQAWebhookDuplicateInboundIsDeduped garante que o retry da Meta com o
// mesmo wamid não gera segundo callback ao SaaS nem segunda linha em message_logs.
func TestQAWebhookDuplicateInboundIsDeduped(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	payload := qaInboundButtonPayloadWithID("phone-beleza", "5511977776666", "APPT_CONFIRM", "wamid.QA.DEDUP.1")

	for range 2 {
		if rec := qaPostMetaWebhook(t, srv, payload); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	qaWaitFor(t, 5*time.Second, "single delivery", func() bool {
		return len(receiver.Received()) >= 1 && qaCountMessageLogs(t) >= 1
	})
	time.Sleep(400 * time.Millisecond)

	deliveries := receiver.Received()
	logs := qaCountMessageLogs(t)
	if len(deliveries) != 1 || logs != 1 {
		t.Fatalf("dedup failed: %d SaaS deliveries and %d logs for a duplicated Meta event", len(deliveries), logs)
	}
	if deliveries[0].Action != "CONFIRM" {
		t.Errorf("button action = %q, want CONFIRM", deliveries[0].Action)
	}
}

// TestQAWebhookForwardsTypedMediaEvents garante image/audio/document/location/reaction
// tipados: audit em message_logs + repasse com IDs/metadados preservados.
func TestQAWebhookForwardsTypedMediaEvents(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	payloads := []struct {
		name      string
		messageID string
		message   string
		eventType string
		check     func(t *testing.T, p saasWebhookPayload)
	}{
		{
			name: "image", messageID: "wamid.QA.MEDIA.IMG", eventType: "image_message",
			message: `{"id":"wamid.QA.MEDIA.IMG","from":"5511977776666","type":"image","image":{"id":"media-1","mime_type":"image/jpeg","caption":"foto"}}`,
			check: func(t *testing.T, p saasWebhookPayload) {
				if p.Media == nil || p.Media.ID != "media-1" || p.Media.MimeType != "image/jpeg" {
					t.Fatalf("image media = %+v", p.Media)
				}
			},
		},
		{
			name: "audio", messageID: "wamid.QA.MEDIA.AUD", eventType: "audio_message",
			message: `{"id":"wamid.QA.MEDIA.AUD","from":"5511977776666","type":"audio","audio":{"id":"media-2","mime_type":"audio/ogg"}}`,
			check: func(t *testing.T, p saasWebhookPayload) {
				if p.Media == nil || p.Media.ID != "media-2" {
					t.Fatalf("audio media = %+v", p.Media)
				}
			},
		},
		{
			name: "document", messageID: "wamid.QA.MEDIA.DOC", eventType: "document_message",
			message: `{"id":"wamid.QA.MEDIA.DOC","from":"5511977776666","type":"document","document":{"id":"media-3","filename":"x.pdf"}}`,
			check: func(t *testing.T, p saasWebhookPayload) {
				if p.Media == nil || p.Media.ID != "media-3" || p.Media.Filename != "x.pdf" {
					t.Fatalf("document media = %+v", p.Media)
				}
			},
		},
		{
			name: "location", messageID: "wamid.QA.MEDIA.LOC", eventType: "location_message",
			message: `{"id":"wamid.QA.MEDIA.LOC","from":"5511977776666","type":"location","location":{"latitude":-23.5,"longitude":-46.6,"name":"SP"}}`,
			check: func(t *testing.T, p saasWebhookPayload) {
				if p.Location == nil || p.Location.Latitude != -23.5 || p.Location.Name != "SP" {
					t.Fatalf("location = %+v", p.Location)
				}
			},
		},
		{
			name: "reaction", messageID: "wamid.QA.MEDIA.REA", eventType: "reaction_message",
			message: `{"id":"wamid.QA.MEDIA.REA","from":"5511977776666","type":"reaction","reaction":{"message_id":"wamid.ORIG","emoji":"👍"}}`,
			check: func(t *testing.T, p saasWebhookPayload) {
				if p.Reaction == nil || p.Reaction.Emoji != "👍" || p.Reaction.MessageID != "wamid.ORIG" {
					t.Fatalf("reaction = %+v", p.Reaction)
				}
			},
		},
	}

	for _, tc := range payloads {
		body := fmt.Sprintf(`{"entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"phone-beleza"},"messages":[%s]}}]}]}`, tc.message)
		if rec := qaPostMetaWebhook(t, srv, body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.name, rec.Code)
		}
	}

	qaWaitFor(t, 5*time.Second, "typed media deliveries", func() bool {
		return len(receiver.Received()) >= len(payloads) && qaCountMessageLogs(t) >= len(payloads)
	})

	byID := map[string]saasWebhookPayload{}
	for _, p := range receiver.Received() {
		byID[p.MetaMessageID] = p
	}
	for _, tc := range payloads {
		got, ok := byID[tc.messageID]
		if !ok {
			t.Fatalf("%s: missing SaaS delivery", tc.name)
		}
		if got.EventType != tc.eventType {
			t.Errorf("%s: event_type = %q, want %q", tc.name, got.EventType, tc.eventType)
		}
		tc.check(t, got)
	}
	if count := qaCountMessageLogs(t); count != len(payloads) {
		t.Fatalf("message_logs = %d, want %d", count, len(payloads))
	}
}

// TestQAWebhookUnknownTypeIsAuditedNotSilent garante que tipo desconhecido
// gera linha de auditoria (nunca drop silencioso).
func TestQAWebhookUnknownTypeIsAuditedNotSilent(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	body := `{"entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"phone-beleza"},"messages":[{"id":"wamid.QA.UNK.1","from":"5511977776666","type":"sticker","sticker":{"id":"stk-1"}}]}}]}]}`
	if rec := qaPostMetaWebhook(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	qaWaitFor(t, 5*time.Second, "unknown audited", func() bool {
		return qaCountMessageLogs(t) >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if hits := receiver.Hits(); hits != 0 {
		t.Fatalf("unknown type must not relay to SaaS, got %d hits", hits)
	}
	logs := qaListMessageLogs(t)
	if len(logs) != 1 || logs[0].TemplateName != "unknown_message" {
		t.Fatalf("expected audited unknown_message log, got %+v", logs)
	}
	if reason := qaMessageLogFailureReason(t, logs[0].ID); reason != "permanent" {
		t.Errorf("failure_reason = %q, want permanent", reason)
	}
}

// TestQAWebhookRejectsForgedUnsignedPayload garante rejeição por assinatura Meta.
func TestQAWebhookRejectsForgedUnsignedPayload(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	forged := qaInboundButtonPayload("phone-beleza", "5511900000000", "APPT_CONFIRM")

	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(forged))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	rec := httptest.NewRecorder()
	srv.handleWhatsAppWebhookEvent(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged payload status = %d, want 401", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if hits := receiver.Hits(); hits != 0 {
		t.Fatalf("forged payload must not reach SaaS, got %d", hits)
	}
}

// TestQAWebhookFailsWithoutTenantWebhook removes the shared MOTHER fallback:
// missing connection/system webhook fails permanently without cross-tenant POST.
func TestQAWebhookFailsWithoutTenantWebhook(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	fallback := newQASaaSReceiver(t)
	t.Setenv("MOTHER_SYSTEM_WEBHOOK_URL", fallback.URL())

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-sem-webhook", "phone-beleza", "token-beleza", "")

	if rec := qaPostMetaWebhook(t, srv, qaInboundTextPayload("phone-beleza", "5511977776666", "oi")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	qaWaitFor(t, 5*time.Second, "failed log without webhook", func() bool {
		logs := qaListMessageLogs(t)
		return len(logs) == 1 && logs[0].Status == string(model.MessageStatusFailed)
	})
	if hits := fallback.Hits(); hits != 0 {
		t.Fatalf("MOTHER fallback must not receive posts, got %d", hits)
	}
}
