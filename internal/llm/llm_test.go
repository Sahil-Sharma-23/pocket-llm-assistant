package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeServer(t *testing.T, h http.HandlerFunc) *LlamaClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewLlamaClient(LlamaOptions{BaseURL: srv.URL, MaxTokens: 64, Temperature: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const okBody = `{"choices":[{"finish_reason":"stop","message":{"content":"  {\"agent\":\"research\"}  "}}],
 "usage":{"prompt_tokens":70,"completion_tokens":9},
 "timings":{"cache_n":40,"prompt_n":30,"prompt_per_second":8.2,"predicted_per_second":4.6}}`

func TestLlamaChatRequestAndResponse(t *testing.T) {
	var got map[string]any
	c := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, okBody)
	})
	schema := json.RawMessage(`{"type":"object"}`)
	resp, err := c.Chat(context.Background(), Request{
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Temperature: Temp(0),
		JSONSchema:  schema,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"agent":"research"}` || resp.Usage.CachedTokens != 40 || resp.Usage.GenPerSec != 4.6 {
		t.Errorf("resp = %+v", resp)
	}
	if got["temperature"].(float64) != 0 || got["max_tokens"].(float64) != 64 || got["stream"] != false {
		t.Errorf("request = %v", got)
	}
	kw := got["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Errorf("thinking not disabled: %v", kw)
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format = %v", rf)
	}
}

func TestLlamaErrors(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		body    string
		wantErr error
		substr  string
	}{
		{"loading", 503, `{"error":{"message":"Loading model"}}`, ErrUnavailable, ""},
		{"garbage", 200, `not json`, ErrMalformed, ""},
		{"no choices", 200, `{"choices":[]}`, ErrMalformed, ""},
		{"bad request", 400, `{"error":"bad"}`, nil, "http 400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.code)
				io.WriteString(w, tt.body)
			})
			_, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.substr != "" && (err == nil || !strings.Contains(err.Error(), tt.substr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.substr)
			}
		})
	}
}

func TestLlamaCancellation(t *testing.T) {
	c := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // server only detects disconnects after the body is read
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation not honoured promptly")
	}
}

func TestLlamaRefusesNonLoopback(t *testing.T) {
	c, err := NewLlamaClient(LlamaOptions{BaseURL: "http://10.255.255.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "secret"}}})
	if err == nil || !strings.Contains(err.Error(), "refusing non-loopback") {
		t.Fatalf("err = %v, want loopback refusal", err)
	}
}

func TestDecodeJSON(t *testing.T) {
	type out struct {
		Agent string `json:"agent"`
	}
	tests := []struct {
		name    string
		resp    Response
		want    string
		wantErr bool
	}{
		{"plain", Response{Content: `{"agent":"chat"}`}, "chat", false},
		{"fenced", Response{Content: "```json\n{\"agent\":\"knowledge\"}\n```"}, "knowledge", false},
		{"prose", Response{Content: `Sure! {"agent": "research", "note": "a } in string"} done`}, "research", false},
		{"truncated", Response{Content: `{"agent":"res`, FinishReason: "length"}, "", true},
		{"empty", Response{Content: "  "}, "", true},
		{"no json", Response{Content: "I think research"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var o out
			err := DecodeJSON(tt.resp, &o)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err != nil && !errors.Is(err, ErrMalformed) {
				t.Fatalf("err not ErrMalformed: %v", err)
			}
			if o.Agent != tt.want {
				t.Errorf("agent = %q, want %q", o.Agent, tt.want)
			}
		})
	}
}

// slowProvider blocks until released.
type slowProvider struct{ release chan struct{} }

func (p *slowProvider) Chat(ctx context.Context, _ Request) (Response, error) {
	select {
	case <-p.release:
		return Response{Content: "ok"}, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}
func (p *slowProvider) Health(context.Context) error { return nil }

func TestGuardSerializesAndRejectsWhenQueueFull(t *testing.T) {
	p := &slowProvider{release: make(chan struct{})}
	g := NewGuard(p, GuardOptions{MaxQueue: 1, Timeout: 5 * time.Second})
	req := Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ { // one running + one queued
		go func() { _, err := g.Chat(context.Background(), req); errs <- err }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for g.waiting.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := g.Chat(context.Background(), req); !errors.Is(err, ErrBusy) {
		t.Fatalf("third request err = %v, want ErrBusy", err)
	}
	close(p.release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("queued request failed: %v", err)
		}
	}
	if st := g.Stats(); st.Requests != 2 || st.Rejected != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestGuardMemoryFloor(t *testing.T) {
	p := &slowProvider{release: make(chan struct{})}
	close(p.release)
	g := NewGuard(p, GuardOptions{MaxQueue: 1, MinFreeMemMB: 500,
		MemAvailable: func() (int64, error) { return 200, nil }})
	_, err := g.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, ErrInsufficientMemory) || !strings.Contains(err.Error(), "200 MB available") {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardTimeout(t *testing.T) {
	p := &slowProvider{release: make(chan struct{})}
	g := NewGuard(p, GuardOptions{MaxQueue: 1, Timeout: 30 * time.Millisecond})
	_, err := g.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
}

func TestSupervisorArgsAndMissingModel(t *testing.T) {
	s, err := NewSupervisor(SupervisorOptions{
		Binary: "llama-server", ModelPath: "/nonexistent/model.gguf", BaseURL: "http://127.0.0.1:8080",
		ContextSize: 4096, Threads: 4, SleepIdleSeconds: 900, StartupTimeout: time.Second,
		RestartBackoff: 20 * time.Millisecond, Health: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := s.Args()
	joined := strings.Join(args, " ")
	for _, want := range []string{"--host 127.0.0.1", "--port 8080", "-np 1", "--sleep-idle-seconds 900", `"enable_thinking":false`} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go s.Run(ctx)
	if err := s.Ensure(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ensure = %v, want ErrUnavailable", err)
	}
	if st := s.Status(); !strings.Contains(st.LastError, "model file") {
		t.Errorf("status = %+v", st)
	}
}
