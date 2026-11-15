package health

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Server runs the admin handler on the admin port.
type Server struct {
	http  *http.Server
	state *State
	drain time.Duration
}

// NewServer makes the admin server. Its timeouts are: read header 5 seconds,
// read 10 seconds, write 10 seconds, and idle 60 seconds.
func NewServer(cfg Config, state *State) *Server {
	cfg = cfg.withDefaults()
	return &Server{
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           NewHandler(state, cfg),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		state: state,
		drain: cfg.DrainDelay,
	}
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
