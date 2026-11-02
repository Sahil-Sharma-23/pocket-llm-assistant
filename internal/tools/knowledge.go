package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"personal-assistant/internal/memory"
)

// KnowledgeView is the model-facing knowledge hit.
type KnowledgeView struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	Source  string `json:"source"` // user | research (external) | conversation
	URL     string `json:"url,omitempty"`
	Created string `json:"created"`
}

type QueryIn struct {
	Query string `json:"query"`
}

func (in *QueryIn) Validate() error {
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 300 {
		return errors.New("query must be 1-300 characters")
	}
	return nil
}

func KnowledgeSearch(d Deps) Tool {
	return New(Spec[QueryIn, []KnowledgeView]{
		Name:        "knowledge_search",
		Description: "Full-text search of saved notes, facts and research.",
		Schema:      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, in QueryIn) ([]KnowledgeView, error) {
			hits, err := d.Store.SearchKnowledge(ctx, in.Query, 5)
			if err != nil {
				return nil, err
			}
			out := make([]KnowledgeView, 0, len(hits))
			for _, h := range hits {
				out = append(out, KnowledgeView{ID: h.ID, Title: h.Title, Text: clip(h.Body, 400),
					Source: h.Source, URL: h.URL, Created: h.CreatedAt.In(d.Loc)})
			}
			return out, nil
		},
	})
}

// MessageView is a past conversation hit.
type MessageView struct {
	Role    string `json:"role"`
	Snippet string `json:"snippet"`
	When    string `json:"when"`
}

func ConversationSearch(d Deps) Tool {
	return New(Spec[QueryIn, []MessageView]{
		Name:        "conversation_search",
		Description: "Full-text search of past conversations with the user.",
		Schema:      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, in QueryIn) ([]MessageView, error) {
			hits, err := d.Store.SearchMessages(ctx, in.Query, 6)
			if err != nil {
				return nil, err
			}
			out := make([]MessageView, 0, len(hits))
			for _, h := range hits {
				out = append(out, MessageView{Role: h.Role, Snippet: clip(h.Snippet, 300), When: h.CreatedAt.In(d.Loc)})
			}
			return out, nil
		},
	})
}

type KnowledgeSaveIn struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags,omitempty"`
}

func (in *KnowledgeSaveIn) Validate() error {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Title == "" || len(in.Title) > 200 {
		return errors.New("title must be 1-200 characters")
	}
	if in.Body == "" || len(in.Body) > 20000 {
		return errors.New("body must be 1-20000 characters")
	}
	if len(in.Tags) > 10 {
		return errors.New("at most 10 tags")
	}
	return nil
}

func KnowledgeSave(d Deps) Tool {
	return New(Spec[KnowledgeSaveIn, KnowledgeView]{
		Name:        "knowledge_save",
		Description: "Save a note or fact the user told you, for later recall.",
		Schema: `{"type":"object","properties":{"title":{"type":"string"},"body":{"type":"string"},
			"tags":{"type":"array","items":{"type":"string"}}},"required":["title","body"],"additionalProperties":false}`,
		Permission: Write,
		Timeout:    5 * time.Second,
		Confirm: func(in KnowledgeSaveIn, k KnowledgeView) string {
			return fmt.Sprintf("📝 Saved to memory (#%d) — %s: %s", k.ID, k.Title, clip(in.Body, 200))
		},
		Run: func(ctx context.Context, in KnowledgeSaveIn) (KnowledgeView, error) {
			k, err := d.Store.AddKnowledge(ctx, memory.NewKnowledge{Title: in.Title, Body: in.Body, Source: memory.SourceUser, Tags: in.Tags})
			if err != nil {
				return KnowledgeView{}, err
			}
			return KnowledgeView{ID: k.ID, Title: k.Title, Text: clip(k.Body, 200), Source: k.Source, Created: k.CreatedAt.In(d.Loc)}, nil
		},
	})
}

type KnowledgeIDIn struct {
	ID int64 `json:"id"`
}

func (in *KnowledgeIDIn) Validate() error {
	if in.ID <= 0 {
		return errors.New("id must be positive")
	}
	return nil
}

// KnowledgeDelete is irreversible: Sensitive.
func KnowledgeDelete(d Deps) Tool {
	return New(Spec[KnowledgeIDIn, map[string]any]{
		Name:        "knowledge_delete",
		Description: "Permanently delete a saved knowledge entry by id (requires user approval).",
		Schema:      `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`,
		Permission:  Sensitive,
		Timeout:     5 * time.Second,
		Summarize: func(in KnowledgeIDIn) string {
			if k, err := d.Store.GetKnowledge(context.Background(), in.ID); err == nil {
				return fmt.Sprintf("Permanently delete knowledge #%d %q", k.ID, k.Title)
			}
			return fmt.Sprintf("Permanently delete knowledge #%d", in.ID)
		},
		Run: func(ctx context.Context, in KnowledgeIDIn) (map[string]any, error) {
			if err := d.Store.DeleteKnowledge(ctx, in.ID); err != nil {
				return nil, fmt.Errorf("delete knowledge %d: %w", in.ID, err)
			}
			return map[string]any{"deleted": in.ID}, nil
		},
	})
}

var prefKey = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,40}$`)

type PreferenceSetIn struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (in *PreferenceSetIn) Validate() error {
	in.Key = strings.ToLower(strings.TrimSpace(in.Key))
	if !prefKey.MatchString(in.Key) {
		return errors.New("key must be lowercase letters/digits/_ (max 41 chars)")
	}
	if len(in.Value) > 500 {
		return errors.New("value too long")
	}
	return nil
}

func PreferenceSet(d Deps) Tool {
	return New(Spec[PreferenceSetIn, map[string]string]{
		Name:        "preference_set",
		Description: "Remember a user preference, e.g. key=name value=Sam, key=diet value=vegetarian.",
		Schema: `{"type":"object","properties":{"key":{"type":"string"},"value":{"type":"string"}},
			"required":["key","value"],"additionalProperties":false}`,
		Permission: Write,
		Timeout:    5 * time.Second,
		Confirm: func(in PreferenceSetIn, _ map[string]string) string {
			return fmt.Sprintf("👍 I'll remember your %s: %s", strings.ReplaceAll(in.Key, "_", " "), in.Value)
		},
		Run: func(ctx context.Context, in PreferenceSetIn) (map[string]string, error) {
			return map[string]string{in.Key: in.Value}, d.Store.SetPreference(ctx, in.Key, in.Value)
		},
	})
}

func PreferenceList(d Deps) Tool {
	return New(Spec[EmptyIn, map[string]string]{
		Name:        "preference_list",
		Description: "List remembered user preferences.",
		Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, _ EmptyIn) (map[string]string, error) {
			ps, err := d.Store.ListPreferences(ctx, false)
			out := map[string]string{}
			for _, p := range ps {
				out[p.Key] = p.Value
			}
			return out, err
		},
	})
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
