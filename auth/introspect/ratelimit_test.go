package introspect_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/auth/introspect"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

func status429(retryAfter string) func(w http.ResponseWriter, r *http.Request, n int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

// Issue 17 U8: a 429 with no OAuth body gives the unavailable error, a warn
// record, and no error record.
func TestI17U8Verifier429(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = status429("7")
	_, err := f.v.Verify(context.Background(), token(1))
	if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if d, ok := auth.RetryAfter(err); !ok || d != 7*time.Second {
		t.Fatalf("RetryAfter = %v, %v", d, ok)
	}
	if got := f.outcomes(t); got["unavailable"] != 1 || got["invalid_response"] != 0 {
		t.Fatalf("outcomes = %v", got)
	}
	if f.logs.count("level=WARN") != 1 || f.logs.count("level=ERROR") != 0 {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

// Issue 17 U9: during the wait time the cached result stays, a new token gets the
// unavailable error, and no HTTP call is made.
func TestI17U9WaitTime(t *testing.T) {
	f := newFixture(t, setup{})
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
	f.handler = status429("10")
	if _, err := f.v.Verify(context.Background(), token(2)); err == nil {
		t.Fatal("want an error")
	}
	calls := f.httpCalls()
	f.clock.advance(3 * time.Second)
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatalf("cached token: %v", err)
	}
	_, err := f.v.Verify(context.Background(), token(3))
	if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if d, ok := auth.RetryAfter(err); !ok || d != 7*time.Second {
		t.Fatalf("RetryAfter = %v, %v", d, ok)
	}
	if f.httpCalls() != calls {
		t.Fatalf("calls = %d, want %d", f.httpCalls(), calls)
	}
	if got := f.outcomes(t); got["unavailable"] != 1 {
		t.Fatalf("a call in the wait time is counted: %v", got)
	}
	f.clock.advance(8 * time.Second)
	f.handler = nil
	if _, err := f.v.Verify(context.Background(), token(3)); err != nil {
		t.Fatalf("after the wait time: %v", err)
	}
}

// Issue 17 U19: a 429 with no Retry-After gives RetryAfter 0 and a wait time of 5 s.
func TestI17U19No429Header(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = status429("")
	_, err := f.v.Verify(context.Background(), token(1))
	if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := auth.RetryAfter(err); ok {
		t.Fatal("RetryAfter must be 0")
	}
	f.clock.advance(3 * time.Second)
	calls := f.httpCalls()
	if _, err := f.v.Verify(context.Background(), token(2)); !errors.Is(err, auth.ErrIntrospectionUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if f.httpCalls() != calls {
		t.Fatal("the second call made an HTTP call")
	}
	f.clock.advance(3 * time.Second)
	f.handler = nil
	if _, err := f.v.Verify(context.Background(), token(2)); err != nil {
		t.Fatal(err)
	}
}

// Issue 17 U20: the local limit.
func TestI17U20LocalLimit(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *introspect.Config) { c.MaxCallsPerSecond = 2 }})
	var overloaded int
	for i := 1; i <= 5; i++ {
		_, err := f.v.Verify(context.Background(), token(i))
		if err == nil {
			continue
		}
		if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if d, ok := auth.RetryAfter(err); !ok || d != time.Second {
			t.Fatalf("RetryAfter = %v, %v", d, ok)
		}
		overloaded++
	}
	if f.httpCalls() != 2 || overloaded != 3 {
		t.Fatalf("calls = %d, overloaded = %d", f.httpCalls(), overloaded)
	}
	if got := f.outcomes(t); got["overloaded"] != 3 {
		t.Fatalf("outcomes = %v", got)
	}
}

// Issue 17 U21: cached results, positive and negative, do not use the local limit.
func TestI17U21CacheNotLimited(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *introspect.Config) { c.MaxCallsPerSecond = 2 }})
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, 200, map[string]any{"active": false})
	}
	if _, err := f.v.Verify(context.Background(), token(10)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("err = %v", err)
	}
	// The limit is full now. A new token gets the exact limit error.
	_, err := f.v.Verify(context.Background(), token(11))
	if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
		t.Fatalf("new token: err = %v", err)
	}
	if d, ok := auth.RetryAfter(err); !ok || d != time.Second {
		t.Fatalf("RetryAfter = %v, %v", d, ok)
	}
	// The cached tokens still work, and they make no HTTP call.
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatalf("positive cache: %v", err)
	}
	if _, err := f.v.Verify(context.Background(), token(10)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("negative cache: %v", err)
	}
	if f.httpCalls() != 2 {
		t.Fatalf("calls = %d, want 2", f.httpCalls())
	}
}

// Issue 17 U22: the range of MaxCallsPerSecond.
func TestI17U22MaxCallsConfig(t *testing.T) {
	base := introspect.Config{
		Issuer: "https://issuer.example", Audience: audience, CacheTTL: 30 * time.Second, InactiveTTL: 5 * time.Second,
		CacheSize: 10000, InactiveCacheSize: 1000, MaxConcurrent: 16, MaxCallsPerSecond: 20, HTTPTimeout: 5 * time.Second,
	}
	for _, n := range []int{0, 1001} {
		c := base
		c.MaxCallsPerSecond = n
		_, err := introspect.New(c, &mock.ClientAssertion{})
		if err == nil || !strings.Contains(err.Error(), "AUTH__INTROSPECT__MAX_CALLS_PER_SECOND") {
			t.Fatalf("n = %d, err = %v", n, err)
		}
	}
	t.Setenv("TEST_AUTH__INTROSPECT__ISSUER", "https://issuer.example")
	t.Setenv("TEST_AUTH__INTROSPECT__AUDIENCE", audience)
	var cfg struct {
		Introspect introspect.Config `envPrefix:"AUTH__INTROSPECT__"`
	}
	if err := config.Load("TEST_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Introspect.MaxCallsPerSecond != 20 {
		t.Fatalf("default = %d", cfg.Introspect.MaxCallsPerSecond)
	}
}

// Issue 17 U24: a 503 with Retry-After gives a wait time. A 503 with no value does not.
func TestI17U24Verifier503(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(503)
	}
	_, err := f.v.Verify(context.Background(), token(1))
	if d, ok := auth.RetryAfter(err); !ok || d != 5*time.Second {
		t.Fatalf("RetryAfter = %v, %v, err = %v", d, ok, err)
	}
	calls := f.httpCalls()
	if _, err := f.v.Verify(context.Background(), token(2)); err == nil || f.httpCalls() != calls {
		t.Fatalf("the wait time is not active, err = %v", err)
	}
	g := newFixture(t, setup{})
	g.handler = func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(503) }
	for i := 1; i <= 2; i++ {
		_, err := g.v.Verify(context.Background(), token(i))
		if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if _, ok := auth.RetryAfter(err); ok {
			t.Fatal("a 503 with no value must not give a wait time")
		}
	}
	if g.httpCalls() != 2 {
		t.Fatalf("calls = %d", g.httpCalls())
	}
}

// Review R1: the wait time of the verifier has a maximum of 60 s, the same as the
// Retry-After header of the middleware.
func TestI17R1MaxWait(t *testing.T) {
	for _, value := range []string{"3600", "99999"} {
		f := newFixture(t, setup{})
		f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Retry-After", value)
			w.WriteHeader(503)
		}
		_, err := f.v.Verify(context.Background(), token(1))
		if d, ok := auth.RetryAfter(err); !ok || d != time.Minute {
			t.Fatalf("%s: RetryAfter = %v, %v", value, d, ok)
		}
		f.clock.advance(61 * time.Second)
		f.handler = nil
		if _, err := f.v.Verify(context.Background(), token(2)); err != nil {
			t.Fatalf("%s: after the maximum wait: %v", value, err)
		}
	}
}

// Review R4: a 429 with a body that is too large still starts the wait time.
func TestI17R4LargeBody429(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(429)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 70<<10))
	}
	_, err := f.v.Verify(context.Background(), token(1))
	if d, ok := auth.RetryAfter(err); !ok || d != 9*time.Second {
		t.Fatalf("RetryAfter = %v, %v, err = %v", d, ok, err)
	}
	if got := f.outcomes(t); got["unavailable"] != 1 || got["invalid_response"] != 0 {
		t.Fatalf("outcomes = %v", got)
	}
}
