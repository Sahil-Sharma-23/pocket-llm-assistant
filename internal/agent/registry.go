// Package agent implements the orchestrator, the specialised agents, the
// LLM planner and the permission-enforcing executor. All agents run inside
// the single Go process; an "agent" is a prompt plus an allowed tool subset.
package agent

import (
	"fmt"
	"slices"
)

// Agent names.
const (
	Assistant    = "assistant" // personal assistant: small talk, fallback
	Productivity = "productivity"
	Research     = "research"
	Developer    = "developer"
	Knowledge    = "knowledge"
)

// Agent is a specialised persona with a restricted toolset.
type Agent struct {
	Name        string
	Description string // used by the LLM router
	// Instructions are static (cache-friendly) agent-specific rules.
	Instructions string
	Tools        []string
	// MustUseTool removes "respond" from the first decision's grammar, so
	// the agent cannot answer from the model's own (possibly stale or
	// invented) knowledge before consulting a tool. Verified necessary
	// on-device: without it the research agent skipped web_search.
	MustUseTool bool
	// FirstTool pins the first decision to one tool (e.g. research must
	// start with web_search; verified on-device that a free choice let the
	// model "save" invented text instead of searching).
	FirstTool string
	// ExternalSources marks answers built from web content: the orchestrator
	// prefixes them, appends the real source URLs from tool results, and
	// saves the answer as research knowledge. None of this is left to the
	// model, so external information is always labelled.
	ExternalSources bool
}

// Registry holds agents in routing order.
type Registry struct {
	agents []Agent
}

// NewRegistry validates agents against the available tools.
func NewRegistry(agents []Agent, hasTool func(string) bool) (*Registry, error) {
	seen := map[string]bool{}
	for _, a := range agents {
		if a.Name == "" || seen[a.Name] {
			return nil, fmt.Errorf("agent %q: empty or duplicate name", a.Name)
		}
		seen[a.Name] = true
		for _, t := range a.Tools {
			if !hasTool(t) {
				return nil, fmt.Errorf("agent %q: unknown tool %q", a.Name, t)
			}
		}
	}
	if !seen[Assistant] {
		return nil, fmt.Errorf("the %q agent is required", Assistant)
	}
	return &Registry{agents: agents}, nil
}

// Get returns an agent by name.
func (r *Registry) Get(name string) (Agent, bool) {
	i := slices.IndexFunc(r.agents, func(a Agent) bool { return a.Name == name })
	if i < 0 {
		return Agent{}, false
	}
	return r.agents[i], true
}

// List returns all agents.
func (r *Registry) List() []Agent { return slices.Clone(r.agents) }

// Names returns agent names.
func (r *Registry) Names() []string {
	out := make([]string, len(r.agents))
	for i, a := range r.agents {
		out[i] = a.Name
	}
	return out
}

// DefaultAgents defines the five initial agents. Tools for disabled features
// (research, GitHub) are dropped.
func DefaultAgents(hasTool func(string) bool) []Agent {
	keep := func(ts ...string) []string {
		var out []string
		for _, t := range ts {
			if hasTool(t) {
				out = append(out, t)
			}
		}
		return out
	}
	return []Agent{
		{
			Name:        Productivity,
			MustUseTool: true,
			Description: "tasks, to-dos, reminders, daily plans, pending activities",
			Instructions: "You manage the user's tasks and reminders. Convert relative times " +
				"(\"tomorrow 6pm\", \"in 2 hours\") into at='YYYY-MM-DD HH:MM' using the current time given " +
				"in the message, or in_minutes. After a tool succeeds, confirm briefly with the exact time.",
			Tools: keep("task_create", "task_list", "task_complete", "task_delete", "plan_data",
				"reminder_create", "reminder_list", "reminder_cancel"),
		},
		{
			Name:            Research,
			MustUseTool:     true,
			FirstTool:       "web_search",
			ExternalSources: true,
			Description:     "look up information on the web, news, facts about the world",
			Instructions: "You research topics on the web. After web_search, optionally web_fetch the most " +
				"promising result, then answer in 3-6 sentences using ONLY the tool results, not your own " +
				"memory. If the results do not answer the question, say so. Sources are added automatically.",
			Tools: keep("web_search", "web_fetch"),
		},
		{
			Name:        Developer,
			MustUseTool: true,
			Description: "GitHub repositories, commits, pull requests, website uptime, API health",
			Instructions: "You monitor development activity. Use github_repo_status for repositories " +
				"(owner/name), http_check to test a URL now, monitor_status for configured monitors. " +
				"Report concrete numbers; never guess.",
			Tools: keep("github_repo_status", "http_check", "monitor_status"),
		},
		{
			Name:        Knowledge,
			MustUseTool: true,
			Description: "remember notes/facts, recall saved information or past conversations, preferences",
			Instructions: "You maintain the user's personal knowledge. Save facts with knowledge_save, " +
				"preferences with preference_set, and search knowledge_search/conversation_search before " +
				"answering recall questions. Only state what the tools returned; say when nothing was found. " +
				"Results with source 'research' came from the web.",
			Tools: keep("knowledge_search", "conversation_search", "knowledge_save", "knowledge_delete",
				"preference_set", "preference_list"),
		},
		{
			Name:        Assistant,
			Description: "greetings, small talk, general questions answerable without tools",
			Instructions: "You are a friendly, concise personal assistant. Answer directly. If the " +
				"user asks for something you cannot do, suggest a /command from /help.",
			Tools: keep("knowledge_search"),
		},
	}
}
