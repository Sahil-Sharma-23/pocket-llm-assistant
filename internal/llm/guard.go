package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// MemAvailableFunc returns free RAM in MB (sysinfo.ReadMem().AvailableMB).
type MemAvailableFunc func() (int64, error)

// Ensurer is satisfied by Supervisor: it makes sure the server is up.
type Ensurer interface {
	Ensure(ctx context.Context) error
}

// GuardOptions configures Guard.
type GuardOptions struct {
	// MaxQueue is how many callers may wait for the single slot.
	MaxQueue int
	// Timeout bounds each request (queue wait + inference).
	Timeout time.Duration
	// MinFreeMemMB refuses inference below this MemAvailable (0 = off).
	MinFreeMemMB int64
	MemAvailable MemAvailableFunc
	Ensurer      Ensurer // optional
	Log          *slog.Logger
}

// Guard enforces the device's inference limits around a Provider:
// exactly one request runs at a time, at most MaxQueue wait, each request is
// time-bounded, and nothing runs when free memory is below the floor. Every
// limit violation is a returned error, never a silent degradation.
type Guard struct {
	inner Provider
	opts  GuardOptions
	slot  chan struct{}

	waiting atomic.Int64
	mu      sync.Mutex
	stats   Stats
}

// Stats summarises inference activity for the dashboard and /healthz.
type Stats struct {
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	Rejected      int64     `json:"rejected"`
	InFlight      int64     `json:"in_flight"`
	Queued        int64     `json:"queued"`
	LastLatencyMS int64     `json:"last_latency_ms"`
	LastUsage     Usage     `json:"last_usage"`
	LastError     string    `json:"last_error,omitempty"`
	LastAt        time.Time `json:"last_at"`
}

// NewGuard wraps inner.
func NewGuard(inner Provider, o GuardOptions) *Guard {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Guard{inner: inner, opts: o, slot: make(chan struct{}, 1)}
}

// Chat implements Provider with limits applied.
func (g *Guard) Chat(ctx context.Context, req Request) (Response, error) {
	if g.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.opts.Timeout)
		defer cancel()
	}

	if err := g.acquire(ctx); err != nil {
		return Response{}, err
	}
	defer func() { <-g.slot }()

	if err := g.checkMemory(); err != nil {
		g.record(func(s *Stats) { s.Rejected++; s.LastError = err.Error() })
		return Response{}, err
	}
	if g.opts.Ensurer != nil {
		if err := g.opts.Ensurer.Ensure(ctx); err != nil {
			g.record(func(s *Stats) { s.Errors++; s.LastError = err.Error() })
			return Response{}, err
		}
	}

	g.record(func(s *Stats) { s.InFlight = 1 })
	start := time.Now()
	resp, err := g.inner.Chat(ctx, req)
	lat := time.Since(start)
	g.record(func(s *Stats) {
		s.InFlight = 0
		s.Requests++
		s.LastLatencyMS = lat.Milliseconds()
		s.LastAt = time.Now()
		if err != nil {
			s.Errors++
			s.LastError = err.Error()
		} else {
			s.LastUsage = resp.Usage
			s.LastError = ""
		}
	})
	if err != nil {
		g.opts.Log.Warn("inference failed", "err", err, "latency_ms", lat.Milliseconds())
	} else {
		g.opts.Log.Info("inference",
			"latency_ms", lat.Milliseconds(), "prompt_tokens", resp.Usage.PromptTokens,
			"cached_tokens", resp.Usage.CachedTokens, "completion_tokens", resp.Usage.CompletionTokens,
			"prompt_tps", round1(resp.Usage.PromptPerSec), "gen_tps", round1(resp.Usage.GenPerSec),
			"schema", len(req.JSONSchema) > 0)
	}
	return resp, err
}

// acquire takes the single inference slot. If it is busy, the caller joins
// the queue only while fewer than MaxQueue callers are already waiting.
func (g *Guard) acquire(ctx context.Context) error {
	select {
	case g.slot <- struct{}{}:
		return nil
	default:
	}
	if n := g.waiting.Add(1); n > int64(g.opts.MaxQueue) {
		g.waiting.Add(-1)
		g.record(func(s *Stats) { s.Rejected++ })
		return ErrBusy
	}
	defer g.waiting.Add(-1)
	select {
	case g.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for inference slot: %w", ctx.Err())
	}
}

func (g *Guard) checkMemory() error {
	if g.opts.MinFreeMemMB <= 0 || g.opts.MemAvailable == nil {
		return nil
	}
	avail, err := g.opts.MemAvailable()
	if err != nil {
		// Unsupported platform (dev machine): cannot enforce, say so.
		g.opts.Log.Debug("memory check skipped", "err", err)
		return nil
	}
	if avail < g.opts.MinFreeMemMB {
		return fmt.Errorf("%w: %d MB available, %d MB required", ErrInsufficientMemory, avail, g.opts.MinFreeMemMB)
	}
	return nil
}

// Health implements Provider.
func (g *Guard) Health(ctx context.Context) error { return g.inner.Health(ctx) }

// Stats returns a snapshot of counters.
func (g *Guard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	s.Queued = max(g.waiting.Load(), 0)
	return s
}

func (g *Guard) record(f func(*Stats)) {
	g.mu.Lock()
	f(&g.stats)
	g.mu.Unlock()
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// IsRetryable reports whether a later retry may succeed.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable)
}
