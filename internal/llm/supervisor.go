package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// SupervisorOptions configures the llama-server child process.
type SupervisorOptions struct {
	Binary           string
	ModelPath        string
	BaseURL          string // host/port are derived from it
	ContextSize      int
	Threads          int
	ThreadsBatch     int
	SleepIdleSeconds int
	ExtraArgs        []string
	LogFile          string
	StartupTimeout   time.Duration
	RestartBackoff   time.Duration
	MinFreeMemMB     int64
	MemAvailable     MemAvailableFunc
	Health           func(ctx context.Context) error
	Log              *slog.Logger
}

// Supervisor states.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateWaiting  = "waiting" // backing off after a failure
)

// Supervisor keeps exactly one llama-server process alive: it starts it
// (only if enough RAM is free), restarts it with backoff if Android or a
// crash kills it, and stops it gracefully on shutdown. Model memory is
// released while idle by llama-server's own --sleep-idle-seconds.
type Supervisor struct {
	o SupervisorOptions

	mu        sync.Mutex
	state     string
	pid       int
	lastErr   string
	restarts  int
	startedAt time.Time
	ready     chan struct{} // closed when state becomes running
}

// NewSupervisor validates options.
func NewSupervisor(o SupervisorOptions) (*Supervisor, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Health == nil {
		return nil, errors.New("supervisor: Health func required")
	}
	return &Supervisor{o: o, state: StateStopped, ready: make(chan struct{})}, nil
}

// Args returns the llama-server command line (exposed for logging/tests).
func (s *Supervisor) Args() ([]string, error) {
	u, err := url.Parse(s.o.BaseURL)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-m", s.o.ModelPath,
		"--host", u.Hostname(),
		"--port", u.Port(),
		"-c", strconv.Itoa(s.o.ContextSize),
		"-t", strconv.Itoa(s.o.Threads),
		"-tb", strconv.Itoa(max(s.o.ThreadsBatch, s.o.Threads)),
		"-np", "1", // one slot: one inference at a time on this device
		"--no-webui",
		"--chat-template-kwargs", `{"enable_thinking":false}`,
	}
	if s.o.SleepIdleSeconds > 0 {
		args = append(args, "--sleep-idle-seconds", strconv.Itoa(s.o.SleepIdleSeconds))
	}
	return append(args, s.o.ExtraArgs...), nil
}

// Run supervises until ctx is cancelled. Call in its own goroutine.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.o.RestartBackoff
	for ctx.Err() == nil {
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		s.restarts++
		s.lastErr = fmt.Sprint(err)
		s.state = StateWaiting
		s.resetReadyLocked()
		s.mu.Unlock()
		s.o.Log.Warn("llama-server stopped; restarting after backoff", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
		if err == nil {
			backoff = s.o.RestartBackoff
		}
	}
	s.mu.Lock()
	s.state = StateStopped
	s.pid = 0
	s.mu.Unlock()
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	if _, err := os.Stat(s.o.ModelPath); err != nil {
		return fmt.Errorf("model file: %w", err)
	}
	bin, err := exec.LookPath(s.o.Binary)
	if err != nil {
		return fmt.Errorf("llama-server binary: %w", err)
	}
	if s.o.MinFreeMemMB > 0 && s.o.MemAvailable != nil {
		if avail, err := s.o.MemAvailable(); err == nil && avail < s.o.MinFreeMemMB {
			return fmt.Errorf("%w: %d MB available, %d MB required to start llama-server",
				ErrInsufficientMemory, avail, s.o.MinFreeMemMB)
		}
	}
	args, err := s.Args()
	if err != nil {
		return err
	}

	logf, err := openChildLog(s.o.LogFile)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.setState(StateStarting)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	s.mu.Lock()
	s.pid, s.startedAt = cmd.Process.Pid, time.Now()
	s.mu.Unlock()
	s.o.Log.Info("llama-server started", "pid", cmd.Process.Pid, "args", args)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	if err := s.waitHealthy(ctx, exited); err != nil {
		stopProcess(cmd, exited)
		return err
	}
	s.mu.Lock()
	s.state, s.lastErr = StateRunning, ""
	close(s.ready)
	s.mu.Unlock()
	s.o.Log.Info("llama-server healthy", "startup_ms", time.Since(s.startedAt).Milliseconds())

	select {
	case <-ctx.Done():
		s.o.Log.Info("stopping llama-server")
		stopProcess(cmd, exited)
		return nil
	case err := <-exited:
		return fmt.Errorf("llama-server exited: %v", err)
	}
}

func (s *Supervisor) waitHealthy(ctx context.Context, exited <-chan error) error {
	deadline := time.Now().Add(s.o.StartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("llama-server exited during startup: %v (see %s)", err, s.o.LogFile)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if s.o.Health(ctx) == nil {
			return nil
		}
	}
	return fmt.Errorf("llama-server not healthy within %s", s.o.StartupTimeout)
}

// stopProcess sends SIGTERM to the process group, then SIGKILL after 10s.
func stopProcess(cmd *exec.Cmd, exited <-chan error) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-exited
	}
}

// Ensure waits until the server is running (or ctx expires). It implements
// Ensurer for Guard so requests made during a (re)start wait instead of
// failing immediately.
func (s *Supervisor) Ensure(ctx context.Context) error {
	s.mu.Lock()
	ready, state, lastErr := s.ready, s.state, s.lastErr
	s.mu.Unlock()
	if state == StateRunning {
		return nil
	}
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		if lastErr != "" {
			return fmt.Errorf("%w: %s", ErrUnavailable, lastErr)
		}
		return fmt.Errorf("%w: %s", ErrUnavailable, state)
	}
}

// Status is a snapshot for health/dashboard.
type Status struct {
	State     string    `json:"state"`
	PID       int       `json:"pid"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"last_error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Status returns the current supervisor state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{State: s.state, PID: s.pid, Restarts: s.restarts, LastError: s.lastErr, StartedAt: s.startedAt}
}

func (s *Supervisor) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *Supervisor) resetReadyLocked() {
	select {
	case <-s.ready:
		s.ready = make(chan struct{})
	default:
	}
}

// openChildLog opens the llama-server log, rotating it once past 10 MB.
func openChildLog(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if st, err := os.Stat(path); err == nil && st.Size() > 10<<20 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
