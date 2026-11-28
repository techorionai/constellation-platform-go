package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

func serve(t *testing.T, h http.Handler, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// U12: Middleware with and without WithResourceMetadata.
func TestU12MiddlewareResourceMetadata(t *testing.T) {
	const url = "https://app.example/.well-known/oauth-protected-resource/mcp"
	verify := func(_ context.Context, token string) (auth.Principal, error) {
		if token == "down" {
			return auth.Principal{}, auth.ErrKeysUnavailable
		}
		return auth.Principal{}, auth.ErrInvalidToken
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	cases := []struct {
		name, authz string
		opts        []auth.Option
		status      int
		www         string
		retry       bool
	}{
		{"no token with option", "", []auth.Option{auth.WithResourceMetadata(url)}, 401, `Bearer resource_metadata="` + url + `"`, false},
		{"bad token with option", "Bearer bad", []auth.Option{auth.WithResourceMetadata(url)}, 401, `Bearer error="invalid_token", resource_metadata="` + url + `"`, false},
		{"unavailable with option", "Bearer down", []auth.Option{auth.WithResourceMetadata(url)}, 503, "", true},
		{"no token no option", "", nil, 401, "Bearer", false},
		{"bad token no option", "Bearer bad", nil, 401, `Bearer error="invalid_token"`, false},
		{"unavailable no option", "Bearer down", nil, 503, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := auth.Middleware(&mock.Verifier{VerifyFunc: verify}, c.opts...)(next)
			rec := serve(t, h, c.authz)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d", rec.Code, c.status)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != c.www {
				t.Errorf("WWW-Authenticate %q, want %q", got, c.www)
			}
			if (rec.Header().Get("Retry-After") != "") != c.retry {
				t.Errorf("Retry-After %q, want present %v", rec.Header().Get("Retry-After"), c.retry)
			}
		})
	}
}

// U13: WriteInsufficientScope and RequireScopes.
func TestU13WriteInsufficientScope(t *testing.T) {
	const url = "https://app.example/meta"
	check := func(t *testing.T, rec *httptest.ResponseRecorder, wantWWW string) {
		t.Helper()
		if rec.Code != 403 {
			t.Fatalf("status %d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != wantWWW {
			t.Errorf("WWW-Authenticate %q, want %q", got, wantWWW)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("content type %q", ct)
		}
		var body struct {
			Type, Title, Code string
			Status            int
			MissingScopes     []string
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Type != "about:blank" || body.Title != "Missing scopes" || body.Status != 403 ||
			body.Code != "insufficient_scope" || strings.Join(body.MissingScopes, ",") != "a:w" {
			t.Errorf("body %+v", body)
		}
	}
	rec := httptest.NewRecorder()
	auth.WriteInsufficientScope(context.Background(), rec, []string{"a:r", "a:w"}, []string{"a:w"}, url)
	check(t, rec, `Bearer error="insufficient_scope", scope="a:r a:w", resource_metadata="`+url+`"`)

	rec = httptest.NewRecorder()
	auth.WriteInsufficientScope(context.Background(), rec, []string{"a:r", "a:w"}, []string{"a:w"}, "")
	check(t, rec, `Bearer error="insufficient_scope", scope="a:r a:w"`)

	h := auth.Middleware(&mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{ID: "u", Scopes: []string{"a:r"}}, nil
	}})(auth.RequireScopes("a:r", "a:w")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	check(t, serve(t, h, "Bearer x"), `Bearer error="insufficient_scope", scope="a:r a:w"`)
}
