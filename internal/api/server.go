// Package api hosts the local HTTP server (health now; dashboard and API in
// later phases).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"personal-assistant/internal/config"
)

// Server wraps http.Server with an explicit listen step so bind failures
// (port in use, bad address) surface before the app reports itself started.
type Server struct {
	srv *http.Server
	ln  net.Listener
	log *slog.Logger
}

// NewServer builds a server for cfg serving handler.
func NewServer(cfg config.ServerConfig, handler http.Handler, log *slog.Logger) *Server {
	return &Server{
		log: log,
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
			ReadTimeout:       cfg.ReadTimeout.Duration,
			WriteTimeout:      cfg.WriteTimeout.Duration,
			IdleTimeout:       cfg.IdleTimeout.Duration,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
	}
}

// Listen binds the configured address and returns the actual address
// (useful when the port is 0).
func (s *Server) Listen() (net.Addr, error) {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", s.srv.Addr, err)
	}
	s.ln = ln
	return ln.Addr(), nil
}

// Serve blocks serving requests until Shutdown. It returns nil on a clean
// shutdown.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("api: Serve called before Listen")
	}
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests
// until ctx expires, then force-closes the rest.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("graceful shutdown timed out; forcing close")
		return errors.Join(err, s.srv.Close())
	}
	return err
}
