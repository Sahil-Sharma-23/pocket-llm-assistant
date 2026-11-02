package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"personal-assistant/internal/memory"
	"personal-assistant/internal/tools"
)

// HelpText lists the deterministic commands. Commands do not need the LLM,
// so they stay fast and keep working when the model is unavailable.
const HelpText = `Commands (fast, no AI needed):
/tasks — pending tasks
/task <title> [@ YYYY-MM-DD HH:MM] — add a task
/done <id> — complete a task
/deltask <id> — delete a task (needs approval)
/remind <in 30m | 18:00 | YYYY-MM-DD HH:MM> <text> — set a reminder
/reminders — upcoming reminders
/plan — today's plan
/note <text> — save a note
/recall <query> — search notes and past chats
/search <query> — research the web (summarised locally)
/github <owner/repo> — repository status
/check <url> — is a site/API up?
/monitors — uptime of configured monitors
/digest — developer digest
/approvals — pending approval requests
/approve <id> · /reject <id>
/status — assistant status
Anything else is answered by the local AI agents.`

var cmdRe = regexp.MustCompile(`^/([a-zA-Z_]+)(?:@\S+)?\s*(.*)$`)

func (o *Orchestrator) command(ctx context.Context, conv memory.Conversation, origin Origin, text string) Reply {
	m := cmdRe.FindStringSubmatch(text)
	if m == nil {
		return Reply{Text: HelpText, Agent: "command"}
	}
	cmd, arg := strings.ToLower(m[1]), strings.TrimSpace(m[2])
	runID, _ := o.o.Store.StartRun(ctx, conv.ID, "command", "command", text)
	reply := o.dispatch(ctx, conv, origin, runID, cmd, arg)
	if reply.Agent == "" {
		reply.Agent = "command"
	}
	if reply.RunID == 0 {
		status := memory.RunOK
		if len(reply.Approvals) > 0 {
			status = memory.RunAwaitingApproval
		}
		o.finish(ctx, runID, status, reply.Text, nil, memory.RunUsage{})
		reply.RunID = runID
	} else {
		// The command delegated to an agent run; close the command record.
		o.finish(ctx, runID, memory.RunOK, "delegated to run "+strconv.FormatInt(reply.RunID, 10), nil, memory.RunUsage{})
	}
	return reply
}

func (o *Orchestrator) dispatch(ctx context.Context, conv memory.Conversation, origin Origin, runID int64, cmd, arg string) Reply {
	call := func(tool string, args any) Outcome {
		raw, _ := json.Marshal(args)
		return o.o.Executor.Propose(ctx, runID, origin, nil, tool, raw)
	}
	need := func(usage string) Reply { return Reply{Text: "Usage: " + usage} }

	switch cmd {
	case "start", "help":
		return Reply{Text: "Hi! I'm your local-first assistant running on your phone.\n\n" + HelpText}

	case "tasks":
		out := call("task_list", map[string]string{"status": "pending"})
		return Reply{Text: renderTasks(out)}

	case "task":
		if arg == "" {
			return need("/task <title> [@ YYYY-MM-DD HH:MM]")
		}
		args := map[string]any{"title": arg}
		if title, due, ok := strings.Cut(arg, "@"); ok {
			args["title"], args["due"] = strings.TrimSpace(title), strings.TrimSpace(due)
		}
		out := call("task_create", args)
		if out.Status != OutcomeOK {
			return Reply{Text: "Could not add task: " + out.Error}
		}
		var t tools.TaskView
		json.Unmarshal(out.Output, &t)
		due := ""
		if t.Due != "" {
			due = " (due " + t.Due + ")"
		}
		return Reply{Text: fmt.Sprintf("✅ Added task #%d: %s%s", t.ID, t.Title, due)}

	case "done":
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return need("/done <task id>")
		}
		out := call("task_complete", map[string]int64{"id": id})
		if out.Status != OutcomeOK {
			return Reply{Text: "Could not complete task: " + out.Error}
		}
		return Reply{Text: fmt.Sprintf("🎉 Completed task #%d.", id)}

	case "deltask":
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return need("/deltask <task id>")
		}
		return approvalReply(call("task_delete", map[string]int64{"id": id}))

	case "remind":
		in, err := parseRemind(arg)
		if err != nil {
			return need("/remind <in 30m | 18:00 | YYYY-MM-DD HH:MM> <text>  (" + err.Error() + ")")
		}
		out := call("reminder_create", in)
		if out.Status != OutcomeOK {
			return Reply{Text: "Could not set reminder: " + out.Error}
		}
		var r tools.ReminderView
		json.Unmarshal(out.Output, &r)
		return Reply{Text: fmt.Sprintf("⏰ Reminder #%d set for %s: %s", r.ID, r.At, r.Text)}

	case "reminders":
		out := call("reminder_list", map[string]any{})
		var rs []tools.ReminderView
		json.Unmarshal(out.Output, &rs)
		if len(rs) == 0 {
			return Reply{Text: "No upcoming reminders."}
		}
		var b strings.Builder
		b.WriteString("⏰ Upcoming reminders\n")
		for _, r := range rs {
			fmt.Fprintf(&b, "#%d %s — %s\n", r.ID, r.At, r.Text)
		}
		return Reply{Text: strings.TrimSpace(b.String())}

	case "plan":
		return Reply{Text: o.DailyPlan(ctx, "command"), Agent: Productivity}

	case "note":
		if arg == "" {
			return need("/note <text>")
		}
		out := call("knowledge_save", map[string]string{"title": clip(firstLine(arg), 60), "body": arg})
		if out.Status != OutcomeOK {
			return Reply{Text: "Could not save note: " + out.Error}
		}
		var k tools.KnowledgeView
		json.Unmarshal(out.Output, &k)
		return Reply{Text: fmt.Sprintf("📝 Saved note #%d.", k.ID)}

	case "recall":
		if arg == "" {
			return need("/recall <query>")
		}
		return Reply{Text: o.recall(ctx, runID, origin, arg)}

	case "search":
		if arg == "" {
			return need("/search <query>")
		}
		if _, ok := o.o.Agents.Get(Research); !ok {
			return Reply{Text: "Research is disabled in the configuration."}
		}
		return o.runAgent(ctx, conv, origin, arg, Research)

	case "github":
		if arg == "" {
			return need("/github <owner/repo>")
		}
		out := call("github_repo_status", map[string]string{"repo": arg})
		if out.Status != OutcomeOK {
			return Reply{Text: "GitHub check failed: " + out.Error}
		}
		var st tools.RepoStatus
		json.Unmarshal(out.Output, &st)
		return Reply{Text: renderRepo(st)}

	case "check":
		if arg == "" {
			return need("/check <url>")
		}
		if !strings.Contains(arg, "://") {
			arg = "https://" + arg
		}
		out := call("http_check", map[string]string{"url": arg})
		if out.Status != OutcomeOK {
			return Reply{Text: "Check failed: " + out.Error}
		}
		var r tools.CheckResult
		json.Unmarshal(out.Output, &r)
		if r.OK {
			return Reply{Text: fmt.Sprintf("🟢 %s is up (HTTP %d, %d ms).", r.URL, r.StatusCode, r.LatencyMS)}
		}
		return Reply{Text: fmt.Sprintf("🔴 %s is DOWN: %s (%d ms).", r.URL, r.Error, r.LatencyMS)}

	case "monitors":
		out := call("monitor_status", map[string]int{"hours": 24})
		var ms []tools.MonitorView
		json.Unmarshal(out.Output, &ms)
		if len(ms) == 0 {
			return Reply{Text: "No monitor results yet. Configure developer.monitors in config.json."}
		}
		var b strings.Builder
		b.WriteString("📈 Monitors (24h)\n")
		for _, m := range ms {
			icon := "🟢"
			if !m.LastOK {
				icon = "🔴"
			}
			fmt.Fprintf(&b, "%s %s: %.1f%% up, avg %d ms, last %s %s\n", icon, m.Name, m.UptimePct, m.AvgLatencyMS, m.LastChecked, m.LastError)
		}
		return Reply{Text: strings.TrimSpace(b.String())}

	case "digest":
		return Reply{Text: o.DevDigest(ctx, "command"), Agent: Developer}

	case "approvals":
		as, err := o.o.Store.ListApprovals(ctx, memory.ApprovalPending, 10)
		if err != nil {
			return Reply{Text: "Could not list approvals: " + err.Error()}
		}
		if len(as) == 0 {
			return Reply{Text: "No pending approval requests."}
		}
		var b strings.Builder
		for _, a := range as {
			b.WriteString(ApprovalPrompt(a) + "\n\n")
		}
		return Reply{Text: strings.TrimSpace(b.String()), Approvals: as}

	case "approve", "reject":
		id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if err != nil {
			return need("/" + cmd + " <id>")
		}
		msg, err := o.DecideApproval(ctx, id, cmd == "approve", origin.Channel+":"+origin.ExternalID)
		if err != nil {
			return Reply{Text: "Error: " + err.Error()}
		}
		return Reply{Text: msg}

	case "status":
		if o.o.Status != nil {
			return Reply{Text: o.o.Status(ctx)}
		}
		return Reply{Text: "Running."}
	}
	return Reply{Text: "Unknown command /" + cmd + ".\n\n" + HelpText}
}

func (o *Orchestrator) recall(ctx context.Context, runID int64, origin Origin, q string) string {
	var b strings.Builder
	out := o.o.Executor.Propose(ctx, runID, origin, nil, "knowledge_search", mustMarshal(map[string]string{"query": q}))
	var ks []tools.KnowledgeView
	json.Unmarshal(out.Output, &ks)
	for _, k := range ks {
		src := "your note"
		if k.Source == memory.SourceResearch {
			src = "web research"
		}
		fmt.Fprintf(&b, "📝 #%d %s (%s, %s): %s\n", k.ID, k.Title, src, k.Created, clip(k.Text, 200))
	}
	out = o.o.Executor.Propose(ctx, runID, origin, nil, "conversation_search", mustMarshal(map[string]string{"query": q}))
	var ms []tools.MessageView
	json.Unmarshal(out.Output, &ms)
	for _, m := range ms {
		fmt.Fprintf(&b, "💬 %s (%s): %s\n", m.When, m.Role, m.Snippet)
	}
	if b.Len() == 0 {
		return "Nothing found for " + strconv.Quote(q) + "."
	}
	return strings.TrimSpace(b.String())
}

var relRe = regexp.MustCompile(`^in\s+(\d+)\s*(m|min|mins|minutes?|h|hr|hrs|hours?|d|days?)\s+(.+)$`)

// parseRemind parses "/remind" arguments into a reminder_create input.
func parseRemind(arg string) (tools.ReminderCreateIn, error) {
	if m := relRe.FindStringSubmatch(strings.ToLower(arg)); m != nil {
		n, _ := strconv.Atoi(m[1])
		mult := 1
		switch m[2][0] {
		case 'h':
			mult = 60
		case 'd':
			mult = 60 * 24
		}
		text := strings.TrimSpace(arg[len(arg)-len(m[3]):])
		return tools.ReminderCreateIn{Text: text, InMinutes: n * mult}, nil
	}
	fields := strings.Fields(arg)
	if len(fields) >= 3 && isDate(fields[0]) && isClock(fields[1]) {
		return tools.ReminderCreateIn{At: fields[0] + " " + fields[1], Text: strings.Join(fields[2:], " ")}, nil
	}
	if len(fields) >= 2 && isClock(fields[0]) {
		return tools.ReminderCreateIn{At: fields[0], Text: strings.Join(fields[1:], " ")}, nil
	}
	return tools.ReminderCreateIn{}, fmt.Errorf("could not understand the time")
}

func isDate(s string) bool  { _, err := time.Parse("2006-01-02", s); return err == nil }
func isClock(s string) bool { _, err := time.Parse("15:04", s); return err == nil }

func approvalReply(out Outcome) Reply {
	switch out.Status {
	case OutcomeAwaitingApproval:
		return Reply{Text: ApprovalPrompt(*out.Approval), Approvals: []memory.Approval{*out.Approval}}
	case OutcomeOK:
		return Reply{Text: "Done."}
	default:
		return Reply{Text: "Could not do that: " + out.Error}
	}
}

func renderTasks(out Outcome) string {
	if out.Status != OutcomeOK {
		return "Could not list tasks: " + out.Error
	}
	var ts []tools.TaskView
	json.Unmarshal(out.Output, &ts)
	if len(ts) == 0 {
		return "No pending tasks. 🎉"
	}
	var b strings.Builder
	b.WriteString("📋 Pending tasks\n")
	for _, t := range ts {
		pr := map[int]string{1: "❗ ", 2: "", 3: "↓ "}[t.Priority]
		due := ""
		if t.Due != "" {
			due = " — due " + t.Due
		}
		fmt.Fprintf(&b, "#%d %s%s%s\n", t.ID, pr, t.Title, due)
	}
	return strings.TrimSpace(b.String())
}

func renderRepo(st tools.RepoStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📦 %s — ★%d, %d open PRs, %d open issues+PRs, last push %s", st.Repo, st.Stars, st.OpenPRs, st.OpenIssuesPRs, st.PushedAt)
	if st.LatestRelease != "" {
		fmt.Fprintf(&b, ", latest release %s", st.LatestRelease)
	}
	b.WriteString("\nRecent commits:\n")
	for _, c := range st.RecentCommits {
		fmt.Fprintf(&b, "• %s %s (%s, %s)\n", c.SHA, c.Message, c.Author, c.Date)
	}
	return strings.TrimSpace(b.String())
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
