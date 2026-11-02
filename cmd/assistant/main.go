// Command assistant runs the local-first personal AI assistant.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"personal-assistant/internal/app"
	"personal-assistant/internal/buildinfo"
	"personal-assistant/internal/config"
	"personal-assistant/internal/logging"
)

// Exit codes.
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("assistant", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", os.Getenv("PA_CONFIG"), "path to JSON config file (env PA_CONFIG)")
	showVersion := fs.Bool("version", false, "print version and exit")
	checkConfig := fs.Bool("check-config", false, "validate config, print the resolved result and exit")
	if err := fs.Parse(args); err != nil {
		return exitConfig
	}

	if *showVersion {
		bi := buildinfo.Get()
		fmt.Fprintf(stdout, "assistant %s (%s) %s %s/%s\n", bi.Version, bi.Commit, bi.GoVersion, bi.OS, bi.Arch)
		return exitOK
	}

	cfg, err := config.Load(*configPath, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "config error:\n%v\n", err)
		return exitConfig
	}
	if *checkConfig {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(cfg)
		return exitOK
	}

	// Make local time correct before anything formats a timestamp (Go on
	// Android otherwise uses UTC regardless of TZ).
	if loc, err := cfg.Location(); err == nil {
		time.Local = loc
	}

	log, closeLog, err := logging.New(cfg.Log, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "logging error: %v\n", err)
		return exitConfig
	}
	defer closeLog()

	bi := buildinfo.Get()
	log.Info("starting assistant",
		"version", bi.Version, "commit", bi.Commit, "go", bi.GoVersion,
		"os", bi.OS, "arch", bi.Arch, "pid", os.Getpid(), "cpus", runtime.NumCPU(),
		"config", *configPath, "home", cfg.Home, "addr", cfg.Server.Addr,
		"data_dir", cfg.Paths.DataDir, "memory_limit_mb", cfg.Runtime.MemoryLimitMB,
		"max_procs", cfg.Runtime.MaxProcs, "timezone", time.Local.String())

	// SIGHUP is deliberately not handled: when launched via nohup it is
	// ignored, and signal.Notify would un-ignore it, so closing the Termux
	// session would then stop the assistant.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Restore default signal behaviour: a second Ctrl-C/SIGTERM during a
		// slow shutdown kills the process immediately.
		stop()
	}()

	a, err := app.New(cfg, log)
	if err != nil {
		log.Error("init failed", "err", err)
		return exitRuntime
	}
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("assistant stopped with error", "err", err)
		return exitRuntime
	}
	log.Info("assistant stopped")
	return exitOK
}
