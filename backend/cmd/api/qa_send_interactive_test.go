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
	"github.com/whatsappgetway/gateway/internal/session"
)

func qaPostInteractive(t *testing.T, srv *server, apiKey string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/interactive", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendInteractive)).ServeHTTP(rec, req)
	return rec
}

func TestQAInteractiveSendTwoButtonsAndFanOutReply(t *testing.T) {
	requireQADB(t)

	receiver := newQASaaSReceiver(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)

	sys := createQASystem(t, "Interactive SaaS", uniqueSlug(t, "int"), receiver.URL())
	conn := createQAConnection(t, sys, "tenant-int", "pn-int-a", "tok-int-a", receiver.URL())
	phone := session.NormalizeWAID("5511999000011")
	if err := repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-int", phone, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}

	rec := qaPostInteractive(t, srv, sys.APIKey, map[string]any{
		"tenant_id":    "tenant-int",
		"phone_number": phone,
		"type":         "button",
		"body_text":    "Confirma sua consulta?",
		"buttons": []map[string]string{
			{"id": "CONFIRM", "title": "Confirmar"},
			{"id": "CANCEL", "title": "Cancelar"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp sendInteractiveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MetaMessageID == "" || resp.Status != model.MessageStatusSent {
		t.Fatalf("response=%+v", resp)
	}
	if stub.CallCount() != 1 {
		t.Fatalf("meta calls=%d", stub.CallCount())
	}
	call := stub.Calls()[0]
	if !strings.Contains(string(call.Body), `"type":"interactive"`) {
		t.Fatalf("expected interactive graph body, got %s", call.Body)
	}

	inboundPayload := `{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": "` + conn.PhoneNumberID + `"},
					"messages": [{
						"id": "wamid.inbound.reply.1",
						"from": "` + phone + `",
						"type": "interactive",
						"context": {"id": "` + resp.MetaMessageID + `"},
						"interactive": {
							"type": "button_reply",
							"button_reply": {"id": "CONFIRM", "title": "Confirmar"}
						}
					}]
				}
			}]
		}]
	}`
	if rec := qaPostMetaWebhook(t, srv, inboundPayload); rec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d body=%s", rec.Code, rec.Body.String())
	}

	qaWaitFor(t, 5*time.Second, "fan-out with reply_id", func() bool {
		got := receiver.Received()
		return len(got) == 1 && got[0].ReplyID == "CONFIRM" && got[0].EventType == "button_reply"
	})
	got := receiver.Received()[0]
	if got.ContextMessageID != resp.MetaMessageID {
		t.Fatalf("context_message_id=%q want %q", got.ContextMessageID, resp.MetaMessageID)
	}
	if got.From != phone {
		t.Fatalf("from=%q", got.From)
	}

	// Reentrega Meta com mesmo meta_message_id não duplica efeito.
	if rec := qaPostMetaWebhook(t, srv, inboundPayload); rec.Code != http.StatusOK {
		t.Fatalf("replay webhook status=%d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(receiver.Received()); n != 1 {
		t.Fatalf("dedup failed: got %d payloads", n)
	}
}

func TestQAInteractiveFourButtonsLocal422NoMeta(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Val SaaS", uniqueSlug(t, "val"), "")
	createQAConnection(t, sys, "tenant-v", "pn-val", "tok-val", "")
	phone := "5511999000022"
	_ = repository.NewPostgresRepository(qaDB).UpsertLastInbound(
		context.Background(), sys.ID, "tenant-v", phone, time.Now().UTC(),
	)

	rec := qaPostInteractive(t, srv, sys.APIKey, map[string]any{
		"tenant_id":    "tenant-v",
		"phone_number": phone,
		"type":         "button",
		"body_text":    "ops",
		"buttons": []map[string]string{
			{"id": "a", "title": "A"},
			{"id": "b", "title": "B"},
			{"id": "c", "title": "C"},
			{"id": "d", "title": "D"},
		},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var errBody errorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != "validation_error" {
		t.Fatalf("code=%q body=%s", errBody.Code, rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta must not be called, got %d", stub.CallCount())
	}
}

func TestQAInteractiveReplyIsolatesTenants(t *testing.T) {
	requireQADB(t)

	recvA := newQASaaSReceiver(t)
	recvB := newQASaaSReceiver(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)

	sysA := createQASystem(t, "Sys A", uniqueSlug(t, "iso_a"), recvA.URL())
	sysB := createQASystem(t, "Sys B", uniqueSlug(t, "iso_b"), recvB.URL())
	connA := createQAConnection(t, sysA, "tenant-a", "pn-iso-a", "tok-a", recvA.URL())
	_ = createQAConnection(t, sysB, "tenant-b", "pn-iso-b", "tok-b", recvB.URL())

	phone := "5511999000033"
	inboundPayload := `{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"metadata": {"phone_number_id": "` + connA.PhoneNumberID + `"},
					"messages": [{
						"id": "wamid.iso.reply",
						"from": "` + phone + `",
						"type": "interactive",
						"interactive": {
							"type": "button_reply",
							"button_reply": {"id": "CONFIRM", "title": "Confirmar"}
						}
					}]
				}
			}]
		}]
	}`
	if rec := qaPostMetaWebhook(t, srv, inboundPayload); rec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d", rec.Code)
	}

	qaWaitFor(t, 5*time.Second, "tenant A callback", func() bool {
		return len(recvA.Received()) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(recvB.Received()); n != 0 {
		t.Fatalf("tenant B must not receive tenant A reply, got %d", n)
	}
}

func TestQAInteractiveOutside24hWindow(t *testing.T) {
	requireQADB(t)
	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 1000)
	sys := createQASystem(t, "Win Int", uniqueSlug(t, "wint"), "")
	createQAConnection(t, sys, "tenant-w", "pn-wint", "tok-w", "")

	rec := qaPostInteractive(t, srv, sys.APIKey, map[string]any{
		"tenant_id":    "tenant-w",
		"phone_number": "5511999000044",
		"type":         "button",
		"body_text":    "fora da janela",
		"buttons": []map[string]string{
			{"id": "CONFIRM", "title": "Confirmar"},
		},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside_24h_window") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if stub.CallCount() != 0 {
		t.Fatalf("meta must not be called outside window")
	}
}
