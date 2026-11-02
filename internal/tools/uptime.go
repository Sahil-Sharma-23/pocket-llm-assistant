package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CheckSpec describes an HTTP uptime / API-health check.
type CheckSpec struct {
	Name         string
	URL          string
	ExpectStatus int    // 0 = any 2xx
	ExpectJSON   string // optional top-level JSON key that must be present
	Timeout      time.Duration
}

// CheckResult is the outcome of an HTTP check.
type CheckResult struct {
	Name       string `json:"name,omitempty"`
	URL        string `json:"url"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// CheckHTTP performs one GET and evaluates it against the spec.
func CheckHTTP(ctx context.Context, client *http.Client, s CheckSpec) CheckResult {
	r := CheckResult{Name: s.Name, URL: s.URL}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	req.Header.Set("User-Agent", "personal-assistant-monitor")
	start := time.Now()
	resp, err := client.Do(req)
	r.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer resp.Body.Close()
	r.StatusCode = resp.StatusCode
	switch {
	case s.ExpectStatus != 0 && resp.StatusCode != s.ExpectStatus:
		r.Error = fmt.Sprintf("status %d, expected %d", resp.StatusCode, s.ExpectStatus)
		return r
	case s.ExpectStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode > 299):
		r.Error = fmt.Sprintf("status %d", resp.StatusCode)
		return r
	}
	if s.ExpectJSON != "" {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			r.Error = "response is not a JSON object"
			return r
		}
		if _, ok := body[s.ExpectJSON]; !ok {
			r.Error = fmt.Sprintf("JSON key %q missing", s.ExpectJSON)
			return r
		}
	}
	r.OK = true
	return r
}

type HTTPCheckIn struct {
	URL          string `json:"url"`
	ExpectStatus int    `json:"expect_status,omitempty"`
	ExpectJSON   string `json:"expect_json_key,omitempty"`
}

func (in *HTTPCheckIn) Validate() error {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	in.URL = u.String()
	if in.ExpectStatus != 0 && (in.ExpectStatus < 100 || in.ExpectStatus > 599) {
		return errors.New("expect_status must be an HTTP status code")
	}
	return nil
}

// HTTPCheck checks any public URL now (website uptime or API health).
func HTTPCheck(d Deps) Tool {
	return New(Spec[HTTPCheckIn, CheckResult]{
		Name:        "http_check",
		Description: "Check if a public website or API is up now; optional expected status / JSON key.",
		Schema: `{"type":"object","properties":{"url":{"type":"string"},"expect_status":{"type":"integer"},
			"expect_json_key":{"type":"string"}},"required":["url"],"additionalProperties":false}`,
		Permission: Read,
		Timeout:    20 * time.Second,
		Run: func(ctx context.Context, in HTTPCheckIn) (CheckResult, error) {
			return CheckHTTP(ctx, d.PublicHTTP, CheckSpec{URL: in.URL, ExpectStatus: in.ExpectStatus,
				ExpectJSON: in.ExpectJSON, Timeout: 15 * time.Second}), nil
		},
	})
}

// MonitorView summarises a configured monitor.
type MonitorView struct {
	Name         string  `json:"name"`
	UptimePct    float64 `json:"uptime_pct"`
	Checks       int     `json:"checks"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
	LastOK       bool    `json:"last_ok"`
	LastError    string  `json:"last_error,omitempty"`
	LastChecked  string  `json:"last_checked"`
}

type HoursIn struct {
	Hours int `json:"hours,omitempty"`
}

func (in *HoursIn) Validate() error {
	if in.Hours == 0 {
		in.Hours = 24
	}
	if in.Hours < 1 || in.Hours > 24*30 {
		return errors.New("hours must be 1-720")
	}
	return nil
}

// MonitorStatus reports configured monitors' recent history.
func MonitorStatus(d Deps) Tool {
	return New(Spec[HoursIn, []MonitorView]{
		Name:        "monitor_status",
		Description: "Uptime summary of the configured website/API monitors over the last N hours (default 24).",
		Schema:      `{"type":"object","properties":{"hours":{"type":"integer"}},"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, in HoursIn) ([]MonitorView, error) {
			sums, err := d.Store.MonitorSummaries(ctx, d.now().Add(-time.Duration(in.Hours)*time.Hour))
			out := make([]MonitorView, 0, len(sums))
			for _, s := range sums {
				out = append(out, MonitorView{Name: s.Monitor, UptimePct: float64(int(s.UptimePct*10)) / 10,
					Checks: s.Checks, AvgLatencyMS: int64(s.AvgLatencyMS), LastOK: s.Last.OK, LastError: s.Last.Error,
					LastChecked: s.Last.CheckedAt.In(d.Loc)})
			}
			return out, err
		},
	})
}
