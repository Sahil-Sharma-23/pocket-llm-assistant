package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clk
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		s, err := Open(ctx, path, Options{})
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		v, err := s.AppliedMigrations(ctx)
		if err != nil || len(v) != 1 || v[0] != "0001_init.sql" {
			t.Fatalf("migrations = %v, %v", v, err)
		}
		s.Close()
	}
}

func TestFTSQuery(t *testing.T) {
	tests := map[string]string{
		"golang":                  `"golang"*`,
		"What did I say about Go": `"say"* OR "go"*`,
		`"); DROP TABLE x; --`:    `"drop"* OR "table"*`,
		"NEAR(a b) OR c*":         `"near"*`,
		"":                        "",
		"a i":                     "",
	}
	for in, want := range tests {
		if got := FTSQuery(in); got != want {
			t.Errorf("FTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConversationsAndMessageSearch(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	c1, err := s.GetOrCreateConversation(ctx, "telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetOrCreateConversation(ctx, "telegram", "42")
	if again.ID != c1.ID {
		t.Fatal("GetOrCreate created a duplicate")
	}
	for _, m := range []struct{ role, text string }{
		{RoleUser, "remind me to renew the passport"},
		{RoleAssistant, "Created a reminder for the passport renewal"},
		{RoleUser, "what is the weather"},
	} {
		if _, err := s.AddMessage(ctx, c1.ID, m.role, "", m.text); err != nil {
			t.Fatal(err)
		}
	}
	recent, _ := s.RecentMessages(ctx, c1.ID, 2)
	if len(recent) != 2 || recent[1].Content != "what is the weather" {
		t.Fatalf("recent = %+v", recent)
	}
	hits, err := s.SearchMessages(ctx, "passport", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %d, err %v", len(hits), err)
	}
	if !strings.Contains(hits[0].Snippet, "[passport]") {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}
	conv, _ := s.GetConversation(ctx, c1.ID)
	if conv.Title != "remind me to renew the passport" {
		t.Errorf("title = %q", conv.Title)
	}
}

func TestKnowledge(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.AddKnowledge(ctx, NewKnowledge{Title: "x", Body: "y", Source: "web"}); err == nil {
		t.Fatal("invalid source accepted")
	}
	k1, err := s.AddKnowledge(ctx, NewKnowledge{Title: "Home Wi-Fi", Body: "Router is a TP-Link in the hallway", Source: SourceUser, Tags: []string{"home"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddKnowledge(ctx, NewKnowledge{Title: "SQLite FTS5", Body: "Full text search extension", Source: SourceResearch, URL: "https://sqlite.org/fts5.html"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchKnowledge(ctx, "where is the router?", 5)
	if err != nil || len(hits) != 1 || hits[0].ID != k1.ID {
		t.Fatalf("hits = %+v err %v", hits, err)
	}
	if err := s.DeleteKnowledge(ctx, k1.ID); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.SearchKnowledge(ctx, "router", 5)
	if len(hits) != 0 {
		t.Fatal("deleted entry still searchable (FTS trigger)")
	}
	if err := s.DeleteKnowledge(ctx, k1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
}

func TestTasks(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	due := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a, err := s.CreateTask(ctx, NewTask{Title: "Pay rent", Priority: 1, DueAt: &due})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, NewTask{Title: "  "}); err == nil {
		t.Fatal("blank title accepted")
	}
	if _, err := s.CreateTask(ctx, NewTask{Title: "x", Priority: 9}); err == nil {
		t.Fatal("bad priority accepted")
	}
	b, _ := s.CreateTask(ctx, NewTask{Title: "Read book"})
	done, err := s.SetTaskStatus(ctx, b.ID, TaskDone)
	if err != nil || done.CompletedAt == nil {
		t.Fatalf("complete: %+v %v", done, err)
	}
	pending, _ := s.ListTasks(ctx, TaskPending, 10)
	if len(pending) != 1 || pending[0].ID != a.ID || pending[0].DueAt == nil || !pending[0].DueAt.Equal(due) {
		t.Fatalf("pending = %+v", pending)
	}
	if _, err := s.SetTaskStatus(ctx, 999, TaskDone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task = %v", err)
	}
}

func TestApprovalLifecycle(t *testing.T) {
	s, clk := newStore(t)
	ctx := context.Background()
	a, err := s.CreateApproval(ctx, NewApproval{Tool: "task_delete", Permission: "sensitive", Args: `{"id":1}`, Summary: "Delete task 1", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApproval(ctx, NewApproval{Tool: "x", Permission: "read", TTL: time.Hour}); err == nil {
		t.Fatal("read permission approval accepted")
	}

	// Finishing before approval must fail (no execution without approval).
	if err := s.FinishApproval(ctx, a.ID, "", nil); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("finish unapproved = %v", err)
	}

	// Two concurrent deciders: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, approve := range []bool{true, false} {
		wg.Add(1)
		go func(approve bool) {
			defer wg.Done()
			_, err := s.DecideApproval(ctx, a.ID, approve, "test")
			results <- err
		}(approve)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrApprovalNotPending) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}

	got, _ := s.GetApproval(ctx, a.ID)
	if got.Status == ApprovalApproved {
		if err := s.FinishApproval(ctx, a.ID, "deleted", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishApproval(ctx, a.ID, "again", nil); !errors.Is(err, ErrApprovalNotPending) {
			t.Fatal("approved action could be executed twice")
		}
	}

	// Expiry.
	b, _ := s.CreateApproval(ctx, NewApproval{Tool: "t", Permission: "write", Args: "{}", Summary: "s", TTL: time.Minute})
	clk.Add(2 * time.Minute)
	if _, err := s.DecideApproval(ctx, b.ID, true, "test"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("expired approval decided: %v", err)
	}
	b, _ = s.GetApproval(ctx, b.ID)
	if b.Status != ApprovalExpired {
		t.Fatalf("status = %s, want expired", b.Status)
	}
}

func TestJobs(t *testing.T) {
	s, clk := newStore(t)
	ctx := context.Background()
	now := clk.Now()
	if err := s.UpsertJob(ctx, JobSpec{Kind: "monitor", Name: "site", Schedule: "every 5m", NextRunAt: now}); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateJob(ctx, JobSpec{Kind: "reminder", Name: "r1", Payload: `{"text":"hi"}`, Schedule: "once", NextRunAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueJobs(ctx, now, 10)
	if len(due) != 1 || due[0].Name != "site" {
		t.Fatalf("due = %+v", due)
	}
	next := now.Add(5 * time.Minute)
	if err := s.RecordJobRun(ctx, due[0].ID, now, nil, &next); err != nil {
		t.Fatal(err)
	}
	// Re-upserting with the same schedule must not reset next_run_at.
	s.UpsertJob(ctx, JobSpec{Kind: "monitor", Name: "site", Schedule: "every 5m", NextRunAt: now})
	j, _ := s.GetJob(ctx, due[0].ID)
	if !j.NextRunAt.Equal(next) {
		t.Fatalf("upsert reset next_run_at to %s", j.NextRunAt)
	}
	nd, _ := s.NextDueAt(ctx)
	if nd == nil || !nd.Equal(next) {
		t.Fatalf("NextDueAt = %v", nd)
	}
	// One-shot job disabled after run.
	s.RecordJobRun(ctx, r.ID, now, errors.New("boom"), nil)
	r, _ = s.GetJob(ctx, r.ID)
	if r.Enabled || r.LastStatus != "error" || r.LastError != "boom" {
		t.Fatalf("reminder after run = %+v", r)
	}
	s.DisableJobsExcept(ctx, "monitor", nil)
	if nd, _ := s.NextDueAt(ctx); nd != nil {
		t.Fatalf("expected no enabled jobs, next = %v", nd)
	}
}

func TestRunsAndStaleRecovery(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.StartRun(ctx, 0, "productivity", "message", "add task")
	if err != nil {
		t.Fatal(err)
	}
	s.AddToolCall(ctx, ToolCall{RunID: id, Tool: "task_create", Permission: "write", Args: "{}", Status: "ok", Attempts: 1})
	stale, _ := s.StartRun(ctx, 0, "research", "message", "x")
	if err := s.FinishRun(ctx, id, RunOK, "done", "", RunUsage{LLMCalls: 2}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.MarkStaleRuns(ctx)
	if n != 1 {
		t.Fatalf("stale = %d", n)
	}
	r, _ := s.GetRun(ctx, stale)
	if r.Status != RunError || !strings.Contains(r.Error, "interrupted") {
		t.Fatalf("stale run = %+v", r)
	}
	calls, _ := s.ListToolCalls(ctx, id)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
}

func TestBackupAndStats(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	s.CreateTask(ctx, NewTask{Title: "t"})
	dest := filepath.Join(t.TempDir(), "b", "backup.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("overwrote existing backup")
	}
	b, err := Open(ctx, dest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	st, _ := b.Stats(ctx)
	if st.PendingTasks != 1 {
		t.Fatalf("backup stats = %+v", st)
	}
}

func TestPreferences(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	s.SetPreference(ctx, "name", "Sam")
	s.SetPreference(ctx, "name", "Sam K")
	s.SetPreference(ctx, "_tg_offset", "5")
	v, ok, _ := s.GetPreference(ctx, "name")
	if !ok || v != "Sam K" {
		t.Fatalf("pref = %q %v", v, ok)
	}
	pub, _ := s.ListPreferences(ctx, false)
	if len(pub) != 1 {
		t.Fatalf("internal prefs leaked: %+v", pub)
	}
}
