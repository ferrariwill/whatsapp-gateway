package security

import (
	"sync"
	"testing"
	"time"
)

func TestRateLimiterBlocksAfterMaxPerMinute(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	systemID := "sys-1"
	clientID := "client-1"

	for i := 1; i <= 3; i++ {
		result := rl.RecordAttempt(systemID, clientID)
		if !result.Allowed {
			t.Fatalf("attempt %d should be allowed, got blocked (count=%d)", i, result.Count)
		}
	}

	result := rl.RecordAttempt(systemID, clientID)
	if result.Allowed {
		t.Fatal("fourth attempt should be blocked")
	}
	if !result.TriggerAlert {
		t.Fatal("fourth attempt should trigger alert")
	}
	if !rl.IsBlacklisted(systemID, clientID) {
		t.Fatal("client should be blacklisted in memory")
	}
}

func TestRateLimiterUnblockClearsBlacklist(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)

	result := rl.RecordAttempt("sys", "client")
	if !result.Allowed {
		t.Fatal("first attempt should be allowed")
	}

	result = rl.RecordAttempt("sys", "client")
	if result.Allowed {
		t.Fatal("second attempt should be blocked")
	}

	rl.Unblock("sys", "client")
	if rl.IsBlacklisted("sys", "client") {
		t.Fatal("client should not be blacklisted after unblock")
	}

	result = rl.RecordAttempt("sys", "client")
	if !result.Allowed {
		t.Fatal("attempt after unblock should be allowed")
	}
}

func TestRateLimiterConcurrentAttempts(t *testing.T) {
	rl := NewRateLimiter(100, time.Minute)
	const workers = 32
	const attempts = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for range workers {
		go func() {
			defer wg.Done()
			for range attempts {
				rl.RecordAttempt("sys", "client")
			}
		}()
	}

	wg.Wait()
}

func TestNewRateLimiterFromEnvDefault(t *testing.T) {
	t.Setenv("MAX_MESSAGES_PER_MINUTE", "")
	rl := NewRateLimiterFromEnv()
	if rl.MaxPerMinute() != defaultMaxMessagesPerMinute {
		t.Fatalf("expected default %d, got %d", defaultMaxMessagesPerMinute, rl.MaxPerMinute())
	}
}
