package main

// Testes QA do fan-out de status (DEV-111):
//   1. delivered + read → 2 rows + 2 POSTs; reentrega Meta → 0 POST extra
//   2. stub 500 → DLQ; reprocess + stub 200 → 1 sucesso
//   3. status do phone A nunca chega ao webhook do tenant B
//   4. stub lento: Meta handler retorna 200 em <300ms
//   5. GET status: 404 inexistente; 403 cross-system

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/security"
)

type qaStatusHit struct {
	Payload   statusCallbackPayload
	Signature string
	Event     string
	RawBody   []byte
}

type qaStatusReceiver struct {
	server *httptest.Server

	mu       sync.Mutex
	received []qaStatusHit

	status  atomic.Int64
	hits    atomic.Int64
	delayMs atomic.Int64
}

func newQAStatusReceiver(t *testing.T) *qaStatusReceiver {
	t.Helper()

	receiver := &qaStatusReceiver{}
	receiver.status.Store(http.StatusOK)
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiver.hits.Add(1)

		if delay := receiver.delayMs.Load(); delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}

		body, _ := io.ReadAll(r.Body)
		var payload statusCallbackPayload
		_ = json.Unmarshal(body, &payload)

		receiver.mu.Lock()
		receiver.received = append(receiver.received, qaStatusHit{
			Payload:   payload,
			Signature: r.Header.Get("X-Gateway-Signature-256"),
			Event:     r.Header.Get("X-Gateway-Event"),
			RawBody:   append([]byte(nil), body...),
		})
		receiver.mu.Unlock()

		status := int(receiver.status.Load())
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":"saas unavailable"}`))
		}
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *qaStatusReceiver) URL() string { return r.server.URL }

func (r *qaStatusReceiver) Hits() int { return int(r.hits.Load()) }

func (r *qaStatusReceiver) Received() []qaStatusHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]qaStatusHit, len(r.received))
	copy(out, r.received)
	return out
}

func (r *qaStatusReceiver) Reset() {
	r.mu.Lock()
	r.received = nil
	r.mu.Unlock()
	r.hits.Store(0)
}

type qaDeliveryEventRow struct {
	ID             string
	Status         string
	CallbackStatus string
	MetaMessageID  string
	TenantID       string
	SystemID       string
}

func qaListDeliveryEvents(t *testing.T) []qaDeliveryEventRow {
	t.Helper()
	rows, err := qaDB.Query(`
		SELECT id::text, status, callback_status, meta_message_id, tenant_id, system_id::text
		FROM message_delivery_events
		ORDER BY meta_timestamp ASC, created_at ASC
	`)
	if err != nil {
		t.Fatalf("list delivery events: %v", err)
	}
	defer rows.Close()

	out := make([]qaDeliveryEventRow, 0)
	for rows.Next() {
		var row qaDeliveryEventRow
		if err := rows.Scan(&row.ID, &row.Status, &row.CallbackStatus, &row.MetaMessageID, &row.TenantID, &row.SystemID); err != nil {
			t.Fatalf("scan delivery event: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func qaDeliveryEventCallbackStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := qaDB.QueryRow(`SELECT callback_status FROM message_delivery_events WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read callback_status: %v", err)
	}
	return status
}

func qaPostAdminReprocess(t *testing.T, srv *server, eventID string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := security.GenerateToken("qa-admin-user", srv.jwtSecret)
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/delivery-events/"+eventID+"/reprocess", nil)
	req.AddCookie(&http.Cookie{Name: security.AuthCookieName, Value: token})
	req.SetPathValue("id", eventID)

	rec := httptest.NewRecorder()
	srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminReprocessDeliveryEvent)).ServeHTTP(rec, req)
	return rec
}

func qaGetMessageStatus(t *testing.T, srv *server, apiKey, metaMessageID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/"+metaMessageID+"/status", nil)
	req.Header.Set(apiKeyHeader, apiKey)
	req.SetPathValue("meta_message_id", metaMessageID)

	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleGetMessageStatus)).ServeHTTP(rec, req)
	return rec
}

// TestQAStatusFanOutDeliveredAndReadIdempotent cobre critério QA #1.
func TestQAStatusFanOutDeliveredAndReadIdempotent(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQAStatusReceiver(t)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	wamid := "wamid.QA.STATUS.1"
	tsDelivered := time.Now().UTC().Add(-2 * time.Minute).Unix()
	tsRead := time.Now().UTC().Add(-1 * time.Minute).Unix()

	rec := qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "UTILITY", tsDelivered))
	if rec.Code != http.StatusOK {
		t.Fatalf("delivered webhook: status = %d, want 200", rec.Code)
	}
	rec = qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", wamid, "read", "UTILITY", tsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("read webhook: status = %d, want 200", rec.Code)
	}

	qaWaitFor(t, 5*time.Second, "2 status callbacks sent", func() bool {
		events := qaListDeliveryEvents(t)
		if len(events) != 2 {
			return false
		}
		return events[0].CallbackStatus == string(model.CallbackStatusSent) &&
			events[1].CallbackStatus == string(model.CallbackStatusSent)
	})

	events := qaListDeliveryEvents(t)
	if len(events) != 2 {
		t.Fatalf("delivery events = %d, want 2", len(events))
	}
	if events[0].Status != "delivered" || events[1].Status != "read" {
		t.Errorf("statuses = %q/%q, want delivered/read", events[0].Status, events[1].Status)
	}
	for _, e := range events {
		if e.CallbackStatus != string(model.CallbackStatusSent) {
			t.Errorf("event %s callback_status = %q, want sent", e.ID, e.CallbackStatus)
		}
		if e.MetaMessageID != wamid || e.TenantID != conn.TenantID {
			t.Errorf("event lost tenant identity: %+v", e)
		}
	}

	if receiver.Hits() < 2 {
		t.Fatalf("SaaS hits = %d, want >= 2", receiver.Hits())
	}
	hits := receiver.Received()
	for _, hit := range hits {
		if hit.Event != gatewayEventMessageStatus {
			t.Errorf("X-Gateway-Event = %q", hit.Event)
		}
		if hit.Payload.EventType != gatewayEventMessageStatus {
			t.Errorf("event_type = %q", hit.Payload.EventType)
		}
		expectedSig := security.SignHMACSHA256Hex(conn.WebhookSecret, hit.RawBody)
		if hit.Signature != expectedSig {
			t.Errorf("HMAC mismatch: got %q want %q", hit.Signature, expectedSig)
		}
	}

	before := receiver.Hits()
	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "UTILITY", tsDelivered))
	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", wamid, "read", "UTILITY", tsRead))
	time.Sleep(300 * time.Millisecond)
	if receiver.Hits() != before {
		t.Errorf("identical Meta redelivery caused extra POSTs: hits %d → %d", before, receiver.Hits())
	}
	if got := len(qaListDeliveryEvents(t)); got != 2 {
		t.Errorf("after redelivery events = %d, want 2", got)
	}
}

// TestQAStatusFanOutDLQAndReprocess cobre critério QA #2.
func TestQAStatusFanOutDLQAndReprocess(t *testing.T) {
	requireQADB(t)
	t.Setenv("STATUS_CALLBACK_MAX_ATTEMPTS", "1")

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQAStatusReceiver(t)
	receiver.status.Store(http.StatusInternalServerError)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	wamid := "wamid.QA.STATUS.DLQ"
	ts := time.Now().UTC().Unix()
	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", wamid, "delivered", "UTILITY", ts))

	qaWaitFor(t, 8*time.Second, "event moved to dlq", func() bool {
		events := qaListDeliveryEvents(t)
		return len(events) == 1 && events[0].CallbackStatus == string(model.CallbackStatusDLQ)
	})

	events := qaListDeliveryEvents(t)
	eventID := events[0].ID
	hitsBefore := receiver.Hits()

	receiver.status.Store(http.StatusOK)
	receiver.Reset()

	rec := qaPostAdminReprocess(t, srv, eventID)
	if rec.Code != http.StatusOK {
		t.Fatalf("reprocess status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := qaDeliveryEventCallbackStatus(t, eventID); got != string(model.CallbackStatusSent) {
		t.Errorf("after reprocess callback_status = %q, want sent", got)
	}
	if receiver.Hits() != 1 {
		t.Errorf("reprocess POSTs = %d, want 1 (hits before reset window were %d)", receiver.Hits(), hitsBefore)
	}
}

// TestQAStatusFanOutNoCrossTenant cobre critério QA #3.
func TestQAStatusFanOutNoCrossTenant(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	recvA := newQAStatusReceiver(t)
	recvB := newQAStatusReceiver(t)

	sysA := createQASystem(t, "System A", "sys_a", "")
	sysB := createQASystem(t, "System B", "sys_b", "")
	createQAConnection(t, sysA, "tenant-a", "phone-a", "token-a", recvA.URL())
	createQAConnection(t, sysB, "tenant-b", "phone-b", "token-b", recvB.URL())

	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-a", "wamid.QA.CROSS.A", "delivered", "UTILITY", time.Now().Unix()))

	qaWaitFor(t, 5*time.Second, "tenant A callback", func() bool {
		return recvA.Hits() >= 1
	})
	time.Sleep(200 * time.Millisecond)

	if recvB.Hits() != 0 {
		t.Fatalf("tenant B received %d status callbacks, want 0", recvB.Hits())
	}
	events := qaListDeliveryEvents(t)
	if len(events) != 1 || events[0].TenantID != "tenant-a" || events[0].SystemID != sysA.ID {
		t.Fatalf("unexpected events: %+v", events)
	}
}

// TestQAStatusFanOutMetaGetsFast200 cobre critério QA #4.
func TestQAStatusFanOutMetaGetsFast200(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQAStatusReceiver(t)
	receiver.delayMs.Store(2000)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	start := time.Now()
	rec := qaPostMetaWebhook(t, srv,
		qaDeliveryStatusPayload("phone-beleza", "wamid.QA.STATUS.SLOW", "delivered", "UTILITY", time.Now().Unix()))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("Meta handler took %s, want <300ms (callback delay must not block 200)", elapsed)
	}

	qaWaitFor(t, 5*time.Second, "slow callback eventually sent", func() bool {
		return receiver.Hits() >= 1
	})
}

// TestQAGetMessageStatusIsolation cobre critério QA #5.
func TestQAGetMessageStatusIsolation(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQAStatusReceiver(t)

	sysA := createQASystem(t, "System A", "sys_a_status", "")
	sysB := createQASystem(t, "System B", "sys_b_status", "")
	createQAConnection(t, sysA, "tenant-a", "phone-a-status", "token-a", receiver.URL())
	createQAConnection(t, sysB, "tenant-b", "phone-b-status", "token-b", receiver.URL())

	wamid := "wamid.QA.STATUS.GET"
	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-a-status", wamid, "sent", "UTILITY", time.Now().Unix()))
	qaWaitFor(t, 5*time.Second, "status persisted", func() bool {
		return len(qaListDeliveryEvents(t)) == 1
	})

	rec404 := qaGetMessageStatus(t, srv, sysA.APIKey, "wamid.DOES.NOT.EXIST")
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rec404.Code)
	}

	rec403 := qaGetMessageStatus(t, srv, sysB.APIKey, wamid)
	if rec403.Code != http.StatusForbidden {
		t.Errorf("cross-system: status = %d body=%s, want 403", rec403.Code, rec403.Body.String())
	}

	rec200 := qaGetMessageStatus(t, srv, sysA.APIKey, wamid)
	if rec200.Code != http.StatusOK {
		t.Fatalf("owner get: status = %d body=%s, want 200", rec200.Code, rec200.Body.String())
	}
	var payload deliveryStatusResponse
	if err := json.Unmarshal(rec200.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if len(payload.Events) != 1 || payload.Events[0].Status != "sent" {
		t.Errorf("events = %+v, want 1 sent", payload.Events)
	}
}

// TestQAStatusFanOutNoWebhookMarksFailed garante persistência sem POST quando não há URL.
func TestQAStatusFanOutNoWebhookMarksFailed(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", "")

	qaPostMetaWebhook(t, srv, qaDeliveryStatusPayload("phone-beleza", "wamid.QA.NO.HOOK", "failed", "UTILITY", time.Now().Unix()))

	qaWaitFor(t, 5*time.Second, "event persisted as failed", func() bool {
		events := qaListDeliveryEvents(t)
		return len(events) == 1 && events[0].CallbackStatus == string(model.CallbackStatusFailed)
	})
}

// Garante que o payload com errors[] da Meta é persistido.
func TestQAStatusFanOutFailedMapsErrors(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQAStatusReceiver(t)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	payload := fmt.Sprintf(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": "phone-beleza"},
					"statuses": [{
						"id": "wamid.QA.FAILED.ERR",
						"status": "failed",
						"timestamp": "%d",
						"recipient_id": "5511988887777",
						"errors": [{"code": 131026, "title": "Message undeliverable"}]
					}]
				}
			}]
		}]
	}`, time.Now().Unix())

	qaPostMetaWebhook(t, srv, payload)
	qaWaitFor(t, 5*time.Second, "failed status callback", func() bool {
		return receiver.Hits() >= 1
	})

	hits := receiver.Received()
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Payload.Status != "failed" {
		t.Errorf("status = %q, want failed", hits[0].Payload.Status)
	}
	if string(hits[0].Payload.Errors) == "[]" || string(hits[0].Payload.Errors) == "" {
		t.Errorf("errors should carry Meta payload, got %s", hits[0].Payload.Errors)
	}
}