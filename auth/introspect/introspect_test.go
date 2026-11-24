package introspect_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/auth/introspect"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const (
	audience  = "demo"
	clientID  = "client-demo"
	assertion = "assertion.jwt.value"
	subject   = "11111111-2222-3333-4444-555555555555"
)

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

// token returns a token in the correct format. Each n gives a different token.
func token(n int) string {
	return "cpat_" + fmt.Sprintf("%043d", n)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) count(substr string) int {
	return strings.Count(l.String(), substr)
}

type fixture struct {
	v      *introspect.Verifier
	srv    *httptest.Server
	clock  *fakeClock
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
	logs   *logBuffer
	asrt   *mock.ClientAssertion

	mu    sync.Mutex
	forms []map[string]string
	calls atomic.Int64
	// handler writes the answer. The default is an active token.
	handler func(w http.ResponseWriter, r *http.Request, n int)
}

type setup struct {
	cfg    func(*introspect.Config)
	client *http.Client
}

func activeBody(exp int64, mutate func(map[string]any)) map[string]any {
	m := map[string]any{
		"active": true, "aud": audience, "exp": exp, "sub": subject,
		"principal_type": "user", "token_type": "api_token",
		"scope": audience + ":thing:read " + audience + ":thing:write",
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newFixture(t *testing.T, s setup) *fixture {
	t.Helper()
	f := &fixture{
		clock: &fakeClock{t: time.Unix(1_800_000_000, 0)},
		logs:  &logBuffer{},
	}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{"_path": r.URL.Path, "_method": r.Method}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		f.mu.Lock()
		f.forms = append(f.forms, form)
		f.mu.Unlock()
		n := int(f.calls.Add(1))
		if f.handler != nil {
			f.handler(w, r, n)
			return
		}
		writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, nil))
	}))
	t.Cleanup(f.srv.Close)

	cfg := introspect.Config{
		Issuer: "http://issuer.example", IntrospectURL: f.srv.URL + "/oauth/introspect", Audience: audience,
		CacheTTL: 30 * time.Second, InactiveTTL: 5 * time.Second, CacheSize: 100, InactiveCacheSize: 10,
		MaxConcurrent: 16, HTTPTimeout: 5 * time.Second, AllowInsecureHTTP: true,
	}
	if s.cfg != nil {
		s.cfg(&cfg)
	}
	f.reader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader))
	f.spans = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(f.spans))
	f.asrt = &mock.ClientAssertion{ID: clientID, AssertionFunc: func(context.Context) (string, error) { return assertion, nil }}
	opts := []introspect.Option{introspect.WithClock(f.clock.now), introspect.WithMeterProvider(mp), introspect.WithTracerProvider(tp)}
	if s.client != nil {
		opts = append(opts, introspect.WithHTTPClient(s.client))
	}
	v, err := introspect.New(cfg, f.asrt, opts...)
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	return f
}

func (f *fixture) httpCalls() int { return int(f.calls.Load()) }

func (f *fixture) form(i int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[i]
}

func (f *fixture) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.auth.introspect" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("outcome")
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// U1: defaults and ranges.
func TestU1Config(t *testing.T) {
	good := introspect.Config{
		Issuer: "https://issuer.example", Audience: audience, CacheTTL: 30 * time.Second, InactiveTTL: 5 * time.Second,
		CacheSize: 10000, InactiveCacheSize: 1000, MaxConcurrent: 16, HTTPTimeout: 5 * time.Second,
	}
	if _, err := introspect.New(good, &mock.ClientAssertion{}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func(*introspect.Config)
		want   string
	}{
		"no Issuer":       {func(c *introspect.Config) { c.Issuer = "" }, "AUTH__INTROSPECT__ISSUER"},
		"no Audience":     {func(c *introspect.Config) { c.Audience = "" }, "AUTH__INTROSPECT__AUDIENCE"},
		"bad Audience":    {func(c *introspect.Config) { c.Audience = "A:b" }, "AUTH__INTROSPECT__AUDIENCE"},
		"http issuer":     {func(c *introspect.Config) { c.Issuer = "http://issuer.example" }, "AUTH__INTROSPECT__ISSUER"},
		"http url":        {func(c *introspect.Config) { c.IntrospectURL = "http://issuer.example/x" }, "AUTH__INTROSPECT__URL"},
		"ftp url":         {func(c *introspect.Config) { c.IntrospectURL = "ftp://issuer.example/x" }, "AUTH__INTROSPECT__URL"},
		"cache ttl low":   {func(c *introspect.Config) { c.CacheTTL = 500 * time.Millisecond }, "AUTH__INTROSPECT__CACHE_TTL"},
		"cache ttl high":  {func(c *introspect.Config) { c.CacheTTL = 61 * time.Second }, "AUTH__INTROSPECT__CACHE_TTL"},
		"inactive ttl":    {func(c *introspect.Config) { c.InactiveTTL = 31 * time.Second }, "AUTH__INTROSPECT__INACTIVE_TTL"},
		"inactive ttl <0": {func(c *introspect.Config) { c.InactiveTTL = -1 }, "AUTH__INTROSPECT__INACTIVE_TTL"},
		"cache size low":  {func(c *introspect.Config) { c.CacheSize = 99 }, "AUTH__INTROSPECT__CACHE_SIZE"},
		"cache size high": {func(c *introspect.Config) { c.CacheSize = 1_000_001 }, "AUTH__INTROSPECT__CACHE_SIZE"},
		"inactive size":   {func(c *introspect.Config) { c.InactiveCacheSize = 9 }, "AUTH__INTROSPECT__INACTIVE_CACHE_SIZE"},
		"concurrent 0":    {func(c *introspect.Config) { c.MaxConcurrent = 0 }, "AUTH__INTROSPECT__MAX_CONCURRENT"},
		"concurrent 257":  {func(c *introspect.Config) { c.MaxConcurrent = 257 }, "AUTH__INTROSPECT__MAX_CONCURRENT"},
		"timeout low":     {func(c *introspect.Config) { c.HTTPTimeout = time.Millisecond }, "AUTH__INTROSPECT__HTTP_TIMEOUT"},
		"timeout high":    {func(c *introspect.Config) { c.HTTPTimeout = time.Minute }, "AUTH__INTROSPECT__HTTP_TIMEOUT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			_, err := introspect.New(c, &mock.ClientAssertion{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want text %q", err, tc.want)
			}
		})
	}
	// InactiveTTL 0 does not check InactiveCacheSize.
	c := good
	c.InactiveTTL, c.InactiveCacheSize = 0, 0
	if _, err := introspect.New(c, &mock.ClientAssertion{}); err != nil {
		t.Fatal(err)
	}
	if _, err := introspect.New(good, nil); err == nil {
		t.Fatal("a nil assertion must fail")
	}
	// Defaults from the environment.
	t.Setenv("TEST_AUTH__INTROSPECT__ISSUER", "https://issuer.example")
	t.Setenv("TEST_AUTH__INTROSPECT__AUDIENCE", audience)
	var cfg struct {
		Introspect introspect.Config `envPrefix:"AUTH__INTROSPECT__"`
	}
	if err := config.Load("TEST_", &cfg); err != nil {
		t.Fatal(err)
	}
	want := introspect.Config{
		Issuer: "https://issuer.example", Audience: audience, CacheTTL: 30 * time.Second, InactiveTTL: 5 * time.Second,
		CacheSize: 10000, InactiveCacheSize: 1000, MaxConcurrent: 16, HTTPTimeout: 5 * time.Second,
	}
	if cfg.Introspect != want {
		t.Fatalf("defaults = %+v", cfg.Introspect)
	}
	if _, err := introspect.New(cfg.Introspect, &mock.ClientAssertion{}); err != nil {
		t.Fatal(err)
	}
}

// The default IntrospectURL is <Issuer>/oauth/introspect.
func TestDefaultURL(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *introspect.Config) { c.IntrospectURL = ""; c.Issuer = f0(t) + "/" }})
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
}

// f0 starts a server that checks the path and returns its URL.
func f0(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/introspect" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, 200, activeBody(4_000_000_000, nil))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// U2: the form.
func TestU2Form(t *testing.T) {
	f := newFixture(t, setup{})
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
	form := f.form(0)
	want := map[string]string{
		"_method": "POST", "_path": "/oauth/introspect", "token": token(1), "token_type_hint": "api_token",
		"client_id": clientID, "client_assertion": assertion,
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
	}
	for k, v := range want {
		if form[k] != v {
			t.Errorf("form[%s] = %q, want %q", k, form[k], v)
		}
	}
	if len(form) != len(want) {
		t.Errorf("form = %v", form)
	}
}

// U3: an active token.
func TestU3Active(t *testing.T) {
	f := newFixture(t, setup{})
	p, err := f.v.Verify(context.Background(), token(1))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != subject || p.Type != auth.PrincipalUser || p.Audience != audience || p.TokenType != auth.TokenTypeAPI || p.Act != nil {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Scopes) != 2 || p.Scopes[0] != "demo:thing:read" || p.Scopes[1] != "demo:thing:write" {
		t.Fatalf("scopes = %v", p.Scopes)
	}
	if got := f.outcomes(t); got["active"] != 1 {
		t.Fatalf("outcomes = %v", got)
	}
	if len(f.spans.Ended()) == 0 {
		t.Error("no span")
	}
}

// U4: cache hit.
func TestU4CacheHit(t *testing.T) {
	f := newFixture(t, setup{})
	for i := 0; i < 2; i++ {
		if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
			t.Fatal(err)
		}
	}
	if f.httpCalls() != 1 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
	if got := f.outcomes(t); got["cached_active"] != 1 || got["active"] != 1 {
		t.Fatalf("outcomes = %v", got)
	}
	// A caller cannot change the cached scopes.
	p, _ := f.v.Verify(context.Background(), token(1))
	p.Scopes[0] = "x"
	p2, _ := f.v.Verify(context.Background(), token(1))
	if p2.Scopes[0] != "demo:thing:read" {
		t.Fatal("the cache shares the scope slice")
	}
}

// U5: cache expiry.
func TestU5CacheExpiry(t *testing.T) {
	f := newFixture(t, setup{})
	_, _ = f.v.Verify(context.Background(), token(1))
	f.clock.advance(29 * time.Second)
	_, _ = f.v.Verify(context.Background(), token(1))
	if f.httpCalls() != 1 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
	f.clock.advance(2 * time.Second)
	_, _ = f.v.Verify(context.Background(), token(1))
	if f.httpCalls() != 2 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
}

// U6: the cache entry does not live after exp.
func TestU6ExpLimitsCache(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, 200, activeBody(f.clock.now().Unix()+10, nil))
	}
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(11 * time.Second)
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, 200, activeBody(f.clock.now().Unix()+10, nil))
	}
	if _, err := f.v.Verify(context.Background(), token(1)); err != nil {
		t.Fatal(err)
	}
	if f.httpCalls() != 2 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
}

// U7: inactive token.
func TestU7Inactive(t *testing.T) {
	f := newFixture(t, setup{})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, 200, map[string]any{"active": false})
	}
	for i := 0; i < 2; i++ {
		_, err := f.v.Verify(context.Background(), token(1))
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	}
	if f.httpCalls() != 1 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
	if got := f.outcomes(t); got["inactive"] != 1 || got["cached_inactive"] != 1 {
		t.Fatalf("outcomes = %v", got)
	}
	f.clock.advance(6 * time.Second)
	_, _ = f.v.Verify(context.Background(), token(1))
	if f.httpCalls() != 2 {
		t.Fatalf("calls after InactiveTTL = %d", f.httpCalls())
	}

	g := newFixture(t, setup{cfg: func(c *introspect.Config) { c.InactiveTTL = 0 }})
	g.handler = f.handler
	for i := 0; i < 2; i++ {
		_, _ = g.v.Verify(context.Background(), token(1))
	}
	if g.httpCalls() != 2 {
		t.Fatalf("calls with InactiveTTL 0 = %d", g.httpCalls())
	}
}

// U8: field checks of an active response.
func TestU8FieldChecks(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong aud":          func(m map[string]any) { m["aud"] = "other" },
		"past exp":           func(m map[string]any) { m["exp"] = 1_700_000_000 },
		"no exp":             func(m map[string]any) { delete(m, "exp") },
		"empty sub":          func(m map[string]any) { m["sub"] = "" },
		"principal agent":    func(m map[string]any) { m["principal_type"] = "agent" },
		"token type":         func(m map[string]any) { m["token_type"] = "access_token" },
		"scope other app":    func(m map[string]any) { m["scope"] = "other:thing:read" },
		"scope malformed":    func(m map[string]any) { m["scope"] = "demo:Thing:read" },
		"scope two parts":    func(m map[string]any) { m["scope"] = "demo:read" },
		"scope double space": func(m map[string]any) { m["scope"] = "demo:a:b  demo:a:c" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
				writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, mutate))
			}
			for i := 0; i < 2; i++ {
				_, err := f.v.Verify(context.Background(), token(1))
				if !errors.Is(err, auth.ErrInvalidToken) {
					t.Fatalf("err = %v", err)
				}
			}
			if f.httpCalls() != 2 {
				t.Fatalf("the result was cached, calls = %d", f.httpCalls())
			}
			if got := f.outcomes(t); got["invalid_response"] != 2 {
				t.Fatalf("outcomes = %v", got)
			}
			if name == "wrong aud" {
				if f.logs.count("level=WARN") != 1 || !strings.Contains(f.logs.String(), "expected_aud=demo") ||
					!strings.Contains(f.logs.String(), "received_aud=other") {
					t.Fatalf("logs = %s", f.logs.String())
				}
			}
		})
	}
}

// U9: unavailable.
func TestU9Unavailable(t *testing.T) {
	handlers := map[string]func(w http.ResponseWriter, r *http.Request, n int){
		"500": func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, 500, map[string]any{"error": "server_error"})
		},
		"503": func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(503) },
		"temporarily_unavailable": func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, 400, map[string]any{"error": "temporarily_unavailable"})
		},
		"temporarily_unavailable 200": func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, 503, map[string]any{"error": "temporarily_unavailable"})
		},
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.handler = h
			for i := 0; i < 2; i++ {
				_, err := f.v.Verify(context.Background(), token(1))
				if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
					t.Fatalf("err = %v", err)
				}
			}
			if f.httpCalls() != 2 {
				t.Fatalf("the result was cached, calls = %d", f.httpCalls())
			}
			if got := f.outcomes(t); got["unavailable"] != 2 {
				t.Fatalf("outcomes = %v", got)
			}
		})
	}
	t.Run("server down", func(t *testing.T) {
		f := newFixture(t, setup{})
		f.srv.Close()
		_, err := f.v.Verify(context.Background(), token(1))
		if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), token(1)) {
			t.Fatal("the error has the token")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFixture(t, setup{client: &http.Client{Timeout: 50 * time.Millisecond}})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		_, err := f.v.Verify(context.Background(), token(1))
		if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

// U10: the client is rejected.
func TestU10ClientRejected(t *testing.T) {
	for name, status := range map[string]int{"invalid_client": 401, "invalid_request": 400} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
				writeJSON(w, status, map[string]any{"error": name})
			}
			for i := 0; i < 2; i++ {
				_, err := f.v.Verify(context.Background(), token(i))
				if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
					t.Fatalf("err = %v", err)
				}
			}
			if got := f.outcomes(t); got["client_rejected"] != 2 {
				t.Fatalf("outcomes = %v", got)
			}
			if f.logs.count("level=ERROR") != 1 {
				t.Fatalf("logs = %s", f.logs.String())
			}
			f.clock.advance(61 * time.Second)
			_, _ = f.v.Verify(context.Background(), token(5))
			if f.logs.count("level=ERROR") != 2 {
				t.Fatalf("after 60 s logs = %s", f.logs.String())
			}
		})
	}
}

// U11: invalid responses.
func TestU11InvalidResponse(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request, n int){
		"too large": func(w http.ResponseWriter, r *http.Request, n int) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"active":false,"pad":"` + strings.Repeat("a", 70<<10) + `"}`))
		},
		"bad json": func(w http.ResponseWriter, r *http.Request, n int) { _, _ = w.Write([]byte("{not json")) },
		"empty":    func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(200) },
		"404":      func(w http.ResponseWriter, r *http.Request, n int) { http.NotFound(w, r) },
		"no active": func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, 200, map[string]any{})
		},
		"redirect": func(w http.ResponseWriter, r *http.Request, n int) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, setup{})
			f.handler = h
			for i := 0; i < 2; i++ {
				_, err := f.v.Verify(context.Background(), token(1))
				if !errors.Is(err, auth.ErrIntrospectionUnavailable) {
					t.Fatalf("err = %v", err)
				}
			}
			if f.httpCalls() != 2 {
				t.Fatalf("calls = %d (cached or redirect followed)", f.httpCalls())
			}
			if got := f.outcomes(t); got["invalid_response"] != 2 {
				t.Fatalf("outcomes = %v", got)
			}
			if f.logs.count("level=ERROR") != 1 {
				t.Fatalf("logs = %s", f.logs.String())
			}
		})
	}
}

// U12: singleflight.
func TestU12Singleflight(t *testing.T) {
	f := newFixture(t, setup{})
	release := make(chan struct{})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		<-release
		writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, nil))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.v.Verify(context.Background(), token(1))
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.httpCalls() != 1 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
}

// U13: the size limit of the active cache.
func TestU13CacheSize(t *testing.T) {
	f := newFixture(t, setup{})
	for i := 0; i < 150; i++ {
		if _, err := f.v.Verify(context.Background(), token(i)); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest 50 entries are gone. The newest 100 are in the cache.
	before := f.httpCalls()
	for i := 50; i < 150; i++ {
		_, _ = f.v.Verify(context.Background(), token(i))
	}
	if f.httpCalls() != before {
		t.Fatalf("a recent entry was removed, new calls = %d", f.httpCalls()-before)
	}
	_, _ = f.v.Verify(context.Background(), token(0))
	if f.httpCalls() != before+1 {
		t.Fatal("the oldest entry is still in the cache")
	}
}

// U14: the format check.
func TestU14Format(t *testing.T) {
	good := token(7)
	bad := map[string]string{
		"no prefix":     "cpax_" + good[5:],
		"jwt":           "eyJhbGciOiJFUzI1NiJ9.e30.sig",
		"513 bytes":     "cpat_" + strings.Repeat("a", 508),
		"42 chars":      good[:len(good)-1],
		"44 chars":      good + "a",
		"bad char":      good[:10] + "+" + good[11:],
		"dot":           good[:10] + "." + good[11:],
		"space":         good[:10] + " " + good[11:],
		"newline":       good[:10] + "\n" + good[11:],
		"trailing nl":   good + "\n",
		"empty":         "",
		"prefix only":   "cpat_",
		"upper prefix":  "CPAT_" + good[5:],
		"multibyte":     good[:10] + "é" + good[12:],
		"padded base64": good[:len(good)-1] + "=",
	}
	f := newFixture(t, setup{})
	for name, tok := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := f.v.Verify(context.Background(), tok)
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if f.httpCalls() != 0 {
		t.Fatalf("calls = %d", f.httpCalls())
	}
	if got := f.outcomes(t); got["invalid_format"] != int64(len(bad)) {
		t.Fatalf("outcomes = %v", got)
	}
	// The base64url characters - and _ are valid.
	if _, err := f.v.Verify(context.Background(), "cpat_"+strings.Repeat("-_Az09", 7)+"a"); err != nil {
		t.Fatal(err)
	}
}

// U15: no raw token, hash, or subject in any output.
func TestU15NoSecrets(t *testing.T) {
	f := newFixture(t, setup{})
	tok := token(1)
	mode := 0
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		switch mode {
		case 0:
			writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, nil))
		case 1:
			writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, func(m map[string]any) { m["aud"] = "other" }))
		case 2:
			writeJSON(w, 401, map[string]any{"error": "invalid_client"})
		default:
			w.WriteHeader(404)
		}
	}
	var errTexts []string
	for mode = 0; mode < 4; mode++ {
		f.clock.advance(61 * time.Second)
		_, err := f.v.Verify(context.Background(), tok)
		if err != nil {
			errTexts = append(errTexts, err.Error())
		}
		_, err = f.v.Verify(context.Background(), token(99+mode))
		if err != nil {
			errTexts = append(errTexts, err.Error())
		}
	}
	_, err := f.v.Verify(context.Background(), "cpat_bad")
	errTexts = append(errTexts, err.Error())
	sum := sha256.Sum256([]byte(tok))
	var out strings.Builder
	out.WriteString(f.logs.String())
	out.WriteString(strings.Join(errTexts, "\n"))
	for _, s := range f.spans.Ended() {
		out.WriteString(s.Name())
		for _, a := range s.Attributes() {
			out.WriteString(string(a.Key) + "=" + a.Value.String() + "\n")
		}
	}
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&out, "%+v", rm)
	for _, secret := range []string{tok, hex.EncodeToString(sum[:]), subject, assertion} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("the output has %q", secret)
		}
	}
}

// U30: a limit on concurrent identity calls.
func TestU30MaxConcurrent(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *introspect.Config) { c.MaxConcurrent = 4 }})
	release := make(chan struct{})
	var running, peak atomic.Int64
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		cur := running.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-release
		running.Add(-1)
		writeJSON(w, 200, map[string]any{"active": false})
	}
	var wg sync.WaitGroup
	var overloaded, other atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.v.Verify(context.Background(), token(i))
			if errors.Is(err, auth.ErrIntrospectionUnavailable) {
				overloaded.Add(1)
			} else {
				other.Add(1)
			}
		}()
	}
	// Wait until the 96 refused calls return.
	deadline := time.Now().Add(5 * time.Second)
	for overloaded.Load() < 96 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if peak.Load() > 4 {
		t.Fatalf("peak = %d", peak.Load())
	}
	if overloaded.Load() < 96 {
		t.Fatalf("overloaded = %d", overloaded.Load())
	}
	if got := f.outcomes(t); got["overloaded"] != overloaded.Load() {
		t.Fatalf("outcomes = %v", got)
	}
	if f.logs.count("level=WARN") != 1 {
		t.Fatalf("warn records = %d", f.logs.count("level=WARN"))
	}
	// The refused result is not cached. The slot is free again.
	f.handler = nil
	if _, err := f.v.Verify(context.Background(), token(1000)); err != nil {
		t.Fatalf("after the load: %v", err)
	}
}

// U31: inactive tokens do not remove active entries.
func TestU31InactiveDoesNotEvictActive(t *testing.T) {
	f := newFixture(t, setup{cfg: func(c *introspect.Config) {
		c.CacheSize = 100
		c.InactiveCacheSize = 100
		c.MaxConcurrent = 256
	}})
	f.handler = func(w http.ResponseWriter, r *http.Request, n int) {
		_ = r.ParseForm()
		if r.PostForm.Get("token") == token(0) {
			writeJSON(w, 200, activeBody(f.clock.now().Unix()+3600, nil))
			return
		}
		writeJSON(w, 200, map[string]any{"active": false})
	}
	if _, err := f.v.Verify(context.Background(), token(0)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5000; i++ {
		_, _ = f.v.Verify(context.Background(), token(i))
	}
	before := f.httpCalls()
	if _, err := f.v.Verify(context.Background(), token(0)); err != nil {
		t.Fatal(err)
	}
	if f.httpCalls() != before {
		t.Fatal("the active entry was removed")
	}
	// The inactive cache has 100 entries or less: the oldest tokens are not in it.
	before = f.httpCalls()
	_, _ = f.v.Verify(context.Background(), token(1))
	if f.httpCalls() != before+1 {
		t.Fatal("the inactive cache is larger than its limit")
	}
}
