package clientcred_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
)

func respond(status int, retryAfter string, body map[string]any) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, body)
	}
}

func wantRetry(t *testing.T, err error, want time.Duration) {
	t.Helper()
	if !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("err = %v, want ErrTokenUnavailable", err)
	}
	d, ok := auth.RetryAfter(err)
	if want == 0 {
		if ok {
			t.Fatalf("RetryAfter = %v, want none", d)
		}
		return
	}
	if !ok || d != want {
		t.Fatalf("RetryAfter = %v, %v, want %v", d, ok, want)
	}
}

// Issue 17 U2, U3, U4: a 429 starts a wait time. No HTTP call in it. One after it.
func TestI17U2U3U4WaitTime(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = respond(429, "7", map[string]any{"error": "temporarily_unavailable"})
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, 7*time.Second)
	if got := f.outcomes(t); got["unavailable"] != 1 {
		t.Fatalf("outcomes = %v", got)
	}
	f.clock.advance(3 * time.Second)
	_, err = f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, 4*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if got := f.outcomes(t); got["unavailable"] != 1 {
		t.Fatalf("a call in the wait time is counted: %v", got)
	}
	f.clock.advance(5 * time.Second)
	f.ep.handler = nil
	if _, err := f.src.Token(context.Background(), "identity", "s:a:b"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

// Issue 17 U5: an unexpired cached token is returned in the wait time.
func TestI17U5CachedTokenInWait(t *testing.T) {
	f := newFixture(t, setup{})
	first, err := f.src.Token(context.Background(), "identity", "s:a:b")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(85 * time.Second) // after refresh time, before expiry
	f.ep.handler = respond(429, "10", nil)
	for i := 0; i < 2; i++ {
		got, err := f.src.Token(context.Background(), "identity", "s:a:b")
		if err != nil || got != first {
			t.Fatalf("token = %q, err = %v", got, err)
		}
	}
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

// Issue 17 U6: a 429 with no Retry-After.
func TestI17U6NoHeader(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = respond(429, "", nil)
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, 0)
	f.clock.advance(4 * time.Second)
	_, err = f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	f.clock.advance(2 * time.Second)
	f.ep.handler = nil
	if _, err := f.src.Token(context.Background(), "identity", "s:a:b"); err != nil {
		t.Fatal(err)
	}
}

// Issue 17 U7: the exchange gets 429. The second call makes no HTTP call.
func TestI17U7ExchangeWait(t *testing.T) {
	f := newExchangeFixture(t)
	f.ep.handler = respond(429, "2", nil)
	_, err := f.src.Exchange(context.Background(), baseReq())
	wantRetry(t, err, 2*time.Second)
	_, err = f.src.Exchange(context.Background(), baseReq())
	wantRetry(t, err, 2*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if got := f.exchangeOutcomes(t); got["unavailable"] != 1 {
		t.Fatalf("outcomes = %v", got)
	}
}

// Issue 17 U14: the exchange keeps the cached token in the wait time.
func TestI17U14ExchangeCachedInWait(t *testing.T) {
	f := newExchangeFixture(t)
	first, err := f.src.Exchange(context.Background(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(500 * time.Second) // after refresh (480 s), before expiry (600 s)
	f.ep.handler = respond(429, "5", nil)
	for i := 0; i < 2; i++ {
		got, err := f.src.Exchange(context.Background(), baseReq())
		if err != nil || got != first {
			t.Fatalf("token = %q, err = %v", got, err)
		}
	}
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

// Issue 17 U15: no cached token in the wait time.
func TestI17U15ExchangeNoCache(t *testing.T) {
	f := newExchangeFixture(t)
	f.ep.handler = respond(429, "9", nil)
	_, _ = f.src.Exchange(context.Background(), baseReq())
	f.clock.advance(4 * time.Second)
	other := baseReq()
	other.Audience = "other"
	_, err := f.src.Exchange(context.Background(), other)
	wantRetry(t, err, 5*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

// Issue 17 U16: the wait time is for all audiences.
func TestI17U16AllAudiences(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = respond(429, "6", nil)
	_, _ = f.src.Token(context.Background(), "a", "s:a:b")
	f.clock.advance(2 * time.Second)
	_, err := f.src.Token(context.Background(), "b", "s:a:b")
	wantRetry(t, err, 4*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

// Issue 17 U17: the token call and the exchange share the wait time.
func TestI17U17TokenThenExchange(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = respond(429, "6", nil)
	_, _ = f.src.Token(context.Background(), "a", "s:a:b")
	_, err := f.src.Exchange(context.Background(), baseReq())
	wantRetry(t, err, 6*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

// Issue 17 U18: each 429 gives the unavailable error, with an OAuth code and with
// a body that is not JSON.
func TestI17U18Any429(t *testing.T) {
	for name, h := range map[string]func(http.ResponseWriter, *http.Request, int){
		"invalid_scope": respond(429, "3", map[string]any{"error": "invalid_scope"}),
		"not JSON": func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
			_, _ = w.Write([]byte("too many"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.ep.handler = h
			_, err := f.src.Token(context.Background(), "identity", "s:a:b")
			wantRetry(t, err, 3*time.Second)
			if got := f.outcomes(t); got["unavailable"] != 1 || len(got) != 1 {
				t.Fatalf("outcomes = %v", got)
			}
		})
	}
}

// Issue 17 U23: a 503 with Retry-After starts a wait time. A 503 without it does not.
func TestI17U23Status503(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = respond(503, "5", nil)
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, 5*time.Second)
	_, err = f.src.Token(context.Background(), "identity", "s:a:b")
	wantRetry(t, err, 5*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}

	g := newFixture(t, setup{})
	g.ep.handler = respond(503, "", nil)
	for i := 0; i < 2; i++ {
		_, err := g.src.Token(context.Background(), "identity", "s:a:b")
		wantRetry(t, err, 0)
	}
	if n := len(g.ep.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

// Issue 17 U24: the exchange gets 503 with Retry-After.
func TestI17U24Exchange503(t *testing.T) {
	f := newExchangeFixture(t)
	f.ep.handler = respond(503, "5", nil)
	_, err := f.src.Exchange(context.Background(), baseReq())
	wantRetry(t, err, 5*time.Second)
	_, err = f.src.Exchange(context.Background(), baseReq())
	wantRetry(t, err, 5*time.Second)
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}
