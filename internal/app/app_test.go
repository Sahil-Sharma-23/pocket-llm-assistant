package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/config"
	"personal-assistant/internal/memory"
)

func testConfig(t *testing.T, addr string) config.Config {
	t.Helper()
	cfg := config.Default()
	home := t.TempDir()
	cfg.Home = home
	cfg.Timezone = "UTC"
	cfg.Server.Addr = addr
	cfg.Server.ShutdownTimeout = config.Duration{Duration: 5 * time.Second}
	cfg.Paths = config.PathsConfig{
		DataDir: filepath.Join(home, "data"), LogDir: filepath.Join(home, "logs"),
		BackupDir: filepath.Join(home, "backups"), ModelsDir: filepath.Join(home, "models"),
	}
	cfg.Database.Path = filepath.Join(home, "data", "assistant.db")
	cfg.Runtime = config.RuntimeConfig{} // don't mutate the test process's runtime
	cfg.LLM.Server.Managed = false       // tests never spawn llama-server
	cfg.Research.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startApp(t *testing.T, cfg config.Config) (*App, context.CancelFunc, chan error) {
	t.Helper()
	a, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-a.Ready():
	case err := <-done:
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server not ready")
	}
	return a, cancel, done
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	a, cancel, done := startApp(t, testConfig(t, "127.0.0.1:0"))
	base := "http://" + a.Addr().String()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Status  string         `json:"status"`
		Details map[string]any `json:"details"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 200 || body.Status != "ok" || body.Details["llm"] == nil {
		t.Fatalf("healthz = %d %+v", resp.StatusCode, body)
	}
	resp, err = http.Get(base + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("dashboard = %v %v", resp, err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if _, err := http.Get(base + "/livez"); err == nil {
		t.Error("server still accepting connections after shutdown")
	}
}

func TestRunFailsWhenPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := New(testConfig(t, ln.Addr().String()), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("expected bind error when port already in use")
	}
}

func TestNewCreatesPrivateDirsAndRecovers(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:0")
	// Simulate a crash: a run left "running" and an approval left "approved".
	ctx := context.Background()
	st, err := memory.Open(ctx, cfg.Database.Path, memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := st.StartRun(ctx, 0, "productivity", "message", "x")
	ap, _ := st.CreateApproval(ctx, memory.NewApproval{Tool: "task_delete", Permission: "sensitive", Args: "{}", Summary: "s", TTL: time.Hour})
	st.DecideApproval(ctx, ap.ID, true, "test")
	st.Close()

	a, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer a.store.Close()
	for _, d := range []string{cfg.Paths.DataDir, cfg.Paths.LogDir, cfg.Paths.BackupDir} {
		st, err := os.Stat(d)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v perm %v", d, err, st.Mode().Perm())
		}
	}
	r, _ := a.store.GetRun(ctx, runID)
	if r.Status != memory.RunError {
		t.Errorf("stale run status = %s", r.Status)
	}
	got, _ := a.store.GetApproval(ctx, ap.ID)
	if got.Status != memory.ApprovalFailed {
		t.Errorf("interrupted approval status = %s (must not be re-executable)", got.Status)
	}
	jobs, _ := a.store.ListJobs(ctx, "", 20)
	kinds := map[string]bool{}
	for _, j := range jobs {
		if j.Enabled {
			kinds[j.Kind] = true
		}
	}
	if !kinds["backup"] || !kinds["maintenance"] || !kinds["daily_plan"] {
		t.Errorf("config jobs not scheduled: %v", kinds)
	}
}

// fakeLlama emulates llama-server's OpenAI endpoint with scripted outputs.
type fakeLlama struct {
	mu  sync.Mutex
	out []string
}

func (f *fakeLlama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		io.WriteString(w, `{"status":"ok"}`)
		return
	}
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	content := `{"action":"respond","message":"ok"}`
	if len(f.out) > 0 {
		content, f.out = f.out[0], f.out[1:]
	}
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}},
		"usage":   map[string]int{"prompt_tokens": 50, "completion_tokens": 8},
		"timings": map[string]float64{"prompt_per_second": 8, "predicted_per_second": 4.5},
	})
	w.Write(b)
}

func TestEndToEndWithFakeModel(t *testing.T) {
	llama := &fakeLlama{out: []string{
		`{"action":"reminder_create","args":{"text":"stretch","in_minutes":30}}`,
		`{"action":"respond","message":"I'll remind you to stretch in 30 minutes."}`,
	}}
	srv := httptest.NewServer(llama)
	defer srv.Close()

	cfg := testConfig(t, "127.0.0.1:0")
	cfg.LLM.BaseURL = srv.URL
	a, cancel, done := startApp(t, cfg)
	defer func() { cancel(); <-done }()

	reply, err := a.Orchestrator().Handle(context.Background(), agent.Inbound{Channel: "web", ExternalID: "t", Text: "remind me to stretch in 30 minutes"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Agent != agent.Productivity || !strings.Contains(reply.Text, "stretch") {
		t.Fatalf("reply = %+v", reply)
	}
	jobs, _ := a.store.ListJobs(context.Background(), "reminder", 10)
	if len(jobs) != 1 || !jobs[0].Enabled {
		t.Fatalf("reminder jobs = %+v", jobs)
	}
	// reminder_create confirms deterministically: one LLM call, no wording call.
	if !strings.HasPrefix(reply.Text, "⏰ Reminder #") || !strings.HasSuffix(reply.Text, ": stretch") {
		t.Errorf("reply text = %q", reply.Text)
	}
	if st := a.guard.Stats(); st.Requests != 1 || st.Errors != 0 {
		t.Errorf("llm stats = %+v", st)
	}
	if txt := a.statusText(context.Background()); !strings.Contains(txt, "requests 1") {
		t.Errorf("status = %q", txt)
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"assistant-20261101-000000.db", "assistant-20261102-000000.db", "assistant-20261103-000000.db", "other.txt"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o600)
	}
	if err := pruneBackups(dir, 2); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "assistant-20261102-000000.db,assistant-20261103-000000.db,other.txt" {
		t.Errorf("after prune: %v", names)
	}
}
