package session

import (
	"testing"
	"time"
)

func TestNormalizeWAID(t *testing.T) {
	if got := NormalizeWAID("+55 (11) 98888-7777"); got != "5511988887777" {
		t.Fatalf("got %q", got)
	}
}

func TestInWindow(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	within := now.Add(-23 * time.Hour)
	if !InWindow(&within, now) {
		t.Fatal("expected inside")
	}
	outside := now.Add(-25 * time.Hour)
	if InWindow(&outside, now) {
		t.Fatal("expected outside")
	}
}
