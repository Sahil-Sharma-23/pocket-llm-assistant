// Package dashboard serves the local, server-rendered web UI.
//
// It is mounted on the loopback-only API server. State-changing requests
// (POST) require an HMAC CSRF token and a same-origin Origin header, so a web
// page open in the phone's browser cannot approve actions or send messages.
package dashboard

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/tools"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Handler is the orchestrator surface the dashboard needs.
type Handler interface {
	Handle(ctx context.Context, in agent.Inbound) (agent.Reply, error)
	DecideApproval(ctx context.Context, id int64, approve bool, by string) (string, error)
}

// Deps are the dashboard's collaborators.
type Deps struct {
	Store   *memory.Store
	Handler Handler
	Agents  *agent.Registry
	Tools   *tools.Registry
	// Status returns the overview data (health, LLM, memory, scheduler).
	Status func(ctx context.Context) StatusView
	// ConfigJSON is the redacted effective configuration, pretty-printed.
	ConfigJSON string
	LogFile    string
	Loc        *time.Location
	Log        *slog.Logger
	// ChatTimeout bounds a dashboard chat request.
	ChatTimeout time.Duration
}

// Dashboard renders pages.
type Dashboard struct {
	d      Deps
	pages  map[string]*template.Template
	secret []byte

	mu      sync.Mutex
	pending bool   // a dashboard chat request is running
	lastErr string // last chat error
}

const webSession = "dashboard"

// New parses templates and creates a per-process CSRF secret.
func New(d Deps) (*Dashboard, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Loc == nil {
		d.Loc = time.Local
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	db := &Dashboard{d: d, pages: map[string]*template.Template{}, secret: secret}
	funcs := template.FuncMap{
		"t":    func(t memory.Time) string { return t.In(d.Loc) },
		"tp":   func(t *time.Time) string { return fmtPtr(t, d.Loc) },
		"tt":   func(t time.Time) string { return fmtTime(t, d.Loc) },
		"clip": clip,
	}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", n, err)
		}
		db.pages[base] = t
	}
	return db, nil
}

// Mount registers routes.
func (db *Dashboard) Mount(mux *http.ServeMux) {
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", db.overview)
	mux.HandleFunc("GET /chat", db.chat)
	mux.HandleFunc("POST /chat", db.csrf(db.chatPost))
	mux.HandleFunc("GET /conversations", db.conversations)
	mux.HandleFunc("GET /conversations/{id}", db.conversation)
	mux.HandleFunc("GET /memory", db.memoryPage)
	mux.HandleFunc("GET /tasks", db.tasks)
	mux.HandleFunc("GET /approvals", db.approvals)
	mux.HandleFunc("POST /approvals/{id}/{decision}", db.csrf(db.decide))
	mux.HandleFunc("GET /jobs", db.jobs)
	mux.HandleFunc("GET /runs", db.runs)
	mux.HandleFunc("GET /runs/{id}", db.run)
	mux.HandleFunc("GET /config", db.config)
	mux.HandleFunc("GET /logs", db.logs)
}

// --- pages ---------------------------------------------------------------------

func (db *Dashboard) overview(w http.ResponseWriter, r *http.Request) {
	st, err := db.d.Store.Stats(r.Context())
	if err != nil {
		db.fail(w, err)
		return
	}
	pending, _ := db.d.Store.ListApprovals(r.Context(), memory.ApprovalPending, 5)
	notes, _ := db.notifications(r.Context(), 5)
	db.render(w, "overview", map[string]any{
		"Stats": st, "Status": db.status(r.Context()), "Agents": db.agentViews(), "Approvals": pending, "Notifications": notes,
	})
}

func (db *Dashboard) chat(w http.ResponseWriter, r *http.Request) {
	conv, err := db.d.Store.GetOrCreateConversation(r.Context(), "web", webSession)
	if err != nil {
		db.fail(w, err)
		return
	}
	msgs, _ := db.d.Store.RecentMessages(r.Context(), conv.ID, 40)
	db.mu.Lock()
	pending, lastErr := db.pending, db.lastErr
	db.mu.Unlock()
	pendingApprovals, _ := db.d.Store.ListApprovals(r.Context(), memory.ApprovalPending, 10)
	db.render(w, "chat", map[string]any{"Messages": msgs, "Pending": pending, "Error": lastErr, "Approvals": pendingApprovals})
}

func (db *Dashboard) chatPost(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	db.mu.Lock()
	if db.pending {
		db.lastErr = "Still working on the previous message."
		db.mu.Unlock()
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	db.pending, db.lastErr = true, ""
	db.mu.Unlock()

	// Inference can take minutes on the phone: run it in the background and
	// let the page refresh, rather than holding the HTTP request open.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), db.d.ChatTimeout)
		defer cancel()
		_, err := db.d.Handler.Handle(ctx, agent.Inbound{Channel: "web", ExternalID: webSession, Text: text})
		db.mu.Lock()
		db.pending = false
		if err != nil {
			db.lastErr = err.Error()
			db.d.Log.Warn("dashboard chat", "err", err)
		}
		db.mu.Unlock()
	}()
	http.Redirect(w, r, "/chat", http.StatusSeeOther)
}

func (db *Dashboard) conversations(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	convs, err := db.d.Store.ListConversations(r.Context(), 50)
	if err != nil {
		db.fail(w, err)
		return
	}
	var hits []memory.MessageHit
	if q != "" {
		hits, _ = db.d.Store.SearchMessages(r.Context(), q, 30)
	}
	db.render(w, "conversations", map[string]any{"Conversations": convs, "Query": q, "Hits": hits})
}

func (db *Dashboard) conversation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conv, err := db.d.Store.GetConversation(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	msgs, _ := db.d.Store.RecentMessages(r.Context(), id, 200)
	db.render(w, "conversation", map[string]any{"Conversation": conv, "Messages": msgs})
}

func (db *Dashboard) memoryPage(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var entries []memory.KnowledgeEntry
	var hits []memory.KnowledgeHit
	if q != "" {
		hits, _ = db.d.Store.SearchKnowledge(r.Context(), q, 30)
	} else {
		entries, _ = db.d.Store.ListKnowledge(r.Context(), 50)
	}
	prefs, _ := db.d.Store.ListPreferences(r.Context(), false)
	db.render(w, "memory", map[string]any{"Query": q, "Entries": entries, "Hits": hits, "Preferences": prefs})
}

func (db *Dashboard) tasks(w http.ResponseWriter, r *http.Request) {
	ts, err := db.d.Store.ListTasks(r.Context(), "", 100)
	if err != nil {
		db.fail(w, err)
		return
	}
	db.render(w, "tasks", map[string]any{"Tasks": ts})
}

func (db *Dashboard) approvals(w http.ResponseWriter, r *http.Request) {
	db.d.Store.ExpireApprovals(r.Context())
	pending, _ := db.d.Store.ListApprovals(r.Context(), memory.ApprovalPending, 50)
	all, _ := db.d.Store.ListApprovals(r.Context(), "", 50)
	db.render(w, "approvals", map[string]any{"Pending": pending, "All": all, "Flash": r.URL.Query().Get("msg")})
}

func (db *Dashboard) decide(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	decision := r.PathValue("decision")
	if err != nil || (decision != "approve" && decision != "reject") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, err := db.d.Handler.DecideApproval(r.Context(), id, decision == "approve", "web")
	if err != nil {
		msg = "Error: " + err.Error()
	}
	http.Redirect(w, r, "/approvals?msg="+url.QueryEscape(clip(msg, 300)), http.StatusSeeOther)
}

func (db *Dashboard) jobs(w http.ResponseWriter, r *http.Request) {
	js, err := db.d.Store.ListJobs(r.Context(), "", 100)
	if err != nil {
		db.fail(w, err)
		return
	}
	sums, _ := db.d.Store.MonitorSummaries(r.Context(), time.Now().Add(-24*time.Hour))
	db.render(w, "jobs", map[string]any{"Jobs": js, "Monitors": sums})
}

func (db *Dashboard) runs(w http.ResponseWriter, r *http.Request) {
	rs, err := db.d.Store.ListRuns(r.Context(), 100)
	if err != nil {
		db.fail(w, err)
		return
	}
	db.render(w, "runs", map[string]any{"Runs": rs})
}

func (db *Dashboard) run(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := db.d.Store.GetRun(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	calls, _ := db.d.Store.ListToolCalls(r.Context(), id)
	db.render(w, "run", map[string]any{"Run": run, "Calls": calls})
}

func (db *Dashboard) config(w http.ResponseWriter, r *http.Request) {
	db.render(w, "config", map[string]any{"Config": db.d.ConfigJSON, "Tools": db.d.Tools.List(), "Agents": db.agentViews()})
}

func (db *Dashboard) logs(w http.ResponseWriter, r *http.Request) {
	lines, err := tail(db.d.LogFile, 200)
	db.render(w, "logs", map[string]any{"File": db.d.LogFile, "Lines": lines, "Error": errString(err)})
}

// --- helpers -------------------------------------------------------------------

type agentView struct {
	agent.Agent
	Perms map[string]string
}

func (db *Dashboard) agentViews() []agentView {
	var out []agentView
	for _, a := range db.d.Agents.List() {
		v := agentView{Agent: a, Perms: map[string]string{}}
		for _, t := range a.Tools {
			if tool, ok := db.d.Tools.Get(t); ok {
				v.Perms[t] = string(tool.Permission)
			}
		}
		out = append(out, v)
	}
	return out
}

func (db *Dashboard) status(ctx context.Context) *StatusView {
	if db.d.Status == nil {
		return nil
	}
	v := db.d.Status(ctx)
	return &v
}

func (db *Dashboard) notifications(ctx context.Context, n int) ([]memory.Message, error) {
	c, err := db.d.Store.GetOrCreateConversation(ctx, "system", "notifications")
	if err != nil {
		return nil, err
	}
	return db.d.Store.RecentMessages(ctx, c.ID, n)
}

func (db *Dashboard) token() string {
	m := hmac.New(sha256.New, db.secret)
	m.Write([]byte("dashboard-csrf"))
	return hex.EncodeToString(m.Sum(nil))
}

// csrf rejects cross-site POSTs: the form must carry the per-process token
// and any Origin header must match the Host.
func (db *Dashboard) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if !hmac.Equal([]byte(r.FormValue("csrf")), []byte(db.token())) {
			http.Error(w, "invalid or missing CSRF token; reload the page", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (db *Dashboard) render(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := db.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	data["Page"] = page
	data["CSRF"] = db.token()
	data["Now"] = time.Now().In(db.d.Loc).Format("Mon 02 Jan 15:04")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := t.Execute(w, data); err != nil {
		db.d.Log.Error("render", "page", page, "err", err)
	}
}

func (db *Dashboard) fail(w http.ResponseWriter, err error) {
	db.d.Log.Error("dashboard", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func tail(path string, n int) ([]string, error) {
	if path == "" {
		return nil, errors.New("logging to stderr; set log.file to view logs here")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 256<<10 {
		f.Seek(st.Size()-256<<10, io.SeekStart)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 { // newest first
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, sc.Err()
}

func fmtPtr(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "—"
	}
	return fmtTime(*t, loc)
}

func fmtTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
