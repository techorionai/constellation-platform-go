package health

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	minWriteTimeout = 10 * time.Second
	writeMargin     = 5 * time.Second
)

// Server runs the admin handler on the admin port.
type Server struct {
	http  *http.Server
	state *State
	drain time.Duration
}

// NewServer makes the admin server. Its timeouts are: read header 5 seconds,
// read 10 seconds, write 10 seconds or CheckTimeout plus 5 seconds (the longer
// one), and idle 60 seconds.
func NewServer(cfg Config, state *State) *Server {
	cfg = cfg.withDefaults()
	return &Server{
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           NewHandler(state, cfg),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      writeTimeout(cfg),
			IdleTimeout:       60 * time.Second,
		},
		state: state,
		drain: cfg.DrainDelay,
	}
}

// writeTimeout returns the write timeout of the server. It is longer than
// CheckTimeout, so that a slow check cannot close the connection before the
// handler writes the response.
func writeTimeout(cfg Config) time.Duration {
	return max(minWriteTimeout, cfg.CheckTimeout+writeMargin)
}

// ListenAndServe serves requests until [Server.Shutdown]. It returns nil after
// Shutdown.
func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Drain calls [State.MarkDraining], then waits for DrainDelay or the end of ctx.
// During the wait, the load balancer sees 503 on /readyz. If ctx ends first,
// Drain returns ctx.Err().
func (s *Server) Drain(ctx context.Context) error {
	s.state.MarkDraining()
	timer := time.NewTimer(s.drain)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the server. The admin server stops last.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
