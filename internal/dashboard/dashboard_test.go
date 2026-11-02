package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/tools"
)

type stubHandler struct {
	mu       sync.Mutex
	inbound  []string
	decided  []int64
	approved []bool
}

func (s *stubHandler) Handle(_ context.Context, in agent.Inbound) (agent.Reply, error) {
	s.mu.Lock()
	s.inbound = append(s.inbound, in.Text)
	s.mu.Unlock()
	return agent.Reply{Text: "ok"}, nil
}

func (s *stubHandler) DecideApproval(_ context.Context, id int64, approve bool, by string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decided = append(s.decided, id)
	s.approved = append(s.approved, approve)
	return "Done #1", nil
}

func setup(t *testing.T) (*Dashboard, http.Handler, *stubHandler) {
	t.Helper()
	ctx := context.Background()
	st, err := memory.Open(ctx, filepath.Join(t.TempDir(), "d.db"), memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	deps := tools.Deps{Store: st, Loc: time.UTC}
	reg := tools.NewRegistry()
	reg.MustRegister(tools.All(deps)...)
	has := func(n string) bool { _, ok := reg.Get(n); return ok }
	agents, _ := agent.NewRegistry(agent.DefaultAgents(has), has)
	h := &stubHandler{}
	db, err := New(Deps{Store: st, Handler: h, Agents: agents, Tools: reg, Loc: time.UTC, ChatTimeout: time.Second,
		ConfigJSON: `{"server":{"addr":"127.0.0.1:8787"}}`,
		Status: func(context.Context) StatusView {
			return StatusView{Version: "test", LLMState: "running", MemAvailableMB: 2800}
		}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	db.Mount(mux)

	// Seed data so every page renders rows.
	c, _ := st.GetOrCreateConversation(ctx, "web", "dashboard")
	st.AddMessage(ctx, c.ID, memory.RoleUser, "", "hello <script>alert(1)</script>")
	st.AddMessage(ctx, c.ID, memory.RoleAssistant, "assistant", "hi")
	st.CreateTask(ctx, memory.NewTask{Title: "Pay rent"})
	st.AddKnowledge(ctx, memory.NewKnowledge{Title: "Wi-Fi", Body: "router", Source: memory.SourceUser})
	st.CreateApproval(ctx, memory.NewApproval{Tool: "task_delete", Permission: "sensitive", Args: `{"id":1}`, Summary: "Delete task #1", TTL: time.Hour})
	st.CreateJob(ctx, memory.JobSpec{Kind: "reminder", Name: "r", Schedule: "once", NextRunAt: time.Now().Add(time.Hour)})
	id, _ := st.StartRun(ctx, c.ID, "productivity", "message", "add task")
	st.AddToolCall(ctx, memory.ToolCall{RunID: id, Tool: "task_create", Permission: "write", Args: "{}", Status: "ok"})
	st.FinishRun(ctx, id, memory.RunOK, "done", "", memory.RunUsage{LLMCalls: 1})
	return db, mux, h
}

func TestPagesRender(t *testing.T) {
	_, mux, _ := setup(t)
	for _, path := range []string{"/", "/chat", "/conversations", "/conversations/1", "/conversations?q=hello",
		"/memory", "/memory?q=router", "/tasks", "/approvals", "/jobs", "/runs", "/runs/1", "/config", "/logs", "/static/style.css"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if rec.Code != 200 {
			t.Errorf("%s: code %d", path, rec.Code)
			continue
		}
		if path != "/static/style.css" && !strings.Contains(body, "</html>") {
			t.Errorf("%s: incomplete render (template error?)", path)
		}
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("%s: unescaped user content (XSS)", path)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/999", nil))
	if rec.Code != 404 {
		t.Errorf("missing run = %d", rec.Code)
	}
}

func post(mux http.Handler, path string, form url.Values, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:8787"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCSRFProtection(t *testing.T) {
	db, mux, h := setup(t)
	good := url.Values{"csrf": {db.token()}}

	if rec := post(mux, "/approvals/1/approve", url.Values{}, ""); rec.Code != 403 {
		t.Errorf("no token = %d, want 403", rec.Code)
	}
	if rec := post(mux, "/approvals/1/approve", url.Values{"csrf": {"forged"}}, ""); rec.Code != 403 {
		t.Errorf("bad token = %d, want 403", rec.Code)
	}
	if rec := post(mux, "/approvals/1/approve", good, "http://evil.example"); rec.Code != 403 {
		t.Errorf("cross origin = %d, want 403", rec.Code)
	}
	if len(h.decided) != 0 {
		t.Fatal("approval decided despite failed CSRF")
	}
	rec := post(mux, "/approvals/1/approve", good, "http://127.0.0.1:8787")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "Done") {
		t.Fatalf("approve = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(h.decided) != 1 || h.decided[0] != 1 || !h.approved[0] {
		t.Fatalf("decided = %v %v", h.decided, h.approved)
	}
	if rec := post(mux, "/approvals/1/delete", good, ""); rec.Code != 400 {
		t.Errorf("bad decision = %d", rec.Code)
	}
}

func TestChatIsAsync(t *testing.T) {
	db, mux, h := setup(t)
	rec := post(mux, "/chat", url.Values{"csrf": {db.token()}, "text": {"remind me"}}, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("chat post = %d", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.inbound)
		h.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chat message not handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
