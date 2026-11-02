package api

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"personal-assistant/internal/buildinfo"
	"personal-assistant/internal/health"
)

// Deps are the collaborators the HTTP handlers need.
type Deps struct {
	Log     *slog.Logger
	Health  *health.Registry
	Started time.Time
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// Mount registers additional routes (dashboard). Optional.
	Mount func(mux *http.ServeMux)
	// Details adds component status (LLM, memory, scheduler) to /healthz.
	Details func(r *http.Request) any
}

// NewRouter returns the HTTP handler with all routes and middleware.
//
//	GET /livez   process is up and serving (no dependency checks; cheap)
//	GET /healthz full readiness report; 503 if any check fails
func NewRouter(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Log, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /healthz", d.handleHealthz)
	if d.Mount != nil {
		d.Mount(mux)
	}
	return recoverer(d.Log, localHostOnly(mux))
}

// HealthResponse is the body of GET /healthz.
type HealthResponse struct {
	Status        health.Status   `json:"status"`
	Build         buildinfo.Info  `json:"build"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Checks        []health.Result `json:"checks"`
	Runtime       RuntimeStats    `json:"runtime"`
	Details       any             `json:"details,omitempty"`
}

// RuntimeStats is a cheap snapshot of Go runtime resource usage.
type RuntimeStats struct {
	Goroutines    int     `json:"goroutines"`
	GOMAXPROCS    int     `json:"gomaxprocs"`
	HeapAllocMB   float64 `json:"heap_alloc_mb"`
	SysMB         float64 `json:"sys_mb"`
	NumGC         uint32  `json:"num_gc"`
	MemoryLimitMB float64 `json:"memory_limit_mb"` // -1 when unlimited
}

func (d Deps) handleHealthz(w http.ResponseWriter, r *http.Request) {
	rep := d.Health.Run(r.Context())
	resp := HealthResponse{
		Status:        rep.Status,
		Build:         buildinfo.Get(),
		StartedAt:     d.Started.UTC(),
		UptimeSeconds: d.Now().Sub(d.Started).Seconds(),
		Checks:        rep.Checks,
		Runtime:       readRuntimeStats(),
	}
	if d.Details != nil {
		resp.Details = d.Details(r)
	}
	code := http.StatusOK
	if rep.Status != health.StatusOK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, d.Log, code, resp)
}

func readRuntimeStats() RuntimeStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	const mb = 1024 * 1024
	limit := debug.SetMemoryLimit(-1) // -1 reads without changing
	lim := -1.0
	if limit != int64(^uint64(0)>>1) { // math.MaxInt64 means "no limit"
		lim = float64(limit) / mb
	}
	return RuntimeStats{
		Goroutines:    runtime.NumGoroutine(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		HeapAllocMB:   round2(float64(ms.HeapAlloc) / mb),
		SysMB:         round2(float64(ms.Sys) / mb),
		NumGC:         ms.NumGC,
		MemoryLimitMB: lim,
	}
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func writeJSON(w http.ResponseWriter, log *slog.Logger, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warn("write response", "err", err)
	}
}

// localHostOnly rejects requests whose Host header is not a loopback name.
// Binding to 127.0.0.1 alone does not stop DNS-rebinding attacks from web
// pages opened in the phone's browser.
func localHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// recoverer converts handler panics into 500s and logs them instead of
// crashing the whole assistant.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				log.Error("handler panic", "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
