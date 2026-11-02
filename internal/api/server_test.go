package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"personal-assistant/internal/config"
)

func testServerConfig() config.ServerConfig {
	c := config.Default().Server
	c.Addr = "127.0.0.1:0"
	return c
}

// startSlowServer serves a handler that signals entry then blocks for delay.
func startSlowServer(t *testing.T, delay time.Duration) (*Server, string, <-chan struct{}, <-chan error) {
	t.Helper()
	entered := make(chan struct{}, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		time.Sleep(delay)
		_, _ = io.WriteString(w, "done")
	})
	s := NewServer(testServerConfig(), h, discardLogger())
	addr, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve() }()
	return s, "http://" + addr.String() + "/", entered, serveErr
}

func TestShutdownDrainsInFlightRequest(t *testing.T) {
	s, url, entered, serveErr := startSlowServer(t, 200*time.Millisecond)

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	r := <-got
	if r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body=%q err=%v, want completed", r.body, r.err)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve returned %v after clean shutdown, want nil", err)
	}
}

func TestShutdownTimeoutForcesClose(t *testing.T) {
	s, url, entered, serveErr := startSlowServer(t, 2*time.Second)
	go func() {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("forced shutdown took %s; should not wait for slow handler", took)
	}
	<-serveErr
}

func TestServeBeforeListen(t *testing.T) {
	s := NewServer(testServerConfig(), http.NotFoundHandler(), discardLogger())
	if err := s.Serve(); err == nil {
		t.Fatal("expected error when Serve is called before Listen")
	}
}
