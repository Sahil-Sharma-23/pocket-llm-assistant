package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"personal-assistant/internal/memory"
)

var tz0530 = time.FixedZone("UTC+05:30", 5*3600+1800)

func testDeps(t *testing.T) Deps {
	t.Helper()
	s, err := memory.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, tz0530)
	return Deps{Store: s, Loc: tz0530, Now: func() time.Time { return now }, PublicHTTP: http.DefaultClient}
}

type echoIn struct {
	N int `json:"n"`
}

func (in *echoIn) Validate() error {
	if in.N < 0 {
		return errors.New("n must be >= 0")
	}
	return nil
}

func echoTool(perm Permission, run func(context.Context, echoIn) (int, error)) Tool {
	return New(Spec[echoIn, int]{
		Name: "echo", Description: "d", Schema: `{"type":"object"}`, Permission: perm,
		Timeout: 100 * time.Millisecond, Retry: RetryPolicy{Attempts: 3, Backoff: time.Millisecond},
		Run: run, Consequential: func(in echoIn) bool { return in.N > 10 },
	})
}

func TestPrepareValidation(t *testing.T) {
	tool := echoTool(Read, func(_ context.Context, in echoIn) (int, error) { return in.N, nil })
	tests := []struct {
		args    string
		wantErr bool
	}{
		{`{"n":1}`, false},
		{``, false},
		{`{"n":-1}`, true},
		{`{"n":"x"}`, true},
		{`{"n":1,"permission":"read"}`, true}, // unknown field: model cannot smuggle extras
		{`[1]`, true},
	}
	for _, tt := range tests {
		_, err := tool.Prepare(json.RawMessage(tt.args))
		if (err != nil) != tt.wantErr {
			t.Errorf("Prepare(%s) err = %v", tt.args, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("error not ErrInvalidArgs: %v", err)
		}
	}
}

func TestNeedsApproval(t *testing.T) {
	run := func(_ context.Context, in echoIn) (int, error) { return in.N, nil }
	tests := []struct {
		perm Permission
		args string
		want bool
	}{
		{Read, `{"n":99}`, false},
		{Write, `{"n":1}`, false},
		{Write, `{"n":11}`, true}, // consequential
		{Sensitive, `{"n":0}`, true},
	}
	for _, tt := range tests {
		c, err := echoTool(tt.perm, run).Prepare(json.RawMessage(tt.args))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.NeedsApproval(); got != tt.want {
			t.Errorf("%s %s: NeedsApproval = %v, want %v", tt.perm, tt.args, got, tt.want)
		}
	}
}

func TestExecuteRetryTimeoutPanic(t *testing.T) {
	var calls atomic.Int32
	flaky := echoTool(Read, func(context.Context, echoIn) (int, error) {
		if calls.Add(1) < 3 {
			return 0, Retryable(errors.New("transient"))
		}
		return 7, nil
	})
	c, _ := flaky.Prepare(json.RawMessage(`{}`))
	res, err := c.Execute(context.Background())
	if err != nil || string(res.Output) != "7" || res.Attempts != 3 {
		t.Fatalf("flaky: %+v %v", res, err)
	}

	calls.Store(0)
	permanent := echoTool(Read, func(context.Context, echoIn) (int, error) {
		calls.Add(1)
		return 0, errors.New("permanent")
	})
	c, _ = permanent.Prepare(json.RawMessage(`{}`))
	if _, err := c.Execute(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatalf("non-retryable error retried %d times", calls.Load())
	}

	slow := echoTool(Read, func(ctx context.Context, _ echoIn) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	c, _ = slow.Prepare(json.RawMessage(`{}`))
	if _, err := c.Execute(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}

	boom := echoTool(Read, func(context.Context, echoIn) (int, error) { panic("kaboom") })
	c, _ = boom.Prepare(json.RawMessage(`{}`))
	if _, err := c.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic err = %v", err)
	}
}

func TestRegistryRejectsBadTools(t *testing.T) {
	r := NewRegistry()
	good := echoTool(Read, func(context.Context, echoIn) (int, error) { return 0, nil })
	if err := r.Register(good); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(good); err == nil {
		t.Error("duplicate accepted")
	}
	bad := good
	bad.Name = "Bad Name"
	if err := r.Register(bad); err == nil {
		t.Error("bad name accepted")
	}
	bad = good
	bad.Name, bad.Permission = "other", "admin"
	if err := r.Register(bad); err == nil {
		t.Error("bad permission accepted")
	}
	bad = good
	bad.Name, bad.Schema = "other2", json.RawMessage("{")
	if err := r.Register(bad); err == nil {
		t.Error("bad schema accepted")
	}
}

func TestAllToolsRegister(t *testing.T) {
	d := testDeps(t)
	r := NewRegistry()
	for _, tool := range All(d) {
		if err := r.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	perms := map[string]Permission{"task_delete": Sensitive, "knowledge_delete": Sensitive,
		"reminder_cancel": Write, "task_list": Read, "task_create": Write}
	for name, want := range perms {
		tool, ok := r.Get(name)
		if !ok || tool.Permission != want {
			t.Errorf("%s permission = %v, want %v", name, tool.Permission, want)
		}
	}
}

func TestParseLocalTime(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, tz0530)
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"2026-11-03 18:00", "2026-11-03 18:00", false},
		{"2026-11-03T07:30", "2026-11-03 07:30", false},
		{"2026-11-05", "2026-11-05 09:00", false},
		{"18:00", "2026-11-02 18:00", false}, // later today
		{"09:00", "2026-11-03 09:00", false}, // already past -> tomorrow
		{"tomorrow", "", true},
	}
	for _, tt := range tests {
		got, err := ParseLocalTime(tt.in, tz0530, now)
		if (err != nil) != tt.wantErr {
			t.Errorf("%q err = %v", tt.in, err)
			continue
		}
		if !tt.wantErr && got.Format("2006-01-02 15:04") != tt.want {
			t.Errorf("%q = %s, want %s", tt.in, got.Format("2006-01-02 15:04"), tt.want)
		}
	}
}

func run(t *testing.T, tool Tool, args string) (json.RawMessage, error) {
	t.Helper()
	c, err := tool.Prepare(json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	res, err := c.Execute(context.Background())
	return res.Output, err
}

func TestTaskAndReminderTools(t *testing.T) {
	d := testDeps(t)
	woke := 0
	d.WakeScheduler = func() { woke++ }

	out, err := run(t, TaskCreate(d), `{"title":"Pay rent","priority":1,"due":"2026-11-01 09:00"}`)
	if err != nil || !strings.Contains(string(out), `"due":"Sun 01 Nov 2026 09:00"`) {
		t.Fatalf("task_create = %s %v", out, err)
	}
	run(t, TaskCreate(d), `{"title":"Read book"}`)
	out, _ = run(t, PlanData(d), `{}`)
	var plan PlanDataOut
	json.Unmarshal(out, &plan)
	if len(plan.Overdue) != 1 || len(plan.Pending) != 1 {
		t.Fatalf("plan = %+v", plan)
	}

	if _, err := run(t, ReminderCreate(d), `{"text":"call mom","at":"2026-11-01 10:00"}`); err == nil {
		t.Fatal("past reminder accepted")
	}
	if _, err := run(t, ReminderCreate(d), `{"text":"x"}`); err == nil {
		t.Fatal("reminder without time accepted")
	}
	out, err = run(t, ReminderCreate(d), `{"text":"call mom","in_minutes":30}`)
	if err != nil || !strings.Contains(string(out), "10:30") || woke != 1 {
		t.Fatalf("reminder = %s %v woke=%d", out, err, woke)
	}
	c, _ := ReminderCancel(d).Prepare(json.RawMessage(`{"id":1}`))
	if !c.NeedsApproval() || !strings.Contains(c.Summary(), "call mom") {
		t.Errorf("cancel approval=%v summary=%q", c.NeedsApproval(), c.Summary())
	}
	c, _ = TaskDelete(d).Prepare(json.RawMessage(`{"id":1}`))
	if !c.NeedsApproval() || !strings.Contains(c.Summary(), "Pay rent") {
		t.Errorf("delete approval=%v summary=%q", c.NeedsApproval(), c.Summary())
	}
}

func TestKnowledgeTools(t *testing.T) {
	d := testDeps(t)
	if _, err := run(t, KnowledgeSave(d), `{"title":"Wi-Fi","body":"Password is on the router sticker"}`); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, KnowledgeSearch(d), `{"query":"router password"}`)
	if err != nil || !strings.Contains(string(out), `"source":"user"`) {
		t.Fatalf("search = %s %v", out, err)
	}
	if _, err := run(t, PreferenceSet(d), `{"key":"Bad Key!","value":"x"}`); err == nil {
		t.Fatal("bad pref key accepted")
	}
}

func TestGitHubStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/repos/o/r":
			io.WriteString(w, `{"full_name":"o/r","stargazers_count":5,"open_issues_count":3,"pushed_at":"2026-11-01T10:00:00Z"}`)
		case "/repos/o/r/commits":
			io.WriteString(w, `[{"sha":"abcdef123456","commit":{"message":"fix: thing\n\nbody","author":{"name":"Sam","date":"2026-11-01T10:00:00Z"}}}]`)
		case "/repos/o/r/pulls":
			io.WriteString(w, `[{},{}]`)
		case "/repos/o/r/releases/latest":
			io.WriteString(w, `{"tag_name":"v1.2.0"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := testDeps(t)
	d.GitHub = &GitHub{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
	out, err := run(t, GitHubRepoStatus(d), `{"repo":"https://github.com/o/r"}`)
	if err != nil {
		t.Fatal(err)
	}
	var st RepoStatus
	json.Unmarshal(out, &st)
	if st.OpenPRs != 2 || st.LatestRelease != "v1.2.0" || st.RecentCommits[0].SHA != "abcdef1" || st.RecentCommits[0].Message != "fix: thing" {
		t.Errorf("status = %+v", st)
	}
	d.RepoAllowlist = []string{"x/y"}
	if _, err := run(t, GitHubRepoStatus(d), `{"repo":"o/r"}`); err == nil {
		t.Error("allowlist not enforced")
	}
}

func TestCheckHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, `{"status":"up"}`)
		case "/down":
			w.WriteHeader(502)
		}
	}))
	defer srv.Close()
	tests := []struct {
		spec   CheckSpec
		wantOK bool
	}{
		{CheckSpec{URL: srv.URL + "/ok"}, true},
		{CheckSpec{URL: srv.URL + "/ok", ExpectJSON: "status"}, true},
		{CheckSpec{URL: srv.URL + "/ok", ExpectJSON: "version"}, false},
		{CheckSpec{URL: srv.URL + "/down"}, false},
		{CheckSpec{URL: srv.URL + "/down", ExpectStatus: 502}, true},
		{CheckSpec{URL: "http://127.0.0.1:1/"}, false},
	}
	for _, tt := range tests {
		r := CheckHTTP(context.Background(), srv.Client(), tt.spec)
		if r.OK != tt.wantOK {
			t.Errorf("%+v -> %+v", tt.spec, r)
		}
	}
}
