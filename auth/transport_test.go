package auth_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// U10: BearerTransport sets the header on a clone.
func TestU10BearerTransport(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	t.Cleanup(srv.Close)

	var gotAud string
	var gotScopes []string
	src := &mock.TokenSource{TokenFunc: func(_ context.Context, aud string, scopes ...string) (string, error) {
		gotAud, gotScopes = aud, scopes
		return "tok", nil
	}}
	client := &http.Client{Transport: auth.BearerTransport(nil, src, "identity", "a:b:c", "d:e:f")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAud != "identity" || strings.Join(gotScopes, ",") != "a:b:c,d:e:f" {
		t.Errorf("audience = %q, scopes = %v", gotAud, gotScopes)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the request of the caller has the header")
	}
}

// U10: a token error stops the request.
func TestU10TokenErrorStopsRequest(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	src := &mock.TokenSource{TokenFunc: func(context.Context, string, ...string) (string, error) {
		return "", auth.ErrTokenUnavailable
	}}
	client := &http.Client{Transport: auth.BearerTransport(nil, src, "identity", "a:b:c")}
	body := io.NopCloser(strings.NewReader("x"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if hits != 0 {
		t.Errorf("the server got %d requests", hits)
	}
}
