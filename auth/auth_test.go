package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

const secretToken = "SECRET.TOKEN.VALUE"

var testPrincipal = auth.Principal{
	ID: "u1", Type: auth.PrincipalUser, ClientID: "c1",
	Scopes: []string{"demo:ticket:read"}, Audience: "demo",
}

// U1
func TestPrincipalContext(t *testing.T) {
	if _, ok := auth.PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty context has a principal")
	}
	ctx := auth.ContextWithPrincipal(context.Background(), testPrincipal)
	got, ok := auth.PrincipalFromContext(ctx)
	if !ok || got.ID != "u1" || got.ClientID != "c1" {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

// U2
func TestScopes(t *testing.T) {
	p := auth.Principal{Scopes: []string{"a:b:c", "a:b:d"}}
	if !p.HasScopes("a:b:d") || !p.HasScopes("a:b:c", "a:b:d") || !p.HasScopes() {
		t.Error("subset or empty request must pass")
	}
	if p.HasScopes("a:b:c", "x:y:z") {
		t.Error("missing scope must fail")
	}
	got := p.MissingScopes("z:z:z", "a:b:c", "y:y:y")
	if len(got) != 2 || got[0] != "z:z:z" || got[1] != "y:y:y" {
		t.Errorf("missing = %v", got)
	}
}

type logCapture struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	json slog.Handler
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	c.json = slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	old := slog.Default()
	slog.SetDefault(slog.New(&lockedHandler{c: c}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

type lockedHandler struct{ c *logCapture }

func (h *lockedHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *lockedHandler) Handle(ctx context.Context, r slog.Record) error {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.c.json.Handle(ctx, r)
}
func (h *lockedHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lockedHandler) WithGroup(string) slog.Handler      { return h }

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type env struct {
	spans   *tracetest.SpanRecorder
	reader  *sdkmetric.ManualReader
	handler http.Handler
	ran     *bool
	got     *auth.Principal
}

// newEnv builds requestid, a span, auth.Middleware, and an inner handler.
func newEnv(t *testing.T, v auth.Verifier, scopes ...string) *env {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	ran := false
	var got auth.Principal
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		got, _ = auth.PrincipalFromContext(r.Context())
		slog.InfoContext(r.Context(), "handled")
		w.WriteHeader(http.StatusNoContent)
	})
	if len(scopes) > 0 {
		inner = auth.RequireScopes(scopes...)(inner)
	}
	chain := requestid.Middleware()(auth.Middleware(v, auth.WithMeterProvider(mp))(inner))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tp.Tracer("t").Start(r.Context(), "req")
		defer span.End()
		chain.ServeHTTP(w, r.WithContext(ctx))
	})
	return &env{spans: sr, reader: reader, handler: h, ran: &ran, got: &got}
}

func (e *env) do(authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.auth.verify" {
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

type prob struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Status        int      `json:"status"`
	Code          string   `json:"code"`
	Detail        string   `json:"detail"`
	RequestID     string   `json:"requestId"`
	TraceID       string   `json:"traceId"`
	MissingScopes []string `json:"missingScopes"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) prob {
	t.Helper()
	var p prob
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return p
}

func okVerifier() *mock.Verifier {
	return &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return testPrincipal, nil
	}}
}

// U3
func TestNoAuthorizationHeader(t *testing.T) {
	v := okVerifier()
	e := newEnv(t, v)
	rec := e.do("")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("status %d, header %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if decode(t, rec).Code != "unauthorized" {
		t.Errorf("body %s", rec.Body)
	}
	if *e.ran || v.VerifyCalls() != 0 {
		t.Error("next handler or verifier ran")
	}
}

// U4
func TestWrongSchemeAndEmptyToken(t *testing.T) {
	e := newEnv(t, okVerifier())
	for _, h := range []string{"Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "Bearerx abc"} {
		rec := e.do(h)
		if rec.Code != 401 || decode(t, rec).Code != "unauthorized" {
			t.Errorf("%q: status %d body %s", h, rec.Code, rec.Body)
		}
	}
	if *e.ran {
		t.Error("next handler ran")
	}
}

// U4a
func TestLowerCaseScheme(t *testing.T) {
	e := newEnv(t, okVerifier())
	rec := e.do("bearer valid-token")
	if rec.Code != 204 || !*e.ran || e.got.ID != "u1" {
		t.Fatalf("status %d ran %v principal %+v", rec.Code, *e.ran, *e.got)
	}
}

// U5
func TestErrorMap(t *testing.T) {
	const invalidHdr = `Bearer error="invalid_token"`
	cases := []struct {
		err                    error
		status                 int
		www, code, title, outc string
	}{
		{auth.ErrNoToken, 401, "Bearer", "unauthorized", "Authentication required", "no_token"},
		{auth.ErrInvalidToken, 401, invalidHdr, "unauthorized", "Authentication required", "invalid"},
		{auth.ErrExpired, 401, invalidHdr, "unauthorized", "Authentication required", "expired"},
		{auth.ErrWrongIssuer, 401, invalidHdr, "unauthorized", "Authentication required", "wrong_issuer"},
		{auth.ErrWrongAudience, 401, invalidHdr, "unauthorized", "Authentication required", "wrong_audience"},
		{auth.ErrKeysUnavailable, 503, "", "auth_unavailable", "Authentication unavailable", "keys_unavailable"},
		{errors.New("other"), 401, invalidHdr, "unauthorized", "Authentication required", "invalid"},
	}
	for _, tc := range cases {
		for _, wrap := range []bool{false, true} {
			err := tc.err
			if wrap {
				err = fmt.Errorf("deep: %w", fmt.Errorf("wrap: %w", err))
			}
			t.Run(fmt.Sprintf("%v/%v", tc.err, wrap), func(t *testing.T) {
				v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
					return auth.Principal{}, err
				}}
				e := newEnv(t, v)
				rec := e.do("Bearer tok")
				p := decode(t, rec)
				if rec.Code != tc.status || rec.Header().Get("WWW-Authenticate") != tc.www ||
					p.Code != tc.code || p.Title != tc.title {
					t.Errorf("status %d www %q body %+v", rec.Code, rec.Header().Get("WWW-Authenticate"), p)
				}
				if got := e.outcomes(t); got[tc.outc] != 1 || len(got) != 1 {
					t.Errorf("outcomes %v, want %s", got, tc.outc)
				}
				if *e.ran {
					t.Error("next handler ran")
				}
			})
		}
	}
}

// U6
func TestKeysUnavailable(t *testing.T) {
	v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{}, auth.ErrKeysUnavailable
	}}
	rec := newEnv(t, v).do("Bearer tok")
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || decode(t, rec).Code != "auth_unavailable" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body)
	}
}

// U7
func TestSuccess(t *testing.T) {
	e := newEnv(t, okVerifier())
	rec := e.do("Bearer tok")
	if rec.Code != 204 || !*e.ran || e.got.ID != "u1" {
		t.Fatalf("status %d ran %v", rec.Code, *e.ran)
	}
	spans := e.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans %d", len(spans))
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs["enduser.id"] != "u1" || attrs["constellation.principal.type"] != "user" ||
		attrs["constellation.client.id"] != "c1" || attrs["http.request.id"] == "" {
		t.Errorf("attributes %v", attrs)
	}
	if got := e.outcomes(t); got["ok"] != 1 {
		t.Errorf("outcomes %v", got)
	}
}

// U8
func TestRequireScopesMissing(t *testing.T) {
	e := newEnv(t, okVerifier(), "demo:ticket:read", "demo:ticket:write", "demo:ticket:admin")
	rec := e.do("Bearer tok")
	p := decode(t, rec)
	if rec.Code != 403 || p.Code != "insufficient_scope" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if len(p.MissingScopes) != 2 || p.MissingScopes[0] != "demo:ticket:write" || p.MissingScopes[1] != "demo:ticket:admin" {
		t.Errorf("missingScopes %v", p.MissingScopes)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Errorf("header %q", rec.Header().Get("WWW-Authenticate"))
	}
	if *e.ran {
		t.Error("next handler ran")
	}
	ok := newEnv(t, okVerifier(), "demo:ticket:read")
	if rec := ok.do("Bearer tok"); rec.Code != 204 {
		t.Errorf("allowed request got %d", rec.Code)
	}
}

// U8a
func TestRequireScopesWithoutMiddleware(t *testing.T) {
	logs := captureLogs(t)
	ran := false
	h := requestid.Middleware()(auth.RequireScopes("a:b:c")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		ran = true
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if rec.Code != 500 || decode(t, rec).Code != "auth_not_configured" || ran {
		t.Fatalf("status %d ran %v body %s", rec.Code, ran, rec.Body)
	}
	if n := strings.Count(logs.String(), `"level":"ERROR"`); n != 1 {
		t.Errorf("error records: %d\n%s", n, logs.String())
	}
}

// U9, U10
func TestProblemBodiesAndNoTokenInOutput(t *testing.T) {
	logs := captureLogs(t)
	verr := errors.New("internal detail " + secretToken)
	cases := []struct {
		name string
		do   func() (*env, *httptest.ResponseRecorder)
	}{
		{"U3", func() (*env, *httptest.ResponseRecorder) { e := newEnv(t, okVerifier()); return e, e.do("") }},
		{"U6", func() (*env, *httptest.ResponseRecorder) {
			e := newEnv(t, &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
				return auth.Principal{}, fmt.Errorf("%w: %s", auth.ErrKeysUnavailable, "internal detail")
			}})
			return e, e.do("Bearer " + secretToken)
		}},
		{"U5", func() (*env, *httptest.ResponseRecorder) {
			e := newEnv(t, &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
				return auth.Principal{}, verr
			}})
			return e, e.do("Bearer " + secretToken)
		}},
		{"U8", func() (*env, *httptest.ResponseRecorder) {
			e := newEnv(t, okVerifier(), "x:y:z")
			return e, e.do("Bearer " + secretToken)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, rec := tc.do()
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content type %q", ct)
			}
			p := decode(t, rec)
			if p.Type == "" || p.Title == "" || p.Status != rec.Code || p.Code == "" {
				t.Errorf("body %+v", p)
			}
			if p.RequestID == "" || p.RequestID != rec.Header().Get("X-Request-ID") {
				t.Errorf("requestId %q header %q", p.RequestID, rec.Header().Get("X-Request-ID"))
			}
			if p.TraceID == "" {
				t.Error("traceId missing")
			}
			if p.Detail != "" || strings.Contains(rec.Body.String(), "internal detail") ||
				strings.Contains(rec.Body.String(), secretToken) {
				t.Errorf("body leaks text: %s", rec.Body)
			}
			for _, s := range e.spans.Ended() {
				for _, a := range s.Attributes() {
					if strings.Contains(a.Value.String(), secretToken) {
						t.Errorf("span attribute %s has the token", a.Key)
					}
				}
			}
		})
	}
	// U10 also covers U8a.
	rec := httptest.NewRecorder()
	requestid.Middleware()(auth.RequireScopes("a:b:c")(http.NotFoundHandler())).
		ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if p := decode(t, rec); p.RequestID != rec.Header().Get("X-Request-ID") || p.Title == "" {
		t.Errorf("U8a body %+v", p)
	}
	if strings.Contains(logs.String(), secretToken) {
		t.Errorf("log has the token:\n%s", logs.String())
	}
}

// U24a
func TestLogFieldsBehindMiddleware(t *testing.T) {
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
	e := newEnv(t, okVerifier())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	e.handler.ServeHTTP(httptest.NewRecorder(), req)
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
		"principal.id": "u1", "principal.type": "user", "client.id": "c1",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %s", k, rec[k], want)
		}
	}
	if s, _ := rec["request.id"].(string); s == "" {
		t.Errorf("request.id missing: %v", rec)
	}
}
