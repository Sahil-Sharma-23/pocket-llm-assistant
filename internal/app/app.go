// Package app wires components together and owns the process lifecycle:
// resource limits, startup, background workers, serving, graceful shutdown.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/api"
	"personal-assistant/internal/buildinfo"
	"personal-assistant/internal/config"
	"personal-assistant/internal/dashboard"
	"personal-assistant/internal/health"
	"personal-assistant/internal/llm"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/notify"
	"personal-assistant/internal/research"
	"personal-assistant/internal/scheduler"
	"personal-assistant/internal/sysinfo"
	"personal-assistant/internal/telegram"
	"personal-assistant/internal/tools"
)

// healthCheckTimeout bounds each individual health check.
const healthCheckTimeout = 2 * time.Second

// App is the running assistant process.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	loc     *time.Location
	started time.Time

	store      *memory.Store
	health     *health.Registry
	server     *api.Server
	guard      *llm.Guard
	supervisor *llm.Supervisor // nil when llama-server is external
	sched      *scheduler.Scheduler
	bot        *telegram.Bot // nil when Telegram is disabled
	botErr     string
	orch       *agent.Orchestrator
	monitor    *resourceMonitor

	mu    sync.Mutex
	addr  net.Addr
	ready chan struct{}
}

// New builds every component. It opens the database (running migrations)
// but starts no goroutines and binds no ports; Run does.
func New(cfg config.Config, log *slog.Logger) (*App, error) {
	for _, dir := range []string{cfg.Paths.DataDir, cfg.Paths.LogDir, cfg.Paths.BackupDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	ApplyRuntimeLimits(cfg.Runtime, log)
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	store, err := memory.Open(ctx, cfg.Database.Path, memory.Options{BusyTimeout: cfg.Database.BusyTimeout.Duration})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Recover from an unclean stop (Android can SIGKILL at any time).
	if n, _ := store.MarkStaleRuns(ctx); n > 0 {
		log.Warn("marked interrupted runs as failed", "count", n)
	}
	if n, _ := store.MarkInterruptedApprovals(ctx); n > 0 {
		log.Warn("marked interrupted approvals as failed (not re-executed)", "count", n)
	}

	a := &App{cfg: cfg, log: log, loc: loc, started: time.Now(), store: store, ready: make(chan struct{})}
	if err := a.build(); err != nil {
		store.Close()
		return nil, err
	}
	return a, nil
}

func (a *App) build() error {
	cfg, log := a.cfg, a.log

	// --- LLM -------------------------------------------------------------
	client, err := llm.NewLlamaClient(llm.LlamaOptions{BaseURL: cfg.LLM.BaseURL, MaxTokens: cfg.LLM.MaxTokens, Temperature: cfg.LLM.Temperature})
	if err != nil {
		return err
	}
	memAvail := func() (int64, error) {
		m, err := sysinfo.ReadMem()
		return m.AvailableMB, err
	}
	var ensurer llm.Ensurer
	if s := cfg.LLM.Server; s.Managed {
		a.supervisor, err = llm.NewSupervisor(llm.SupervisorOptions{
			Binary: s.Binary, ModelPath: s.ModelPath, BaseURL: cfg.LLM.BaseURL, ContextSize: cfg.LLM.ContextSize,
			Threads: s.Threads, ThreadsBatch: s.ThreadsBatch, SleepIdleSeconds: s.SleepIdleSeconds, ExtraArgs: s.ExtraArgs,
			LogFile: s.LogFile, StartupTimeout: s.StartupTimeout.Duration, RestartBackoff: s.RestartBackoff.Duration,
			MinFreeMemMB: int64(s.MinFreeMemMBToStart), MemAvailable: memAvail, Health: client.Health,
			Log: log.With("component", "llama-supervisor"),
		})
		if err != nil {
			return err
		}
		ensurer = a.supervisor
	}
	a.guard = llm.NewGuard(client, llm.GuardOptions{
		MaxQueue: cfg.LLM.MaxQueue, Timeout: cfg.LLM.RequestTimeout.Duration,
		MinFreeMemMB: int64(cfg.LLM.MinFreeMemMB), MemAvailable: memAvail, Ensurer: ensurer,
		Log: log.With("component", "llm"),
	})

	// --- tools -----------------------------------------------------------
	deps := tools.Deps{
		Store:      a.store,
		Loc:        a.loc,
		PublicHTTP: research.NewPublicHTTPClient(20 * time.Second),
		// Bound late: the scheduler is created below.
		WakeScheduler: func() {
			if a.sched != nil {
				a.sched.Wake()
			}
		},
		RepoAllowlist: nil,
	}
	if cfg.Research.Enabled {
		deps.Research = research.New(research.Options{
			Providers: cfg.Research.Providers, MaxResults: cfg.Research.MaxResults,
			FetchMaxBytes: cfg.Research.FetchMaxBytes, Timeout: cfg.Research.Timeout.Duration, UserAgent: cfg.Research.UserAgent,
		})
	}
	deps.GitHub = &tools.GitHub{
		BaseURL: cfg.Developer.GitHubAPIURL,
		Token:   os.Getenv(cfg.Developer.GitHubTokenEnv),
		HTTP:    research.NewPublicHTTPClient(20 * time.Second),
	}
	reg := tools.NewRegistry()
	for _, t := range tools.All(deps) {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	has := func(n string) bool { _, ok := reg.Get(n); return ok }
	agents, err := agent.NewRegistry(agent.DefaultAgents(has), has)
	if err != nil {
		return err
	}

	// --- notifications + orchestrator ------------------------------------
	notifiers := notify.Multi{notify.Store{S: a.store}}
	if cfg.Telegram.Enabled {
		token := os.Getenv(cfg.Telegram.TokenEnv)
		if token == "" {
			a.botErr = fmt.Sprintf("telegram enabled but $%s is empty; put the token in configs/secrets.env", cfg.Telegram.TokenEnv)
			log.Error(a.botErr)
		}
		if token != "" {
			a.bot = telegram.New(telegram.Options{
				Client:      telegram.NewClient(cfg.Telegram.APIBaseURL, token, &http.Client{}),
				Offsets:     a.store,
				AllowedIDs:  cfg.Telegram.AllowedUserIDs,
				PollTimeout: cfg.Telegram.PollTimeout.Duration,
				MaxInFlight: cfg.Agents.MaxConcurrentRuns + 1,
				Log:         log.With("component", "telegram"),
			})
			notifiers = append(notifiers, a.bot)
		}
	}
	a.orch = agent.New(agent.Options{
		Store:           a.store,
		Planner:         &agent.Planner{LLM: a.guard, Tools: reg, Loc: a.loc, Now: time.Now, MaxTokens: cfg.LLM.MaxTokens},
		Executor:        &agent.Executor{Tools: reg, Store: a.store, Log: log.With("component", "executor"), ApprovalTTL: cfg.Agents.ApprovalTTL.Duration},
		Agents:          agents,
		ToolDeps:        deps,
		Notifier:        notifiers,
		Log:             log.With("component", "orchestrator"),
		HistoryMessages: cfg.Agents.HistoryMessages,
		MaxSteps:        cfg.Agents.MaxSteps,
		MaxConcurrent:   cfg.Agents.MaxConcurrentRuns,
		RunTimeout:      cfg.Agents.RunTimeout.Duration,
		Status:          a.statusText,
		DigestRepos:     cfg.Developer.GitHubRepos,
	})
	if a.bot != nil {
		a.bot.SetHandler(a.orch)
	}

	// --- scheduler -------------------------------------------------------
	a.sched = scheduler.New(scheduler.Options{
		Store: a.store, Log: log.With("component", "scheduler"), Loc: a.loc,
		MaxWorkers: cfg.Scheduler.MaxWorkers, MaxIdleSleep: cfg.Scheduler.MaxIdleSleep.Duration,
		JobTimeout: cfg.Agents.RunTimeout.Duration,
	})
	j := &jobs{a: a, notifier: notifiers, monitorHTTP: &http.Client{Timeout: 30 * time.Second}}
	j.register(a.sched)
	if err := j.ensure(context.Background()); err != nil {
		return fmt.Errorf("schedule jobs: %w", err)
	}

	// --- health, resources, HTTP ----------------------------------------
	a.monitor = newResourceMonitor(a, cfg.Resources)
	a.health = health.NewRegistry(healthCheckTimeout)
	if err := a.health.Register("data_dir_writable", health.DirWritable(cfg.Paths.DataDir)); err != nil {
		return err
	}
	if err := a.health.Register("database", a.store.Ping); err != nil {
		return err
	}

	dash, err := dashboard.New(dashboard.Deps{
		Store: a.store, Handler: a.orch, Agents: agents, Tools: reg,
		Status: a.statusView, ConfigJSON: redactedConfig(cfg), LogFile: cfg.Log.File, Loc: a.loc,
		Log: log.With("component", "dashboard"), ChatTimeout: cfg.Agents.RunTimeout.Duration,
	})
	if err != nil {
		return err
	}
	router := api.NewRouter(api.Deps{
		Log: log, Health: a.health, Started: a.started, Mount: dash.Mount,
		Details: func(r *http.Request) any { return a.details() },
	})
	a.server = api.NewServer(cfg.Server, router, log)
	return nil
}

// Health exposes the registry so components can add checks.
func (a *App) Health() *health.Registry { return a.health }

// Ready is closed once the server is listening.
func (a *App) Ready() <-chan struct{} { return a.ready }

// Addr returns the bound address, or nil before Ready.
func (a *App) Addr() net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// Orchestrator is exposed for integration tests.
func (a *App) Orchestrator() *agent.Orchestrator { return a.orch }

// Run starts background workers, serves until ctx is cancelled (signal) or
// the server fails, then shuts everything down gracefully.
func (a *App) Run(ctx context.Context) error {
	defer a.store.Close()

	addr, err := a.server.Listen()
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.addr = addr
	a.mu.Unlock()
	close(a.ready)
	a.log.Info("server listening", "addr", addr.String(), "dashboard", "http://"+addr.String()+"/")

	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	start := func(name string, f func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					a.log.Error("worker panicked", "worker", name, "panic", p, "stack", string(debug.Stack()))
				}
			}()
			f(workCtx)
		}()
	}
	if a.supervisor != nil {
		start("llama-supervisor", a.supervisor.Run)
	}
	start("scheduler", a.sched.Run)
	start("resource-monitor", a.monitor.run)
	if a.bot != nil {
		start("telegram", func(c context.Context) {
			if err := a.bot.Run(c); err != nil {
				a.mu.Lock()
				a.botErr = err.Error()
				a.mu.Unlock()
				a.log.Error("telegram bot stopped", "err", err)
			}
		})
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.server.Serve() }()

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested", "cause", context.Cause(ctx))
	case err := <-serveErr:
		if err == nil {
			err = errors.New("server stopped unexpectedly")
		}
		runErr = fmt.Errorf("serve: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout.Duration)
	defer cancel()
	begin := time.Now()
	var errs []error
	if runErr == nil {
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, err)
		}
		if err := <-serveErr; err != nil {
			errs = append(errs, err)
		}
	}
	stopWork()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		errs = append(errs, errors.New("background workers did not stop in time"))
	}
	if runErr != nil {
		return runErr
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	a.log.Info("shutdown complete", "took_ms", time.Since(begin).Milliseconds())
	return nil
}

// --- status ------------------------------------------------------------------

func (a *App) statusView(ctx context.Context) dashboard.StatusView {
	v := dashboard.StatusView{
		Version:         buildinfo.Get().Version,
		Uptime:          time.Since(a.started).Round(time.Second).String(),
		Model:           a.cfg.LLM.Model,
		Health:          a.health.Run(ctx),
		LLMManaged:      a.supervisor != nil,
		LLM:             a.guard.Stats(),
		TelegramEnabled: a.bot != nil,
	}
	if a.supervisor != nil {
		st := a.supervisor.Status()
		v.LLMState, v.LLMPID, v.LLMRestarts, v.LLMError = st.State, st.PID, st.Restarts, st.LastError
	}
	snap := a.monitor.latest()
	v.MemTotalMB, v.MemAvailableMB, v.SelfRSSMB = snap.Mem.TotalMB, snap.Mem.AvailableMB, snap.Self.RSSMB
	if snap.Llama != nil {
		v.LlamaRSSMB, v.LlamaAnonMB = snap.Llama.RSSMB, snap.Llama.AnonMB
	}
	ss := a.sched.Status()
	v.SchedulerRunning, v.SchedulerLastRun = ss.Running, ss.LastRun
	return v
}

func (a *App) statusText(ctx context.Context) string {
	v := a.statusView(ctx)
	var b strings.Builder
	fmt.Fprintf(&b, "🩺 Status: %s · up %s · %s\n", v.Health.Status, v.Uptime, v.Version)
	llmState := "external"
	if v.LLMManaged {
		llmState = v.LLMState
	}
	fmt.Fprintf(&b, "🧠 Model %s: %s (requests %d, errors %d)", v.Model, llmState, v.LLM.Requests, v.LLM.Errors)
	if v.LLM.LastLatencyMS > 0 {
		fmt.Fprintf(&b, ", last %.1fs @ %.1f/%.1f tok/s", float64(v.LLM.LastLatencyMS)/1000, v.LLM.LastUsage.PromptPerSec, v.LLM.LastUsage.GenPerSec)
	}
	fmt.Fprintf(&b, "\n💾 Memory: %d MB free of %d MB · assistant %d MB", v.MemAvailableMB, v.MemTotalMB, v.SelfRSSMB)
	if v.LlamaRSSMB > 0 {
		fmt.Fprintf(&b, " · llama-server %d MB (%d MB non-reclaimable)", v.LlamaRSSMB, v.LlamaAnonMB)
	}
	st, _ := a.store.Stats(ctx)
	fmt.Fprintf(&b, "\n📋 %d pending tasks · %d pending approvals · %d jobs", st.PendingTasks, st.PendingApprovals, st.Jobs)
	return b.String()
}

func (a *App) details() any {
	snap := a.monitor.latest()
	d := map[string]any{
		"llm":       a.guard.Stats(),
		"memory":    snap.Mem,
		"process":   snap.Self,
		"scheduler": a.sched.Status(),
	}
	if a.supervisor != nil {
		d["llama_server"] = a.supervisor.Status()
		if snap.Llama != nil {
			d["llama_process"] = snap.Llama
		}
	}
	a.mu.Lock()
	tg := map[string]any{"enabled": a.cfg.Telegram.Enabled, "running": a.bot != nil}
	if a.botErr != "" {
		tg["error"] = a.botErr
	}
	a.mu.Unlock()
	d["telegram"] = tg
	return d
}

// redactedConfig renders the effective config. Secrets never live in the
// config struct (only env var names), but the GitHub/Telegram token presence
// is shown so misconfiguration is visible.
func redactedConfig(cfg config.Config) string {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	extra := fmt.Sprintf("\n\n$%s set: %v\n$%s set: %v",
		cfg.Telegram.TokenEnv, os.Getenv(cfg.Telegram.TokenEnv) != "",
		cfg.Developer.GitHubTokenEnv, os.Getenv(cfg.Developer.GitHubTokenEnv) != "")
	return string(b) + extra
}

// ApplyRuntimeLimits sets GOMEMLIMIT and GOMAXPROCS from config. An explicit
// GOMEMLIMIT/GOMAXPROCS environment variable wins, so operators can override
// without editing config.
func ApplyRuntimeLimits(rc config.RuntimeConfig, log *slog.Logger) {
	if rc.MemoryLimitMB > 0 {
		if _, set := os.LookupEnv("GOMEMLIMIT"); set {
			log.Info("GOMEMLIMIT env set; ignoring runtime.memory_limit_mb")
		} else {
			debug.SetMemoryLimit(int64(rc.MemoryLimitMB) << 20)
		}
	}
	if rc.MaxProcs > 0 {
		if _, set := os.LookupEnv("GOMAXPROCS"); set {
			log.Info("GOMAXPROCS env set; ignoring runtime.max_procs")
		} else {
			runtime.GOMAXPROCS(min(rc.MaxProcs, runtime.NumCPU()))
		}
	}
}
