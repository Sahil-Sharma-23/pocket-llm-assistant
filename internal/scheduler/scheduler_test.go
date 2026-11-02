package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"personal-assistant/internal/memory"
)

var tz0530 = time.FixedZone("UTC+05:30", 19800)

func TestNextRun(t *testing.T) {
	after := time.Date(2026, 11, 2, 10, 0, 0, 0, tz0530)
	tests := []struct {
		sched   string
		want    string // "" = nil
		wantErr bool
	}{
		{"once", "", false},
		{"every 5m", "2026-11-02 10:05", false},
		{"daily 08:00", "2026-11-03 08:00", false},
		{"daily 18:30", "2026-11-02 18:30", false},
		{"daily 10:00", "2026-11-03 10:00", false}, // strictly after
		{"every 0s", "", true},
		{"daily 25:00", "", true},
		{"weekly", "", true},
	}
	for _, tt := range tests {
		got, err := NextRun(tt.sched, after, tz0530)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v", tt.sched, err)
			continue
		}
		switch {
		case tt.want == "" && got != nil:
			t.Errorf("%s: got %v, want nil", tt.sched, got)
		case tt.want != "" && (got == nil || got.In(tz0530).Format("2006-01-02 15:04") != tt.want):
			t.Errorf("%s: got %v, want %s", tt.sched, got, tt.want)
		}
	}
}

func TestSchedulerRunsDueJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := memory.Open(ctx, filepath.Join(t.TempDir(), "s.db"), memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := New(Options{Store: st, Loc: tz0530, MaxWorkers: 2, MaxIdleSleep: time.Minute})
	var reminders, monitors atomic.Int32
	fired := make(chan string, 10)
	s.Register("reminder", func(ctx context.Context, j memory.Job) error {
		reminders.Add(1)
		fired <- j.Name
		return nil
	})
	s.Register("monitor", func(ctx context.Context, j memory.Job) error {
		monitors.Add(1)
		fired <- j.Name
		return errors.New("site down")
	})
	s.Register("boom", func(context.Context, memory.Job) error { panic("x") })

	now := time.Now()
	st.CreateJob(ctx, memory.JobSpec{Kind: "reminder", Name: "r1", Schedule: "once", NextRunAt: now.Add(-time.Second)})
	st.CreateJob(ctx, memory.JobSpec{Kind: "monitor", Name: "m1", Schedule: "every 1h", NextRunAt: now})
	st.CreateJob(ctx, memory.JobSpec{Kind: "boom", Name: "b1", Schedule: "once", NextRunAt: now})
	go s.Run(ctx)

	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case n := <-fired:
			got[n] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("jobs not run, got %v", got)
		}
	}
	// A reminder created later must fire promptly after Wake.
	st.CreateJob(ctx, memory.JobSpec{Kind: "reminder", Name: "r2", Schedule: "once", NextRunAt: time.Now().Add(200 * time.Millisecond)})
	s.Wake()
	select {
	case n := <-fired:
		if n != "r2" {
			t.Fatalf("fired %s", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("woken reminder did not fire")
	}
	time.Sleep(100 * time.Millisecond)

	jobs, _ := st.ListJobs(ctx, "", 10)
	for _, j := range jobs {
		switch j.Name {
		case "r1", "r2":
			if j.Enabled || j.LastStatus != "ok" {
				t.Errorf("%s = %+v", j.Name, j)
			}
		case "m1":
			if !j.Enabled || j.LastStatus != "error" || j.NextRunAt.Before(now.Add(50*time.Minute)) {
				t.Errorf("monitor = %+v", j)
			}
		case "b1":
			if j.Enabled || j.LastError == "" {
				t.Errorf("panicking job = %+v", j)
			}
		}
	}
	if reminders.Load() != 2 || monitors.Load() != 1 {
		t.Errorf("reminders=%d monitors=%d", reminders.Load(), monitors.Load())
	}
}
