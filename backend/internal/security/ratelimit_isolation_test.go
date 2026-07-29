package security

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimiterQuotaIsolatedPerSystem garante que o mesmo tenant_id em duas
// aplicações mãe diferentes não compartilha cota nem bloqueio.
func TestRateLimiterQuotaIsolatedPerSystem(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)

	for i := 1; i <= 2; i++ {
		if result := rl.RecordAttempt("system-A", "tenant-1"); !result.Allowed {
			t.Fatalf("system-A attempt %d should be allowed (count=%d)", i, result.Count)
		}
	}
	if result := rl.RecordAttempt("system-A", "tenant-1"); result.Allowed {
		t.Fatal("system-A tenant-1 should be blocked after exceeding the limit")
	}
	if !rl.IsBlacklisted("system-A", "tenant-1") {
		t.Fatal("system-A tenant-1 should be blacklisted")
	}

	if rl.IsBlacklisted("system-B", "tenant-1") {
		t.Fatal("multi-tenant leak: tenant-1 of system-B was blacklisted by system-A")
	}
	result := rl.RecordAttempt("system-B", "tenant-1")
	if !result.Allowed || result.Count != 1 {
		t.Fatalf("multi-tenant leak: system-B tenant-1 should start a fresh window, got allowed=%v count=%d", result.Allowed, result.Count)
	}
}

// TestRateLimiterQuotaIsolatedPerTenant garante que tenants distintos da mesma
// aplicação mãe têm janelas independentes.
func TestRateLimiterQuotaIsolatedPerTenant(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)

	if !rl.RecordAttempt("system-A", "tenant-1").Allowed {
		t.Fatal("first attempt for tenant-1 should be allowed")
	}
	if rl.RecordAttempt("system-A", "tenant-1").Allowed {
		t.Fatal("second attempt for tenant-1 should be blocked")
	}

	if rl.IsBlacklisted("system-A", "tenant-2") {
		t.Fatal("multi-tenant leak: tenant-2 blacklisted by tenant-1")
	}
	if !rl.RecordAttempt("system-A", "tenant-2").Allowed {
		t.Fatal("multi-tenant leak: tenant-2 should have its own quota")
	}
}

// TestRateLimiterKeyDelimiterIsNotEscaped documenta que a chave em memória é a
// concatenação crua system_id + "|" + tenant_id: um tenant_id contendo "|"
// colide com outro par cujo system_id termine no mesmo prefixo. Hoje é
// inalcançável porque system_id é sempre UUID vindo do banco — o teste existe
// para travar essa premissa: se algum dia a chave passar a aceitar slug ou
// entrada do SaaS no lugar do UUID, este teste vira uma falha de isolamento.
func TestRateLimiterKeyDelimiterIsNotEscaped(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)

	if !rl.RecordAttempt("system-A", "b|tenant").Allowed {
		t.Fatal("first attempt should be allowed")
	}
	if rl.RecordAttempt("system-A", "b|tenant").Allowed {
		t.Fatal("second attempt should be blocked")
	}

	if !rl.IsBlacklisted("system-A|b", "tenant") {
		t.Fatal("key composition changed: system-A|b + tenant no longer collides — escaping was added, update the QA report")
	}
}

// TestRateLimiterExactQuotaUnderMassiveConcurrency valida ausência de lost
// update no contador sob concorrência: nem mais nem menos que o limite passa.
func TestRateLimiterExactQuotaUnderMassiveConcurrency(t *testing.T) {
	const (
		workers          = 40
		attemptsPerWorke = 5
		perKeyQuota      = workers * attemptsPerWorke
	)

	keys := [][2]string{
		{"system-A", "tenant-1"},
		{"system-A", "tenant-2"},
		{"system-B", "tenant-1"},
		{"system-B", "tenant-2"},
	}

	rl := NewRateLimiter(perKeyQuota, time.Minute)

	allowed := make([]atomic.Int64, len(keys))
	var wg sync.WaitGroup
	start := make(chan struct{})

	for keyIdx, key := range keys {
		for range workers {
			wg.Add(1)
			go func(keyIdx int, systemID, tenantID string) {
				defer wg.Done()
				<-start
				for range attemptsPerWorke {
					if rl.RecordAttempt(systemID, tenantID).Allowed {
						allowed[keyIdx].Add(1)
					}
				}
			}(keyIdx, key[0], key[1])
		}
	}

	close(start)
	wg.Wait()

	for keyIdx, key := range keys {
		got := allowed[keyIdx].Load()
		if got != perKeyQuota {
			t.Errorf(
				"%s/%s: expected exactly %d allowed attempts, got %d (lost or duplicated counter updates)",
				key[0], key[1], perKeyQuota, got,
			)
		}
		if result := rl.RecordAttempt(key[0], key[1]); result.Allowed {
			t.Errorf("%s/%s: attempt %d should be blocked, quota was not enforced", key[0], key[1], perKeyQuota+1)
		}
	}
}

// TestRateLimiterConcurrentReadWriteIsolation exercita RecordAttempt,
// IsBlacklisted e Unblock em paralelo sobre as mesmas chaves (rodar com -race).
func TestRateLimiterConcurrentReadWriteIsolation(t *testing.T) {
	rl := NewRateLimiter(5, 50*time.Millisecond)

	const workers = 24
	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			systemID := fmt.Sprintf("system-%d", w%3)
			tenantID := fmt.Sprintf("tenant-%d", w%4)
			for time.Now().Before(deadline) {
				switch w % 3 {
				case 0:
					rl.RecordAttempt(systemID, tenantID)
				case 1:
					rl.IsBlacklisted(systemID, tenantID)
				default:
					rl.Unblock(systemID, tenantID)
				}
			}
		}(w)
	}

	wg.Wait()
}

// TestRateLimiterBlacklistExpiresAndResetsWindow garante que o bloqueio
// temporário expira e a janela reinicia limpa (sem tenant preso para sempre).
func TestRateLimiterBlacklistExpiresAndResetsWindow(t *testing.T) {
	rl := NewRateLimiter(1, 80*time.Millisecond)

	if !rl.RecordAttempt("system-A", "tenant-1").Allowed {
		t.Fatal("first attempt should be allowed")
	}
	result := rl.RecordAttempt("system-A", "tenant-1")
	if result.Allowed || !result.TriggerAlert {
		t.Fatalf("second attempt should be blocked with alert, got %+v", result)
	}

	time.Sleep(120 * time.Millisecond)

	if rl.IsBlacklisted("system-A", "tenant-1") {
		t.Fatal("blacklist should have expired")
	}
	result = rl.RecordAttempt("system-A", "tenant-1")
	if !result.Allowed || result.Count != 1 {
		t.Fatalf("window should restart clean after blacklist expiry, got %+v", result)
	}
}

// TestRateLimiterEntriesAreNeverEvicted documenta o crescimento da sync.Map:
// nenhuma entrada é removida, então o footprint é limitado pelo número de pares
// (system_id, tenant_id) já vistos desde o boot do processo.
func TestRateLimiterEntriesAreNeverEvicted(t *testing.T) {
	rl := NewRateLimiter(1, time.Millisecond)

	const distinctKeys = 2000
	for i := range distinctKeys {
		rl.RecordAttempt("system-A", fmt.Sprintf("tenant-%d", i))
	}

	time.Sleep(5 * time.Millisecond)

	entries := 0
	rl.entries.Range(func(_, _ any) bool {
		entries++
		return true
	})

	if entries != distinctKeys {
		t.Fatalf("expected %d retained entries, got %d", distinctKeys, entries)
	}
}
