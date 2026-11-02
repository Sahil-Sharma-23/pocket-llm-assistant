package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-assistant/internal/llm"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/research"
	"personal-assistant/internal/tools"
)

// scripted returns canned responses in order and records requests.
type scripted struct {
	mu    sync.Mutex
	out   []string
	err   error
	calls []llm.Request
}

func (s *scripted) Chat(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return llm.Response{}, s.err
	}
	if len(s.out) == 0 {
		return llm.Response{Content: `{"action":"respond","message":"(script exhausted)"}`, FinishReason: "stop"}, nil
	}
	c := s.out[0]
	s.out = s.out[1:]
	return llm.Response{Content: c, FinishReason: "stop", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10}}, nil
}
func (s *scripted) Health(context.Context) error { return nil }

var tz0530 = time.FixedZone("UTC+05:30", 19800)

type env struct {
	o     *Orchestrator
	store *memory.Store
	llm   *scripted
}

func newEnv(t *testing.T, responses ...string) env {
	t.Helper()
	ctx := context.Background()
	st, err := memory.Open(ctx, filepath.Join(t.TempDir(), "a.db"), memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return time.Date(2026, 11, 2, 10, 0, 0, 0, tz0530) }
	deps := tools.Deps{Store: st, Loc: tz0530, Now: now}
	reg := tools.NewRegistry()
	reg.MustRegister(tools.All(deps)...)
	has := func(n string) bool { _, ok := reg.Get(n); return ok }
	agents, err := NewRegistry(DefaultAgents(has), has)
	if err != nil {
		t.Fatal(err)
	}
	fake := &scripted{out: responses}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := New(Options{
		Store:    st,
		Planner:  &Planner{LLM: fake, Tools: reg, Loc: tz0530, Now: now, MaxTokens: 256},
		Executor: &Executor{Tools: reg, Store: st, Log: log, ApprovalTTL: time.Hour},
		Agents:   agents, ToolDeps: deps, Log: log,
		HistoryMessages: 4, MaxSteps: 3, MaxConcurrent: 2, RunTimeout: time.Minute,
	})
	return env{o: o, store: st, llm: fake}
}

func (e env) say(t *testing.T, text string) Reply {
	t.Helper()
	r, err := e.o.Handle(context.Background(), Inbound{Channel: "web", ExternalID: "s1", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouteByKeywords(t *testing.T) {
	all := []string{Productivity, Research, Developer, Knowledge, Assistant}
	tests := map[string]string{
		"remind me to call mom at 6":            Productivity,
		"add a task to renew passport":          Productivity,
		"is example.com up?":                    Developer,
		"what's new in golang/go repo":          Developer,
		"search the latest news about the moon": Research,
		"remember that my locker code is 4412":  Knowledge,
		"what did I say about the trip":         Knowledge,
		"what is my gym locker code?":           Knowledge,
		"what's my wifi password":               Knowledge,
		"hello there":                           "",
		"remind me to search for flights":       "", // ambiguous -> LLM router
	}
	for in, want := range tests {
		if got := RouteByKeywords(in, all); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := RouteByKeywords("search golang", []string{Assistant}); got != "" {
		t.Errorf("routed to unavailable agent %q", got)
	}
}

func TestWriteToolConfirmsWithoutSecondLLMCall(t *testing.T) {
	e := newEnv(t,
		`{"action":"task_create","args":{"title":"Buy milk","due":"2026-11-02 18:00","priority":1}}`)
	r := e.say(t, "add a task to buy milk by 6pm")
	if r.Agent != Productivity || r.Text != "✅ Added task #1: Buy milk (due Mon 02 Nov 2026 18:00) — high priority" {
		t.Fatalf("reply = %+v", r)
	}
	ts, _ := e.store.ListTasks(context.Background(), memory.TaskPending, 10)
	if len(ts) != 1 || ts[0].Title != "Buy milk" || ts[0].DueAt == nil {
		t.Fatalf("tasks = %+v", ts)
	}
	run, _ := e.store.GetRun(context.Background(), r.RunID)
	if run.Status != memory.RunOK || run.LLMCalls != 1 {
		t.Errorf("run = %+v (deterministic confirmation should need one LLM call)", run)
	}
	if !strings.Contains(e.llm.calls[0].Messages[1].Content, "[now: Mon 2026-11-02 10:00") {
		t.Errorf("prompt missing current time: %q", e.llm.calls[0].Messages[1].Content)
	}
	// MustUseTool: the first decision's grammar has no "respond" branch.
	if strings.Contains(string(e.llm.calls[0].JSONSchema), `"const":"respond"`) {
		t.Errorf("first step allowed respond: %s", e.llm.calls[0].JSONSchema)
	}
}

func TestReadToolResultFedBack(t *testing.T) {
	e := newEnv(t,
		`{"action":"task_list","args":{"status":"pending"}}`,
		`{"action":"respond","message":"You have no pending tasks."}`)
	r := e.say(t, "what tasks do I have?")
	if r.Text != "You have no pending tasks." || len(e.llm.calls) != 2 {
		t.Fatalf("reply = %q, calls = %d", r.Text, len(e.llm.calls))
	}
	second := e.llm.calls[1]
	last := second.Messages[len(second.Messages)-1].Content
	if !strings.Contains(last, "Tool result for task_list") {
		t.Errorf("tool result not fed back: %q", last)
	}
	var schema map[string]any
	json.Unmarshal(second.JSONSchema, &schema)
	if _, ok := schema["oneOf"]; !ok || !strings.Contains(string(second.JSONSchema), `"maxLength":900`) {
		t.Errorf("second decision schema = %s", second.JSONSchema)
	}
}

func TestDecideFailureAfterToolReportsWork(t *testing.T) {
	e := newEnv(t, `{"action":"task_list","args":{}}`)
	e.llm.out = append(e.llm.out, `{"action":"respond","message":"unterminated`) // truncated
	r := e.say(t, "show my tasks")
	if !strings.Contains(r.Text, "Done (task_list)") {
		t.Fatalf("reply = %q", r.Text)
	}
}

func TestToolOutsideAgentIsDenied(t *testing.T) {
	// Productivity agent tries a knowledge tool: must not execute.
	e := newEnv(t,
		`{"action":"knowledge_delete","args":{"id":1}}`,
		`{"action":"respond","message":"ok"}`)
	ctx := context.Background()
	k, _ := e.store.AddKnowledge(ctx, memory.NewKnowledge{Title: "keep", Body: "me", Source: memory.SourceUser})
	r := e.say(t, "add a task please")
	if _, err := e.store.GetKnowledge(ctx, k.ID); err != nil {
		t.Fatal("knowledge was deleted by an agent without that tool")
	}
	calls, _ := e.store.ListToolCalls(ctx, r.RunID)
	if len(calls) != 1 || calls[0].Status != "invalid" || !strings.Contains(calls[0].Error, "not available") {
		t.Fatalf("tool calls = %+v", calls)
	}
	if as, _ := e.store.ListApprovals(ctx, "", 10); len(as) != 0 {
		t.Fatal("denied call created an approval")
	}
}

func TestSensitiveToolNeedsApprovalAndRunsOnce(t *testing.T) {
	e := newEnv(t, `{"action":"task_delete","args":{"id":1}}`)
	ctx := context.Background()
	task, _ := e.store.CreateTask(ctx, memory.NewTask{Title: "Old task"})

	r := e.say(t, "delete task 1")
	if len(r.Approvals) != 1 || !strings.Contains(r.Text, "needs your approval") || !strings.Contains(r.Text, "Old task") {
		t.Fatalf("reply = %+v", r)
	}
	if _, err := e.store.GetTask(ctx, task.ID); err != nil {
		t.Fatal("sensitive action executed before approval")
	}
	run, _ := e.store.GetRun(ctx, r.RunID)
	if run.Status != memory.RunAwaitingApproval {
		t.Errorf("run status = %s", run.Status)
	}

	id := r.Approvals[0].ID
	msg, err := e.o.DecideApproval(ctx, id, true, "web")
	if err != nil || !strings.Contains(msg, "Done") {
		t.Fatalf("approve: %q %v", msg, err)
	}
	if _, err := e.store.GetTask(ctx, task.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("approved action not executed")
	}
	msg, _ = e.o.DecideApproval(ctx, id, true, "web")
	if !strings.Contains(msg, "no longer pending") {
		t.Fatalf("second approve = %q", msg)
	}
	a, _ := e.store.GetApproval(ctx, id)
	if a.Status != memory.ApprovalExecuted {
		t.Fatalf("approval status = %s", a.Status)
	}
}

func TestRejectDoesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task, _ := e.store.CreateTask(ctx, memory.NewTask{Title: "Keep me"})
	r := e.say(t, "/deltask 1")
	if len(r.Approvals) != 1 {
		t.Fatalf("reply = %+v", r)
	}
	r = e.say(t, "/reject "+itoa(r.Approvals[0].ID))
	if !strings.Contains(r.Text, "Rejected") {
		t.Fatalf("reject reply = %q", r.Text)
	}
	if _, err := e.store.GetTask(ctx, task.ID); err != nil {
		t.Fatal("rejected action executed")
	}
}

func TestSmuggledArgumentsRejected(t *testing.T) {
	e := newEnv(t,
		`{"action":"task_create","args":{"title":"x","permission":"read"}}`,
		`{"action":"respond","message":"failed"}`)
	// (second response is used after the invalid call is reported back)
	r := e.say(t, "add a task x")
	calls, _ := e.store.ListToolCalls(context.Background(), r.RunID)
	if len(calls) != 1 || calls[0].Status != "invalid" {
		t.Fatalf("calls = %+v", calls)
	}
	if ts, _ := e.store.ListTasks(context.Background(), "", 10); len(ts) != 0 {
		t.Fatal("task created from invalid args")
	}
}

func TestLLMErrorsAreUserFriendly(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{llm.ErrBusy, "busy"},
		{llm.ErrInsufficientMemory, "low on free memory"},
		{llm.ErrUnavailable, "isn't available"},
		{context.DeadlineExceeded, "too long"},
	}
	for _, tt := range tests {
		e := newEnv(t)
		e.llm.err = tt.err
		r := e.say(t, "add a task")
		if !strings.Contains(r.Text, tt.want) {
			t.Errorf("%v -> %q", tt.err, r.Text)
		}
		run, _ := e.store.GetRun(context.Background(), r.RunID)
		if run.Status != memory.RunError {
			t.Errorf("run status = %s", run.Status)
		}
	}
}

func TestMaxStepsForcesAnswer(t *testing.T) {
	e := newEnv(t,
		`{"action":"task_list","args":{}}`,
		`{"action":"task_list","args":{}}`,
		`{"action":"task_list","args":{}}`,
		`{"action":"respond","message":"You have no tasks."}`)
	r := e.say(t, "show my tasks")
	if r.Text != "You have no tasks." {
		t.Fatalf("reply = %q", r.Text)
	}
	last := e.llm.calls[len(e.llm.calls)-1]
	if !strings.Contains(string(last.JSONSchema), `"const":"respond"`) || strings.Contains(string(last.JSONSchema), "oneOf") {
		t.Errorf("final call not respond-only: %s", last.JSONSchema)
	}
}

func TestCommands(t *testing.T) {
	e := newEnv(t)
	tests := []struct{ in, want string }{
		{"/help", "/tasks"},
		{"/task Pay rent @ 2026-11-03 09:00", "Added task #1: Pay rent (due Tue 03 Nov 2026 09:00)"},
		{"/tasks", "#1 Pay rent"},
		{"/done 1", "Completed task #1"},
		{"/remind in 30m stretch", "set for Mon 02 Nov 2026 10:30: stretch"},
		{"/remind 18:00 call mom", "Mon 02 Nov 2026 18:00: call mom"},
		{"/remind sometime soon", "Usage"},
		{"/reminders", "call mom"},
		{"/note Locker code is 4412", "Saved note #1"},
		{"/recall locker", "Locker code"},
		{"/approvals", "No pending"},
		{"/bogus", "Unknown command"},
	}
	for _, tt := range tests {
		r := e.say(t, tt.in)
		if !strings.Contains(r.Text, tt.want) {
			t.Errorf("%s -> %q, want containing %q", tt.in, r.Text, tt.want)
		}
	}
	if len(e.llm.calls) != 0 {
		t.Errorf("commands used the LLM %d times", len(e.llm.calls))
	}
}

func TestMergeConsecutiveAlternates(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleSystem, Content: "s"},
		{Role: llm.RoleAssistant, Content: "orphan"},
		{Role: llm.RoleUser, Content: "a"},
		{Role: llm.RoleUser, Content: "b"},
		{Role: llm.RoleAssistant, Content: "c"},
	}
	out := mergeConsecutive(in)
	if len(out) != 3 || out[1].Content != "a\nb" || out[2].Role != llm.RoleAssistant {
		t.Fatalf("out = %+v", out)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// newResearchEnv wires a research client against a fake search backend.
func newResearchEnv(t *testing.T, searchOK bool, responses ...string) env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !searchOK {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"query":{"search":[{"title":"Gemma (language model)","snippet":"Gemma is a family of open models by Google DeepMind"}]}}`)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	st, err := memory.Open(ctx, filepath.Join(t.TempDir(), "r.db"), memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return time.Date(2026, 11, 2, 10, 0, 0, 0, tz0530) }
	deps := tools.Deps{Store: st, Loc: tz0530, Now: now, Research: research.New(research.Options{
		Providers: []string{"wikipedia"}, MaxResults: 3, Timeout: 5 * time.Second, UserAgent: "t",
		HTTP: srv.Client(), WikipediaURL: srv.URL,
	})}
	reg := tools.NewRegistry()
	reg.MustRegister(tools.All(deps)...)
	has := func(n string) bool { _, ok := reg.Get(n); return ok }
	agents, _ := NewRegistry(DefaultAgents(has), has)
	fake := &scripted{out: responses}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := New(Options{
		Store: st, Planner: &Planner{LLM: fake, Tools: reg, Loc: tz0530, Now: now, MaxTokens: 256},
		Executor: &Executor{Tools: reg, Store: st, Log: log, ApprovalTTL: time.Hour},
		Agents:   agents, ToolDeps: deps, Log: log, MaxSteps: 3, MaxConcurrent: 1, RunTimeout: time.Minute,
	})
	return env{o: o, store: st, llm: fake}
}

func TestResearchMustSearchAndLabelsExternal(t *testing.T) {
	e := newResearchEnv(t, true,
		`{"action":"web_search","args":{"query":"Gemma 4 language model"}}`,
		`{"action":"respond","message":"Gemma is a family of open models from Google DeepMind."}`)
	r := e.say(t, "/search Gemma 4 language model")
	first := string(e.llm.calls[0].JSONSchema)
	if !strings.Contains(first, `"const":"web_search"`) || strings.Contains(first, "respond") ||
		strings.Contains(first, "research_save") || strings.Contains(first, "oneOf") {
		t.Fatalf("first research step must allow only web_search: %s", first)
	}
	if !strings.HasPrefix(r.Text, "🌐 From the web:") || !strings.Contains(r.Text, "https://en.wikipedia.org/wiki/Gemma_%28language_model%29") {
		t.Fatalf("reply = %q", r.Text)
	}
	ks, _ := e.store.ListKnowledge(context.Background(), 10)
	if len(ks) != 1 || ks[0].Source != memory.SourceResearch || !strings.Contains(ks[0].Body, "Sources: https://") {
		t.Fatalf("knowledge = %+v", ks)
	}
}

func TestResearchWithoutResultsIsMarkedUnverified(t *testing.T) {
	e := newResearchEnv(t, false,
		`{"action":"web_search","args":{"query":"x"}}`,
		`{"action":"respond","message":"I think x is y."}`)
	r := e.say(t, "/search x")
	if !strings.Contains(r.Text, "NOT verified") {
		t.Fatalf("reply = %q", r.Text)
	}
	if ks, _ := e.store.ListKnowledge(context.Background(), 10); len(ks) != 0 {
		t.Fatalf("unverified answer saved as research: %+v", ks)
	}
}

func TestSourceURLsRanksRelevantResults(t *testing.T) {
	steps := []Step{{Tool: "web_search", Result: `[
		{"title":"R (programming language)","url":"https://en.wikipedia.org/wiki/R","snippet":"a language"},
		{"title":"Termux","url":"https://termux.dev","snippet":"Termux is a terminal emulator for Android"},
		{"title":"Bash","url":"https://en.wikipedia.org/wiki/Bash","snippet":"Unix shell"}]`}}
	got := sourceURLs(steps, "what is Termux?", 3)
	if len(got) != 1 || got[0] != "https://termux.dev" {
		t.Fatalf("sources = %v", got)
	}
}

func TestDailyPlan(t *testing.T) {
	e := newEnv(t, "- 09:00 Pay rent (overdue)\n- Read book")
	ctx := context.Background()
	if got := e.o.DailyPlan(ctx, "test"); !strings.Contains(got, "No pending tasks") {
		t.Fatalf("empty plan = %q", got)
	}
	due := time.Date(2026, 11, 1, 9, 0, 0, 0, tz0530)
	e.store.CreateTask(ctx, memory.NewTask{Title: "Pay rent", DueAt: &due})
	e.store.CreateTask(ctx, memory.NewTask{Title: "Read book"})
	if got := e.o.DailyPlan(ctx, "test"); !strings.HasPrefix(got, "📋 Plan for today\n- 09:00 Pay rent") {
		t.Fatalf("plan = %q", got)
	}
	e.llm.err = llm.ErrUnavailable // model down: deterministic fallback
	got := e.o.DailyPlan(ctx, "test")
	if !strings.Contains(got, "OVERDUE #1 Pay rent") || !strings.Contains(got, "#2 Read book") || !strings.Contains(got, "isn't available") {
		t.Fatalf("fallback plan = %q", got)
	}
}
