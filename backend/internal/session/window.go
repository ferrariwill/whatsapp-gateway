package session

import (
	"strings"
	"time"
	"unicode"
)

const Window = 24 * time.Hour

// NormalizeWAID keep digits only (E.164 without +), matching outbound phone_number style.
func NormalizeWAID(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// InWindow reports whether lastInbound is within the Cloud API 24h customer care window.
func InWindow(lastInbound *time.Time, now time.Time) bool {
	if lastInbound == nil || lastInbound.IsZero() {
		return false
	}
	return !now.Before(*lastInbound) && now.Sub(*lastInbound) <= Window
}
