package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryRun(t *testing.T) {
	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errors.New("boom") }
	slow := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	panics := func(context.Context) error { panic("kaboom") }

	tests := []struct {
		name       string
		checks     map[string]CheckFunc
		order      []string
		wantStatus Status
		wantErrs   map[string]string // name -> substring
	}{
		{
			name:       "empty registry is ok",
			wantStatus: StatusOK,
		},
		{
			name:       "all pass",
			checks:     map[string]CheckFunc{"a": ok, "b": ok},
			order:      []string{"a", "b"},
			wantStatus: StatusOK,
		},
		{
			name:       "one failure fails report",
			checks:     map[string]CheckFunc{"a": ok, "b": fail},
			order:      []string{"a", "b"},
			wantStatus: StatusFail,
			wantErrs:   map[string]string{"b": "boom"},
		},
		{
			name:       "timeout is a failure",
			checks:     map[string]CheckFunc{"slow": slow},
			order:      []string{"slow"},
			wantStatus: StatusFail,
			wantErrs:   map[string]string{"slow": "deadline exceeded"},
		},
		{
			name:       "panic is contained",
			checks:     map[string]CheckFunc{"p": panics, "a": ok},
			order:      []string{"p", "a"},
			wantStatus: StatusFail,
			wantErrs:   map[string]string{"p": "panic: kaboom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry(50 * time.Millisecond)
			for _, n := range tt.order {
				if err := r.Register(n, tt.checks[n]); err != nil {
					t.Fatal(err)
				}
			}
			rep := r.Run(context.Background())
			if rep.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s", rep.Status, tt.wantStatus)
			}
			if len(rep.Checks) != len(tt.order) {
				t.Fatalf("got %d results, want %d", len(rep.Checks), len(tt.order))
			}
			for i, res := range rep.Checks {
				if res.Name != tt.order[i] {
					t.Errorf("result %d name = %s, want %s (order must be preserved)", i, res.Name, tt.order[i])
				}
				want, expectErr := tt.wantErrs[res.Name]
				if expectErr {
					if res.Status != StatusFail || !strings.Contains(res.Error, want) {
						t.Errorf("%s: status=%s err=%q, want fail containing %q", res.Name, res.Status, res.Error, want)
					}
				} else if res.Status != StatusOK {
					t.Errorf("%s: status=%s err=%q, want ok", res.Name, res.Status, res.Error)
				}
			}
		})
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := NewRegistry(time.Second)
	noop := func(context.Context) error { return nil }
	if err := r.Register("x", noop); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("x", noop); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if err := DirWritable(dir)(context.Background()); err != nil {
		t.Fatalf("writable dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe file left behind: %v", entries)
	}

	missing := filepath.Join(dir, "nope")
	if err := DirWritable(missing)(context.Background()); err == nil {
		t.Error("expected error for missing dir")
	}
}
