// Package config loads and validates application configuration.
//
// Precedence (lowest to highest): built-in defaults, JSON config file,
// PA_* environment variables. Relative paths are resolved against Home.
// Secrets (Telegram/GitHub tokens) are never read from the JSON file; only
// the *name* of the environment variable holding them is configured.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the root application configuration.
type Config struct {
	// Home is the base directory for resolving relative paths.
	// Defaults to the current working directory.
	Home      string          `json:"home"`
	Timezone  string          `json:"timezone"` // IANA name; empty = TZ env / system local
	Server    ServerConfig    `json:"server"`
	Log       LogConfig       `json:"log"`
	Runtime   RuntimeConfig   `json:"runtime"`
	Paths     PathsConfig     `json:"paths"`
	Database  DatabaseConfig  `json:"database"`
	LLM       LLMConfig       `json:"llm"`
	Agents    AgentsConfig    `json:"agents"`
	Telegram  TelegramConfig  `json:"telegram"`
	Scheduler SchedulerConfig `json:"scheduler"`
	Developer DeveloperConfig `json:"developer"`
	Research  ResearchConfig  `json:"research"`
	Backup    BackupConfig    `json:"backup"`
	Resources ResourcesConfig `json:"resources"`
}

// ServerConfig configures the local HTTP server (API + dashboard).
type ServerConfig struct {
	// Addr must be a loopback host:port. LAN binding is rejected until
	// authentication exists.
	Addr              string   `json:"addr"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  string `json:"level"`  // debug | info | warn | error
	Format string `json:"format"` // json | text
	// File is the log file path; empty means stderr. Relative paths are
	// resolved against Paths.LogDir.
	File       string `json:"file"`
	MaxSizeMB  int    `json:"max_size_mb"`
	MaxBackups int    `json:"max_backups"`
}

// RuntimeConfig bounds Go runtime resource usage.
type RuntimeConfig struct {
	// MemoryLimitMB sets the Go soft memory limit (GOMEMLIMIT). 0 = unset.
	MemoryLimitMB int `json:"memory_limit_mb"`
	// MaxProcs sets GOMAXPROCS. 0 = Go default (all CPUs).
	MaxProcs int `json:"max_procs"`
}

// PathsConfig holds on-disk locations.
type PathsConfig struct {
	DataDir   string `json:"data_dir"`
	LogDir    string `json:"log_dir"`
	BackupDir string `json:"backup_dir"`
	ModelsDir string `json:"models_dir"`
}

// DatabaseConfig configures SQLite.
type DatabaseConfig struct {
	// Path is relative to Paths.DataDir unless absolute.
	Path        string   `json:"path"`
	BusyTimeout Duration `json:"busy_timeout"`
}

// LLMConfig configures local inference. Only loopback endpoints are allowed:
// prompts and user data must never leave the device.
type LLMConfig struct {
	Provider       string   `json:"provider"` // "llamacpp"
	BaseURL        string   `json:"base_url"`
	Model          string   `json:"model"` // label used in logs/UI
	RequestTimeout Duration `json:"request_timeout"`
	MaxTokens      int      `json:"max_tokens"`
	Temperature    float64  `json:"temperature"`
	ContextSize    int      `json:"context_size"`
	// MaxQueue is how many requests may wait for the single inference slot.
	MaxQueue int `json:"max_queue"`
	// MinFreeMemMB: refuse an inference request when MemAvailable is lower.
	MinFreeMemMB int               `json:"min_free_mem_mb"`
	Server       LlamaServerConfig `json:"server"`
}

// LlamaServerConfig configures the supervised llama-server child process.
type LlamaServerConfig struct {
	Managed   bool   `json:"managed"` // false = assume an externally run server
	Binary    string `json:"binary"`
	ModelPath string `json:"model_path"` // relative to Paths.ModelsDir
	Threads   int    `json:"threads"`    // generation threads
	// ThreadsBatch is used for prompt processing (-tb). Benchmarked on the
	// reference phone: pp64 6.8 tok/s @2 threads -> 9.5 tok/s @8; generation
	// does not improve past 4 threads, so 4/8 is the default. scripts/termux-setup.sh
	// caps both at the device core count when it creates config.json.
	ThreadsBatch int `json:"threads_batch"`
	// SleepIdleSeconds unloads the model after this much idleness
	// (llama-server --sleep-idle-seconds); 0 keeps it resident.
	SleepIdleSeconds int      `json:"sleep_idle_seconds"`
	StartupTimeout   Duration `json:"startup_timeout"`
	// MinFreeMemMBToStart: refuse to start llama-server below this.
	MinFreeMemMBToStart int      `json:"min_free_mem_mb_to_start"`
	ExtraArgs           []string `json:"extra_args"`
	LogFile             string   `json:"log_file"` // relative to Paths.LogDir
	RestartBackoff      Duration `json:"restart_backoff"`
}

// AgentsConfig bounds agent execution.
type AgentsConfig struct {
	MaxSteps          int      `json:"max_steps"`        // tool-loop iterations per request
	HistoryMessages   int      `json:"history_messages"` // prior messages given to the LLM
	MaxConcurrentRuns int      `json:"max_concurrent_runs"`
	RunTimeout        Duration `json:"run_timeout"`
	ApprovalTTL       Duration `json:"approval_ttl"`
}

// TelegramConfig configures the bot. The token is read from the env var
// named by TokenEnv, never from this file.
type TelegramConfig struct {
	Enabled        bool     `json:"enabled"`
	TokenEnv       string   `json:"token_env"`
	AllowedUserIDs []int64  `json:"allowed_user_ids"`
	PollTimeout    Duration `json:"poll_timeout"`
	APIBaseURL     string   `json:"api_base_url"`
}

// SchedulerConfig configures background jobs.
type SchedulerConfig struct {
	MaxWorkers    int      `json:"max_workers"`
	MaxIdleSleep  Duration `json:"max_idle_sleep"`
	DailyPlanTime string   `json:"daily_plan_time"` // "HH:MM" local; empty = off
}

// DeveloperConfig configures the Developer agent's monitors.
type DeveloperConfig struct {
	GitHubRepos    []string        `json:"github_repos"` // "owner/repo"
	GitHubTokenEnv string          `json:"github_token_env"`
	GitHubAPIURL   string          `json:"github_api_url"`
	DigestTime     string          `json:"digest_time"` // "HH:MM" local; empty = off
	Monitors       []MonitorConfig `json:"monitors"`
}

// MonitorConfig is an uptime or API-health check.
type MonitorConfig struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	ExpectStatus int      `json:"expect_status"`   // 0 = any 2xx
	ExpectJSON   string   `json:"expect_json_key"` // optional top-level key that must exist
	Interval     Duration `json:"interval"`
	Timeout      Duration `json:"timeout"`
}

// ResearchConfig configures web research.
type ResearchConfig struct {
	Enabled       bool     `json:"enabled"`
	Providers     []string `json:"providers"` // wikipedia, duckduckgo
	MaxResults    int      `json:"max_results"`
	FetchMaxBytes int      `json:"fetch_max_bytes"`
	Timeout       Duration `json:"timeout"`
	UserAgent     string   `json:"user_agent"`
}

// BackupConfig configures SQLite backups.
type BackupConfig struct {
	Interval Duration `json:"interval"` // 0 = disabled
	Keep     int      `json:"keep"`
}

// ResourcesConfig configures resource sampling.
type ResourcesConfig struct {
	SampleInterval     Duration `json:"sample_interval"`
	WarnMemAvailableMB int      `json:"warn_mem_available_mb"`
}

// Duration is a time.Duration that marshals as a Go duration string ("10s").
type Duration struct{ time.Duration }

// UnmarshalJSON parses a duration string such as "1m30s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

func dur(d time.Duration) Duration { return Duration{d} }

// Default returns the built-in configuration, tuned for a 6 GB Android device.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:              "127.0.0.1:8787",
			ReadHeaderTimeout: dur(5 * time.Second),
			ReadTimeout:       dur(15 * time.Second),
			WriteTimeout:      dur(30 * time.Second),
			IdleTimeout:       dur(60 * time.Second),
			ShutdownTimeout:   dur(10 * time.Second),
		},
		Log:     LogConfig{Level: "info", Format: "json", MaxSizeMB: 10, MaxBackups: 5},
		Runtime: RuntimeConfig{MemoryLimitMB: 256, MaxProcs: 2},
		Paths:   PathsConfig{DataDir: "data", LogDir: "logs", BackupDir: "backups", ModelsDir: "models"},
		Database: DatabaseConfig{
			Path:        "assistant.db",
			BusyTimeout: dur(5 * time.Second),
		},
		LLM: LLMConfig{
			Provider:       "llamacpp",
			BaseURL:        "http://127.0.0.1:8080",
			Model:          "gemma-4-E2B-it-qat-q4_0",
			RequestTimeout: dur(240 * time.Second),
			MaxTokens:      384,
			Temperature:    0.2,
			ContextSize:    4096,
			MaxQueue:       4,
			MinFreeMemMB:   250,
			Server: LlamaServerConfig{
				Managed:             true,
				Binary:              "llama-server",
				ModelPath:           "gemma-4-E2B_q4_0-it.gguf",
				Threads:             4,
				ThreadsBatch:        8,
				SleepIdleSeconds:    900,
				StartupTimeout:      dur(240 * time.Second),
				MinFreeMemMBToStart: 1200,
				LogFile:             "llama-server.log",
				RestartBackoff:      dur(10 * time.Second),
			},
		},
		Agents: AgentsConfig{
			MaxSteps:          3,
			HistoryMessages:   6,
			MaxConcurrentRuns: 2,
			RunTimeout:        dur(10 * time.Minute),
			ApprovalTTL:       dur(24 * time.Hour),
		},
		Telegram: TelegramConfig{
			Enabled:     false,
			TokenEnv:    "PA_TELEGRAM_TOKEN",
			PollTimeout: dur(50 * time.Second),
			APIBaseURL:  "https://api.telegram.org",
		},
		Scheduler: SchedulerConfig{
			MaxWorkers:    2,
			MaxIdleSleep:  dur(5 * time.Minute),
			DailyPlanTime: "08:00",
		},
		Developer: DeveloperConfig{
			GitHubTokenEnv: "PA_GITHUB_TOKEN",
			GitHubAPIURL:   "https://api.github.com",
			DigestTime:     "",
		},
		Research: ResearchConfig{
			Enabled:       true,
			Providers:     []string{"duckduckgo", "wikipedia"},
			MaxResults:    5,
			FetchMaxBytes: 512 << 10,
			Timeout:       dur(20 * time.Second),
			UserAgent:     "personal-assistant/0.1 (local-first research agent)",
		},
		Backup:    BackupConfig{Interval: dur(24 * time.Hour), Keep: 7},
		Resources: ResourcesConfig{SampleInterval: dur(60 * time.Second), WarnMemAvailableMB: 500},
	}
}

// LookupEnvFunc matches os.LookupEnv; injectable for tests.
type LookupEnvFunc func(key string) (string, bool)

// Load builds a Config from defaults, an optional JSON file (path may be
// empty), and environment overrides, then resolves paths and validates.
func Load(path string, lookupEnv LookupEnvFunc) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if err := applyEnv(&cfg, lookupEnv); err != nil {
		return Config{}, err
	}
	if err := cfg.resolvePaths(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config, lookup LookupEnvFunc) error {
	str := map[string]*string{
		"PA_HOME":         &cfg.Home,
		"PA_TIMEZONE":     &cfg.Timezone,
		"PA_HTTP_ADDR":    &cfg.Server.Addr,
		"PA_LOG_LEVEL":    &cfg.Log.Level,
		"PA_LOG_FORMAT":   &cfg.Log.Format,
		"PA_LOG_FILE":     &cfg.Log.File,
		"PA_DATA_DIR":     &cfg.Paths.DataDir,
		"PA_LOG_DIR":      &cfg.Paths.LogDir,
		"PA_LLM_BASE_URL": &cfg.LLM.BaseURL,
	}
	for key, dst := range str {
		if v, ok := lookup(key); ok {
			*dst = v
		}
	}
	ints := map[string]*int{
		"PA_MEMORY_LIMIT_MB": &cfg.Runtime.MemoryLimitMB,
		"PA_MAX_PROCS":       &cfg.Runtime.MaxProcs,
	}
	for key, dst := range ints {
		if v, ok := lookup(key); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("env %s: not an integer: %q", key, v)
			}
			*dst = n
		}
	}
	if v, ok := lookup("PA_TELEGRAM_ENABLED"); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("env PA_TELEGRAM_ENABLED: not a boolean: %q", v)
		}
		cfg.Telegram.Enabled = b
	}
	if v, ok := lookup("PA_LLM_MANAGED"); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("env PA_LLM_MANAGED: not a boolean: %q", v)
		}
		cfg.LLM.Server.Managed = b
	}
	return nil
}

func (c *Config) resolvePaths() error {
	if c.Home == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve home: %w", err)
		}
		c.Home = wd
	}
	home, err := filepath.Abs(c.Home)
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	c.Home = home
	c.Paths.DataDir = resolve(c.Home, c.Paths.DataDir)
	c.Paths.LogDir = resolve(c.Home, c.Paths.LogDir)
	c.Paths.BackupDir = resolve(c.Home, c.Paths.BackupDir)
	c.Paths.ModelsDir = resolve(c.Home, c.Paths.ModelsDir)
	if c.Log.File != "" {
		c.Log.File = resolve(c.Paths.LogDir, c.Log.File)
	}
	c.Database.Path = resolve(c.Paths.DataDir, c.Database.Path)
	c.LLM.Server.ModelPath = resolve(c.Paths.ModelsDir, c.LLM.Server.ModelPath)
	if c.LLM.Server.LogFile != "" {
		c.LLM.Server.LogFile = resolve(c.Paths.LogDir, c.LLM.Server.LogFile)
	}
	return nil
}

func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

var (
	hhmm     = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Location returns the configured time zone: config "timezone", else the TZ
// environment variable, else time.Local. The TZ fallback matters on Android:
// Go's Android runtime ignores TZ and initialises time.Local to UTC
// (verified on-device), while time.LoadLocation reads Android's tzdata fine.
func (c Config) Location() (*time.Location, error) {
	tz := c.Timezone
	if tz == "" {
		tz = os.Getenv("TZ")
	}
	if tz == "" {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}

// Validate reports every configuration problem at once.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	positive := func(name string, d Duration) {
		if d.Duration <= 0 {
			add("%s: must be > 0, got %s", name, d.Duration)
		}
	}

	if _, err := c.Location(); err != nil {
		add("timezone: %w", err)
	}

	if err := validateLoopbackAddr(c.Server.Addr); err != nil {
		add("server.addr: %w", err)
	}
	positive("server.read_header_timeout", c.Server.ReadHeaderTimeout)
	positive("server.read_timeout", c.Server.ReadTimeout)
	positive("server.write_timeout", c.Server.WriteTimeout)
	positive("server.idle_timeout", c.Server.IdleTimeout)
	positive("server.shutdown_timeout", c.Server.ShutdownTimeout)

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format: must be json|text, got %q", c.Log.Format)
	}
	if c.Log.File != "" && c.Log.MaxSizeMB < 1 {
		add("log.max_size_mb: must be >= 1 when log.file is set, got %d", c.Log.MaxSizeMB)
	}
	if c.Log.MaxBackups < 0 {
		add("log.max_backups: must be >= 0, got %d", c.Log.MaxBackups)
	}

	if c.Runtime.MemoryLimitMB < 0 || (c.Runtime.MemoryLimitMB > 0 && c.Runtime.MemoryLimitMB < 32) {
		add("runtime.memory_limit_mb: must be 0 (unset) or >= 32, got %d", c.Runtime.MemoryLimitMB)
	}
	if c.Runtime.MaxProcs < 0 {
		add("runtime.max_procs: must be >= 0, got %d", c.Runtime.MaxProcs)
	}

	for name, p := range map[string]string{
		"paths.data_dir": c.Paths.DataDir, "paths.log_dir": c.Paths.LogDir,
		"paths.backup_dir": c.Paths.BackupDir, "paths.models_dir": c.Paths.ModelsDir,
		"database.path": c.Database.Path,
	} {
		if p == "" {
			add("%s: must not be empty", name)
		}
	}
	positive("database.busy_timeout", c.Database.BusyTimeout)

	// LLM: local only.
	if c.LLM.Provider != "llamacpp" {
		add("llm.provider: only \"llamacpp\" is supported, got %q", c.LLM.Provider)
	}
	if err := validateLoopbackURL(c.LLM.BaseURL); err != nil {
		add("llm.base_url: %w", err)
	}
	positive("llm.request_timeout", c.LLM.RequestTimeout)
	if c.LLM.MaxTokens < 16 || c.LLM.MaxTokens > 4096 {
		add("llm.max_tokens: must be in [16, 4096], got %d", c.LLM.MaxTokens)
	}
	if c.LLM.Temperature < 0 || c.LLM.Temperature > 2 {
		add("llm.temperature: must be in [0, 2], got %v", c.LLM.Temperature)
	}
	if c.LLM.ContextSize < 512 || c.LLM.ContextSize > 32768 {
		add("llm.context_size: must be in [512, 32768], got %d", c.LLM.ContextSize)
	}
	if c.LLM.MaxQueue < 0 {
		add("llm.max_queue: must be >= 0, got %d", c.LLM.MaxQueue)
	}
	if c.LLM.MinFreeMemMB < 0 {
		add("llm.min_free_mem_mb: must be >= 0, got %d", c.LLM.MinFreeMemMB)
	}
	if s := c.LLM.Server; s.Managed {
		if s.Binary == "" {
			add("llm.server.binary: must not be empty when managed")
		}
		if s.ModelPath == "" {
			add("llm.server.model_path: must not be empty when managed")
		}
		if s.Threads < 1 || s.Threads > 8 {
			add("llm.server.threads: must be in [1, 8], got %d", s.Threads)
		}
		if s.ThreadsBatch < 1 || s.ThreadsBatch > 8 {
			add("llm.server.threads_batch: must be in [1, 8], got %d", s.ThreadsBatch)
		}
		if s.SleepIdleSeconds < 0 {
			add("llm.server.sleep_idle_seconds: must be >= 0, got %d", s.SleepIdleSeconds)
		}
		positive("llm.server.startup_timeout", s.StartupTimeout)
		positive("llm.server.restart_backoff", s.RestartBackoff)
		for _, a := range s.ExtraArgs {
			if a == "--host" || strings.HasPrefix(a, "--host=") || a == "--port" || strings.HasPrefix(a, "--port=") {
				add("llm.server.extra_args: --host/--port are derived from llm.base_url")
			}
		}
	}

	if c.Agents.MaxSteps < 1 || c.Agents.MaxSteps > 8 {
		add("agents.max_steps: must be in [1, 8], got %d", c.Agents.MaxSteps)
	}
	if c.Agents.HistoryMessages < 0 || c.Agents.HistoryMessages > 50 {
		add("agents.history_messages: must be in [0, 50], got %d", c.Agents.HistoryMessages)
	}
	if c.Agents.MaxConcurrentRuns < 1 || c.Agents.MaxConcurrentRuns > 8 {
		add("agents.max_concurrent_runs: must be in [1, 8], got %d", c.Agents.MaxConcurrentRuns)
	}
	positive("agents.run_timeout", c.Agents.RunTimeout)
	positive("agents.approval_ttl", c.Agents.ApprovalTTL)

	if c.Telegram.Enabled {
		if c.Telegram.TokenEnv == "" {
			add("telegram.token_env: must name an environment variable")
		}
		if _, err := url.ParseRequestURI(c.Telegram.APIBaseURL); err != nil {
			add("telegram.api_base_url: %w", err)
		}
		positive("telegram.poll_timeout", c.Telegram.PollTimeout)
	}

	if c.Scheduler.MaxWorkers < 1 || c.Scheduler.MaxWorkers > 8 {
		add("scheduler.max_workers: must be in [1, 8], got %d", c.Scheduler.MaxWorkers)
	}
	positive("scheduler.max_idle_sleep", c.Scheduler.MaxIdleSleep)
	if t := c.Scheduler.DailyPlanTime; t != "" && !hhmm.MatchString(t) {
		add("scheduler.daily_plan_time: must be HH:MM, got %q", t)
	}

	for _, r := range c.Developer.GitHubRepos {
		if !repoName.MatchString(r) {
			add("developer.github_repos: %q is not owner/repo", r)
		}
	}
	if t := c.Developer.DigestTime; t != "" && !hhmm.MatchString(t) {
		add("developer.digest_time: must be HH:MM, got %q", t)
	}
	seen := map[string]bool{}
	for i, m := range c.Developer.Monitors {
		p := fmt.Sprintf("developer.monitors[%d]", i)
		if m.Name == "" || seen[m.Name] {
			add("%s.name: must be non-empty and unique", p)
		}
		seen[m.Name] = true
		u, err := url.ParseRequestURI(m.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			add("%s.url: must be an http(s) URL", p)
		}
		if m.Interval.Duration < 30*time.Second {
			add("%s.interval: must be >= 30s (battery), got %s", p, m.Interval)
		}
		positive(p+".timeout", m.Timeout)
	}

	if c.Research.Enabled {
		if len(c.Research.Providers) == 0 {
			add("research.providers: at least one provider required when enabled")
		}
		for _, p := range c.Research.Providers {
			if p != "wikipedia" && p != "duckduckgo" {
				add("research.providers: unknown provider %q", p)
			}
		}
		if c.Research.MaxResults < 1 || c.Research.MaxResults > 10 {
			add("research.max_results: must be in [1, 10], got %d", c.Research.MaxResults)
		}
		if c.Research.FetchMaxBytes < 4096 {
			add("research.fetch_max_bytes: must be >= 4096")
		}
		positive("research.timeout", c.Research.Timeout)
	}

	if c.Backup.Interval.Duration < 0 {
		add("backup.interval: must be >= 0")
	}
	if c.Backup.Interval.Duration > 0 && c.Backup.Keep < 1 {
		add("backup.keep: must be >= 1 when backups are enabled")
	}
	positive("resources.sample_interval", c.Resources.SampleInterval)
	return errors.Join(errs...)
}

// validateLoopbackAddr accepts only loopback host:port pairs. Port 0 is
// allowed (ephemeral port, used in tests).
func validateLoopbackAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid host:port %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid port %q", portStr)
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("host %q is not a loopback address; LAN binding requires authentication, which is not implemented yet", host)
	}
	return nil
}

// validateLoopbackURL enforces that the inference endpoint is on this device.
func validateLoopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("must be an http://127.0.0.1:<port> URL, got %q", raw)
	}
	if !IsLoopbackHost(u.Hostname()) {
		return fmt.Errorf("host %q is not loopback; cloud/remote inference is not allowed", u.Hostname())
	}
	return nil
}

// IsLoopbackHost reports whether host is localhost or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
