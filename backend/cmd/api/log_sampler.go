package main

// Amostragem de log para caminhos de rejeição em endpoints públicos.
//
// Os webhooks da Meta são expostos sem autenticação: uma linha de log por
// requisição rejeitada permite que qualquer origem infle o volume de log do
// gateway. O sampler emite no máximo uma linha por intervalo e informa quantas
// ocorrências foram suprimidas nesse meio-tempo.

import (
	"log"
	"sync"
	"time"
)

const defaultRejectionLogInterval = 30 * time.Second

type logSampler struct {
	interval time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func newLogSampler(interval time.Duration) *logSampler {
	if interval <= 0 {
		interval = defaultRejectionLogInterval
	}
	return &logSampler{interval: interval}
}

// Printf emite a linha se o intervalo já passou; caso contrário conta como
// suprimida e a próxima linha emitida reporta o total acumulado.
func (l *logSampler) Printf(format string, args ...any) {
	now := time.Now()

	l.mu.Lock()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	l.last = now
	interval := l.interval
	l.mu.Unlock()

	if suppressed > 0 {
		log.Printf(format+" (+%d similar suppressed in the last %s)", append(args, suppressed, interval)...)
		return
	}
	log.Printf(format, args...)
}
