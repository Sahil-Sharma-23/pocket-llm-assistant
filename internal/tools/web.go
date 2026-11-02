package tools

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"personal-assistant/internal/memory"
	"personal-assistant/internal/research"
)

func WebSearch(d Deps) Tool {
	return New(Spec[QueryIn, []research.Result]{
		Name:        "web_search",
		Description: "Search the web (Wikipedia, DuckDuckGo). Results are EXTERNAL information.",
		Schema:      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     25 * time.Second,
		Retry:       RetryPolicy{Attempts: 2, Backoff: time.Second},
		Run: func(ctx context.Context, in QueryIn) ([]research.Result, error) {
			res, err := d.Research.Search(ctx, in.Query)
			if err != nil {
				return nil, Retryable(err)
			}
			for i := range res {
				res[i].Snippet = clip(res[i].Snippet, 240)
			}
			return res, nil
		},
	})
}

type URLIn struct {
	URL string `json:"url"`
}

func (in *URLIn) Validate() error {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	in.URL = u.String()
	return nil
}

// WebFetchOut is a fetched page excerpt.
type WebFetchOut struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Excerpt string `json:"excerpt"`
	Source  string `json:"source"`
}

func WebFetch(d Deps) Tool {
	return New(Spec[URLIn, WebFetchOut]{
		Name:        "web_fetch",
		Description: "Fetch a public web page and return its text excerpt (EXTERNAL information).",
		Schema:      `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     30 * time.Second,
		Retry:       RetryPolicy{Attempts: 2, Backoff: time.Second},
		Run: func(ctx context.Context, in URLIn) (WebFetchOut, error) {
			p, err := d.Research.Fetch(ctx, in.URL)
			if err != nil {
				if strings.Contains(err.Error(), "blocked non-public") {
					return WebFetchOut{}, err // never retry SSRF blocks
				}
				return WebFetchOut{}, Retryable(err)
			}
			// Keep tool output small: prompt processing is ~8 tok/s on device.
			return WebFetchOut{URL: p.URL, Title: p.Title, Excerpt: clip(p.Text, 1500), Source: "external:" + hostOf(p.URL)}, nil
		},
	})
}

type ResearchSaveIn struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	URL     string `json:"url,omitempty"`
}

func (in *ResearchSaveIn) Validate() error {
	in.Title, in.Summary = strings.TrimSpace(in.Title), strings.TrimSpace(in.Summary)
	if in.Title == "" || in.Summary == "" {
		return errors.New("title and summary required")
	}
	return nil
}

// ResearchSave stores a research summary tagged as external information.
func ResearchSave(d Deps) Tool {
	return New(Spec[ResearchSaveIn, KnowledgeView]{
		Name:        "research_save",
		Description: "Save a research summary locally (stored as external/research knowledge).",
		Schema: `{"type":"object","properties":{"title":{"type":"string"},"summary":{"type":"string"},
			"url":{"type":"string"}},"required":["title","summary"],"additionalProperties":false}`,
		Permission: Write,
		Timeout:    5 * time.Second,
		Run: func(ctx context.Context, in ResearchSaveIn) (KnowledgeView, error) {
			k, err := d.Store.AddKnowledge(ctx, memory.NewKnowledge{
				Title: in.Title, Body: in.Summary, Source: memory.SourceResearch, URL: in.URL, Tags: []string{"research"},
			})
			if err != nil {
				return KnowledgeView{}, err
			}
			return KnowledgeView{ID: k.ID, Title: k.Title, Source: k.Source, URL: k.URL, Created: k.CreatedAt.In(d.Loc)}, nil
		},
	})
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}
