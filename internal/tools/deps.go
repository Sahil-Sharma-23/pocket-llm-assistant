package tools

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"personal-assistant/internal/memory"
	"personal-assistant/internal/research"
)

// Deps are the collaborators tools need.
type Deps struct {
	Store    *memory.Store
	Research *research.Client // nil = research disabled
	GitHub   *GitHub
	// PublicHTTP is used for model-proposed URLs (public internet only).
	PublicHTTP *http.Client
	Loc        *time.Location
	Now        func() time.Time
	// WakeScheduler is called after jobs change so the scheduler re-plans.
	WakeScheduler func()
	// RepoAllowlist restricts github_repo_status to configured repos when
	// non-empty (empty = any public repo).
	RepoAllowlist []string
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) wake() {
	if d.WakeScheduler != nil {
		d.WakeScheduler()
	}
}

// ParseLocalTime parses a user/model supplied time in loc. Accepted forms:
// "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02" (09:00), "15:04"
// (today, or tomorrow if already past).
func ParseLocalTime(s string, loc *time.Location, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	now = now.In(loc)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.Add(9 * time.Hour), nil
	}
	if t, err := time.ParseInLocation("15:04", s, loc); err == nil {
		at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q (use YYYY-MM-DD HH:MM)", s)
}

// FormatLocal renders t for humans in loc.
func FormatLocal(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon 02 Jan 2006 15:04")
}

// All returns every tool, skipping those whose dependencies are missing.
func All(d Deps) []Tool {
	ts := []Tool{
		TaskCreate(d), TaskList(d), TaskComplete(d), TaskDelete(d), PlanData(d),
		ReminderCreate(d), ReminderList(d), ReminderCancel(d),
		KnowledgeSearch(d), ConversationSearch(d), KnowledgeSave(d), KnowledgeDelete(d),
		PreferenceSet(d), PreferenceList(d),
		HTTPCheck(d), MonitorStatus(d),
	}
	if d.Research != nil {
		ts = append(ts, WebSearch(d), WebFetch(d), ResearchSave(d))
	}
	if d.GitHub != nil {
		ts = append(ts, GitHubRepoStatus(d))
	}
	return ts
}
