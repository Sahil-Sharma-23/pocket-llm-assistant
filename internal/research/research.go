package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Result is one search hit. Source names the provider so answers can cite
// where external information came from.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
}

// Page is fetched, text-extracted web content.
type Page struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Options configures Client.
type Options struct {
	Providers     []string // wikipedia, duckduckgo
	MaxResults    int
	FetchMaxBytes int
	Timeout       time.Duration
	UserAgent     string
	// HTTP overrides the public-only client (tests only).
	HTTP *http.Client
	// WikipediaURL / DuckDuckGoURL override endpoints (tests only).
	WikipediaURL, DuckDuckGoURL string
}

// Client performs searches and fetches.
type Client struct {
	o    Options
	http *http.Client
}

// New builds a Client.
func New(o Options) *Client {
	if o.HTTP == nil {
		o.HTTP = NewPublicHTTPClient(o.Timeout)
	}
	if o.WikipediaURL == "" {
		o.WikipediaURL = "https://en.wikipedia.org"
	}
	if o.DuckDuckGoURL == "" {
		o.DuckDuckGoURL = "https://html.duckduckgo.com/html/"
	}
	if o.MaxResults <= 0 {
		o.MaxResults = 5
	}
	if o.FetchMaxBytes <= 0 {
		o.FetchMaxBytes = 512 << 10
	}
	return &Client{o: o, http: o.HTTP}
}

// Search queries every configured provider and merges unique results.
// It succeeds if at least one provider answered.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	var all []Result
	var errs []error
	seen := map[string]bool{}
	for _, p := range c.o.Providers {
		var res []Result
		var err error
		switch p {
		case "wikipedia":
			res, err = c.wikipedia(ctx, query)
		case "duckduckgo":
			res, err = c.duckduckgo(ctx, query)
		default:
			err = fmt.Errorf("unknown provider %q", p)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		for _, r := range res {
			if !seen[r.URL] {
				seen[r.URL] = true
				all = append(all, r)
			}
		}
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(all) > c.o.MaxResults*2 {
		all = all[:c.o.MaxResults*2]
	}
	return all, nil
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.o.UserAgent)
	req.Header.Set("Accept-Language", "en")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("http %d from %s", resp.StatusCode, req.URL.Host)
	}
	return resp, nil
}

func (c *Client) wikipedia(ctx context.Context, q string) ([]Result, error) {
	u := fmt.Sprintf("%s/w/api.php?action=query&list=search&format=json&utf8=1&srlimit=%d&srsearch=%s",
		c.o.WikipediaURL, c.o.MaxResults, url.QueryEscape(q))
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var out []Result
	for _, s := range body.Query.Search {
		out = append(out, Result{
			Title:   s.Title,
			URL:     "https://en.wikipedia.org/wiki/" + url.PathEscape(strings.ReplaceAll(s.Title, " ", "_")),
			Snippet: StripTags(s.Snippet),
			Source:  "wikipedia",
		})
	}
	return out, nil
}

func (c *Client) duckduckgo(ctx context.Context, q string) ([]Result, error) {
	resp, err := c.get(ctx, c.o.DuckDuckGoURL+"?q="+url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	doc, err := html.Parse(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return parseDDG(doc, c.o.MaxResults), nil
}

// parseDDG extracts organic results from DuckDuckGo's HTML endpoint.
func parseDDG(doc *html.Node, limit int) []Result {
	var out []Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "result") && !hasClass(n, "result--ad") {
			var r Result
			forEach(n, func(m *html.Node) {
				if m.Type != html.ElementNode {
					return
				}
				switch {
				case m.Data == "a" && hasClass(m, "result__a"):
					r.Title = textOf(m)
					r.URL = ddgTarget(attr(m, "href"))
				case hasClass(m, "result__snippet"):
					r.Snippet = textOf(m)
				}
			})
			if r.Title != "" && strings.HasPrefix(r.URL, "http") {
				r.Source = "duckduckgo"
				out = append(out, r)
			}
			return
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	return out
}

// ddgTarget unwraps "//duckduckgo.com/l/?uddg=<url>" redirect links.
func ddgTarget(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if strings.HasSuffix(u.Host, "duckduckgo.com") && u.Path == "/l/" {
		if t := u.Query().Get("uddg"); t != "" {
			return t
		}
	}
	return href
}

// Fetch downloads an http(s) URL and returns readable text.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Page{}, fmt.Errorf("invalid url %q", rawURL)
	}
	resp, err := c.get(ctx, u.String())
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	limited := io.LimitReader(resp.Body, int64(c.o.FetchMaxBytes)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return Page{}, err
	}
	truncated := len(raw) > c.o.FetchMaxBytes
	if truncated {
		raw = raw[:c.o.FetchMaxBytes]
	}
	p := Page{URL: resp.Request.URL.String(), Truncated: truncated}
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		p.Title, p.Text = ExtractText(string(raw))
	case strings.HasPrefix(mt, "text/") || mt == "application/json":
		p.Text = collapse(string(raw))
	default:
		return Page{}, fmt.Errorf("unsupported content type %q", mt)
	}
	return p, nil
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "nav": true, "header": true,
	"footer": true, "svg": true, "form": true, "aside": true, "iframe": true, "template": true,
}

// ExtractText returns the page title and visible body text, preferring
// <article>/<main> content when present.
func ExtractText(doc string) (title, text string) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", collapse(StripTags(doc))
	}
	var titleNode, mainNode, body *html.Node
	forEach(root, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.Data {
		case "title":
			if titleNode == nil {
				titleNode = n
			}
		case "article", "main":
			if mainNode == nil {
				mainNode = n
			}
		case "body":
			body = n
		}
	})
	if titleNode != nil {
		title = collapse(textOf(titleNode))
	}
	target := mainNode
	if target == nil {
		target = body
	}
	if target == nil {
		target = root
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
		if n.Type == html.ElementNode && (n.Data == "p" || n.Data == "li" || strings.HasPrefix(n.Data, "h")) {
			b.WriteString("\n")
		}
	}
	walk(target)
	return title, collapse(b.String())
}

// StripTags removes HTML markup from a fragment.
func StripTags(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(b.String())
		case html.TextToken:
			b.Write(z.Text())
		}
	}
}

// collapse normalises whitespace while keeping paragraph breaks.
func collapse(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func forEach(n *html.Node, f func(*html.Node)) {
	f(n)
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		forEach(ch, f)
	}
}

func textOf(n *html.Node) string {
	var b strings.Builder
	forEach(n, func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}
