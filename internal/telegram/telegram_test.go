package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/memory"
)

const token = "123456:SECRET-token"

type fakeAPI struct {
	mu      sync.Mutex
	updates []Update
	sent    []map[string]any
	edited  []map[string]any
	answers []string
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		reply := func(v any) {
			b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
			w.Write(b)
		}
		if method == "getUpdates" {
			f.mu.Lock()
			ups := f.updates
			f.updates = nil
			f.mu.Unlock()
			if len(ups) == 0 {
				time.Sleep(50 * time.Millisecond)
			}
			reply(ups)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case "getMe":
			reply(User{ID: 1, Username: "test_bot"})
		case "sendMessage":
			f.sent = append(f.sent, p)
			reply(Message{MessageID: int64(len(f.sent))})
		case "editMessageText":
			f.edited = append(f.edited, p)
			reply(true)
		case "answerCallbackQuery":
			f.answers = append(f.answers, p["text"].(string))
			reply(true)
		default:
			reply(true)
		}
	})
}

type fakeHandler struct {
	mu        sync.Mutex
	inbound   []agent.Inbound
	decisions []string
}

func (h *fakeHandler) Handle(_ context.Context, in agent.Inbound) (agent.Reply, error) {
	h.mu.Lock()
	h.inbound = append(h.inbound, in)
	h.mu.Unlock()
	if strings.Contains(in.Text, "delete") {
		return agent.Reply{Text: "needs approval", Approvals: []memory.Approval{{ID: 7, Permission: "sensitive", Summary: "Delete task #1"}}}, nil
	}
	return agent.Reply{Text: "echo: " + in.Text}, nil
}

func (h *fakeHandler) DecideApproval(_ context.Context, id int64, approve bool, by string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.decisions = append(h.decisions, by)
	if approve {
		return "Done #7", nil
	}
	return "Rejected #7", nil
}

type memOffsets struct {
	mu sync.Mutex
	m  map[string]string
}

func (o *memOffsets) GetPreference(_ context.Context, k string) (string, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.m[k]
	return v, ok, nil
}
func (o *memOffsets) SetPreference(_ context.Context, k, v string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[k] = v
	return nil
}

func msg(updateID, from int64, text string) Update {
	return Update{UpdateID: updateID, Message: &Message{MessageID: updateID, From: &User{ID: from},
		Chat: Chat{ID: from, Type: "private"}, Text: text}}
}

func TestBotFlow(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	h := &fakeHandler{}
	offsets := &memOffsets{m: map[string]string{}}
	bot := New(Options{
		Client: NewClient(srv.URL, token, srv.Client()), Handler: h, Offsets: offsets,
		AllowedIDs: []int64{42}, PollTimeout: time.Second, MaxInFlight: 1,
	})

	api.updates = []Update{
		msg(10, 99, "let me in"),      // unknown user
		msg(11, 42, "hello"),          // allowed
		msg(12, 42, "delete my task"), // approval flow
		{UpdateID: 13, Message: &Message{From: &User{ID: 42}, Chat: Chat{ID: -5, Type: "group"}, Text: "group msg"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()

	waitFor(t, func() bool { api.mu.Lock(); defer api.mu.Unlock(); return len(api.sent) >= 3 })

	api.mu.Lock()
	api.updates = []Update{{UpdateID: 14, CallbackQuery: &CallbackQuery{ID: "cb1", From: User{ID: 42},
		Message: &Message{MessageID: 3, Chat: Chat{ID: 42}, Text: "approve?"}, Data: "ap:7:y"}}}
	api.mu.Unlock()
	waitFor(t, func() bool { api.mu.Lock(); defer api.mu.Unlock(); return len(api.edited) == 1 })
	cancel()
	<-done

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.inbound) != 2 || h.inbound[0].Channel != "telegram" || h.inbound[0].ExternalID != "42" {
		t.Fatalf("inbound = %+v (unknown/group users must not reach the handler)", h.inbound)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if !strings.Contains(api.sent[0]["text"].(string), "Your Telegram user id is 99") {
		t.Errorf("unknown user reply = %v", api.sent[0]["text"])
	}
	if api.sent[1]["text"] != "echo: hello" {
		t.Errorf("reply = %v", api.sent[1]["text"])
	}
	kb, _ := json.Marshal(api.sent[2]["reply_markup"])
	if !strings.Contains(string(kb), `"ap:7:y"`) || !strings.Contains(string(kb), `"ap:7:n"`) {
		t.Errorf("approval keyboard = %s", kb)
	}
	if len(h.decisions) != 1 || h.decisions[0] != "telegram:42" || api.answers[0] != "Done #7" {
		t.Errorf("decisions = %v answers = %v", h.decisions, api.answers)
	}
	if !strings.Contains(api.edited[0]["text"].(string), "→ Done #7") {
		t.Errorf("edited = %v", api.edited[0])
	}
	offsets.mu.Lock()
	if offsets.m[offsetKey] != "15" {
		t.Errorf("offset = %q, want 15", offsets.m[offsetKey])
	}
	offsets.mu.Unlock()
}

func TestCallbackFromStrangerRejected(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	h := &fakeHandler{}
	bot := New(Options{Client: NewClient(srv.URL, token, srv.Client()), Handler: h,
		Offsets: &memOffsets{m: map[string]string{}}, AllowedIDs: []int64{42}, PollTimeout: time.Second})
	bot.handleCallback(context.Background(), &CallbackQuery{ID: "x", From: User{ID: 666}, Data: "ap:7:y"})
	if len(h.decisions) != 0 {
		t.Fatal("stranger decided an approval")
	}
}

func TestTokenRedacted(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", token, nil)
	_, err := c.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v (token must be redacted)", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("err = %v", err)
	}
}

func TestParseCallbackAndSplit(t *testing.T) {
	for data, want := range map[string]bool{"ap:7:y": true, "ap:7:n": true, "ap:x:y": false, "ap:7": false, "zz:7:y": false, "ap:-1:y": false} {
		_, _, ok := parseCallback(data)
		if ok != want {
			t.Errorf("parseCallback(%q) ok = %v", data, ok)
		}
	}
	long := strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000)
	parts := splitText(long, 4000)
	if len(parts) != 2 || parts[0] != strings.Repeat("a", 3000) {
		t.Errorf("split = %d parts, first len %d", len(parts), len(parts[0]))
	}
	if got := strings.Join(parts, ""); got != long {
		t.Error("split lost content")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
