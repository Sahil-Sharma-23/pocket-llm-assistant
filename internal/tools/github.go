package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHub is a minimal REST client for public repository status.
type GitHub struct {
	BaseURL string // https://api.github.com
	Token   string // optional; raises the rate limit from 60/h to 5000/h
	HTTP    *http.Client
}

// RepoStatus summarises a repository.
type RepoStatus struct {
	Repo          string       `json:"repo"`
	Description   string       `json:"description,omitempty"`
	Stars         int          `json:"stars"`
	OpenIssuesPRs int          `json:"open_issues_and_prs"`
	OpenPRs       int          `json:"open_prs"`
	PushedAt      string       `json:"last_push"`
	LatestRelease string       `json:"latest_release,omitempty"`
	RecentCommits []CommitView `json:"recent_commits"`
}

// CommitView is one commit.
type CommitView struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

func (g *GitHub) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(g.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "personal-assistant")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return Retryable(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("github: %s not found", path)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("github: rate limited (set %s for a higher limit)", "PA_GITHUB_TOKEN")
	case resp.StatusCode >= 500:
		return Retryable(fmt.Errorf("github: http %d", resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("github: http %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(v)
}

// Status fetches repository status (3-4 API calls).
func (g *GitHub) Status(ctx context.Context, repo string, loc *time.Location) (RepoStatus, error) {
	var r struct {
		FullName    string    `json:"full_name"`
		Description string    `json:"description"`
		Stars       int       `json:"stargazers_count"`
		OpenIssues  int       `json:"open_issues_count"`
		PushedAt    time.Time `json:"pushed_at"`
	}
	if err := g.get(ctx, "/repos/"+repo, &r); err != nil {
		return RepoStatus{}, err
	}
	out := RepoStatus{Repo: r.FullName, Description: r.Description, Stars: r.Stars,
		OpenIssuesPRs: r.OpenIssues, PushedAt: FormatLocal(r.PushedAt, loc)}

	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := g.get(ctx, "/repos/"+repo+"/commits?per_page=5", &commits); err != nil {
		return out, err
	}
	for _, c := range commits {
		msg, _, _ := strings.Cut(c.Commit.Message, "\n")
		out.RecentCommits = append(out.RecentCommits, CommitView{
			SHA: c.SHA[:min(7, len(c.SHA))], Message: clip(msg, 100),
			Author: c.Commit.Author.Name, Date: FormatLocal(c.Commit.Author.Date, loc)})
	}
	var pulls []struct{}
	if err := g.get(ctx, "/repos/"+repo+"/pulls?state=open&per_page=30", &pulls); err == nil {
		out.OpenPRs = len(pulls)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := g.get(ctx, "/repos/"+repo+"/releases/latest", &rel); err == nil {
		out.LatestRelease = rel.Tag
	}
	return out, nil
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type RepoIn struct {
	Repo string `json:"repo"`
}

func (in *RepoIn) Validate() error {
	in.Repo = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(in.Repo), "https://github.com/"), "/")
	if !repoRe.MatchString(in.Repo) {
		return errors.New("repo must be owner/name")
	}
	return nil
}

func GitHubRepoStatus(d Deps) Tool {
	return New(Spec[RepoIn, RepoStatus]{
		Name:        "github_repo_status",
		Description: "Get a GitHub repo's recent commits, open PRs/issues and latest release.",
		Schema:      `{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     30 * time.Second,
		Retry:       RetryPolicy{Attempts: 2, Backoff: 2 * time.Second},
		Run: func(ctx context.Context, in RepoIn) (RepoStatus, error) {
			if len(d.RepoAllowlist) > 0 && !slices.Contains(d.RepoAllowlist, in.Repo) {
				return RepoStatus{}, fmt.Errorf("repo %s is not in developer.github_repos", in.Repo)
			}
			return d.GitHub.Status(ctx, in.Repo, d.Loc)
		},
	})
}
