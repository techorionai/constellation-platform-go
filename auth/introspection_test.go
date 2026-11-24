package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

// Issue 15 U16: ByFormat.
func TestU16ByFormat(t *testing.T) {
	var gotJWT, gotAPI string
	jwtV := &mock.Verifier{VerifyFunc: func(_ context.Context, tok string) (auth.Principal, error) {
		gotJWT = tok
		return auth.Principal{ID: "jwt"}, nil
	}}
	apiV := &mock.Verifier{VerifyFunc: func(_ context.Context, tok string) (auth.Principal, error) {
		gotAPI = tok
		return auth.Principal{ID: "api"}, nil
	}}
	v := auth.ByFormat(jwtV, apiV)
	if p, err := v.Verify(context.Background(), "cpat_abc"); err != nil || p.ID != "api" || gotAPI != "cpat_abc" {
		t.Fatalf("api token: %+v %v", p, err)
	}
	if p, err := v.Verify(context.Background(), "a.b.c"); err != nil || p.ID != "jwt" || gotJWT != "a.b.c" {
		t.Fatalf("jwt: %+v %v", p, err)
	}
	// Only the exact prefix goes to the API verifier.
	if p, _ := v.Verify(context.Background(), "CPAT_abc"); p.ID != "jwt" {
		t.Fatalf("upper case prefix: %+v", p)
	}
	_, err := auth.ByFormat(jwtV, nil).Verify(context.Background(), "cpat_abc")
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("nil apiToken: %v", err)
	}
	_, err = auth.ByFormat(nil, apiV).Verify(context.Background(), "a.b.c")
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("nil jwt: %v", err)
	}
	if auth.Outcome(auth.ErrIntrospectionUnavailable) != "unavailable" {
		t.Fatal("outcome")
	}
}

// Issue 15 U17: 503 for ErrIntrospectionUnavailable.
func TestU17IntrospectionUnavailable(t *testing.T) {
	v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{}, errors.Join(errors.New("detail"), auth.ErrIntrospectionUnavailable)
	}}
	e := newEnv(t, v)
	rec := e.do("Bearer cpat_x")
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || decode(t, rec).Code != "auth_unavailable" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body)
	}
	if got := e.outcomes(t); got["unavailable"] != 1 || len(got) != 1 {
		t.Fatalf("outcomes = %v", got)
	}
	if strings.Contains(rec.Body.String(), "detail") {
		t.Fatal("the body has the error text")
	}
}

// Issue 15 U18: act and token type attributes on the span and in the log.
func TestU18ActAttributes(t *testing.T) {
	p := testPrincipal
	p.Act = &auth.Actor{ID: "agent-1", Type: auth.PrincipalAgent}
	p.TokenType = auth.TokenTypeAPI
	v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) { return p, nil }}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{ServiceName: "authtest"})
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, v)
	e.do("Bearer tok")
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, _ := io.ReadAll(r)
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "handled" {
			rec = m
		}
	}
	if rec == nil {
		t.Fatalf("no record in output: %s", out)
	}
	for k, want := range map[string]string{
		"principal.act.id": "agent-1", "principal.act.type": "agent", "principal.token_type": "api_token",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %s", k, rec[k], want)
		}
	}
	spans := e.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans %d", len(spans))
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	for k, want := range map[string]string{
		"constellation.principal.act.id": "agent-1", "constellation.principal.act.type": "agent",
		"constellation.principal.token_type": "api_token",
	} {
		if attrs[k] != want {
			t.Errorf("%s = %q, want %q", k, attrs[k], want)
		}
	}

	// A principal with no act and no token type has no act attribute and the
	// token type access_token.
	e2 := newEnv(t, okVerifier())
	e2.do("Bearer tok")
	attrs = map[string]string{}
	for _, a := range e2.spans.Ended()[0].Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if _, ok := attrs["constellation.principal.act.id"]; ok {
		t.Error("act attribute without act")
	}
	if attrs["constellation.principal.token_type"] != "access_token" {
		t.Errorf("token_type = %q", attrs["constellation.principal.token_type"])
	}
}
