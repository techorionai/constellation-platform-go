package clientcred_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/auth"
)

const accessURI = "urn:ietf:params:oauth:token-type:access_token"

func exchangeOK(w http.ResponseWriter, n int) {
	writeJSON(w, 200, map[string]any{
		"access_token": fmt.Sprintf("xtok-%d", n), "issued_token_type": accessURI,
		"token_type": "Bearer", "expires_in": 600,
	})
}

func newExchangeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, setup{})
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, n int) { exchangeOK(w, n) }
	return f
}

func baseReq() auth.ExchangeRequest {
	//nolint:gosec // test data
	return auth.ExchangeRequest{SubjectToken: "user.jwt.value", AgentID: "agent-77", Audience: "tickets", Scopes: []string{"tickets:thing:write", "tickets:thing:read"}}
}

// U20: the form with an agent ID.
func TestU20ExchangeForm(t *testing.T) {
	f := newExchangeFixture(t)
	tok, err := f.src.Exchange(context.Background(), baseReq())
	if err != nil || tok != "xtok-1" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	form := f.ep.requests()[0].form
	//nolint:gosec // test data
	want := map[string]string{
		"_method": "POST", "_path": "/oauth/token", "_ct": "application/x-www-form-urlencoded",
		"grant_type":            "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token":         "user.jwt.value",
		"subject_token_type":    accessURI,
		"actor_token":           "agent-77",
		"actor_token_type":      "urn:constellation:params:oauth:token-type:agent-id",
		"requested_token_type":  accessURI,
		"audience":              "tickets",
		"scope":                 "tickets:thing:read tickets:thing:write",
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
	}
	for k, v := range want {
		if form[k] != v {
			t.Errorf("form[%s] = %q, want %q", k, form[k], v)
		}
	}
	claims := verifyAssertion(t, form["client_assertion"], &f.key.PublicKey, f.clock.now())
	if claims["iss"] != clientID || claims["sub"] != clientID || claims["aud"] != issuer {
		t.Errorf("claims = %v", claims)
	}
}

// U21: no agent ID.
func TestU21ExchangeNoAgent(t *testing.T) {
	f := newExchangeFixture(t)
	req := baseReq()
	req.AgentID = ""
	if _, err := f.src.Exchange(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	form := f.ep.requests()[0].form
	for _, k := range []string{"actor_token", "actor_token_type"} {
		if _, ok := form[k]; ok {
			t.Errorf("form has %s", k)
		}
	}
}

// U22: the cache.
func TestU22ExchangeCache(t *testing.T) {
	f := newExchangeFixture(t)
	ctx := context.Background()
	a, _ := f.src.Exchange(ctx, baseReq())
	b, _ := f.src.Exchange(ctx, baseReq())
	if a != b || len(f.ep.requests()) != 1 {
		t.Fatalf("same request: %q %q, calls = %d", a, b, len(f.ep.requests()))
	}
	// The order of the scopes does not matter.
	r := baseReq()
	r.Scopes = []string{"tickets:thing:read", "tickets:thing:write", "tickets:thing:read"}
	_, _ = f.src.Exchange(ctx, r)
	if len(f.ep.requests()) != 1 {
		t.Fatal("scope order made a new call")
	}
	r = baseReq()
	r.SubjectToken = "other.jwt"
	_, _ = f.src.Exchange(ctx, r)
	r = baseReq()
	r.AgentID = "agent-78"
	_, _ = f.src.Exchange(ctx, r)
	r = baseReq()
	r.AgentID = ""
	_, _ = f.src.Exchange(ctx, r)
	r = baseReq()
	r.Audience = "other"
	_, _ = f.src.Exchange(ctx, r)
	if len(f.ep.requests()) != 5 {
		t.Fatalf("calls = %d, want 5", len(f.ep.requests()))
	}
	// Refresh at 80 percent of the lifetime (600 s).
	f.clock.advance(479 * time.Second)
	_, _ = f.src.Exchange(ctx, baseReq())
	if len(f.ep.requests()) != 5 {
		t.Fatal("refresh too early")
	}
	f.clock.advance(2 * time.Second)
	_, _ = f.src.Exchange(ctx, baseReq())
	if len(f.ep.requests()) != 6 {
		t.Fatal("no refresh at 80 percent")
	}
}

// U23: a bad response.
func TestU23ExchangeBadResponse(t *testing.T) {
	cases := map[string]map[string]any{
		"wrong issued type": {"access_token": "x", "issued_token_type": "urn:other", "token_type": "Bearer", "expires_in": 60},
		"no issued type":    {"access_token": "x", "token_type": "Bearer", "expires_in": 60},
		"not bearer":        {"access_token": "x", "issued_token_type": accessURI, "token_type": "DPoP", "expires_in": 60},
		"expires 0":         {"access_token": "x", "issued_token_type": accessURI, "token_type": "Bearer", "expires_in": 0},
		"expires 0.5":       {"access_token": "x", "issued_token_type": accessURI, "token_type": "Bearer", "expires_in": 0.5},
		"expires 16 min":    {"access_token": "x", "issued_token_type": accessURI, "token_type": "Bearer", "expires_in": 960},
		"no token":          {"issued_token_type": accessURI, "token_type": "Bearer", "expires_in": 60},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newExchangeFixture(t)
			f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) { writeJSON(w, 200, body) }
			_, err := f.src.Exchange(context.Background(), baseReq())
			if !errors.Is(err, auth.ErrBadTokenResponse) {
				t.Fatalf("err = %v", err)
			}
			_, _ = f.src.Exchange(context.Background(), baseReq())
			if len(f.ep.requests()) != 2 {
				t.Fatal("a bad response was cached")
			}
		})
	}
	// The limits 1 s and 15 min are valid.
	for _, exp := range []float64{1, 900} {
		f := newExchangeFixture(t)
		f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
			writeJSON(w, 200, map[string]any{"access_token": "x", "issued_token_type": accessURI, "token_type": "Bearer", "expires_in": exp})
		}
		if _, err := f.src.Exchange(context.Background(), baseReq()); err != nil {
			t.Errorf("expires_in %v: %v", exp, err)
		}
	}
}

// U24 and U33: OAuth errors.
func TestU33ExchangeOAuthErrors(t *testing.T) {
	cases := map[string]int{
		"invalid_client": 401, "unauthorized_client": 400, "invalid_request": 400, "unsupported_token_type": 400,
		"invalid_grant": 400, "invalid_target": 400, "invalid_scope": 400,
	}
	for code, status := range cases {
		t.Run(code, func(t *testing.T) {
			f := newExchangeFixture(t)
			f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
				writeJSON(w, status, map[string]any{"error": code, "error_description": "text"})
			}
			_, err := f.src.Exchange(context.Background(), baseReq())
			var oe *auth.OAuthError
			if !errors.As(err, &oe) || oe.Code != code {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, auth.ErrTokenUnavailable) {
				t.Fatal("an OAuth error must not be ErrTokenUnavailable")
			}
			if got := f.exchangeOutcomes(t); got["oauth_error"] != 1 {
				t.Fatalf("outcomes = %v", got)
			}
		})
	}
}

// U34: unavailable.
func TestU34ExchangeUnavailable(t *testing.T) {
	handlers := map[string]func(w http.ResponseWriter, _ *http.Request, _ int){
		"500": func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) },
		"503": func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(503) },
		"temporarily_unavailable": func(w http.ResponseWriter, _ *http.Request, _ int) {
			writeJSON(w, 400, map[string]any{"error": "temporarily_unavailable"})
		},
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			f := newExchangeFixture(t)
			f.ep.handler = h
			for i := 0; i < 2; i++ {
				if _, err := f.src.Exchange(context.Background(), baseReq()); !errors.Is(err, auth.ErrTokenUnavailable) {
					t.Fatalf("err = %v", err)
				}
			}
			if len(f.ep.requests()) != 2 {
				t.Fatal("an error was cached")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		f := newFixture(t, setup{client: &http.Client{Timeout: 50 * time.Millisecond}})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.ep.handler = func(http.ResponseWriter, *http.Request, int) { <-release }
		if _, err := f.src.Exchange(context.Background(), baseReq()); !errors.Is(err, auth.ErrTokenUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("keeps a cached token when the refresh fails", func(t *testing.T) {
		f := newExchangeFixture(t)
		first, _ := f.src.Exchange(context.Background(), baseReq())
		f.clock.advance(500 * time.Second) // after 80 percent, before expiry
		f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) }
		got, err := f.src.Exchange(context.Background(), baseReq())
		if err != nil || got != first {
			t.Fatalf("got %q, err %v", got, err)
		}
	})
}

// U25: input checks. No HTTP call.
func TestU25ExchangeInput(t *testing.T) {
	f := newExchangeFixture(t)
	r := baseReq()
	r.Scopes = nil
	if _, err := f.src.Exchange(context.Background(), r); !errors.Is(err, auth.ErrNoScopes) {
		t.Fatalf("no scopes: %v", err)
	}
	r.Scopes = []string{"", ""}
	if _, err := f.src.Exchange(context.Background(), r); !errors.Is(err, auth.ErrNoScopes) {
		t.Fatalf("empty scopes: %v", err)
	}
	r = baseReq()
	r.SubjectToken = ""
	if _, err := f.src.Exchange(context.Background(), r); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("no subject token: %v", err)
	}
	if len(f.ep.requests()) != 0 {
		t.Fatal("an HTTP call was made")
	}
}

// U26: the assertion of the source.
func TestU26Assertion(t *testing.T) {
	f := newFixture(t, setup{})
	if f.src.ClientID() != clientID {
		t.Fatalf("ClientID = %q", f.src.ClientID())
	}
	a1, err := f.src.Assertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := f.src.Assertion(context.Background())
	c1 := verifyAssertion(t, a1, &f.key.PublicKey, f.clock.now())
	c2 := verifyAssertion(t, a2, &f.key.PublicKey, f.clock.now())
	if c1["iss"] != clientID || c1["sub"] != clientID || c1["aud"] != issuer {
		t.Errorf("claims = %v", c1)
	}
	if c1["jti"] == c2["jti"] {
		t.Error("the jti is the same")
	}
}

// U32: no secret in the output, and the span of the design.
func TestU32ExchangeNoSecrets(t *testing.T) {
	f := newExchangeFixture(t)
	ctx := context.Background()
	var texts []string
	record := func(err error) {
		if err != nil {
			texts = append(texts, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
		}
	}
	tok, err := f.src.Exchange(ctx, baseReq())
	record(err)
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
	}
	r := baseReq()
	r.Audience = "other"
	_, err = f.src.Exchange(ctx, r)
	record(err)
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) }
	r.Audience = "other2"
	_, err = f.src.Exchange(ctx, r)
	record(err)
	r.SubjectToken = "has" + string(rune(0)) + "zero"
	_, err = f.src.Exchange(ctx, r)
	record(err)
	f.ep.srv.Close()
	r = baseReq()
	r.Audience = "other3"
	_, err = f.src.Exchange(ctx, r)
	record(err)

	for _, s := range f.spans.Ended() {
		texts = append(texts, s.Name())
		for _, a := range s.Attributes() {
			texts = append(texts, string(a.Key), a.Value.String())
		}
		texts = append(texts, s.Status().Description)
	}
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	texts = append(texts, fmt.Sprintf("%+v", rm))
	secrets := []string{"user.jwt.value", "agent-77", tok, f.privPEM}
	for _, rq := range f.ep.requests() {
		secrets = append(secrets, rq.form["client_assertion"])
	}
	for _, text := range texts {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Fatalf("a secret is in %q", text)
			}
		}
	}
	var found bool
	for _, s := range f.spans.Ended() {
		if s.Name() != "auth.clientcred.exchange" {
			continue
		}
		attrs := map[string]attribute.Value{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a.Value
		}
		if attrs["constellation.auth.client_id"].AsString() == clientID &&
			attrs["constellation.auth.audience"].AsString() == "tickets" &&
			attrs["constellation.auth.exchange.with_agent"].AsBool() &&
			attrs["constellation.auth.outcome"].AsString() == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatal("the span auth.clientcred.exchange has no correct attributes")
	}
}

// U35: cache keys at the part borders, and a zero byte.
func TestU35ExchangeKeys(t *testing.T) {
	f := newExchangeFixture(t)
	ctx := context.Background()
	a := auth.ExchangeRequest{SubjectToken: "a", AgentID: "bc", Audience: "x", Scopes: []string{"s:a:b"}}
	b := auth.ExchangeRequest{SubjectToken: "ab", AgentID: "c", Audience: "x", Scopes: []string{"s:a:b"}}
	ta, _ := f.src.Exchange(ctx, a)
	tb, _ := f.src.Exchange(ctx, b)
	if ta == tb || len(f.ep.requests()) != 2 {
		t.Fatalf("keys collide: %q %q calls %d", ta, tb, len(f.ep.requests()))
	}
	c := auth.ExchangeRequest{SubjectToken: "a", AgentID: "", Audience: "bc", Scopes: []string{"s:a:b"}}
	d := auth.ExchangeRequest{SubjectToken: "a", AgentID: "b", Audience: "c", Scopes: []string{"s:a:b"}}
	_, _ = f.src.Exchange(ctx, c)
	_, _ = f.src.Exchange(ctx, d)
	if len(f.ep.requests()) != 4 {
		t.Fatalf("calls = %d", len(f.ep.requests()))
	}
	zero := "a" + string(rune(0)) + "b"
	for name, mut := range map[string]func(*auth.ExchangeRequest){
		"subject":  func(r *auth.ExchangeRequest) { r.SubjectToken = zero },
		"agent":    func(r *auth.ExchangeRequest) { r.AgentID = zero },
		"audience": func(r *auth.ExchangeRequest) { r.Audience = zero },
		"scope":    func(r *auth.ExchangeRequest) { r.Scopes = []string{"s:a:b", zero} },
	} {
		r := a
		mut(&r)
		if _, err := f.src.Exchange(ctx, r); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(f.ep.requests()) != 4 {
		t.Fatal("a call was made for a zero byte")
	}
}

// Concurrent equal requests make one call.
func TestExchangeSingleflight(t *testing.T) {
	f := newExchangeFixture(t)
	release := make(chan struct{})
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, n int) {
		<-release
		exchangeOK(w, n)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.src.Exchange(context.Background(), baseReq()); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	if len(f.ep.requests()) != 1 {
		t.Fatalf("calls = %d", len(f.ep.requests()))
	}
}

// exchangeOutcomes returns the counts of constellation.auth.token.exchange by outcome.
func (f *fixture) exchangeOutcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.auth.token.exchange" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// R1: a refresh after 80 percent of the lifetime that gets an OAuth error
// returns the error. The old token is not used and the entry is removed.
func TestExchangeRefreshOAuthError(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_scope", "unauthorized_client"} {
		t.Run(code, func(t *testing.T) {
			f := newExchangeFixture(t)
			if _, err := f.src.Exchange(context.Background(), baseReq()); err != nil {
				t.Fatal(err)
			}
			f.clock.advance(500 * time.Second) // after 80 percent, before expiry
			f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
				writeJSON(w, 400, map[string]any{"error": code})
			}
			got, err := f.src.Exchange(context.Background(), baseReq())
			var oe *auth.OAuthError
			if !errors.As(err, &oe) || got != "" {
				t.Fatalf("got %q, err %v", got, err)
			}
			// The entry is removed. A second call goes to the endpoint again.
			n := len(f.ep.requests())
			if _, err := f.src.Exchange(context.Background(), baseReq()); !errors.As(err, &oe) {
				t.Fatalf("second call: %v", err)
			}
			if len(f.ep.requests()) != n+1 {
				t.Fatal("the old token stayed in the cache")
			}
		})
	}
}
