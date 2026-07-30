package main

// Testes do hardening do inbound (DEV-49):
//   - reconciliação das linhas presas em pending (sweep);
//   - pool de repasse com concorrência limitada, backpressure e drenagem;
//   - amostragem do log nos caminhos de rejeição dos webhooks públicos.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/security"
)

// ----------------------------------------------------------------------------
// Sweep de reconciliação
// ----------------------------------------------------------------------------

// TestQAPendingSweepReprocessesStaleInbound cobre o buraco central: pending era
// escrito e nunca lido. Uma linha órfã de restart precisa ser repassada ao SaaS
// e fechada; sem destino, precisa virar failed em vez de ficar pending eterno.
func TestQAPendingSweepReprocessesStaleInbound(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	semDestino := createQASystem(t, "Sem Webhook", "sem_webhook", "")
	connSemDestino := createQAConnection(t, semDestino, "salao-9", "phone-sem-webhook", "token-9", "")

	stale := time.Now().UTC().Add(-30 * time.Minute)
	recent := time.Now().UTC()

	relayable := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.1", "5511900000000", "text_message", "quero remarcar", stale)
	orphan := qaInsertPendingInbound(t, semDestino.ID, connSemDestino.ID, semDestino.Slug, "salao-9",
		"wamid.QA.PENDING.2", "5511900000001", "text_message", "oi", stale)
	tooRecent := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.3", "5511900000002", "text_message", "ainda em voo", recent)

	result, err := srv.sweepPendingInbound(context.Background(), 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep pending inbound: %v", err)
	}

	if result.Claimed != 2 {
		t.Fatalf("claimed = %d, want 2 (a linha recente não pode entrar no lote)", result.Claimed)
	}
	if result.Relayed != 1 || result.Failed != 1 {
		t.Errorf("relayed = %d, failed = %d, want 1 and 1", result.Relayed, result.Failed)
	}

	if got := qaMessageLogStatus(t, relayable); got != string(model.MessageStatusSent) {
		t.Errorf("linha reprocessada: status = %q, want sent", got)
	}
	if got := qaMessageLogStatus(t, orphan); got != string(model.MessageStatusFailed) {
		t.Errorf("linha sem webhook_url: status = %q, want failed", got)
	}
	if got := qaMessageLogStatus(t, tooRecent); got != string(model.MessageStatusPending) {
		t.Errorf("linha recente: status = %q, want pending (pode estar em voo)", got)
	}

	received := receiver.Received()
	if len(received) != 1 {
		t.Fatalf("SaaS recebeu %d repasses, want 1", len(received))
	}
	if received[0].TenantID != "salao-1" || received[0].SystemID != beleza.ID {
		t.Errorf("repasse perdeu a identidade do tenant: %+v", received[0])
	}
	if received[0].Text != "quero remarcar" {
		t.Errorf("repasse text = %q, want o received_content da linha", received[0].Text)
	}
	if !received[0].Replay {
		t.Error("repasse do sweep deve carregar replay=true")
	}
	if received[0].MetaMessageID != "wamid.QA.PENDING.1" {
		t.Errorf("meta_message_id = %q, want wamid.QA.PENDING.1 (chave idempotente)", received[0].MetaMessageID)
	}
}

// TestQAPendingSweepFailsWhenSaaSKeepsFailing garante que o sweep não deixa a
// linha em pending quando o SaaS continua fora: ela vira failed e sai do radar.
func TestQAPendingSweepFailsWhenSaaSKeepsFailing(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	receiver.status.Store(http.StatusInternalServerError)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.FAIL", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep pending inbound: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("failed = %d, want 1", result.Failed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusFailed) {
		t.Errorf("status = %q, want failed", got)
	}
	if attempts := receiver.Hits(); attempts < 2 {
		t.Errorf("tentativas de repasse = %d, want pelo menos 2 (backoff)", attempts)
	}
}

// TestQAPendingSweepDoesNotOverwriteClosedRows fixa a guarda de concorrência:
// o sweep só fecha linhas que ainda estão em pending.
func TestQAPendingSweepDoesNotOverwriteClosedRows(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.CLOSED", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	if err := srv.repo.UpdateMessageLogStatus(context.Background(), logID, model.MessageStatusDelivered); err != nil {
		t.Fatalf("close row before sweep: %v", err)
	}

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep pending inbound: %v", err)
	}
	if result.Claimed != 0 {
		t.Errorf("claimed = %d, want 0 — linha já fechada não é candidata", result.Claimed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusDelivered) {
		t.Errorf("status = %q, want delivered (o sweep não pode sobrescrever)", got)
	}
	if hits := receiver.Hits(); hits != 0 {
		t.Errorf("SaaS recebeu %d repasses de uma linha já fechada", hits)
	}
}

// TestQAPendingSweepConcurrentClaimPostsOnce prova o claim atômico: dois
// sweepers concorrentes claimam a mesma linha stale e exatamente um POST chega
// ao SaaS. Sem o claim-antes-do-POST, ambos fariam forwardWebhookWithRetry.
func TestQAPendingSweepConcurrentClaimPostsOnce(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srvA := newQAServer(t, stub, 100)
	srvB := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	// Atraso no SaaS amplia a janela em que o segundo sweeper poderia POSTAR
	// se o claim não fosse atômico antes do HTTP.
	receiver.delayMs.Store(400)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.RACE", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	var (
		wg      sync.WaitGroup
		results [2]pendingSweepResult
		errs    [2]error
	)
	start := make(chan struct{})
	for i, srv := range []*server{srvA, srvB} {
		wg.Add(1)
		go func(i int, srv *server) {
			defer wg.Done()
			<-start
			results[i], errs[i] = srv.sweepPendingInbound(context.Background(), time.Minute, 100)
		}(i, srv)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("sweeper %d: %v", i, err)
		}
	}

	claimed := results[0].Claimed + results[1].Claimed
	if claimed != 1 {
		t.Fatalf("claimed total = %d, want 1 (um sweeper claima, o outro sai vazio)", claimed)
	}
	relayed := results[0].Relayed + results[1].Relayed
	if relayed != 1 {
		t.Fatalf("relayed total = %d, want 1", relayed)
	}

	qaWaitFor(t, 5*time.Second, "exactly one SaaS delivery", func() bool {
		return receiver.Hits() >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if hits := receiver.Hits(); hits != 1 {
		t.Fatalf("SaaS deliveries = %d, want exatamente 1 sob dois sweepers concorrentes", hits)
	}
	if got := receiver.Received()[0].MetaMessageID; got != "wamid.QA.PENDING.RACE" {
		t.Errorf("meta_message_id = %q, want wamid.QA.PENDING.RACE", got)
	}
}

// TestQAPendingSweepReclaimsOrphanedClaim garante que um claim em relaying cujo
// lease expirou (processo morto no meio do POST) volta a ser elegível.
func TestQAPendingSweepReclaimsOrphanedClaim(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.PENDING.ORPHAN", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	// Simula um claim órfão: status=relaying com sweep_claimed_at antigo.
	_, err := qaDB.Exec(`
		UPDATE message_logs
		SET status = 'relaying', sweep_claimed_at = NOW() - INTERVAL '10 minutes'
		WHERE id = $1
	`, logID)
	if err != nil {
		t.Fatalf("plant orphan claim: %v", err)
	}

	result, err := srv.sweepPendingInboundWithClaimTTL(context.Background(), time.Minute, time.Minute, 100, defaultDLQMaxAttempts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Claimed != 1 || result.Relayed != 1 {
		t.Fatalf("claimed=%d relayed=%d, want 1 e 1 (orphan reclaim)", result.Claimed, result.Relayed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusSent) {
		t.Errorf("status = %q, want sent", got)
	}
	if hits := receiver.Hits(); hits != 1 {
		t.Errorf("SaaS deliveries = %d, want 1", hits)
	}
}

// TestQAPendingSweepFailsWithoutTenantWebhook covers the removal of the shared
// MOTHER_SYSTEM_WEBHOOK_URL fallback: connection+system without webhook_url
// must fail permanently (no cross-tenant destination).
func TestQAPendingSweepFailsWithoutTenantWebhook(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	fallback := newQASaaSReceiver(t)
	t.Setenv("MOTHER_SYSTEM_WEBHOOK_URL", fallback.URL())

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-sem-webhook", "phone-beleza", "token-beleza", "")

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-sem-webhook",
		"wamid.QA.PENDING.MOTHER", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep pending inbound: %v", err)
	}
	if result.Failed != 1 || result.Relayed != 0 {
		t.Fatalf("failed=%d relayed=%d, want failed=1 relayed=0 (no MOTHER fallback)", result.Failed, result.Relayed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusFailed) {
		t.Errorf("status = %q, want failed", got)
	}
	if hits := fallback.Hits(); hits != 0 {
		t.Fatalf("MOTHER fallback received %d posts — shared destination must not be used", hits)
	}
	if reason := qaMessageLogFailureReason(t, logID); reason != "permanent" {
		t.Errorf("failure_reason = %q, want permanent", reason)
	}
}

// TestQAInboundRelayCarriesMetaMessageIDInBothFormats garante a chave
// idempotente nos dois contratos em tempo real e no replay do sweep.
func TestQAInboundRelayCarriesMetaMessageIDInBothFormats(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)

	beleza := createQASystem(t, "Beleza Web", "beleza_web", receiver.URL())
	// Conexão sem webhook próprio: unificado e legado resolvem para systems.webhook_url.
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", "")
	createQAClientChannel(t, beleza, "salao-1", "phone-beleza")

	const unifiedID = "wamid.QA.KEY.UNIFIED"
	const legacyID = "wamid.QA.KEY.LEGACY"

	if rec := qaPostMetaWebhook(t, srv, qaInboundTextPayloadWithID("phone-beleza", "5511977776666", "oi", unifiedID)); rec.Code != http.StatusOK {
		t.Fatalf("unified status = %d", rec.Code)
	}
	qaWaitFor(t, 5*time.Second, "unified delivery", func() bool {
		return len(receiver.Received()) >= 1
	})
	if got := receiver.Received()[0].MetaMessageID; got != unifiedID {
		t.Errorf("unified meta_message_id = %q, want %q", got, unifiedID)
	}

	payload := qaInboundButtonPayloadWithID("phone-beleza", "5511900000000", "APPT_CONFIRM", legacyID)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/meta/phone-beleza", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload(qaMetaAppSecret(t), []byte(payload)))
	req.SetPathValue("phone_number_id", "phone-beleza")
	rec := httptest.NewRecorder()
	srv.handleMetaWebhookEvent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy status = %d", rec.Code)
	}
	qaWaitFor(t, 5*time.Second, "legacy delivery", func() bool {
		return receiver.Hits() >= 2
	})
	foundLegacy := false
	for _, got := range receiver.Received() {
		if got.MetaMessageID == legacyID {
			foundLegacy = true
			break
		}
	}
	if !foundLegacy {
		t.Errorf("legacy meta_message_id %q not found in %+v", legacyID, receiver.Received())
	}

	// Replay do sweep deve carregar a mesma chave.
	sweepRecv := newQASaaSReceiver(t)
	conn := createQAConnection(t, createQASystem(t, "Replay Sys", "replay_sys", ""), "t1", "phone-replay", "tok", sweepRecv.URL())
	qaInsertPendingInbound(t, conn.SystemID, conn.ID, "replay_sys", "t1",
		"wamid.QA.KEY.REPLAY", "5511900000000", "text_message", "replay-me",
		time.Now().UTC().Add(-time.Hour))
	if _, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	qaWaitFor(t, 5*time.Second, "sweep delivery", func() bool {
		return len(sweepRecv.Received()) >= 1
	})
	replay := sweepRecv.Received()[0]
	if replay.MetaMessageID != "wamid.QA.KEY.REPLAY" {
		t.Errorf("replay meta_message_id = %q", replay.MetaMessageID)
	}
	if !replay.Replay {
		t.Error("replay payload must set replay=true")
	}
}

// ----------------------------------------------------------------------------
// Pool de repasse: limite, backpressure e drenagem
// ----------------------------------------------------------------------------

func TestRelayPoolBoundsConcurrency(t *testing.T) {
	pool := newRelayPool(relayPoolConfig{Workers: 4, QueueSize: 64, SubmitTimeout: time.Second, TaskTimeout: 5 * time.Second})

	var running, peak atomic.Int64
	var wg sync.WaitGroup

	for range 40 {
		wg.Add(1)
		if err := pool.Submit(func(context.Context) {
			defer wg.Done()
			current := running.Add(1)
			for {
				observed := peak.Load()
				if current <= observed || peak.CompareAndSwap(observed, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			running.Add(-1)
		}); err != nil {
			wg.Done()
			t.Fatalf("submit: %v", err)
		}
	}
	wg.Wait()

	if got := peak.Load(); got > 4 {
		t.Errorf("concorrência máxima = %d, want <= 4 workers", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestRelayPoolDrainsQueuedTasksOnShutdown(t *testing.T) {
	pool := newRelayPool(relayPoolConfig{Workers: 2, QueueSize: 128, SubmitTimeout: time.Second, TaskTimeout: 5 * time.Second})

	const tasks = 50
	var done atomic.Int64
	for range tasks {
		if err := pool.Submit(func(context.Context) {
			time.Sleep(5 * time.Millisecond)
			done.Add(1)
		}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := done.Load(); got != tasks {
		t.Errorf("tarefas concluídas = %d, want %d — o shutdown descartou repasses admitidos", got, tasks)
	}
	if err := pool.Submit(func(context.Context) {}); !errors.Is(err, ErrRelayPoolClosed) {
		t.Errorf("submit após shutdown = %v, want ErrRelayPoolClosed", err)
	}
}

func TestRelayPoolRejectsWhenSaturated(t *testing.T) {
	pool := newRelayPool(relayPoolConfig{
		Workers:       1,
		QueueSize:     1,
		SubmitTimeout: 50 * time.Millisecond,
		TaskTimeout:   5 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
	})

	release := make(chan struct{})
	blocking := func(context.Context) { <-release }

	// Ocupa o único worker e a única vaga da fila.
	if err := pool.Submit(blocking); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	if err := pool.Submit(blocking); err != nil {
		t.Fatalf("submit 2: %v", err)
	}

	var saturated error
	for range 5 {
		if err := pool.Submit(blocking); err != nil {
			saturated = err
			break
		}
	}
	if !errors.Is(saturated, ErrRelayPoolSaturated) {
		t.Fatalf("submit com fila cheia = %v, want ErrRelayPoolSaturated", saturated)
	}

	close(release)
}

// TestQAWebhookRejectsWithServiceUnavailableWhenSaturated prova a backpressure
// ponta a ponta: saturado, o gateway devolve 503 (Meta reentrega) em vez de
// aceitar o evento e criar uma goroutine por requisição.
func TestQAWebhookRejectsWithServiceUnavailableWhenSaturated(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServerWithRelay(t, stub, 100, relayPoolConfig{
		Workers:       1,
		QueueSize:     1,
		SubmitTimeout: 50 * time.Millisecond,
		TaskTimeout:   30 * time.Second,
	})

	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	receiver := newQASaaSReceiver(t)
	receiver.delayMs.Store(3000)
	createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	var rejected int
	for i := range 6 {
		rec := qaPostMetaWebhook(t, srv,
			qaInboundTextPayload("phone-beleza", "5511977776666", fmt.Sprintf("burst-%d", i)))
		switch rec.Code {
		case http.StatusOK:
		case http.StatusServiceUnavailable:
			rejected++
		default:
			t.Fatalf("event %d: status = %d, want 200 ou 503 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}

	if rejected == 0 {
		t.Fatal("nenhuma requisição recebeu 503 — a admissão não aplicou backpressure")
	}
	t.Logf("backpressure: %d de 6 eventos recusados com 503 por pool saturado", rejected)
}

// ----------------------------------------------------------------------------
// Amostragem do log de rejeição
// ----------------------------------------------------------------------------

func TestLogSamplerSuppressesRepeatedLines(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })

	sampler := newLogSampler(200 * time.Millisecond)
	for range 100 {
		sampler.Printf("webhook rejected for %s", "phone-x")
	}

	if lines := strings.Count(captured.String(), "webhook rejected"); lines != 1 {
		t.Fatalf("linhas emitidas = %d, want 1 para 100 rejeições no mesmo intervalo", lines)
	}

	// Passado o intervalo, a próxima linha reporta quantas foram suprimidas.
	time.Sleep(250 * time.Millisecond)
	sampler.Printf("webhook rejected for %s", "phone-x")

	if !strings.Contains(captured.String(), "+99 similar suppressed") {
		t.Errorf("a linha seguinte deve reportar as 99 suprimidas, got:\n%s", captured.String())
	}
}

// TestQALegacyMetaWebhookRejectionLogIsSampled garante que o caminho 401 do
// endpoint público usa o sampler: 50 requisições forjadas não geram 50 linhas.
func TestQALegacyMetaWebhookRejectionLogIsSampled(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	beleza := createQASystem(t, "Beleza Legacy", "beleza_legacy", "")
	createQAClientChannel(t, beleza, "salao-1", "phone-legacy")

	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })

	payload := qaInboundButtonPayload("phone-legacy", "5511900000000", "APPT_CONFIRM")
	for range 50 {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/meta/phone-legacy", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload("wrong-secret", []byte(payload)))
		req.SetPathValue("phone_number_id", "phone-legacy")

		rec := httptest.NewRecorder()
		srv.handleMetaWebhookEvent(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}

	if lines := strings.Count(captured.String(), "invalid or missing X-Hub-Signature-256"); lines > 1 {
		t.Errorf("linhas de log = %d para 50 rejeições, want no máximo 1 por intervalo", lines)
	}
}

// TestQAFailedInboundDLQRetriesTransientThenSucceeds reprocessa failed transitório
// após next_attempt_at, preservando meta_message_id e sem POST duplicado.
func TestQAFailedInboundDLQRetriesTransientThenSucceeds(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)

	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.DLQ.1", "5511900000000", "text_message", "retry-me",
		time.Now().UTC().Add(-time.Hour))

	_, err := qaDB.Exec(`
		UPDATE message_logs
		SET status = 'failed',
		    failure_reason = 'transient',
		    relay_attempts = 1,
		    next_attempt_at = NOW() - INTERVAL '1 second',
		    inbound_payload = $2::jsonb
		WHERE id = $1
	`, logID, `{"id":"wamid.QA.DLQ.1","from":"5511900000000","text":"retry-me","event_type":"text_message"}`)
	if err != nil {
		t.Fatalf("plant failed row: %v", err)
	}

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Claimed != 1 || result.Relayed != 1 {
		t.Fatalf("claimed=%d relayed=%d, want 1/1", result.Claimed, result.Relayed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusSent) {
		t.Errorf("status = %q, want sent", got)
	}
	received := receiver.Received()
	if len(received) != 1 || received[0].MetaMessageID != "wamid.QA.DLQ.1" || !received[0].Replay {
		t.Fatalf("unexpected delivery: %+v", received)
	}
}

// TestQAFailedInboundDLQSkipsPermanentAndFutureAttempt cobre classificação permanente
// e next_attempt_at no futuro.
func TestQAFailedInboundDLQSkipsPermanentAndFutureAttempt(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	permanentID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.DLQ.PERM", "5511900000000", "text_message", "nope",
		time.Now().UTC().Add(-time.Hour))
	futureID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.DLQ.FUT", "5511900000001", "text_message", "later",
		time.Now().UTC().Add(-time.Hour))

	_, err := qaDB.Exec(`UPDATE message_logs SET status='failed', failure_reason='permanent', relay_attempts=1 WHERE id=$1`, permanentID)
	if err != nil {
		t.Fatalf("plant permanent: %v", err)
	}
	_, err = qaDB.Exec(`
		UPDATE message_logs
		SET status='failed', failure_reason='transient', relay_attempts=1,
		    next_attempt_at = NOW() + INTERVAL '1 hour'
		WHERE id=$1
	`, futureID)
	if err != nil {
		t.Fatalf("plant future: %v", err)
	}

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Claimed != 0 {
		t.Fatalf("claimed=%d, want 0", result.Claimed)
	}
	if hits := receiver.Hits(); hits != 0 {
		t.Fatalf("SaaS hits=%d, want 0", hits)
	}
}

// TestQAFailedInboundDLQPermanentHTTPDoesNotReschedule marca 400 como permanente.
func TestQAFailedInboundDLQPermanentHTTPDoesNotReschedule(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQASaaSReceiver(t)
	receiver.status.Store(http.StatusBadRequest)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.DLQ.400", "5511900000000", "text_message", "bad",
		time.Now().UTC().Add(-time.Hour))

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("failed=%d, want 1", result.Failed)
	}
	if reason := qaMessageLogFailureReason(t, logID); reason != "permanent" {
		t.Errorf("failure_reason=%q, want permanent", reason)
	}

	// Segunda passagem não reclaima permanente.
	result2, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if result2.Claimed != 0 {
		t.Fatalf("permanent reclaimed: claimed=%d", result2.Claimed)
	}
}

// TestQAPendingSweepReplayPreservesMediaPayload restaura media de inbound_payload.
func TestQAPendingSweepReplayPreservesMediaPayload(t *testing.T) {
	requireQADB(t)

	stub := newQAMetaStub()
	srv := newQAServer(t, stub, 100)
	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.DLQ.IMG", "5511900000000", "image_message", "legenda",
		time.Now().UTC().Add(-time.Hour))
	_, err := qaDB.Exec(`
		UPDATE message_logs SET inbound_payload = $2::jsonb WHERE id = $1
	`, logID, `{"id":"wamid.QA.DLQ.IMG","from":"5511900000000","text":"legenda","event_type":"image_message","media":{"id":"media-replay","mime_type":"image/png"}}`)
	if err != nil {
		t.Fatalf("set payload: %v", err)
	}

	result, err := srv.sweepPendingInbound(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Relayed != 1 {
		t.Fatalf("relayed=%d, want 1", result.Relayed)
	}
	got := receiver.Received()[0]
	if got.Media == nil || got.Media.ID != "media-replay" || got.EventType != "image_message" {
		t.Fatalf("replay lost media: %+v", got)
	}
}
