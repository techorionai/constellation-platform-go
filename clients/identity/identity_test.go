package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clients/identity"
	"github.com/techorionai/constellation-platform-go/manifest"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

var testManifest = manifest.PermissionManifest{
	App: "helpdesk",
	Permissions: []manifest.PermissionDefinition{
		{Key: "helpdesk:ticket:read", Description: "Read tickets."},
	},
	DefaultRoles: []manifest.RoleDefinition{
		{Key: "agent", Name: "Agent", Description: "An agent.", Permissions: []string{"helpdesk:ticket:read"}},
	},
}

type seen struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  []identity.PermissionManifest
}

func (s *seen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = append(s.headers, r.Header.Clone())
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	var b identity.PermissionManifest
	_ = json.NewDecoder(r.Body).Decode(&b)
	s.bodies = append(s.bodies, b)
}

func problem(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func okTokens() *mock.TokenSource {
	return &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) { return "tok", nil }}
}

// U9b: config rules.
func TestU9bConfig(t *testing.T) {
	_, err := identity.New(identity.Config{}, okTokens(), []string{"a:b:c"})
	if err == nil || !strings.Contains(err.Error(), "IDENTITY__URL") {
		t.Fatalf("err = %v", err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	// The Timeout default applies: a zero Timeout is valid.
	if _, err := identity.New(identity.Config{URL: srv.URL}, okTokens(), []string{"a:b:c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.New(identity.Config{URL: srv.URL, Timeout: -1}, okTokens(), []string{"a:b:c"}); err == nil {
		t.Error("no error for a negative Timeout")
	}
	if _, err := identity.New(identity.Config{URL: "ftp://x"}, okTokens(), []string{"a:b:c"}); err == nil {
		t.Error("no error for a wrong scheme")
	}
	if _, err := identity.New(identity.Config{URL: srv.URL}, nil, []string{"a:b:c"}); err == nil {
		t.Error("no error for a nil token source")
	}
}

// U15a: no scopes.
func TestU15aNoScopes(t *testing.T) {
	for _, scopes := range [][]string{nil, {}, {""}} {
		if _, err := identity.New(identity.Config{URL: "http://localhost"}, okTokens(), scopes); err == nil {
			t.Errorf("no error for scopes %v", scopes)
		}
	}
}

// U15: bearer token for the audience and scopes, and a trace header.
func TestU15BearerAndTrace(t *testing.T) {
	var s seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"manifest":{"app":"helpdesk","permissions":[],"defaultRoles":[]},"version":1,"publishedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	var gotAud string
	var gotScopes []string
	ts := &mock.TokenSource{TokenFunc: func(_ context.Context, aud string, scopes ...string) (string, error) {
		gotAud, gotScopes = aud, scopes
		return "tok-123", nil
	}}
	tp := sdktrace.NewTracerProvider()
	c, err := identity.New(identity.Config{URL: srv.URL}, ts, []string{"identity:manifest:publish"},
		identity.WithTracerProvider(tp), identity.WithPropagators(propagation.TraceContext{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()
	if err := identity.NewManifestRegistry(c).Publish(ctx, testManifest); err != nil {
		t.Fatal(err)
	}
	if gotAud != "identity" || len(gotScopes) != 1 || gotScopes[0] != "identity:manifest:publish" {
		t.Errorf("audience = %q, scopes = %v", gotAud, gotScopes)
	}
	h := s.headers[0]
	if h.Get("Authorization") != "Bearer tok-123" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if !strings.HasPrefix(h.Get("Traceparent"), "00-"+span.SpanContext().TraceID().String()) {
		t.Errorf("Traceparent = %q", h.Get("Traceparent"))
	}
	if s.paths[0] != "PUT /v1/permission-manifests/helpdesk" {
		t.Errorf("request = %q", s.paths[0])
	}
	if b := s.bodies[0]; b.App != "helpdesk" || len(b.Permissions) != 1 || b.Permissions[0].Key != "helpdesk:ticket:read" ||
		len(b.DefaultRoles) != 1 || b.DefaultRoles[0].Permissions[0] != "helpdesk:ticket:read" {
		t.Errorf("body = %+v", b)
	}
}

// The client does not send the token to a redirect target.
func TestNoRedirect(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, _ := identity.New(identity.Config{URL: srv.URL}, okTokens(), []string{"a:b:c"})
	err := identity.NewManifestRegistry(c).Publish(context.Background(), testManifest)
	if !errors.Is(err, manifest.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if hit {
		t.Error("the client followed the redirect")
	}
}

// U16: the error map of the registry adapter.
func TestU16RegistryErrors(t *testing.T) {
	type tc struct {
		name    string
		handler http.HandlerFunc
		tokens  *mock.TokenSource
		check   func(t *testing.T, err error)
	}
	is := func(want error) func(*testing.T, error) {
		return func(t *testing.T, err error) {
			t.Helper()
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
		}
	}
	tokenErr := func(err error) *mock.TokenSource {
		return &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) { return "", err }}
	}
	cases := []tc{
		{name: "200", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"manifest":{"app":"helpdesk","permissions":[],"defaultRoles":[]},"version":2,"publishedAt":"2026-01-01T00:00:00Z"}`))
		}, check: func(t *testing.T, err error) {
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "422", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 422, map[string]any{
				"type": "about:blank", "title": "Unprocessable", "status": 422, "code": "unprocessable_content",
				"errors": []map[string]any{{"field": "/permissions/0/key", "code": "permission_app_mismatch", "message": "m"}},
			})
		}, check: func(t *testing.T, err error) {
			var inv *manifest.InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("err = %v", err)
			}
			if inv.Code != "unprocessable_content" || len(inv.Problems) != 1 ||
				inv.Problems[0].Code != "permission_app_mismatch" || inv.Problems[0].Field != "/permissions/0/key" {
				t.Errorf("invalid = %+v", inv)
			}
		}},
		{name: "422 with no body", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(422) },
			check: func(t *testing.T, err error) {
				var inv *manifest.InvalidError
				if !errors.As(err, &inv) {
					t.Fatalf("err = %v", err)
				}
			}},
		{name: "401", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 401, map[string]any{"type": "about:blank", "title": "x", "status": 401, "code": "unauthorized"})
		}, check: is(manifest.ErrDenied)},
		{name: "403", handler: func(w http.ResponseWriter, _ *http.Request) {
			problem(w, 403, map[string]any{"type": "about:blank", "title": "x", "status": 403, "code": "forbidden"})
		}, check: is(manifest.ErrDenied)},
		{name: "503", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, check: is(manifest.ErrUnavailable)},
		{name: "500", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, check: is(manifest.ErrUnavailable)},
		{name: "400", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(400) }, check: is(manifest.ErrUnavailable)},
		{name: "closed connection", handler: func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, check: is(manifest.ErrUnavailable)},
		{name: "token unavailable", tokens: tokenErr(auth.ErrTokenUnavailable), check: is(manifest.ErrUnavailable)},
		{name: "token bad response", tokens: tokenErr(auth.ErrBadTokenResponse), check: is(manifest.ErrUnavailable)},
		{name: "invalid_client", tokens: tokenErr(&auth.OAuthError{Code: "invalid_client"}), check: is(manifest.ErrDenied)},
		{name: "invalid_scope", tokens: tokenErr(&auth.OAuthError{Code: "invalid_scope"}), check: is(manifest.ErrDenied)},
		{name: "unauthorized_client", tokens: tokenErr(&auth.OAuthError{Code: "unauthorized_client"}), check: is(manifest.ErrDenied)},
		{name: "other OAuth error", tokens: tokenErr(&auth.OAuthError{Code: "server_error"}), check: is(manifest.ErrUnavailable)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if c.handler != nil {
					c.handler(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			ts := c.tokens
			if ts == nil {
				ts = okTokens()
			}
			cl, err := identity.New(identity.Config{URL: srv.URL, Timeout: 5 * time.Second}, ts, []string{"identity:manifest:publish"})
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, identity.NewManifestRegistry(cl).Publish(context.Background(), testManifest))
			if c.tokens != nil && hits != 0 {
				t.Errorf("the server got %d requests after a token error", hits)
			}
		})
	}
}

// A token error is not a request error of the server: the error text has no token.
func TestErrorHasNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	t.Cleanup(srv.Close)
	ts := &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) { return "SECRET-TOKEN", nil }}
	c, _ := identity.New(identity.Config{URL: srv.URL}, ts, []string{"a:b:c"})
	err := identity.NewManifestRegistry(c).Publish(context.Background(), testManifest)
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("err = %v", err)
	}
}
