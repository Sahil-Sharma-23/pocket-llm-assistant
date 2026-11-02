package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) LookupEnvFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load("", envMap(map[string]string{"PA_HOME": home}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:8787" {
		t.Errorf("addr = %q, want 127.0.0.1:8787", cfg.Server.Addr)
	}
	if got, want := cfg.Paths.DataDir, filepath.Join(home, "data"); got != want {
		t.Errorf("data dir = %q, want %q", got, want)
	}
	if cfg.Log.File != "" {
		t.Errorf("default log file = %q, want stderr (empty)", cfg.Log.File)
	}
	if cfg.Server.ShutdownTimeout.Duration != 10*time.Second {
		t.Errorf("shutdown timeout = %s", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadFileAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "config.json", `{
		"home": "`+filepath.ToSlash(dir)+`",
		"server": {"addr": "127.0.0.1:9000", "shutdown_timeout": "3s"},
		"log": {"level": "debug", "file": "app.log"},
		"runtime": {"memory_limit_mb": 128}
	}`)

	cfg, err := Load(path, envMap(map[string]string{
		"PA_HTTP_ADDR":       "localhost:9001",
		"PA_MEMORY_LIMIT_MB": "64",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"env overrides file addr", cfg.Server.Addr, "localhost:9001"},
		{"file overrides default level", cfg.Log.Level, "debug"},
		{"file duration parsed", cfg.Server.ShutdownTimeout.Duration, 3 * time.Second},
		{"default kept when absent", cfg.Server.ReadTimeout.Duration, 15 * time.Second},
		{"env int override", cfg.Runtime.MemoryLimitMB, 64},
		{"log file resolved under log dir", cfg.Log.File, filepath.Join(dir, "logs", "app.log")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		file    string // empty = no file
		missing bool   // point at a non-existent file
		env     map[string]string
		wantErr string
	}{
		{
			name:    "unknown field rejected",
			file:    `{"server": {"adr": "127.0.0.1:1"}}`,
			wantErr: `unknown field "adr"`,
		},
		{
			name:    "bad duration",
			file:    `{"server": {"read_timeout": "soon"}}`,
			wantErr: "invalid duration",
		},
		{
			name:    "non-integer env",
			env:     map[string]string{"PA_MAX_PROCS": "two"},
			wantErr: "PA_MAX_PROCS",
		},
		{
			name:    "missing file",
			missing: true,
			wantErr: "read config",
		},
		{
			name:    "validation runs after env",
			env:     map[string]string{"PA_HTTP_ADDR": "0.0.0.0:8787"},
			wantErr: "not a loopback",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"PA_HOME": dir}
			for k, v := range tt.env {
				env[k] = v
			}
			path := ""
			switch {
			case tt.missing:
				path = filepath.Join(dir, "does-not-exist.json")
			case tt.file != "":
				path = writeFile(t, dir, "c.json", tt.file)
			}
			_, err := Load(path, envMap(env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty = valid
	}{
		{"defaults valid", func(*Config) {}, ""},
		{"ipv6 loopback ok", func(c *Config) { c.Server.Addr = "[::1]:8787" }, ""},
		{"localhost ok", func(c *Config) { c.Server.Addr = "localhost:8787" }, ""},
		{"ephemeral port ok", func(c *Config) { c.Server.Addr = "127.0.0.1:0" }, ""},
		{"all interfaces rejected", func(c *Config) { c.Server.Addr = ":8787" }, "not a loopback"},
		{"lan ip rejected", func(c *Config) { c.Server.Addr = "192.168.1.10:8787" }, "not a loopback"},
		{"wildcard rejected", func(c *Config) { c.Server.Addr = "0.0.0.0:8787" }, "not a loopback"},
		{"hostname rejected", func(c *Config) { c.Server.Addr = "example.com:8787" }, "not a loopback"},
		{"missing port", func(c *Config) { c.Server.Addr = "127.0.0.1" }, "invalid host:port"},
		{"port out of range", func(c *Config) { c.Server.Addr = "127.0.0.1:70000" }, "invalid port"},
		{"bad level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"zero timeout", func(c *Config) { c.Server.ShutdownTimeout = Duration{} }, "shutdown_timeout"},
		{"tiny memory limit", func(c *Config) { c.Runtime.MemoryLimitMB = 8 }, "memory_limit_mb"},
		{"memory limit unset ok", func(c *Config) { c.Runtime.MemoryLimitMB = 0 }, ""},
		{"negative procs", func(c *Config) { c.Runtime.MaxProcs = -1 }, "max_procs"},
		{"file without size", func(c *Config) { c.Log.File = "/x.log"; c.Log.MaxSizeMB = 0 }, "max_size_mb"},
		{"empty data dir", func(c *Config) { c.Paths.DataDir = "" }, "data_dir"},
		{"remote llm rejected", func(c *Config) { c.LLM.BaseURL = "http://api.example.com:8080" }, "not loopback"},
		{"https cloud llm rejected", func(c *Config) { c.LLM.BaseURL = "https://127.0.0.1:8080" }, "llm.base_url"},
		{"unknown provider", func(c *Config) { c.LLM.Provider = "openai" }, "llm.provider"},
		{"too many threads", func(c *Config) { c.LLM.Server.Threads = 16 }, "threads"},
		{"host in extra args", func(c *Config) { c.LLM.Server.ExtraArgs = []string{"--host", "0.0.0.0"} }, "extra_args"},
		{"unmanaged ignores server fields", func(c *Config) { c.LLM.Server = LlamaServerConfig{} }, ""},
		{"bad repo", func(c *Config) { c.Developer.GitHubRepos = []string{"not-a-repo"} }, "owner/repo"},
		{"good repo", func(c *Config) { c.Developer.GitHubRepos = []string{"golang/go"} }, ""},
		{"bad plan time", func(c *Config) { c.Scheduler.DailyPlanTime = "8am" }, "HH:MM"},
		{"bad timezone", func(c *Config) { c.Timezone = "Mars/Olympus" }, "timezone"},
		{"good timezone", func(c *Config) { c.Timezone = "Europe/Berlin" }, ""},
		{"monitor too frequent", func(c *Config) {
			c.Developer.Monitors = []MonitorConfig{{Name: "a", URL: "https://x.dev", Interval: Duration{time.Second}, Timeout: Duration{time.Second}}}
		}, "interval"},
		{"monitor bad url", func(c *Config) {
			c.Developer.Monitors = []MonitorConfig{{Name: "a", URL: "ftp://x", Interval: Duration{time.Minute}, Timeout: Duration{time.Second}}}
		}, "url"},
		{"duplicate monitors", func(c *Config) {
			m := MonitorConfig{Name: "a", URL: "https://x.dev", Interval: Duration{time.Minute}, Timeout: Duration{time.Second}}
			c.Developer.Monitors = []MonitorConfig{m, m}
		}, "unique"},
		{"unknown research provider", func(c *Config) { c.Research.Providers = []string{"google"} }, "unknown provider"},
		{"telegram enabled needs token env", func(c *Config) { c.Telegram.Enabled = true; c.Telegram.TokenEnv = "" }, "token_env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.Home = "/tmp"
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	c := Default()
	c.Log.Level = "nope"
	c.Log.Format = "nope"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"log.level", "log.format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
