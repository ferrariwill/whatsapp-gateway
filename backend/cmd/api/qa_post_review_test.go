package main

// Testes dos ajustes pós-code-review do DEV-64: agendamento do GC de
// oauth_state_nonces e teto de tentativas no reclaim de lease órfão.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

func qaInsertOAuthNonce(t *testing.T, systemID, tenantID, hashSeed string, expiresAt time.Time) string {
	t.Helper()

	hash := fmt.Sprintf("%064s", hashSeed)
	_, err := qaDB.Exec(`
		INSERT INTO oauth_state_nonces (nonce_hash, system_id, tenant_id, expires_at)
		VALUES ($1, $2, $3, $4)
	`, hash, systemID, tenantID, expiresAt.UTC())
	if err != nil {
		t.Fatalf("insert oauth state nonce %s: %v", hashSeed, err)
	}
	return hash
}

func qaCountOAuthNonces(t *testing.T) int {
	t.Helper()
	var count int
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM oauth_state_nonces`).Scan(&count); err != nil {
		t.Fatalf("count oauth state nonces: %v", err)
	}
	return count
}

func qaOAuthNonceExists(t *testing.T, hash string) bool {
	t.Helper()
	var exists bool
	err := qaDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM oauth_state_nonces WHERE nonce_hash = $1)`, hash).Scan(&exists)
	if err != nil {
		t.Fatalf("check oauth state nonce: %v", err)
	}
	return exists
}

// TestQAOAuthNonceGCKeepsRecentlyExpiredNonces garante que a coleta respeita a
// janela de retenção: nonce vencido há pouco continua na tabela para que um
// replay tardio bata em "state já utilizado" em vez de sumir sem rastro.
func TestQAOAuthNonceGCKeepsRecentlyExpiredNonces(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	now := time.Now().UTC()
	active := qaInsertOAuthNonce(t, beleza.ID, "salao-1", "active", now.Add(30*time.Minute))
	recent := qaInsertOAuthNonce(t, beleza.ID, "salao-2", "recent", now.Add(-time.Hour))
	stale := qaInsertOAuthNonce(t, beleza.ID, "salao-3", "stale", now.Add(-48*time.Hour))

	removed, err := srv.collectExpiredOAuthStateNonces(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatalf("collect expired nonces: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the nonce expired beyond retention)", removed)
	}
	if qaOAuthNonceExists(t, stale) {
		t.Error("nonce expired 48h ago should have been collected")
	}
	if !qaOAuthNonceExists(t, recent) {
		t.Error("nonce expired 1h ago is inside the 24h retention window and must be kept")
	}
	if !qaOAuthNonceExists(t, active) {
		t.Error("unexpired nonce must never be collected")
	}
}

// TestQAOAuthNonceGCSchedulerCollectsAndStops cobre o agendamento em si: o
// ticker coleta sem intervenção e o cancelamento do contexto encerra a rotina
// (é o mesmo contexto cancelado no shutdown do processo).
func TestQAOAuthNonceGCSchedulerCollectsAndStops(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")

	qaInsertOAuthNonce(t, beleza.ID, "salao-1", "sched1", time.Now().UTC().Add(-48*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runOAuthNonceGC(ctx, oauthNonceGCConfig{
			Interval:  20 * time.Millisecond,
			Retention: time.Hour,
		})
	}()

	qaWaitFor(t, 5*time.Second, "scheduled GC to collect the expired nonce", func() bool {
		return qaCountOAuthNonces(t) == 0
	})

	// O ticker precisa seguir coletando o que aparecer depois da primeira volta.
	qaInsertOAuthNonce(t, beleza.ID, "salao-2", "sched2", time.Now().UTC().Add(-48*time.Hour))
	qaWaitFor(t, 5*time.Second, "scheduled GC to collect a nonce inserted later", func() bool {
		return qaCountOAuthNonces(t) == 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOAuthNonceGC did not return after context cancellation")
	}
}

func TestQAOAuthNonceGCDisabledByZeroInterval(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	hash := qaInsertOAuthNonce(t, beleza.ID, "salao-1", "disabled", time.Now().UTC().Add(-48*time.Hour))

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runOAuthNonceGC(context.Background(), oauthNonceGCConfig{Interval: 0, Retention: time.Hour})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GC with interval 0 should return immediately")
	}
	if !qaOAuthNonceExists(t, hash) {
		t.Error("GC disabled by interval 0 must not delete anything")
	}
}

// TestQAPendingSweepOrphanReclaimStopsAtMaxAttempts fecha o buraco apontado no
// code review: o ramo órfão do claim não filtra por relay_attempts, então uma
// linha cujo processo morre no meio do repasse era reclaimada indefinidamente.
// O teto passou a ser aplicado no sweep — acima dele a linha vira exhausted em
// vez de gerar mais um POST.
func TestQAPendingSweepOrphanReclaimStopsAtMaxAttempts(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.ORPHAN.MAX", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	// Lease órfão já no teto: o claim leva relay_attempts a 6 (> 5).
	_, err := qaDB.Exec(`
		UPDATE message_logs
		SET status = 'relaying',
		    sweep_claimed_at = NOW() - INTERVAL '10 minutes',
		    relay_attempts = 5
		WHERE id = $1
	`, logID)
	if err != nil {
		t.Fatalf("plant orphan claim at max attempts: %v", err)
	}

	result, err := srv.sweepPendingInboundWithClaimTTL(context.Background(), time.Minute, time.Minute, 100, 5)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Claimed != 1 || result.Failed != 1 || result.Relayed != 0 {
		t.Fatalf("claimed=%d failed=%d relayed=%d, want 1/1/0", result.Claimed, result.Failed, result.Relayed)
	}
	if hits := receiver.Hits(); hits != 0 {
		t.Errorf("SaaS deliveries = %d, want 0 — linha acima do teto não deve gerar novo POST", hits)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusFailed) {
		t.Errorf("status = %q, want failed", got)
	}
	if got := qaMessageLogFailureReason(t, logID); got != failureReasonExhausted {
		t.Errorf("failure_reason = %q, want %q", got, failureReasonExhausted)
	}

	// Segundo sweep: a linha terminou, não pode ser reclaimada de novo.
	again, err := srv.sweepPendingInboundWithClaimTTL(context.Background(), time.Minute, time.Minute, 100, 5)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.Claimed != 0 {
		t.Fatalf("second sweep claimed=%d, want 0 — exhausted não reentra no DLQ", again.Claimed)
	}
}

// TestQAPendingSweepOrphanReclaimBelowMaxStillRelays garante que o teto não
// engoliu a recuperação normal de lease órfão.
func TestQAPendingSweepOrphanReclaimBelowMaxStillRelays(t *testing.T) {
	requireQADB(t)

	srv := newQAServer(t, newQAMetaStub(), 100)
	receiver := newQASaaSReceiver(t)
	beleza := createQASystem(t, "Beleza Web", "beleza_web", "")
	conn := createQAConnection(t, beleza, "salao-1", "phone-beleza", "token-beleza", receiver.URL())

	logID := qaInsertPendingInbound(t, beleza.ID, conn.ID, beleza.Slug, "salao-1",
		"wamid.QA.ORPHAN.UNDER", "5511900000000", "text_message", "oi",
		time.Now().UTC().Add(-time.Hour))

	_, err := qaDB.Exec(`
		UPDATE message_logs
		SET status = 'relaying',
		    sweep_claimed_at = NOW() - INTERVAL '10 minutes',
		    relay_attempts = 3
		WHERE id = $1
	`, logID)
	if err != nil {
		t.Fatalf("plant orphan claim below max: %v", err)
	}

	result, err := srv.sweepPendingInboundWithClaimTTL(context.Background(), time.Minute, time.Minute, 100, 5)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Claimed != 1 || result.Relayed != 1 {
		t.Fatalf("claimed=%d relayed=%d, want 1/1", result.Claimed, result.Relayed)
	}
	if got := qaMessageLogStatus(t, logID); got != string(model.MessageStatusSent) {
		t.Errorf("status = %q, want sent", got)
	}
}
