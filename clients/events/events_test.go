package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clients/events"
	"github.com/techorionai/constellation-platform-go/eventtypes"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

var testReg = eventtypes.Registration{
	Source: "/helpdesk", App: "helpdesk",
	EventTypes: []eventtypes.EventType{
		{Type: "helpdesk.ticket.created", Description: "Created.", DataSchema: json.RawMessage(`{"type":"object"}`)},
		{Type: "helpdesk.ticket.closed", Description: "Closed.", DataSchema: json.RawMessage(`{"type":"object"}`), Deprecated: true},
	},
}

func okTokens() *mock.TokenSource {
	return &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) { return "tok", nil }}
}

func problem(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newClient(t *testing.T, url string, ts auth.TokenSource, opts ...events.Option) *events.ClientWithResponses {
	t.Helper()
	c, err := events.New(events.Config{URL: url, Timeout: 2 * time.Second, AllowInsecureHTTP: true}, ts, []string{"events:event_type:register"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Issue 22 U3: the error map of the registry adapter.
func TestU3RegistryErrors(t *testing.T) {
	type tc struct {
		name    string
		handler http.HandlerFunc
		tokens  *mock.TokenSource
		check   func(t *testing.T, res eventtypes.Result, err error)
	}
	is := func(want error) func(*testing.T, eventtypes.Result, error) {
		return func(t *testing.T, _ eventtypes.Result, err error) {
			t.Helper()
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
		}
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	tokenErr := func(err error) *mock.TokenSource {
		return &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) { return "", err }}
	}
	var gotBody events.EventTypeRegistration
	var gotPath, gotAuth string
	cases := []tc{
		{name: "200", handler: func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"app":"helpdesk","version":3,"eventTypeCount":2,"registeredAt":"2026-01-01T00:00:00Z"}`))
		}, check: func(t *testing.T, res eventtypes.Result, err error) {
			if err != nil {
				t.Fatal(err)
			}
			if res.App != "helpdesk" || res.Version != 3 || res.EventTypeCount != 2 || res.RegisteredAt.Year() != 2026 {
				t.Errorf("result = %+v", res)
			}
		}},
		{name: "413", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 413, map[string]any{"type": "about:blank", "title": "Too large", "status": 413, "code": "content_too_large"})
		}, check: func(t *testing.T, _ eventtypes.Result, err error) {
			var inv *eventtypes.InvalidError
			if !errors.As(err, &inv) || inv.Code != "content_too_large" {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "422 mismatch", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 422, map[string]any{"type": "about:blank", "title": "Unprocessable", "status": 422, "code": "registration_app_mismatch"})
		}, check: func(t *testing.T, _ eventtypes.Result, err error) {
			var inv *eventtypes.InvalidError
			if !errors.As(err, &inv) || len(inv.Codes()) != 1 || inv.Codes()[0] != "registration_app_mismatch" {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "422 schema", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 422, map[string]any{
				"type": "about:blank", "title": "Unprocessable", "status": 422, "code": "unprocessable_content",
				"errors": []map[string]any{{"field": "/eventTypes/0/dataSchema", "code": "invalid_data_schema", "message": "m"}},
			})
		}, check: func(t *testing.T, _ eventtypes.Result, err error) {
			var inv *eventtypes.InvalidError
			if !errors.As(err, &inv) || len(inv.Problems) != 1 ||
				inv.Problems[0].Code != "invalid_data_schema" || inv.Problems[0].Field != "/eventTypes/0/dataSchema" {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "422 no body", handler: status(422), check: func(t *testing.T, _ eventtypes.Result, err error) {
			var inv *eventtypes.InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "401", handler: status(401), check: is(eventtypes.ErrDenied)},
		{name: "403", handler: status(403), check: is(eventtypes.ErrDenied)},
		{name: "429 no header", handler: status(429), check: is(eventtypes.ErrUnavailable)},
		{name: "408", handler: status(408), check: is(eventtypes.ErrUnavailable)},
		{name: "500", handler: status(500), check: is(eventtypes.ErrUnavailable)},
		{name: "503 retry-after", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(503)
		}, check: func(t *testing.T, _ eventtypes.Result, err error) {
			if !errors.Is(err, eventtypes.ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if d, ok := eventtypes.RetryAfter(err); !ok || d != 30*time.Second {
				t.Errorf("RetryAfter = %v %v", d, ok)
			}
		}},
		{name: "400", handler: status(400), check: is(eventtypes.ErrRejected)},
		{name: "404", handler: status(404), check: is(eventtypes.ErrRejected)},
		{name: "409", handler: status(409), check: is(eventtypes.ErrRejected)},
		{name: "invalid_client", handler: status(200), tokens: tokenErr(&auth.OAuthError{Code: "invalid_client"}), check: is(eventtypes.ErrDenied)},
		{name: "invalid_scope", handler: status(200), tokens: tokenErr(&auth.OAuthError{Code: "invalid_scope"}), check: is(eventtypes.ErrDenied)},
		{name: "other oauth", handler: status(200), tokens: tokenErr(&auth.OAuthError{Code: "server_error"}), check: is(eventtypes.ErrUnavailable)},
		{name: "token unavailable", handler: status(200), tokens: tokenErr(auth.ErrTokenUnavailable), check: is(eventtypes.ErrUnavailable)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			t.Cleanup(srv.Close)
			ts := c.tokens
			if ts == nil {
				ts = okTokens()
			}
			res, err := events.NewEventTypeRegistry(newClient(t, srv.URL, ts)).Register(context.Background(), testReg)
			c.check(t, res, err)
		})
	}
	if gotPath != "PUT /v1/event-type-registrations/helpdesk" {
		t.Errorf("request = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody.App != "helpdesk" || gotBody.Source != "/helpdesk" || len(gotBody.EventTypes) != 2 ||
		gotBody.EventTypes[1].Deprecated == nil || !*gotBody.EventTypes[1].Deprecated {
		t.Errorf("body = %+v", gotBody)
	}
}

// Issue 22 U13: config rules, response limit, and no redirect.
func TestU13Config(t *testing.T) {
	good := events.Config{URL: "http://localhost", Timeout: time.Second, AllowInsecureHTTP: true}
	scopes := []string{"events:event_type:register"}
	for _, tc := range []struct {
		name string
		cfg  events.Config
		ts   auth.TokenSource
		sc   []string
		opts []events.Option
		want string
	}{
		{"no url", events.Config{Timeout: time.Second}, okTokens(), scopes, nil, "EVENTS__URL"},
		{"http", events.Config{URL: "http://localhost", Timeout: time.Second}, okTokens(), scopes, nil, "EVENTS__URL"},
		{"no token source", good, nil, scopes, nil, "token source"},
		{"no scopes", good, okTokens(), nil, nil, "scope"},
		{"no timeout", events.Config{URL: "http://localhost", AllowInsecureHTTP: true}, okTokens(), scopes, nil, "EVENTS__TIMEOUT"},
		{"limit too small", good, okTokens(), scopes, []events.Option{events.WithMaxResponseBytes(1023)}, "WithMaxResponseBytes"},
		{"limit too large", good, okTokens(), scopes, []events.Option{events.WithMaxResponseBytes(64<<20 + 1)}, "WithMaxResponseBytes"},
	} {
		_, err := events.New(tc.cfg, tc.ts, tc.sc, tc.opts...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want text %q", tc.name, err, tc.want)
		}
	}
	if _, err := events.New(good, okTokens(), scopes, events.WithMaxResponseBytes(1024), events.WithMaxResponseBytes(64<<20)); err != nil {
		t.Errorf("limit at the range edge: %v", err)
	}

	// A response of 1 MiB plus 1 byte.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"helpdesk","version":1,"eventTypeCount":2,"registeredAt":"2026-01-01T00:00:00Z","pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
		_, _ = w.Write([]byte(`"}`))
	}))
	t.Cleanup(big.Close)
	_, err := eventtypes.Registry(events.NewEventTypeRegistry(newClient(t, big.URL, okTokens()))).Register(context.Background(), testReg)
	if !errors.Is(err, events.ErrResponseTooLarge) || !errors.Is(err, eventtypes.ErrUnavailable) {
		t.Errorf("default limit: err = %v", err)
	}
	res, err := events.NewEventTypeRegistry(newClient(t, big.URL, okTokens(), events.WithMaxResponseBytes(2<<20))).Register(context.Background(), testReg)
	if err != nil || res.Version != 1 {
		t.Errorf("larger limit: %+v %v", res, err)
	}

	// A redirect is not followed.
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	_, err = events.NewEventTypeRegistry(newClient(t, srv.URL, okTokens())).Register(context.Background(), testReg)
	if !errors.Is(err, eventtypes.ErrUnavailable) {
		t.Errorf("redirect: err = %v", err)
	}
	if hit.Load() {
		t.Error("the client followed the redirect")
	}
}

// The audience and the scopes go to the token source.
func TestAudienceAndScopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"helpdesk","version":1,"eventTypeCount":2,"registeredAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var aud string
	var scopes []string
	ts := &mock.TokenSource{TokenFunc: func(_ context.Context, a string, s ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		aud, scopes = a, s
		return "tok", nil
	}}
	if _, err := events.NewEventTypeRegistry(newClient(t, srv.URL, ts)).Register(context.Background(), testReg); err != nil {
		t.Fatal(err)
	}
	if aud != "events" || len(scopes) != 1 || scopes[0] != "events:event_type:register" {
		t.Errorf("audience %q scopes %v", aud, scopes)
	}
}
