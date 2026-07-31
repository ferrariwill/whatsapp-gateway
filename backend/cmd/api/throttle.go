package main

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

type rateLimitCacheEntry struct {
	resolved repository.RateLimitResolved
	expires  time.Time
}

type structuredError struct {
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	Hint       string `json:"hint,omitempty"`
	Scope      string `json:"scope,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (s *server) rateLimitCacheKey(systemID, tenantID string) string {
	return systemID + "|" + tenantID
}

func (s *server) invalidateRateLimitCache(systemID, tenantID string) {
	if s.rateLimitCache != nil {
		s.rateLimitCache.Delete(s.rateLimitCacheKey(systemID, tenantID))
	}
	if s.tokenBucket != nil {
		s.tokenBucket.Reset(security.TenantBucketKey(systemID, tenantID))
		s.tokenBucket.Reset(security.ProductBucketKey(systemID))
	}
}

func (s *server) resolveRateLimitCached(ctx context.Context, systemID, tenantID string) (repository.RateLimitResolved, error) {
	key := s.rateLimitCacheKey(systemID, tenantID)
	if s.rateLimitCache != nil {
		if v, ok := s.rateLimitCache.Load(key); ok {
			entry := v.(rateLimitCacheEntry)
			if time.Now().Before(entry.expires) {
				return entry.resolved, nil
			}
		}
	}
	resolved, err := s.repo.ResolveRateLimitConfig(ctx, systemID, tenantID)
	if err != nil {
		return repository.RateLimitResolved{}, err
	}
	if s.rateLimitCache != nil {
		s.rateLimitCache.Store(key, rateLimitCacheEntry{
			resolved: resolved,
			expires:  time.Now().Add(repository.RateLimitConfigCacheTTL()),
		})
	}
	return resolved, nil
}

// enforceCommercialThrottle applies token-bucket for tenant + product.
// Returns false when the response has already been written (429).
func (s *server) enforceCommercialThrottle(
	w http.ResponseWriter,
	r *http.Request,
	conn *model.WhatsAppConnection,
) bool {
	if s.tokenBucket == nil {
		return true
	}
	resolved, err := s.resolveRateLimitCached(r.Context(), conn.SystemID, conn.TenantID)
	if err != nil {
		log.Printf("resolve rate limit %s/%s: %v", conn.SystemID, conn.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to resolve rate limit"})
		return false
	}

	tenantKey := security.TenantBucketKey(conn.SystemID, conn.TenantID)
	ok, retryAfter := s.tokenBucket.Allow(tenantKey, resolved.TenantCfg)
	if !ok {
		log.Printf("rate_limit_rejected system=%s tenant=%s scope=tenant", conn.SystemID, conn.TenantID)
		writeRateLimited(w, "tenant", retryAfter)
		return false
	}

	productKey := security.ProductBucketKey(conn.SystemID)
	ok, retryAfter = s.tokenBucket.Allow(productKey, resolved.ProductCfg)
	if !ok {
		log.Printf("rate_limit_rejected system=%s tenant=%s scope=product", conn.SystemID, conn.TenantID)
		writeRateLimited(w, "product", retryAfter)
		return false
	}
	return true
}

func writeRateLimited(w http.ResponseWriter, scope string, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, structuredError{
		Error:      "rate limit exceeded",
		Code:       "rate_limited",
		Scope:      scope,
		RetryAfter: secs,
	})
}

func writeOutside24h(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnprocessableEntity, structuredError{
		Error: "outside_24h_window: use an approved template",
		Code:  "outside_24h_window",
		Hint:  "send template or wait for customer inbound",
	})
}

// ensureThrottleFields initializes throttle fields when server is built by hand in tests.
func (s *server) ensureThrottleFields() {
	if s.tokenBucket == nil {
		s.tokenBucket = security.NewTokenBucketLimiter()
	}
	if s.rateLimitCache == nil {
		s.rateLimitCache = &sync.Map{}
	}
}
