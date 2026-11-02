// Package health runs named readiness checks with per-check timeouts.
package health

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Status is the outcome of a check or of the whole report.
type Status string

const (
	StatusOK   Status = "ok"
	StatusFail Status = "fail"
)

// CheckFunc returns nil when the dependency is healthy. It must honour ctx.
type CheckFunc func(ctx context.Context) error

// Result is the outcome of one check.
type Result struct {
	Name       string  `json:"name"`
	Status     Status  `json:"status"`
	Error      string  `json:"error,omitempty"`
	DurationMS float64 `json:"duration_ms"`
}

// Report aggregates all check results. Status is fail if any check failed.
type Report struct {
	Status Status   `json:"status"`
	Checks []Result `json:"checks"`
}

type namedCheck struct {
	name string
	fn   CheckFunc
}

// Registry holds checks in registration order. Checks run sequentially: the
// set is small and running them serially keeps CPU wake-ups minimal.
type Registry struct {
	mu      sync.RWMutex
	checks  []namedCheck
	timeout time.Duration
}

// NewRegistry creates a registry applying timeout to each individual check.
func NewRegistry(timeout time.Duration) *Registry {
	return &Registry{timeout: timeout}
}

// Register adds a named check. Names must be unique.
func (r *Registry) Register(name string, fn CheckFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.checks {
		if c.name == name {
			return fmt.Errorf("health check %q already registered", name)
		}
	}
	r.checks = append(r.checks, namedCheck{name: name, fn: fn})
	return nil
}

// Run executes every check and returns the aggregated report.
func (r *Registry) Run(ctx context.Context) Report {
	r.mu.RLock()
	checks := append([]namedCheck(nil), r.checks...)
	r.mu.RUnlock()

	rep := Report{Status: StatusOK, Checks: make([]Result, 0, len(checks))}
	for _, c := range checks {
		res := r.runOne(ctx, c)
		if res.Status != StatusOK {
			rep.Status = StatusFail
		}
		rep.Checks = append(rep.Checks, res)
	}
	return rep
}

func (r *Registry) runOne(ctx context.Context, c namedCheck) (res Result) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	start := time.Now()
	res = Result{Name: c.name, Status: StatusOK}
	defer func() {
		if p := recover(); p != nil {
			res.Status = StatusFail
			res.Error = fmt.Sprintf("panic: %v", p)
		}
		res.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	}()

	if err := c.fn(ctx); err != nil {
		res.Status = StatusFail
		res.Error = err.Error()
	} else if ctx.Err() != nil {
		res.Status = StatusFail
		res.Error = ctx.Err().Error()
	}
	return res
}

// DirWritable returns a check that creates and removes a probe file in dir.
// It catches full storage and permission problems (e.g. after Android
// storage changes) before they surface as database errors.
func DirWritable(dir string) CheckFunc {
	return func(ctx context.Context) error {
		f, err := os.CreateTemp(dir, ".healthcheck-*")
		if err != nil {
			return fmt.Errorf("dir %s not writable: %w", dir, err)
		}
		name := f.Name()
		_, werr := f.Write([]byte("ok"))
		cerr := f.Close()
		rerr := os.Remove(name)
		switch {
		case werr != nil:
			return fmt.Errorf("write probe: %w", werr)
		case cerr != nil:
			return fmt.Errorf("close probe: %w", cerr)
		case rerr != nil:
			return fmt.Errorf("remove probe: %w", rerr)
		}
		return ctx.Err()
	}
}
