package security

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenBucketBurstAndIsolation(t *testing.T) {
	l := NewTokenBucketLimiter()
	cfg := BucketConfig{RPS: 1, Burst: 3}
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(TenantBucketKey("s1", "a"), cfg); !ok {
			t.Fatalf("burst attempt %d", i+1)
		}
	}
	if ok, _ := l.Allow(TenantBucketKey("s1", "a"), cfg); ok {
		t.Fatal("expected deny")
	}
	if ok, _ := l.Allow(TenantBucketKey("s1", "b"), cfg); !ok {
		t.Fatal("tenant B isolated")
	}
}

func TestTokenBucketExactBurstUnderConcurrency(t *testing.T) {
	l := NewTokenBucketLimiter()
	cfg := BucketConfig{RPS: 0.001, Burst: 50}
	key := TenantBucketKey("sys", "t")
	var allowed atomic.Int64
	var wg sync.WaitGroup
	n := 200
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow(key, cfg); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 50 {
		t.Fatalf("allowed=%d", got)
	}
	_ = time.Second
}
