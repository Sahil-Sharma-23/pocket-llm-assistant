package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-assistant/internal/health"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestRouter(t *testing.T, checkErr error) http.Handler {
	t.Helper()
	reg := health.NewRegistry(time.Second)
	if err := reg.Register("probe", func(context.Context) error { return checkErr }); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return NewRouter(Deps{
		Log:     discardLogger(),
		Health:  reg,
		Started: started,
		Now:     func() time.Time { return started.Add(90 * time.Second) },
	})
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		host     string
		checkErr error
		wantCode int
		wantBody string
	}{
		{"livez ok", http.MethodGet, "/livez", "127.0.0.1:8787", nil, http.StatusOK, `"status":"ok"`},
		{"healthz ok", http.MethodGet, "/healthz", "127.0.0.1:8787", nil, http.StatusOK, `"status":"ok"`},
		{"healthz failing check", http.MethodGet, "/healthz", "127.0.0.1:8787", errors.New("disk full"), http.StatusServiceUnavailable, "disk full"},
		{"localhost host allowed", http.MethodGet, "/livez", "localhost:8787", nil, http.StatusOK, ""},
		{"ipv6 loopback allowed", http.MethodGet, "/livez", "[::1]:8787", nil, http.StatusOK, ""},
		{"rebinding host rejected", http.MethodGet, "/livez", "evil.example.com", nil, http.StatusForbidden, "forbidden host"},
		{"lan host rejected", http.MethodGet, "/healthz", "192.168.1.5:8787", nil, http.StatusForbidden, ""},
		{"post not allowed", http.MethodPost, "/healthz", "127.0.0.1:8787", nil, http.StatusMethodNotAllowed, ""},
		{"unknown path", http.MethodGet, "/nope", "127.0.0.1:8787", nil, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestRouter(t, tt.checkErr)
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("code = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body %q missing %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestHealthzBody(t *testing.T) {
	h := newTestRouter(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var body HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.UptimeSeconds != 90 {
		t.Errorf("uptime = %v, want 90", body.UptimeSeconds)
	}
	if len(body.Checks) != 1 || body.Checks[0].Name != "probe" {
		t.Errorf("checks = %+v", body.Checks)
	}
	if body.Build.GoVersion == "" || body.Runtime.Goroutines == 0 || body.Runtime.GOMAXPROCS == 0 {
		t.Errorf("build/runtime not populated: %+v %+v", body.Build, body.Runtime)
	}
}

func TestRecovererContainsPanic(t *testing.T) {
	h := recoverer(discardLogger(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:8787": true,
		"127.0.0.1":      true,
		"127.1.2.3:80":   true,
		"localhost":      true,
		"LOCALHOST:1":    true,
		"[::1]:8787":     true,
		"::1":            true,
		"10.0.0.2:8787":  false,
		"localhost.evil": false,
		"":               false,
	}
	for in, want := range tests {
		if got := isLoopbackHost(in); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}
