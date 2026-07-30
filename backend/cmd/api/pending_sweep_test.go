package main

import (
	"testing"
	"time"
)

// TestDLQBackoffJitterDoesNotCollapseOnFirstAttempt fixa o piso do jitter em
// 0.5 × delay. Com o piso em base, na primeira tentativa (exp == base) todo
// sorteio abaixo de 1.0 devolvia exatamente base — metade dos replays
// reagendava no mesmo instante, que é o efeito manada que o jitter evita.
func TestDLQBackoffJitterDoesNotCollapseOnFirstAttempt(t *testing.T) {
	t.Setenv("INBOUND_DLQ_BASE_BACKOFF", "30s")
	t.Setenv("INBOUND_DLQ_MAX_BACKOFF", "15m")

	const (
		base    = 30 * time.Second
		floor   = base / 2
		samples = 400
	)

	distinct := make(map[time.Duration]struct{}, samples)
	belowBase := 0

	for i := 0; i < samples; i++ {
		d := dlqBackoffWithJitter(1)
		if d < floor {
			t.Fatalf("delay %s below floor %s", d, floor)
		}
		if d > base+base/2 {
			t.Fatalf("delay %s above 1.5 × base", d)
		}
		if d < base {
			belowBase++
		}
		distinct[d] = struct{}{}
	}

	if belowBase == 0 {
		t.Fatalf("no delay below base in %d samples — jitter still collapsing into base", samples)
	}
	if len(distinct) < 10 {
		t.Fatalf("only %d distinct delays in %d samples, want a spread", len(distinct), samples)
	}
}

func TestDLQBackoffRespectsMaxAndTreatsZeroAsFirstAttempt(t *testing.T) {
	t.Setenv("INBOUND_DLQ_BASE_BACKOFF", "30s")
	t.Setenv("INBOUND_DLQ_MAX_BACKOFF", "15m")

	const max = 15 * time.Minute

	for i := 0; i < 100; i++ {
		if d := dlqBackoffWithJitter(20); d > max {
			t.Fatalf("delay %s above max %s", d, max)
		}
		if d := dlqBackoffWithJitter(0); d < 15*time.Second || d > 45*time.Second {
			t.Fatalf("attempts=0 delay %s outside the first-attempt range", d)
		}
	}
}
