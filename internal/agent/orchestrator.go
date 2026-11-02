package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"personal-assistant/internal/llm"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/notify"
	"personal-assistant/internal/tools"
)

// Inbound is a user message from any interface.
type Inbound struct {
	Channel    string // telegram | web
	ExternalID string // chat id / session id
	Text       string
}

// Reply is the assistant's answer.
type Reply struct {
	Text      string
	Agent     string
	RunID     int64
	Approvals []memory.Approval
}

// Options configures the Orchestrator.
type Options struct {
	Store           *memory.Store
	Planner         *Planner
	Executor        *Executor
	Agents          *Registry
	ToolDeps        tools.Deps
	Notifier        notify.Notifier
	Log             *slog.Logger
	HistoryMessages int
	MaxSteps        int
	MaxConcurrent   int
	RunTimeout      time.Duration
	// Status renders a short system status for /status (optional).
	Status func(ctx context.Context) string
	// DigestRepos are reported by the developer digest.
	DigestRepos []string
}

// Orchestrator routes messages to agents and owns the run lifecycle. Every
// interface (Telegram, dashboard, scheduler) goes through it.
type Orchestrator struct {
	o      Options
	sem    chan struct{}
	convMu sync.Map // key -> *sync.Mutex: one run at a time per conversation
}

// New builds an orchestrator.
func New(o Options) *Orchestrator {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Notifier == nil {
		o.Notifier = notify.Multi{}
	}
	return &Orchestrator{o: o, sem: make(chan struct{}, max(o.MaxConcurrent, 1))}
}

// Agents exposes the registry (dashboard).
func (o *Orchestrator) Agents() *Registry { return o.o.Agents }

// Handle processes one inbound message end to end.
func (o *Orchestrator) Handle(ctx context.Context, in Inbound) (Reply, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return Reply{Text: "Send me a message, or /help for commands."}, nil
	}
	if len(text) > 4000 {
		text = text[:4000]
	}

	mu, _ := o.convMu.LoadOrStore(in.Channel+":"+in.ExternalID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	select {
	case o.sem <- struct{}{}:
		defer func() { <-o.sem }()
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, o.o.RunTimeout)
	defer cancel()

	conv, err := o.o.Store.GetOrCreateConversation(ctx, in.Channel, in.ExternalID)
	if err != nil {
		return Reply{}, fmt.Errorf("conversation: %w", err)
	}
	if _, err := o.o.Store.AddMessage(ctx, conv.ID, memory.RoleUser, "", text); err != nil {
		return Reply{}, fmt.Errorf("store message: %w", err)
	}

	origin := Origin{Channel: in.Channel, ExternalID: in.ExternalID}
	var reply Reply
	if strings.HasPrefix(text, "/") {
		reply = o.command(ctx, conv, origin, text)
	} else {
		reply = o.runAgent(ctx, conv, origin, text, "")
	}
	if _, err := o.o.Store.AddMessage(context.WithoutCancel(ctx), conv.ID, memory.RoleAssistant, reply.Agent, reply.Text); err != nil {
		o.o.Log.Warn("store reply", "err", err)
	}
	return reply, nil
}

// runAgent executes the plan/act loop for free text.
func (o *Orchestrator) runAgent(ctx context.Context, conv memory.Conversation, origin Origin, text, forced string) Reply {
	var usage memory.RunUsage
	addUsage := func(u llm.Usage) {
		usage.LLMCalls++
		usage.PromptTokens += u.PromptTokens
		usage.CompletionTokens += u.CompletionTokens
	}

	agentName := forced
	if agentName == "" {
		name, u, err := o.o.Planner.Route(ctx, text, o.o.Agents.List())
		if u != nil {
			addUsage(*u)
		}
		if err != nil {
			runID, _ := o.o.Store.StartRun(ctx, conv.ID, "router", "message", text)
			msg := UserMessage(err)
			o.finish(ctx, runID, memory.RunError, msg, err, usage)
			return Reply{Text: msg, Agent: "router", RunID: runID}
		}
		agentName = name
	}
	ag, ok := o.o.Agents.Get(agentName)
	if !ok {
		ag, _ = o.o.Agents.Get(Assistant)
	}
	runID, err := o.o.Store.StartRun(ctx, conv.ID, ag.Name, "message", text)
	if err != nil {
		o.o.Log.Warn("start run", "err", err)
	}
	o.o.Log.Info("agent run", "run_id", runID, "agent", ag.Name, "channel", origin.Channel)

	pr := Prompt{Agent: ag, Preferences: o.preferences(ctx), History: o.history(ctx, conv.ID), User: text}
	var final string
	var approvals []memory.Approval
	status := memory.RunOK

	for step := 0; step < o.o.MaxSteps && final == ""; step++ {
		d, u, err := o.o.Planner.Decide(ctx, pr)
		addUsage(u)
		if err != nil {
			msg := UserMessage(err)
			st := memory.RunError
			if len(pr.Steps) > 0 {
				// Work was already done: report it rather than a bare error.
				msg = fallbackFromSteps(pr.Steps, err)
			}
			o.finish(ctx, runID, st, msg, err, usage)
			return Reply{Text: msg, Agent: ag.Name, RunID: runID}
		}
		if d.Action == ActionRespond {
			final = d.Message
			break
		}
		out := o.o.Executor.Propose(ctx, runID, origin, ag.Tools, d.Action, d.Args)
		pr.Steps = append(pr.Steps, Step{Tool: d.Action, Args: d.Args, Result: out.ForModel()})
		switch {
		case out.Status == OutcomeAwaitingApproval:
			approvals = append(approvals, *out.Approval)
			status = memory.RunAwaitingApproval
			final = ApprovalPrompt(*out.Approval)
		case out.Status == OutcomeOK && out.Confirmation != "":
			// Deterministic confirmation: no extra LLM call for wording.
			final = out.Confirmation
		}
	}
	if final == "" {
		pr.RespondOnly = true
		d, u, err := o.o.Planner.Decide(ctx, pr)
		addUsage(u)
		if err != nil {
			final = fallbackFromSteps(pr.Steps, err)
			status = memory.RunError
		} else {
			final = d.Message
		}
	}
	if ag.ExternalSources && status == memory.RunOK {
		final = o.labelExternal(ctx, text, final, pr.Steps)
	}
	o.finish(ctx, runID, status, final, nil, usage)
	return Reply{Text: final, Agent: ag.Name, RunID: runID, Approvals: approvals}
}

// labelExternal marks a web-derived answer as external, appends the real
// source URLs found in tool results, and saves it as research knowledge.
// If no web tool succeeded, the answer is labelled as unverified instead.
func (o *Orchestrator) labelExternal(ctx context.Context, query, answer string, steps []Step) string {
	urls := sourceURLs(steps, query, 3)
	if len(urls) == 0 {
		return "⚠️ I could not get web results for this, so this is NOT verified external information:\n" + answer
	}
	body := strings.TrimSpace(answer)
	out := "🌐 From the web:\n" + body + "\n\nSources:\n- " + strings.Join(urls, "\n- ")
	if _, err := o.o.Store.AddKnowledge(ctx, memory.NewKnowledge{
		Title: clip(query, 120), Body: body + "\n\nSources: " + strings.Join(urls, " "),
		Source: memory.SourceResearch, URL: urls[0], Tags: []string{"research"},
	}); err != nil {
		o.o.Log.Warn("save research", "err", err)
	}
	return out
}

// sourceURLs returns up to limit source URLs from successful tool results:
// pages actually fetched first, then search hits ranked by how many query
// terms appear in their title/snippet (search engines sometimes return
// unrelated pages, observed on-device with Wikipedia search).
func sourceURLs(steps []Step, query string, limit int) []string {
	type hit struct {
		url   string
		score int
		order int
	}
	terms := strings.Fields(strings.ToLower(query))
	score := func(text string) int {
		text = strings.ToLower(text)
		n := 0
		for _, t := range terms {
			if len(t) > 2 && strings.Contains(text, strings.Trim(t, "?!.,:;\"'")) {
				n++
			}
		}
		return n
	}
	var fetched []string
	var hits []hit
	seen := map[string]bool{}
	for _, s := range steps {
		if strings.HasPrefix(s.Result, "ERROR") {
			continue
		}
		switch s.Tool {
		case "web_fetch":
			var page struct {
				URL string `json:"url"`
			}
			if json.Unmarshal([]byte(s.Result), &page) == nil && strings.HasPrefix(page.URL, "http") && !seen[page.URL] {
				seen[page.URL] = true
				fetched = append(fetched, page.URL)
			}
		case "web_search":
			var results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			}
			if json.Unmarshal([]byte(s.Result), &results) != nil {
				continue
			}
			for _, r := range results {
				if strings.HasPrefix(r.URL, "http") && !seen[r.URL] {
					seen[r.URL] = true
					hits = append(hits, hit{r.URL, score(r.Title + " " + r.Snippet), len(hits)})
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := fetched
	for _, h := range hits {
		if h.score == 0 && len(out) > 0 {
			break // do not pad citations with unrelated pages
		}
		out = append(out, h.url)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (o *Orchestrator) finish(ctx context.Context, runID int64, status, output string, err error, u memory.RunUsage) {
	if runID == 0 {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if ferr := o.o.Store.FinishRun(context.WithoutCancel(ctx), runID, status, output, msg, u); ferr != nil {
		o.o.Log.Warn("finish run", "run_id", runID, "err", ferr)
	}
}

func (o *Orchestrator) history(ctx context.Context, convID int64) []llm.Message {
	n := o.o.HistoryMessages
	if n == 0 {
		return nil
	}
	msgs, err := o.o.Store.RecentMessages(ctx, convID, n+1)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	msgs = msgs[:len(msgs)-1] // drop the message being answered
	var out []llm.Message
	for _, m := range msgs {
		if m.Role != memory.RoleUser && m.Role != memory.RoleAssistant {
			continue
		}
		if m.Role == memory.RoleUser && strings.HasPrefix(m.Content, "/") {
			continue
		}
		out = append(out, llm.Message{Role: m.Role, Content: clip(m.Content, 300)})
	}
	return out
}

func (o *Orchestrator) preferences(ctx context.Context) map[string]string {
	ps, err := o.o.Store.ListPreferences(ctx, false)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(ps))
	for _, p := range ps {
		out[p.Key] = p.Value
	}
	return out
}

// DecideApproval approves or rejects a request and, if approved, executes
// it exactly once. by describes the decider ("telegram:123", "web").
func (o *Orchestrator) DecideApproval(ctx context.Context, id int64, approve bool, by string) (string, error) {
	a, err := o.o.Store.DecideApproval(ctx, id, approve, by)
	if errors.Is(err, memory.ErrApprovalNotPending) {
		cur, gerr := o.o.Store.GetApproval(ctx, id)
		if gerr != nil {
			return "", gerr
		}
		return fmt.Sprintf("Request #%d is no longer pending (status: %s).", id, cur.Status), nil
	}
	if errors.Is(err, memory.ErrNotFound) {
		return fmt.Sprintf("There is no approval request #%d.", id), nil
	}
	if err != nil {
		return "", err
	}
	var msg string
	if !approve {
		msg = fmt.Sprintf("Rejected #%d: %s. Nothing was changed.", id, a.Summary)
	} else {
		a, out, err := o.o.Executor.ExecuteApproved(ctx, id)
		switch {
		case err != nil:
			msg = fmt.Sprintf("Approved #%d but execution could not be recorded: %v", id, err)
		case out.Status == OutcomeOK:
			msg = fmt.Sprintf("Done #%d: %s.", id, a.Summary)
		default:
			msg = fmt.Sprintf("Approved #%d but it failed: %s", id, out.Error)
		}
	}
	// Tell the originating channel if the decision was made elsewhere.
	if a.Channel != "" && !strings.HasPrefix(by, a.Channel) {
		if err := o.o.Notifier.Notify(ctx, notify.Message{Text: msg}); err != nil {
			o.o.Log.Warn("notify approval outcome", "err", err)
		}
	}
	return msg, nil
}

// ApprovalPrompt is the user-facing text for a pending approval.
func ApprovalPrompt(a memory.Approval) string {
	return fmt.Sprintf("⚠️ This needs your approval (%s): %s\nApprove with /approve %d or reject with /reject %d (expires %s).",
		a.Permission, a.Summary, a.ID, a.ID, a.ExpiresAt.T().Local().Format("02 Jan 15:04 MST"))
}

// DailyPlan builds and returns today's plan (used by /plan and the job).
func (o *Orchestrator) DailyPlan(ctx context.Context, trigger string) string {
	data, err := tools.BuildPlanData(ctx, o.o.ToolDeps)
	if err != nil {
		return "Could not load tasks: " + err.Error()
	}
	if len(data.Pending)+len(data.Overdue)+len(data.Reminders) == 0 {
		return "No pending tasks or reminders. Enjoy your day!"
	}
	runID, _ := o.o.Store.StartRun(ctx, 0, Productivity, trigger, "daily plan")
	raw, _ := json.Marshal(data)
	text, u, err := o.o.Planner.Write(ctx,
		"Write a short prioritized plan for today (max 8 lines, '- ' bullets). Overdue items first, include due times. No preamble.",
		string(raw), 320)
	usage := memory.RunUsage{LLMCalls: 1, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
	if err != nil {
		text = formatPlan(data) + "\n(" + UserMessage(err) + ")"
		o.finish(ctx, runID, memory.RunError, text, err, usage)
		return text
	}
	text = "📋 Plan for today\n" + text
	o.finish(ctx, runID, memory.RunOK, text, nil, usage)
	return text
}

// DevDigest summarises configured GitHub repos and monitors.
func (o *Orchestrator) DevDigest(ctx context.Context, trigger string) string {
	var parts []string
	runID, _ := o.o.Store.StartRun(ctx, 0, Developer, trigger, "developer digest")
	for _, repo := range o.o.DigestRepos {
		out := o.o.Executor.Propose(ctx, runID, Origin{}, nil, "github_repo_status", mustMarshal(map[string]string{"repo": repo}))
		parts = append(parts, repo+": "+out.ForModel())
	}
	out := o.o.Executor.Propose(ctx, runID, Origin{}, nil, "monitor_status", json.RawMessage(`{"hours":24}`))
	parts = append(parts, "monitors: "+out.ForModel())
	data := strings.Join(parts, "\n")
	text, u, err := o.o.Planner.Write(ctx,
		"Summarise this development activity in at most 8 short lines: notable commits, open PRs, releases, any monitor failures.",
		data, 320)
	usage := memory.RunUsage{LLMCalls: 1, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
	if err != nil {
		text = "Developer digest (raw):\n" + clip(data, 1500)
		o.finish(ctx, runID, memory.RunError, text, err, usage)
		return text
	}
	text = "🛠 Developer digest\n" + text
	o.finish(ctx, runID, memory.RunOK, text, nil, usage)
	return text
}

func formatPlan(d tools.PlanDataOut) string {
	var b strings.Builder
	b.WriteString("📋 Plan for today\n")
	for _, t := range d.Overdue {
		fmt.Fprintf(&b, "- OVERDUE #%d %s (due %s)\n", t.ID, t.Title, t.Due)
	}
	for _, r := range d.Reminders {
		fmt.Fprintf(&b, "- ⏰ %s: %s\n", r.At, r.Text)
	}
	for _, t := range d.Pending {
		due := ""
		if t.Due != "" {
			due = " (due " + t.Due + ")"
		}
		fmt.Fprintf(&b, "- #%d %s%s\n", t.ID, t.Title, due)
	}
	return strings.TrimSpace(b.String())
}

func fallbackFromSteps(steps []Step, err error) string {
	if len(steps) == 0 {
		return UserMessage(err)
	}
	last := steps[len(steps)-1]
	if strings.HasPrefix(last.Result, "ERROR: ") {
		return fmt.Sprintf("%s failed: %s", last.Tool, strings.TrimPrefix(last.Result, "ERROR: "))
	}
	return fmt.Sprintf("Done (%s), but I couldn't phrase a summary on this device. Result: %s",
		last.Tool, clip(last.Result, 800))
}

// UserMessage maps internal errors to clear, honest user-facing text.
func UserMessage(err error) string {
	switch {
	case errors.Is(err, llm.ErrBusy):
		return "I'm busy with another request on this phone. Please try again in a minute."
	case errors.Is(err, llm.ErrInsufficientMemory):
		return "The phone is low on free memory, so I didn't run the local model (" + err.Error() + "). Commands like /tasks still work."
	case errors.Is(err, llm.ErrUnavailable):
		return "The local model isn't available right now (it may be loading or restarting). Please try again shortly; /help commands still work."
	case errors.Is(err, context.DeadlineExceeded):
		return "That took too long on this device and was stopped. Try a shorter request or a /command."
	case errors.Is(err, llm.ErrMalformed):
		return "I couldn't produce a valid answer for that. Try rephrasing, or use a /command (see /help)."
	default:
		return "Something went wrong: " + err.Error()
	}
}
