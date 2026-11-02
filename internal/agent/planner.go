package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"personal-assistant/internal/llm"
	"personal-assistant/internal/tools"
)

// ActionRespond is the decision that ends a run with a message.
const ActionRespond = "respond"

// Decision is the model's next step: a tool call or a final response.
type Decision struct {
	Action  string          `json:"action"`
	Args    json.RawMessage `json:"args,omitempty"`
	Message string          `json:"message,omitempty"`
}

// Step is an executed tool call fed back to the model.
type Step struct {
	Tool   string
	Args   json.RawMessage
	Result string // JSON output or "ERROR: ..."
}

// Planner turns conversation state into LLM decisions.
//
// Prompts are deliberately small and ordered static-first (instructions and
// tool list, then dynamic context) so llama.cpp's prompt cache can reuse the
// prefix: prompt processing runs at only ~8 tok/s on a mid-range reference phone.
type Planner struct {
	LLM       llm.Provider
	Tools     *tools.Registry
	Loc       *time.Location
	Now       func() time.Time
	MaxTokens int
}

// --- routing -------------------------------------------------------------------

var routeRules = []struct {
	agent string
	re    *regexp.Regexp
}{
	{Developer, regexp.MustCompile(`(?i)\b(github|repo(sitory)?|commits?|pull requests?|PRs?|uptime|is .{1,60} (up|down)|api health|monitors?|deploy(ment)?s?)\b`)},
	{Productivity, regexp.MustCompile(`(?i)\b(tasks?|to-?dos?|remind(er|ers)?|plan (my|the) day|daily plan|deadline|agenda|schedule)\b`)},
	{Knowledge, regexp.MustCompile(`(?i)\b(remember|recall|note that|save (this|that|a note)|what did (i|we)|did i (tell|say|mention)|forget|my preferences?|preference|what('s| is| are| was| were) my|do you know my|my \w+( \w+)? (code|password|pin|number|address|birthday))\b`)},
	{Research, regexp.MustCompile(`(?i)\b(search|look ?up|research|google|wikipedia|find (out|info(rmation)?)|latest news|news about)\b`)},
}

// RouteByKeywords returns the single agent whose keywords match, or "" if
// zero or several match (then the LLM router decides).
func RouteByKeywords(text string, available []string) string {
	var hit []string
	for _, r := range routeRules {
		if slices.Contains(available, r.agent) && r.re.MatchString(text) {
			hit = append(hit, r.agent)
		}
	}
	if len(hit) == 1 {
		return hit[0]
	}
	return ""
}

// Route chooses an agent: keywords first (free), LLM enum otherwise.
func (p *Planner) Route(ctx context.Context, text string, agents []Agent) (string, *llm.Usage, error) {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	if a := RouteByKeywords(text, names); a != "" {
		return a, nil, nil
	}
	var b strings.Builder
	b.WriteString("Pick the agent for the user's message.\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "%s: %s\n", a.Name, a.Description)
	}
	schema := mustMarshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"agent": map[string]any{"type": "string", "enum": names}},
		"required":             []string{"agent"},
		"additionalProperties": false,
	})
	resp, err := p.LLM.Chat(ctx, llm.Request{
		Messages:    []llm.Message{{Role: llm.RoleSystem, Content: b.String()}, {Role: llm.RoleUser, Content: clip(text, 400)}},
		MaxTokens:   24,
		Temperature: llm.Temp(0),
		JSONSchema:  schema,
	})
	if err != nil {
		return "", nil, err
	}
	var out struct {
		Agent string `json:"agent"`
	}
	if err := llm.DecodeJSON(resp, &out); err != nil || !slices.Contains(names, out.Agent) {
		return Assistant, &resp.Usage, nil // safe fallback: no side-effecting tools
	}
	return out.Agent, &resp.Usage, nil
}

// --- decisions -----------------------------------------------------------------

// DecisionSchema builds the oneOf schema: one branch per allowed tool (with
// that tool's own argument schema) plus "respond". With respondOnly, only the
// respond branch is allowed (used to force a final answer).
func (p *Planner) DecisionSchema(agent Agent, respondOnly, requireTool bool, onlyTool string) json.RawMessage {
	respond := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"const": ActionRespond},
			// maxLength bounds the grammar: a runaway answer once hit the
			// max_tokens cap on-device and was truncated into invalid JSON.
			"message": map[string]any{"type": "string", "maxLength": 900},
		},
		"required":             []string{"action", "message"},
		"additionalProperties": false,
	}
	branches := []any{}
	if !respondOnly {
		names := agent.Tools
		if onlyTool != "" {
			names = []string{onlyTool}
		}
		for _, name := range names {
			t, ok := p.Tools.Get(name)
			if !ok {
				continue
			}
			branches = append(branches, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"const": name},
					"args":   t.Schema,
				},
				"required":             []string{"action", "args"},
				"additionalProperties": false,
			})
		}
	}
	if !requireTool || len(branches) == 0 {
		branches = append(branches, respond)
	}
	if len(branches) == 1 {
		return mustMarshal(branches[0])
	}
	return mustMarshal(map[string]any{"oneOf": branches})
}

// Prompt holds what the planner needs for one decision.
type Prompt struct {
	Agent       Agent
	Preferences map[string]string
	History     []llm.Message // prior turns (already clipped)
	User        string
	Steps       []Step
	RespondOnly bool
}

// Messages renders the chat messages for a decision.
func (p *Planner) Messages(pr Prompt) []llm.Message {
	var sys strings.Builder
	fmt.Fprintf(&sys, "You are the %s agent of a private assistant that runs entirely on the user's phone.\n%s\n",
		pr.Agent.Name, pr.Agent.Instructions)
	if len(pr.Agent.Tools) > 0 && !pr.RespondOnly {
		sys.WriteString("Tools:\n")
		for _, name := range pr.Agent.Tools {
			if t, ok := p.Tools.Get(name); ok {
				fmt.Fprintf(&sys, "- %s(%s): %s\n", t.Name, argSignature(t.Schema), t.Description)
			}
		}
		sys.WriteString(`Output JSON only: {"action":"<tool>","args":{...}} to use a tool, or {"action":"respond","message":"..."} to answer. ` +
			"Use a tool when the request needs one; never invent tool results.\n")
	} else {
		sys.WriteString(`Output JSON only: {"action":"respond","message":"..."}.` + "\n")
	}
	if len(pr.Preferences) > 0 {
		keys := make([]string, 0, len(pr.Preferences))
		for k := range pr.Preferences {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sys.WriteString("Known user preferences:")
		for _, k := range keys[:min(len(keys), 10)] {
			fmt.Fprintf(&sys, " %s=%s;", k, clip(pr.Preferences[k], 60))
		}
		sys.WriteString("\n")
	}

	msgs := []llm.Message{{Role: llm.RoleSystem, Content: sys.String()}}
	msgs = append(msgs, pr.History...)
	now := p.Now().In(p.Loc)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser,
		Content: fmt.Sprintf("[now: %s, %s]\n%s", now.Format("Mon 2006-01-02 15:04"), p.Loc.String(), pr.User)})
	for _, s := range pr.Steps {
		call, _ := json.Marshal(Decision{Action: s.Tool, Args: s.Args})
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: string(call)},
			llm.Message{Role: llm.RoleUser, Content: "Tool result for " + s.Tool + ": " + clip(s.Result, 2000)})
	}
	if pr.RespondOnly {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "Now answer the user using the tool results above."})
	}
	return mergeConsecutive(msgs)
}

// Decide asks the model for the next step.
func (p *Planner) Decide(ctx context.Context, pr Prompt) (Decision, llm.Usage, error) {
	first := len(pr.Steps) == 0 && !pr.RespondOnly
	firstTool := ""
	if first {
		firstTool = pr.Agent.FirstTool
	}
	resp, err := p.LLM.Chat(ctx, llm.Request{
		Messages:    p.Messages(pr),
		MaxTokens:   p.MaxTokens,
		Temperature: llm.Temp(0.2),
		JSONSchema:  p.DecisionSchema(pr.Agent, pr.RespondOnly, first && pr.Agent.MustUseTool, firstTool),
	})
	if err != nil {
		return Decision{}, llm.Usage{}, err
	}
	var d Decision
	if err := llm.DecodeJSON(resp, &d); err != nil {
		return Decision{}, resp.Usage, err
	}
	d.Message = strings.TrimSpace(d.Message)
	if d.Action == ActionRespond && d.Message == "" {
		return Decision{}, resp.Usage, fmt.Errorf("%w: empty message", llm.ErrMalformed)
	}
	return d, resp.Usage, nil
}

// Write produces free text (no schema), e.g. daily plans and digests.
func (p *Planner) Write(ctx context.Context, instruction, data string, maxTokens int) (string, llm.Usage, error) {
	now := p.Now().In(p.Loc)
	resp, err := p.LLM.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are a concise personal assistant running on the user's phone. Plain text, no markdown tables."},
			{Role: llm.RoleUser, Content: fmt.Sprintf("[now: %s]\n%s\n\nData:\n%s", now.Format("Mon 2006-01-02 15:04"), instruction, clip(data, 3000))},
		},
		MaxTokens:   maxTokens,
		Temperature: llm.Temp(0.3),
	})
	if err != nil {
		return "", llm.Usage{}, err
	}
	text := strings.TrimSpace(resp.Content)
	if text == "" {
		return "", resp.Usage, fmt.Errorf("%w: empty text", llm.ErrMalformed)
	}
	return text, resp.Usage, nil
}

// argSignature renders "title, notes?, due?" from a JSON schema.
func argSignature(schema json.RawMessage) string {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}
	names := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := slices.Contains(s.Required, names[i]), slices.Contains(s.Required, names[j])
		if ri != rj {
			return ri
		}
		return names[i] < names[j]
	})
	for i, n := range names {
		if !slices.Contains(s.Required, n) {
			names[i] = n + "?"
		}
	}
	return strings.Join(names, ", ")
}

// mergeConsecutive joins adjacent same-role messages: Gemma's chat template
// requires strictly alternating user/assistant turns.
func mergeConsecutive(in []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range in {
		if n := len(out); n > 0 && out[n-1].Role == m.Role && m.Role != llm.RoleSystem {
			out[n-1].Content += "\n" + m.Content
			continue
		}
		out = append(out, m)
	}
	// The first non-system turn must be the user's.
	if len(out) > 1 && out[1].Role == llm.RoleAssistant {
		out = append(out[:1], out[2:]...)
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
