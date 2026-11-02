// Package scheduler runs persisted jobs (reminders, monitors, daily plans,
// backups). It is battery-conscious: instead of polling it sleeps until the
// next due job (capped by MaxIdleSleep) or until woken because jobs changed.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"personal-assistant/internal/memory"
)

// Handler executes one job.
type Handler func(ctx context.Context, job memory.Job) error

// Options configures the scheduler.
type Options struct {
	Store        *memory.Store
	Log          *slog.Logger
	Loc          *time.Location
	Now          func() time.Time
	MaxWorkers   int
	MaxIdleSleep time.Duration
	JobTimeout   time.Duration
}

// Scheduler dispatches due jobs to handlers by kind.
type Scheduler struct {
	o        Options
	mu       sync.RWMutex
	handlers map[string]Handler
	wake     chan struct{}

	statMu  sync.Mutex
	lastRun time.Time
	running int
}

// New builds a scheduler.
func New(o Options) *Scheduler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = 5 * time.Minute
	}
	return &Scheduler{o: o, handlers: map[string]Handler{}, wake: make(chan struct{}, 1)}
}

// Register sets the handler for a job kind.
func (s *Scheduler) Register(kind string, h Handler) {
	s.mu.Lock()
	s.handlers[kind] = h
	s.mu.Unlock()
}

// Wake makes the scheduler re-check due jobs now (non-blocking).
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run loops until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.o.Log.Info("scheduler started", "workers", s.o.MaxWorkers)
	for ctx.Err() == nil {
		s.runDue(ctx)
		sleep := s.o.MaxIdleSleep
		if next, err := s.o.Store.NextDueAt(ctx); err == nil && next != nil {
			if d := time.Until(*next); d < sleep {
				sleep = max(d, 0)
			}
		}
		if sleep == 0 {
			sleep = 50 * time.Millisecond // a job is due again immediately; avoid a hot loop
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-s.wake:
			t.Stop()
		case <-t.C:
		}
	}
	s.o.Log.Info("scheduler stopped")
}

func (s *Scheduler) runDue(ctx context.Context) {
	now := s.o.Now()
	jobs, err := s.o.Store.DueJobs(ctx, now, 50)
	if err != nil {
		if ctx.Err() == nil {
			s.o.Log.Error("load due jobs", "err", err)
		}
		return
	}
	if len(jobs) == 0 {
		return
	}
	sem := make(chan struct{}, max(s.o.MaxWorkers, 1))
	var wg sync.WaitGroup
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(j memory.Job) {
			defer func() { <-sem; wg.Done() }()
			s.runJob(ctx, j)
		}(j)
	}
	wg.Wait()
}

func (s *Scheduler) runJob(ctx context.Context, j memory.Job) {
	s.mu.RLock()
	h := s.handlers[j.Kind]
	s.mu.RUnlock()

	s.statMu.Lock()
	s.running++
	s.statMu.Unlock()
	defer func() {
		s.statMu.Lock()
		s.running--
		s.lastRun = s.o.Now()
		s.statMu.Unlock()
	}()

	var runErr error
	start := s.o.Now()
	if h == nil {
		runErr = fmt.Errorf("no handler for job kind %q", j.Kind)
	} else {
		jctx, cancel := context.WithTimeout(ctx, s.o.JobTimeout)
		runErr = safeRun(jctx, h, j)
		cancel()
	}
	if ctx.Err() != nil {
		return // shutting down: leave the job due so it runs after restart
	}
	next, err := NextRun(j.Schedule, s.o.Now(), s.o.Loc)
	if err != nil {
		runErr = errors.Join(runErr, err)
		next = nil // invalid schedule: disable rather than spin
	}
	if err := s.o.Store.RecordJobRun(context.WithoutCancel(ctx), j.ID, start, runErr, next); err != nil {
		s.o.Log.Error("record job run", "job", j.Name, "err", err)
	}
	lvl := slog.LevelInfo
	if runErr != nil {
		lvl = slog.LevelWarn
	}
	s.o.Log.Log(ctx, lvl, "job ran", "kind", j.Kind, "name", j.Name, "err", runErr,
		"took_ms", s.o.Now().Sub(start).Milliseconds(), "next", next)
}

func safeRun(ctx context.Context, h Handler, j memory.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("job panicked: %v", p)
		}
	}()
	return h(ctx, j)
}

// Status is a snapshot for the dashboard.
type Status struct {
	Running int       `json:"running"`
	LastRun time.Time `json:"last_run"`
}

// Status returns scheduler activity.
func (s *Scheduler) Status() Status {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return Status{Running: s.running, LastRun: s.lastRun}
}

var dailyRe = regexp.MustCompile(`^daily ([01]\d|2[0-3]):([0-5]\d)$`)

// NextRun computes the next run after `after`. nil means "do not run again".
//
//	once            -> nil
//	every <dur>     -> after + dur (missed runs are not replayed)
//	daily HH:MM     -> next HH:MM in loc strictly after `after`
func NextRun(schedule string, after time.Time, loc *time.Location) (*time.Time, error) {
	switch {
	case schedule == "once":
		return nil, nil
	case strings.HasPrefix(schedule, "every "):
		d, err := time.ParseDuration(strings.TrimPrefix(schedule, "every "))
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("invalid schedule %q", schedule)
		}
		t := after.Add(d)
		return &t, nil
	case dailyRe.MatchString(schedule):
		m := dailyRe.FindStringSubmatch(schedule)
		var hh, mm int
		fmt.Sscanf(m[1]+" "+m[2], "%d %d", &hh, &mm)
		a := after.In(loc)
		t := time.Date(a.Year(), a.Month(), a.Day(), hh, mm, 0, 0, loc)
		if !t.After(a) {
			t = t.AddDate(0, 0, 1)
		}
		return &t, nil
	}
	return nil, fmt.Errorf("invalid schedule %q", schedule)
}

// FirstRun is the initial next_run_at for a new recurring job.
func FirstRun(schedule string, now time.Time, loc *time.Location) (time.Time, error) {
	if strings.HasPrefix(schedule, "every ") {
		return now, nil // run immediately, then every interval
	}
	t, err := NextRun(schedule, now, loc)
	if err != nil || t == nil {
		return time.Time{}, fmt.Errorf("invalid recurring schedule %q", schedule)
	}
	return *t, nil
}
