package security

import (
	"math"
	"sync"
	"time"
)

// BucketConfig describes a token-bucket throttle (commercial layer, not spam blacklist).
type BucketConfig struct {
	RPS   float64
	Burst int
}

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// TokenBucketLimiter is an in-memory multi-key token bucket.
type TokenBucketLimiter struct {
	buckets sync.Map // string -> *tokenBucket
}

func NewTokenBucketLimiter() *TokenBucketLimiter {
	return &TokenBucketLimiter{}
}

// Allow consumes one token for key under cfg. retryAfter is meaningful when allowed=false.
func (l *TokenBucketLimiter) Allow(key string, cfg BucketConfig) (allowed bool, retryAfter time.Duration) {
	if cfg.RPS <= 0 || cfg.Burst <= 0 {
		return false, time.Second
	}
	value, _ := l.buckets.LoadOrStore(key, &tokenBucket{})
	b := value.(*tokenBucket)

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if b.lastRefill.IsZero() {
		b.tokens = float64(cfg.Burst)
		b.lastRefill = now
	} else {
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.tokens = math.Min(float64(cfg.Burst), b.tokens+elapsed*cfg.RPS)
		b.lastRefill = now
	}

	if b.tokens < 1 {
		needed := 1 - b.tokens
		secs := needed / cfg.RPS
		retryAfter = time.Duration(math.Ceil(secs*1000)) * time.Millisecond
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return false, retryAfter
	}

	b.tokens--
	return true, 0
}

// Reset clears the bucket for key (used by tests / admin invalidation of in-memory state).
func (l *TokenBucketLimiter) Reset(key string) {
	l.buckets.Delete(key)
}

func TenantBucketKey(systemID, tenantID string) string {
	return "tenant:" + systemID + ":" + tenantID
}

func ProductBucketKey(systemID string) string {
	return "product:" + systemID
}
