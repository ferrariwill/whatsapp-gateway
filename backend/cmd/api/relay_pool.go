package main

// Pool de workers para o processamento assíncrono dos webhooks inbound.
//
// Antes cada evento da Meta disparava um `go` sem limite: uma rajada em um
// endpoint público criava uma goroutine por evento e um restart descartava
// todos os repasses em voo. O pool troca isso por concorrência limitada,
// backpressure na admissão e drenagem no shutdown.

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRelayWorkers       = 64
	defaultRelayQueueSize     = 1024
	defaultRelaySubmitTimeout = 2 * time.Second
	defaultRelayTaskTimeout   = 45 * time.Second
)

var (
	// ErrRelayPoolSaturated indica que a fila continuou cheia até o timeout de
	// admissão — o chamador deve devolver 503 para a Meta reentregar depois.
	ErrRelayPoolSaturated = errors.New("inbound relay pool saturated")
	// ErrRelayPoolClosed indica que o pool está drenando por causa do shutdown.
	ErrRelayPoolClosed = errors.New("inbound relay pool is shutting down")
)

type relayTask func(context.Context)

type relayPool struct {
	tasks         chan relayTask
	submitTimeout time.Duration
	taskTimeout   time.Duration
	workers       int
	queueCapacity int

	inFlight          atomic.Int64
	rejectedSaturated atomic.Int64
	rejectedClosed    atomic.Int64

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.RWMutex
	stopping bool
}

type relayPoolConfig struct {
	Workers       int
	QueueSize     int
	SubmitTimeout time.Duration
	TaskTimeout   time.Duration
}

func relayPoolConfigFromEnv() relayPoolConfig {
	return relayPoolConfig{
		Workers:       envInt("INBOUND_RELAY_WORKERS", defaultRelayWorkers),
		QueueSize:     envInt("INBOUND_RELAY_QUEUE", defaultRelayQueueSize),
		SubmitTimeout: envDuration("INBOUND_RELAY_SUBMIT_TIMEOUT", defaultRelaySubmitTimeout),
		TaskTimeout:   envDuration("INBOUND_RELAY_TASK_TIMEOUT", defaultRelayTaskTimeout),
	}
}

func newRelayPool(cfg relayPoolConfig) *relayPool {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultRelayWorkers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultRelayQueueSize
	}
	if cfg.SubmitTimeout <= 0 {
		cfg.SubmitTimeout = defaultRelaySubmitTimeout
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = defaultRelayTaskTimeout
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	pool := &relayPool{
		tasks:         make(chan relayTask, cfg.QueueSize),
		submitTimeout: cfg.SubmitTimeout,
		taskTimeout:   cfg.TaskTimeout,
		workers:       cfg.Workers,
		queueCapacity: cfg.QueueSize,
		baseCtx:       baseCtx,
		cancel:        cancel,
	}

	pool.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go pool.work()
	}
	return pool
}

func (p *relayPool) work() {
	defer p.wg.Done()
	for task := range p.tasks {
		p.run(task)
	}
}

func (p *relayPool) run(task relayTask) {
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	ctx, cancel := context.WithTimeout(p.baseCtx, p.taskTimeout)
	defer cancel()
	task(ctx)
}

// Submit enfileira o processamento. Com a fila cheia espera até submitTimeout
// antes de recusar, de modo que a rajada vire 503 (Meta reentrega) em vez de
// goroutines sem limite.
func (p *relayPool) Submit(task relayTask) error {
	if p == nil {
		// Servidores montados sem pool (testes unitários) mantêm o comportamento
		// anterior de uma goroutine por evento.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), defaultRelayTaskTimeout)
			defer cancel()
			task(ctx)
		}()
		return nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopping {
		p.rejectedClosed.Add(1)
		return ErrRelayPoolClosed
	}

	select {
	case p.tasks <- task:
		return nil
	default:
	}

	timer := time.NewTimer(p.submitTimeout)
	defer timer.Stop()
	select {
	case p.tasks <- task:
		return nil
	case <-timer.C:
		p.rejectedSaturated.Add(1)
		return ErrRelayPoolSaturated
	}
}

// Shutdown para de aceitar tarefas e drena as pendentes. Estourado o prazo de
// ctx, cancela o contexto base para que os repasses em voo abortem rápido.
func (p *relayPool) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	close(p.tasks)
	p.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		p.cancel()
		return nil
	case <-ctx.Done():
		log.Printf("inbound relay pool: drain deadline exceeded, cancelling in-flight relays")
		p.cancel()
		<-drained
		return ctx.Err()
	}
}

// Queued informa quantas tarefas aguardam um worker (diagnóstico/testes).
func (p *relayPool) Queued() int {
	if p == nil {
		return 0
	}
	return len(p.tasks)
}

// relayPoolStats são métricas de infraestrutura do processo. Não carregam
// system_id/tenant_id porque o pool é deliberadamente compartilhado.
type relayPoolStats struct {
	QueueDepth        int   `json:"queue_depth"`
	QueueCapacity     int   `json:"queue_capacity"`
	Workers           int   `json:"workers"`
	WorkersInFlight   int64 `json:"workers_in_flight"`
	RejectedSaturated int64 `json:"rejected_saturated"`
	RejectedClosed    int64 `json:"rejected_closed"`
}

func (p *relayPool) Stats() relayPoolStats {
	if p == nil {
		return relayPoolStats{}
	}
	return relayPoolStats{
		QueueDepth:        len(p.tasks),
		QueueCapacity:     p.queueCapacity,
		Workers:           p.workers,
		WorkersInFlight:   p.inFlight.Load(),
		RejectedSaturated: p.rejectedSaturated.Load(),
		RejectedClosed:    p.rejectedClosed.Load(),
	}
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf("invalid %s=%q, using %d", key, raw, fallback)
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		log.Printf("invalid %s=%q, using %s", key, raw, fallback)
		return fallback
	}
	return value
}
