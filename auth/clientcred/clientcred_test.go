package clientcred_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/auth/clientcred"
	"github.com/techorionai/constellation-platform-go/config"
)

const (
	clientID = "svc-helpdesk"
	keyID    = "key-1"
	issuer   = "http://localhost:8080"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pemPKCS8(t *testing.T, k any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// request is one request that the fake token endpoint got.
type request struct {
	form map[string]string
}

// endpoint is a fake token endpoint.
type endpoint struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []request
	// handler writes the answer. The default is a valid token.
	handler func(w http.ResponseWriter, r *http.Request, n int)
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form := map[string]string{"_path": r.URL.Path, "_method": r.Method, "_ct": r.Header.Get("Content-Type")}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		e.mu.Lock()
		e.got = append(e.got, request{form: form})
		n := len(e.got)
		h := e.handler
		e.mu.Unlock()
		if h != nil {
			h(w, r, n)
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 100})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *endpoint) requests() []request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]request(nil), e.got...)
}

func (e *endpoint) url() string { return e.srv.URL + "/oauth/token" }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type fixture struct {
	src     *clientcred.Source
	ep      *endpoint
	key     *ecdsa.PrivateKey
	reader  *sdkmetric.ManualReader
	spans   *tracetest.SpanRecorder
	clock   *fakeClock
	privPEM string
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type setup struct {
	cfg    func(*clientcred.Config)
	client *http.Client
}

func newFixture(t *testing.T, s setup) *fixture {
	t.Helper()
	ep := newEndpoint(t)
	key := newKey(t)
	privPEM := pemPKCS8(t, key)
	cfg := clientcred.Config{
		ID: clientID, KeyID: keyID, PrivateKey: config.Secret(privPEM),
		Issuer: issuer, TokenURL: ep.url(), HTTPTimeout: 5 * time.Second,
		AllowInsecureHTTP: true,
	}
	if s.cfg != nil {
		s.cfg(&cfg)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	opts := []clientcred.Option{
		clientcred.WithClock(clock.now), clientcred.WithMeterProvider(mp), clientcred.WithTracerProvider(tp),
	}
	if s.client != nil {
		opts = append(opts, clientcred.WithHTTPClient(s.client))
	}
	src, err := clientcred.New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{src: src, ep: ep, key: key, reader: reader, spans: spans, clock: clock, privPEM: privPEM}
}

// outcomes returns the counts of constellation.auth.token.request by outcome.
func (f *fixture) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.auth.token.request" {
				continue
			}
			if m.Unit != "{request}" {
				t.Errorf("unit = %q", m.Unit)
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// U2: first call.
func TestU2FirstCall(t *testing.T) {
	f := newFixture(t, setup{})
	tok, err := f.src.Token(context.Background(), "identity", "identity:manifest:publish")
	if err != nil || tok != "tok-1" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	reqs := f.ep.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	form := reqs[0].form
	want := map[string]string{
		"_method": "POST", "_path": "/oauth/token", "_ct": "application/x-www-form-urlencoded",
		"grant_type":            "client_credentials",
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"audience":              "identity",
		"scope":                 "identity:manifest:publish",
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
	if claims["jti"] == "" || claims["jti"] == nil {
		t.Error("no jti")
	}
}

func verifyAssertion(t *testing.T, raw string, pub *ecdsa.PublicKey, now time.Time) map[string]any {
	t.Helper()
	sig, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	h := sig.Signatures[0].Header
	if h.KeyID != keyID || h.Algorithm != "ES256" {
		t.Errorf("header kid = %q, alg = %q", h.KeyID, h.Algorithm)
	}
	if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != "JWT" {
		t.Errorf("typ = %v", h.ExtraHeaders[jose.HeaderType])
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if int64(iat) != now.Unix() || int64(exp)-int64(iat) != 60 {
		t.Errorf("iat = %v, exp = %v", iat, exp)
	}
	return claims
}

// U2a: Issuer and TokenURL differ.
func TestU2aIssuerAndTokenURLDiffer(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *clientcred.Config) { c.Issuer = "http://localhost:8080" }})
	if _, err := f.src.Token(context.Background(), "identity", "s:a:b"); err != nil {
		t.Fatal(err)
	}
	reqs := f.ep.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	claims := verifyAssertion(t, reqs[0].form["client_assertion"], &f.key.PublicKey, f.clock.now())
	if claims["aud"] != "http://localhost:8080" {
		t.Errorf("aud = %v", claims["aud"])
	}
}

// U3: second call before 80 percent of the lifetime.
func TestU3CacheHit(t *testing.T) {
	f := newFixture(t, setup{})
	ctx := context.Background()
	_, _ = f.src.Token(ctx, "identity", "s:a:b")
	f.clock.advance(79 * time.Second)
	tok, err := f.src.Token(ctx, "identity", "s:a:b")
	if err != nil || tok != "tok-1" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if got := f.outcomes(t); got["ok"] != 1 {
		t.Errorf("outcomes = %v (a cache hit must not count)", got)
	}
}

// U4: call after 80 percent of the lifetime.
func TestU4Refresh(t *testing.T) {
	f := newFixture(t, setup{})
	ctx := context.Background()
	_, _ = f.src.Token(ctx, "identity", "s:a:b")
	f.clock.advance(81 * time.Second)
	tok, err := f.src.Token(ctx, "identity", "s:a:b")
	if err != nil || tok != "tok-2" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

// U5: 20 concurrent calls share one request.
func TestU5Concurrent(t *testing.T) {
	f := newFixture(t, setup{})
	release := make(chan struct{})
	f.ep.mu.Lock()
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		<-release
		writeJSON(w, 200, map[string]any{"access_token": "shared", "expires_in": 100})
	}
	f.ep.mu.Unlock()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := f.src.Token(context.Background(), "identity", "s:a:b")
			if err != nil || tok != "shared" {
				bad.Add(1)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d calls failed", bad.Load())
	}
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

// U6: scopes in another order share one cache entry.
func TestU6ScopeOrder(t *testing.T) {
	f := newFixture(t, setup{})
	ctx := context.Background()
	_, _ = f.src.Token(ctx, "identity", "b:b:b", "a:a:a")
	_, _ = f.src.Token(ctx, "identity", "a:a:a", "b:b:b")
	reqs := f.ep.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].form["scope"] != "a:a:a b:b:b" {
		t.Errorf("scope = %q", reqs[0].form["scope"])
	}
	// An empty scope and a duplicate scope are removed. The entry is the same.
	_, _ = f.src.Token(ctx, "identity", "", "b:b:b", "a:a:a", "a:a:a")
	if n := len(f.ep.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1 (same cache entry)", n)
	}
	// Another audience is another entry.
	_, _ = f.src.Token(ctx, "other", "a:a:a", "b:b:b")
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

// U6a: no scopes.
func TestU6aNoScopes(t *testing.T) {
	f := newFixture(t, setup{})
	if _, err := f.src.Token(context.Background(), "identity"); !errors.Is(err, auth.ErrNoScopes) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.src.Token(context.Background(), "identity", ""); !errors.Is(err, auth.ErrNoScopes) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.ep.requests()); n != 0 {
		t.Fatalf("requests = %d", n)
	}
}

// U7: OAuth error.
func TestU7OAuthError(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 401, map[string]any{"error": "invalid_client", "error_description": "bad"})
	}
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	var oe *auth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_client" {
		t.Fatalf("err = %v", err)
	}
	assertion := f.ep.requests()[0].form["client_assertion"]
	if strings.Contains(err.Error(), assertion) {
		t.Error("error text has the assertion")
	}
	if got := f.outcomes(t); got["oauth_error"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// U7a: 503 and 500.
func TestU7aUnavailable(t *testing.T) {
	for name, h := range map[string]func(http.ResponseWriter, *http.Request, int){
		"503 temporarily_unavailable": func(w http.ResponseWriter, _ *http.Request, _ int) {
			writeJSON(w, 503, map[string]any{"error": "temporarily_unavailable"})
		},
		"500": func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) },
		"400 temporarily_unavailable": func(w http.ResponseWriter, _ *http.Request, _ int) {
			writeJSON(w, 400, map[string]any{"error": "temporarily_unavailable"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.ep.handler = h
			_, err := f.src.Token(context.Background(), "identity", "s:a:b")
			if !errors.Is(err, auth.ErrTokenUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if got := f.outcomes(t); got["unavailable"] != 1 {
				t.Errorf("outcomes = %v", got)
			}
		})
	}
}

// U7b: bad expires_in.
func TestU7bBadExpiresIn(t *testing.T) {
	bodies := map[string]map[string]any{
		"zero":       {"access_token": "x", "expires_in": 0},
		"missing":    {"access_token": "x"},
		"negative":   {"access_token": "x", "expires_in": -5},
		"no token":   {"expires_in": 100},
		"empty body": {},
		"too large":  {"access_token": "x", "expires_in": 1e10},
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) { writeJSON(w, 200, body) }
			_, err := f.src.Token(context.Background(), "identity", "s:a:b")
			if !errors.Is(err, auth.ErrBadTokenResponse) {
				t.Fatalf("err = %v", err)
			}
			_, _ = f.src.Token(context.Background(), "identity", "s:a:b")
			if n := len(f.ep.requests()); n != 2 {
				t.Fatalf("requests = %d, want 2 (nothing in the cache)", n)
			}
			if got := f.outcomes(t); got["bad_response"] != 2 {
				t.Errorf("outcomes = %v", got)
			}
		})
	}
}

// U7c: body larger than 64 KiB.
func TestU7cLargeBody(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + strings.Repeat("a", 70<<10) + `","expires_in":100}`))
	}
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	if !errors.Is(err, auth.ErrBadTokenResponse) {
		t.Fatalf("err = %v", err)
	}
}

// U7d: time out.
func TestU7dTimeout(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *clientcred.Config) { c.HTTPTimeout = 100 * time.Millisecond }})
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	f.ep.handler = func(_ http.ResponseWriter, _ *http.Request, _ int) {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	start := time.Now()
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	if err == nil || !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("the call did not stop at the time out")
	}
	if got := f.outcomes(t); got["network_error"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// U7e: a new jti for each request.
func TestU7eNewJTI(t *testing.T) {
	f := newFixture(t, setup{})
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 503, map[string]any{"error": "temporarily_unavailable"})
	}
	_, _ = f.src.Token(context.Background(), "identity", "s:a:b")
	_, _ = f.src.Token(context.Background(), "identity", "s:a:b")
	reqs := f.ep.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	c1 := verifyAssertion(t, reqs[0].form["client_assertion"], &f.key.PublicKey, f.clock.now())
	c2 := verifyAssertion(t, reqs[1].form["client_assertion"], &f.key.PublicKey, f.clock.now())
	if c1["jti"] == c2["jti"] {
		t.Error("the jti values are equal")
	}
}

// A redirect is not followed.
func TestNoRedirect(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	t.Cleanup(target.Close)
	f := newFixture(t, setup{})
	f.ep.handler = func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}
	_, err := f.src.Token(context.Background(), "identity", "s:a:b")
	if !errors.Is(err, auth.ErrBadTokenResponse) {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Error("the source followed the redirect")
	}
}

// A caller that stops does not wait for the request.
func TestCallerContext(t *testing.T) {
	f := newFixture(t, setup{})
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	f.ep.handler = func(_ http.ResponseWriter, _ *http.Request, _ int) { <-done }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.src.Token(ctx, "identity", "s:a:b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// U8: no secret in the logs, spans, and errors. A span exists for each request.
func TestU8NoSecrets(t *testing.T) {
	f := newFixture(t, setup{})
	ctx := context.Background()
	var texts []string
	record := func(err error) {
		if err != nil {
			texts = append(texts, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
		}
	}
	tok, err := f.src.Token(ctx, "identity", "s:a:b")
	record(err)
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 401, map[string]any{"error": "invalid_client"})
	}
	_, err = f.src.Token(ctx, "other", "s:a:b")
	record(err)
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) }
	_, err = f.src.Token(ctx, "other2", "s:a:b")
	record(err)

	reqs := f.ep.requests()
	secrets := []string{f.privPEM, tok}
	for _, r := range reqs {
		secrets = append(secrets, r.form["client_assertion"])
	}
	for _, s := range f.spans.Ended() {
		texts = append(texts, s.Name())
		for _, a := range s.Attributes() {
			texts = append(texts, string(a.Key), a.Value.String())
		}
		for _, e := range s.Events() {
			texts = append(texts, e.Name)
			for _, a := range e.Attributes {
				texts = append(texts, a.Value.String())
			}
		}
		texts = append(texts, s.Status().Description)
	}
	for _, text := range texts {
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				t.Fatalf("a secret is in %q", text)
			}
		}
	}
	var n int
	for _, s := range f.spans.Ended() {
		if s.Name() == "auth.clientcred.token" {
			n++
			var gotClient, gotAud bool
			for _, a := range s.Attributes() {
				gotClient = gotClient || (a.Key == "constellation.client.id" && a.Value.AsString() == clientID)
				gotAud = gotAud || a.Key == "constellation.token.audience"
			}
			if !gotClient || !gotAud {
				t.Errorf("span attributes = %v", s.Attributes())
			}
		}
	}
	if n != len(reqs) {
		t.Errorf("spans = %d, requests = %d", n, len(reqs))
	}
}

// U9: key from a file, and a wrong key.
func TestU9KeyFile(t *testing.T) {
	key := newKey(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte(pemPKCS8(t, key)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_CLIENT__ID", clientID)
	t.Setenv("TEST_CLIENT__KEY_ID", keyID)
	t.Setenv("TEST_CLIENT__ISSUER", issuer)
	t.Setenv("TEST_CLIENT__ALLOW_INSECURE_HTTP", "true")
	t.Setenv("TEST_CLIENT__PRIVATE_KEY_FILE", path)
	var cfg struct {
		Client clientcred.Config `envPrefix:"CLIENT__"`
	}
	if err := config.Load("TEST_", &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := clientcred.New(cfg.Client); err != nil {
		t.Fatal(err)
	}

	// SEC 1 format loads too.
	der, _ := x509.MarshalECPrivateKey(key)
	sec1 := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	c := cfg.Client
	c.PrivateKey = config.Secret(sec1)
	if _, err := clientcred.New(c); err != nil {
		t.Fatalf("SEC 1 key: %v", err)
	}

	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, pk := range map[string]string{
		"P-384":   pemPKCS8(t, p384),
		"RSA":     pemPKCS8(t, rsaKey),
		"not PEM": "garbage",
		"bad DER": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})),
	} {
		c := cfg.Client
		c.PrivateKey = config.Secret(pk)
		_, err := clientcred.New(c)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "garbage") {
			t.Errorf("%s: error has key material", name)
		}
	}
}

// U9a: required fields and defaults.
func TestU9aConfig(t *testing.T) {
	key := pemPKCS8(t, newKey(t))
	good := clientcred.Config{
		ID: clientID, KeyID: keyID, PrivateKey: config.Secret(key), Issuer: issuer,
		HTTPTimeout: 10 * time.Second, AllowInsecureHTTP: true,
	}
	cases := map[string]struct {
		mutate func(*clientcred.Config)
		want   string
	}{
		"no ID":     {func(c *clientcred.Config) { c.ID = "" }, "CLIENT__ID"},
		"no KeyID":  {func(c *clientcred.Config) { c.KeyID = "" }, "CLIENT__KEY_ID"},
		"no Issuer": {func(c *clientcred.Config) { c.Issuer = "" }, "CLIENT__ISSUER"},
		"no key":    {func(c *clientcred.Config) { c.PrivateKey = "" }, "CLIENT__PRIVATE_KEY"},
		"timeout":   {func(c *clientcred.Config) { c.HTTPTimeout = -1 }, "CLIENT__HTTP_TIMEOUT"},
		"timeout 0": {func(c *clientcred.Config) { c.HTTPTimeout = 0 }, "CLIENT__HTTP_TIMEOUT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			_, err := clientcred.New(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Defaults: TokenURL is <Issuer>/oauth/token and HTTPTimeout is 10s.
	t.Setenv("TEST_CLIENT__ID", clientID)
	t.Setenv("TEST_CLIENT__KEY_ID", keyID)
	t.Setenv("TEST_CLIENT__ISSUER", issuer)
	t.Setenv("TEST_CLIENT__PRIVATE_KEY", key)
	t.Setenv("TEST_CLIENT__ALLOW_INSECURE_HTTP", "true")
	var cfg struct {
		Client clientcred.Config `envPrefix:"CLIENT__"`
	}
	if err := config.Load("TEST_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Client.HTTPTimeout != 10*time.Second {
		t.Errorf("HTTPTimeout = %v", cfg.Client.HTTPTimeout)
	}
	if !cfg.Client.AllowInsecureHTTP {
		t.Error("CLIENT__ALLOW_INSECURE_HTTP is not loaded")
	}
	// The default TokenURL goes to <Issuer>/oauth/token.
	ep := newEndpoint(t)
	c := good
	c.Issuer = ep.srv.URL + "/"
	src, err := clientcred.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(context.Background(), "identity", "s:a:b"); err != nil {
		t.Fatal(err)
	}
	if got := ep.requests(); len(got) != 1 || got[0].form["_path"] != "/oauth/token" {
		t.Errorf("requests = %v", got)
	}
}

// The private key does not appear in the formatted config.
func TestConfigRedactsKey(t *testing.T) {
	key := pemPKCS8(t, newKey(t))
	c := clientcred.Config{ID: clientID, KeyID: keyID, PrivateKey: config.Secret(key), Issuer: issuer}
	for _, out := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("config output has the key: %s", out)
		}
	}
}

// U6: the request has no empty or duplicate scope.
func TestU6RequestScopes(t *testing.T) {
	f := newFixture(t, setup{})
	if _, err := f.src.Token(context.Background(), "identity", "b:b:b", "", "a:a:a", "b:b:b"); err != nil {
		t.Fatal(err)
	}
	reqs := f.ep.requests()
	if len(reqs) != 1 || reqs[0].form["scope"] != "a:a:a b:b:b" {
		t.Fatalf("requests = %v", reqs)
	}
}

// U7f: a failed refresh before the expiry returns the cached token. After the
// expiry it returns the error.
func TestU7fRefreshFailsBeforeExpiry(t *testing.T) {
	f := newFixture(t, setup{})
	ctx := context.Background()
	tok, err := f.src.Token(ctx, "identity", "s:a:b")
	if err != nil || tok != "tok-1" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	f.ep.mu.Lock()
	f.ep.handler = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 503, map[string]any{"error": "temporarily_unavailable"})
	}
	f.ep.mu.Unlock()
	f.clock.advance(85 * time.Second)
	tok, err = f.src.Token(ctx, "identity", "s:a:b")
	if err != nil || tok != "tok-1" {
		t.Fatalf("before expiry: token = %q, err = %v", tok, err)
	}
	if n := len(f.ep.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	f.clock.advance(20 * time.Second)
	tok, err = f.src.Token(ctx, "identity", "s:a:b")
	if !errors.Is(err, auth.ErrTokenUnavailable) || tok != "" {
		t.Fatalf("after expiry: token = %q, err = %v", tok, err)
	}
	// The next call tries again.
	_, _ = f.src.Token(ctx, "identity", "s:a:b")
	if n := len(f.ep.requests()); n != 4 {
		t.Fatalf("requests = %d, want 4", n)
	}
}

// U9c: the scheme of Issuer and TokenURL.
func TestU9cScheme(t *testing.T) {
	key := pemPKCS8(t, newKey(t))
	base := clientcred.Config{
		ID: clientID, KeyID: keyID, PrivateKey: config.Secret(key),
		Issuer: "https://id.example.com", HTTPTimeout: time.Second,
	}
	cases := map[string]struct {
		mutate  func(*clientcred.Config)
		wantErr string
	}{
		"https":                    {func(*clientcred.Config) {}, ""},
		"http issuer":              {func(c *clientcred.Config) { c.Issuer = "http://id.example.com" }, "CLIENT__ISSUER"},
		"http issuer, default url": {func(c *clientcred.Config) { c.Issuer = "http://id.example.com"; c.TokenURL = "" }, "CLIENT__ISSUER"},
		"http token url":           {func(c *clientcred.Config) { c.TokenURL = "http://id.internal/oauth/token" }, "CLIENT__TOKEN_URL"},
		"ftp issuer":               {func(c *clientcred.Config) { c.Issuer = "ftp://id.example.com" }, "CLIENT__ISSUER"},
		"ftp token url":            {func(c *clientcred.Config) { c.TokenURL = "ftp://id.internal/oauth/token" }, "CLIENT__TOKEN_URL"},
		"no host":                  {func(c *clientcred.Config) { c.TokenURL = "https:///oauth/token" }, "CLIENT__TOKEN_URL"},
		"allow, http issuer": {func(c *clientcred.Config) {
			c.Issuer = "http://id.example.com"
			c.TokenURL = "http://id.internal/oauth/token"
			c.AllowInsecureHTTP = true
		}, ""},
		"allow, default url": {func(c *clientcred.Config) {
			c.Issuer = "http://id.example.com"
			c.TokenURL = ""
			c.AllowInsecureHTTP = true
		}, ""},
		"allow, ftp issuer": {func(c *clientcred.Config) {
			c.Issuer = "ftp://id.example.com"
			c.AllowInsecureHTTP = true
		}, "CLIENT__ISSUER"},
		"allow, ftp token url": {func(c *clientcred.Config) {
			c.TokenURL = "ftp://id.internal/oauth/token"
			c.AllowInsecureHTTP = true
		}, "CLIENT__TOKEN_URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			_, err := clientcred.New(c)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want the name %s", err, tc.wantErr)
			}
		})
	}
}

// U9e: the token source does not follow a redirect. The target gets no request.
func TestU9eNoRedirectTarget(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		f := newFixture(t, setup{})
		f.ep.handler = func(w http.ResponseWriter, r *http.Request, _ int) {
			http.Redirect(w, r, target.URL, code)
		}
		if _, err := f.src.Token(context.Background(), "identity", "s:a:b"); err == nil {
			t.Fatalf("%d: no error", code)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the target got %d requests", hits.Load())
	}
}
