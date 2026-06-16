package security

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultMaxMessagesPerMinute = 50
	defaultBlacklistDuration    = 15 * time.Minute
	rateLimitWindow             = time.Minute
)

// ClientChannelStatus representa o estado operacional de um canal WhatsApp vinculado.
type ClientChannelStatus string

const (
	ClientChannelStatusActive         ClientChannelStatus = "ACTIVE"
	ClientChannelStatusSuspendedSpam  ClientChannelStatus = "SUSPENDED_SPAM"
)

// RateLimitResult descreve o resultado de uma tentativa de envio sob rate limit.
type RateLimitResult struct {
	Allowed      bool
	Count        int
	TriggerAlert bool
	Blacklisted  bool
}

type rateLimitEntry struct {
	mu               sync.Mutex
	windowStart      time.Time
	count            int
	blacklistedUntil time.Time
}

// RateLimiter controla volume de envios por minuto e blacklist temporária em memória.
type RateLimiter struct {
	maxPerMinute       int
	blacklistDuration  time.Duration
	entries            sync.Map
}

// NewRateLimiterFromEnv cria um rate limiter lendo MAX_MESSAGES_PER_MINUTE (padrão 50).
func NewRateLimiterFromEnv() *RateLimiter {
	max := defaultMaxMessagesPerMinute
	if raw := os.Getenv("MAX_MESSAGES_PER_MINUTE"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			max = int(parsed)
		}
	}
	return NewRateLimiter(max, defaultBlacklistDuration)
}

// NewRateLimiter cria um rate limiter com limites explícitos (útil em testes).
func NewRateLimiter(maxPerMinute int, blacklistDuration time.Duration) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = defaultMaxMessagesPerMinute
	}
	if blacklistDuration <= 0 {
		blacklistDuration = defaultBlacklistDuration
	}
	return &RateLimiter{
		maxPerMinute:      maxPerMinute,
		blacklistDuration: blacklistDuration,
	}
}

func rateLimitKey(systemID, externalClientID string) string {
	return systemID + "|" + externalClientID
}

func (rl *RateLimiter) getEntry(key string) *rateLimitEntry {
	value, _ := rl.entries.LoadOrStore(key, &rateLimitEntry{})
	return value.(*rateLimitEntry)
}

// IsBlacklisted indica se o par system_id + external_client_id está bloqueado em memória.
func (rl *RateLimiter) IsBlacklisted(systemID, externalClientID string) bool {
	key := rateLimitKey(systemID, externalClientID)
	value, ok := rl.entries.Load(key)
	if !ok {
		return false
	}
	entry := value.(*rateLimitEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return time.Now().Before(entry.blacklistedUntil)
}

// RecordAttempt registra uma tentativa de envio e aplica blacklist ao exceder o limite.
func (rl *RateLimiter) RecordAttempt(systemID, externalClientID string) RateLimitResult {
	now := time.Now()
	entry := rl.getEntry(rateLimitKey(systemID, externalClientID))

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if now.Before(entry.blacklistedUntil) {
		return RateLimitResult{
			Allowed:     false,
			Count:       entry.count,
			Blacklisted: true,
		}
	}

	if entry.blacklistedUntil.IsZero() == false && !now.Before(entry.blacklistedUntil) {
		entry.blacklistedUntil = time.Time{}
		entry.windowStart = time.Time{}
		entry.count = 0
	}

	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= rateLimitWindow {
		entry.windowStart = now
		entry.count = 0
	}

	entry.count++
	count := entry.count

	if count > rl.maxPerMinute {
		entry.blacklistedUntil = now.Add(rl.blacklistDuration)
		return RateLimitResult{
			Allowed:      false,
			Count:        count,
			TriggerAlert: true,
			Blacklisted:  true,
		}
	}

	return RateLimitResult{
		Allowed: true,
		Count:   count,
	}
}

// Unblock remove o bloqueio em memória para o par system_id + external_client_id.
func (rl *RateLimiter) Unblock(systemID, externalClientID string) {
	key := rateLimitKey(systemID, externalClientID)
	value, ok := rl.entries.Load(key)
	if !ok {
		return
	}
	entry := value.(*rateLimitEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.blacklistedUntil = time.Time{}
	entry.windowStart = time.Time{}
	entry.count = 0
}

// MaxPerMinute retorna o limite configurado por minuto.
func (rl *RateLimiter) MaxPerMinute() int {
	return rl.maxPerMinute
}

// BlacklistDuration retorna a duração do bloqueio temporário em memória.
func (rl *RateLimiter) BlacklistDuration() time.Duration {
	return rl.blacklistDuration
}

// FormatRateLimitError monta mensagem JSON-friendly para respostas 429.
func FormatRateLimitError(count, maxPerMinute int) string {
	return fmt.Sprintf("rate limit exceeded: %d messages in the last minute (max %d)", count, maxPerMinute)
}
