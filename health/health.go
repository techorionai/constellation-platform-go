package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAddr         = ":9090"
	defaultCheckTimeout = 2 * time.Second
	defaultDrainDelay   = 5 * time.Second
)

// Checker checks one dependency of the service.
type Checker interface {
	// Name is the name of the check in the /readyz body, for example "postgres".
	Name() string
	// Check returns nil when the dependency works.
	Check(ctx context.Context) error
}

// Config is the configuration of the admin server. A service nests it with
// envPrefix:"ADMIN__".
type Config struct {
	// Addr is the listen address of the admin server. The default is ":9090".
	Addr string `env:"ADDR" envDefault:":9090"`
	// CheckTimeout is the time limit for the checkers of one /readyz request. The
	// default is 2 seconds.
	CheckTimeout time.Duration `env:"CHECK_TIMEOUT" envDefault:"2s"`
	// DrainDelay is the time that [Server.Drain] waits after /readyz changes to
	// 503. The default is 5 seconds. It must be longer than the time that the
	// load balancer needs to remove the instance.
	DrainDelay time.Duration `env:"DRAIN_DELAY" envDefault:"5s"`
}

func (c Config) withDefaults() Config {
	if c.Addr == "" {
		c.Addr = defaultAddr
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = defaultCheckTimeout
	}
	if c.DrainDelay <= 0 {
		c.DrainDelay = defaultDrainDelay
	}
	return c
}

// State is the readiness state of the service.
type State struct {
	checkers []Checker
	started  atomic.Bool
	draining atomic.Bool
}

// NewState makes the state with the given checkers. It panics if a checker has
// an empty name or if two checkers have the same name. This is a programming
// error.
func NewState(checkers ...Checker) *State {
	seen := make(map[string]struct{}, len(checkers))
	for _, c := range checkers {
		name := c.Name()
		if name == "" {
			panic("health: a checker has an empty name")
		}
		if _, dup := seen[name]; dup {
			panic("health: two checkers have the name " + name)
		}
		seen[name] = struct{}{}
	}
	return &State{checkers: append([]Checker(nil), checkers...)}
}

// MarkStarted tells the state that the migrations and the startup work are
// complete.
func (s *State) MarkStarted() { s.started.Store(true) }

// MarkDraining tells the state that a graceful shutdown started. /readyz is 503
// from then on.
func (s *State) MarkDraining() { s.draining.Store(true) }

func (s *State) accepting() bool { return s.started.Load() && !s.draining.Load() }

// problem has the same JSON field names as the Problem model of
// constellation-contracts v0.1.0 (RFC 9457).
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

// NewHandler returns the handler for /livez and /readyz.
func NewHandler(state *State, cfg Config) http.Handler {
	cfg = cfg.withDefaults()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, "application/json", map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !state.accepting() {
			writeNotReady(w)
			return
		}
		results := runChecks(r.Context(), state.checkers, cfg.CheckTimeout)
		checks := make(map[string]string, len(results))
		failed := false
		for _, res := range results {
			if res.err != nil {
				failed = true
				slog.WarnContext(r.Context(), "health: check failed",
					slog.String("check", res.name), slog.Any("error", res.err))
				continue
			}
			checks[res.name] = "ok"
		}
		if failed || !state.accepting() {
			writeNotReady(w)
			return
		}
		writeJSON(w, http.StatusOK, "application/json", struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}{Status: "ready", Checks: checks})
	})
	return mux
}

type checkResult struct {
	name string
	err  error
}

// runChecks runs all checkers at the same time. It waits a maximum of timeout. A
// checker that has no result at that time is a failed check. The goroutine of
// such a checker ends later, and its result is ignored.
func runChecks(parent context.Context, checkers []Checker, timeout time.Duration) []checkResult {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	out := make([]checkResult, len(checkers))
	var mu sync.Mutex
	done := make([]bool, len(checkers))
	finished := make(chan struct{}, len(checkers))
	for i, c := range checkers {
		go func() {
			err := c.Check(ctx)
			mu.Lock()
			out[i] = checkResult{name: c.Name(), err: err}
			done[i] = true
			mu.Unlock()
			finished <- struct{}{}
		}()
	}
	// Wait for all checkers, or until the time limit ends. The loop below marks
	// the open checks as failed.
	for remaining := len(checkers); remaining > 0; {
		select {
		case <-finished:
			remaining--
		case <-ctx.Done():
			remaining = 0
		}
	}
	mu.Lock()
	defer mu.Unlock()
	res := make([]checkResult, len(checkers))
	for i, c := range checkers {
		if done[i] {
			res[i] = out[i]
		} else {
			res[i] = checkResult{name: c.Name(), err: context.DeadlineExceeded}
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].name < res[j].name })
	return res
}

func writeNotReady(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, "application/problem+json", problem{
		Type:   "about:blank",
		Title:  "Service Unavailable",
		Status: http.StatusServiceUnavailable,
		Detail: "The service is not ready.",
		Code:   "service_not_ready",
	})
}

func writeJSON(w http.ResponseWriter, status int, contentType string, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
