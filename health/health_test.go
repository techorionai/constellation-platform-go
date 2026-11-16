package health_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/health"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

func get(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

func passing(name string) *mock.Checker { return &mock.Checker{CheckName: name} }

// U7
func TestLivez(t *testing.T) {
	h := health.NewHandler(health.NewState(), health.Config{})
	code, _, body := get(t, h, "/livez")
	if code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("got %d %q", code, body)
	}
}

// U8
func TestReadyzBeforeStarted(t *testing.T) {
	h := health.NewHandler(health.NewState(passing("postgres")), health.Config{})
	code, ct, body := get(t, h, "/readyz")
	if code != 503 || ct != "application/problem+json" {
		t.Fatalf("got %d %q", code, ct)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if p["code"] != "service_not_ready" {
		t.Fatalf("code = %v", p["code"])
	}
}

// U9
func TestReadyzStarted(t *testing.T) {
	s := health.NewState(passing("postgres"), passing("valkey"))
	s.MarkStarted()
	code, _, body := get(t, health.NewHandler(s, health.Config{}), "/readyz")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	var r struct {
		Status string
		Checks map[string]string
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != "ready" || r.Checks["postgres"] != "ok" || r.Checks["valkey"] != "ok" || len(r.Checks) != 2 {
		t.Fatalf("body = %s", body)
	}
}

// U10
func TestReadyzHidesErrorText(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	bad := &mock.Checker{CheckName: "postgres", CheckFunc: func(context.Context) error { return errors.New("secret-detail") }}
	s := health.NewState(bad)
	s.MarkStarted()
	code, _, body := get(t, health.NewHandler(s, health.Config{}), "/readyz")
	if code != 503 {
		t.Fatalf("code = %d", code)
	}
	if strings.Contains(body, "secret-detail") {
		t.Fatalf("body has the error text: %s", body)
	}
	if !strings.Contains(buf.String(), "secret-detail") {
		t.Fatalf("log has no error text: %s", buf.String())
	}
}

// U11
func TestReadyzCheckerIgnoresContext(t *testing.T) {
	slow := &mock.Checker{CheckName: "slow", CheckFunc: func(context.Context) error {
		time.Sleep(5 * time.Second)
		return nil
	}}
	s := health.NewState(slow)
	s.MarkStarted()
	start := time.Now()
	code, _, _ := get(t, health.NewHandler(s, health.Config{CheckTimeout: 100 * time.Millisecond}), "/readyz")
	if code != 503 {
		t.Fatalf("code = %d", code)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

// U12
func TestReadyzChecksRunTogether(t *testing.T) {
	wait := func(name string) *mock.Checker {
		return &mock.Checker{CheckName: name, CheckFunc: func(context.Context) error {
			time.Sleep(200 * time.Millisecond)
			return nil
		}}
	}
	s := health.NewState(wait("a"), wait("b"))
	s.MarkStarted()
	start := time.Now()
	code, _, _ := get(t, health.NewHandler(s, health.Config{}), "/readyz")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if d := time.Since(start); d >= 350*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

// U13
func TestDraining(t *testing.T) {
	s := health.NewState(passing("postgres"))
	s.MarkStarted()
	h := health.NewHandler(s, health.Config{})
	if code, _, _ := get(t, h, "/readyz"); code != 200 {
		t.Fatalf("before drain: %d", code)
	}
	s.MarkDraining()
	if code, _, _ := get(t, h, "/readyz"); code != 503 {
		t.Fatalf("readyz after drain: %d", code)
	}
	if code, _, _ := get(t, h, "/livez"); code != 200 {
		t.Fatalf("livez after drain: %d", code)
	}
}

// U14
func TestConfig(t *testing.T) {
	type svc struct {
		Admin health.Config `envPrefix:"ADMIN__"`
	}
	t.Setenv("SVC_ADMIN__ADDR", ":9191")
	t.Setenv("SVC_ADMIN__CHECK_TIMEOUT", "3s")
	t.Setenv("SVC_ADMIN__DRAIN_DELAY", "1s")
	var a svc
	if err := config.Load("SVC_", &a); err != nil {
		t.Fatal(err)
	}
	want := health.Config{Addr: ":9191", CheckTimeout: 3 * time.Second, DrainDelay: time.Second}
	if a.Admin != want {
		t.Fatalf("got %+v", a.Admin)
	}

	t.Setenv("SVC_ADMIN__ADDR", "")
	t.Setenv("SVC_ADMIN__CHECK_TIMEOUT", "")
	t.Setenv("SVC_ADMIN__DRAIN_DELAY", "")
	var b svc
	if err := config.Load("SVC_", &b); err != nil {
		t.Fatal(err)
	}
	want = health.Config{Addr: ":9090", CheckTimeout: 2 * time.Second, DrainDelay: 5 * time.Second}
	if b.Admin != want {
		t.Fatalf("got %+v", b.Admin)
	}
}

// U15
func TestProblemBody(t *testing.T) {
	h := health.NewHandler(health.NewState(), health.Config{})
	_, _, body := get(t, h, "/readyz")
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "title", "status", "code", "detail"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("field %s is missing: %s", k, body)
		}
	}
	if p["status"] != float64(503) || p["type"] != "about:blank" || p["title"] != "Service Unavailable" ||
		p["detail"] != "The service is not ready." {
		t.Fatalf("body = %s", body)
	}
	if _, ok := p["traceId"]; ok {
		t.Fatalf("body has traceId: %s", body)
	}
}

// U17
func TestReadyzOneChecker(t *testing.T) {
	s := health.NewState(passing("postgres"))
	s.MarkStarted()
	code, _, body := get(t, health.NewHandler(s, health.Config{}), "/readyz")
	if code != 200 || strings.TrimSpace(body) != `{"status":"ready","checks":{"postgres":"ok"}}` {
		t.Fatalf("got %d %q", code, body)
	}
}

// U18
func TestReadyzNoChecker(t *testing.T) {
	s := health.NewState()
	s.MarkStarted()
	code, _, body := get(t, health.NewHandler(s, health.Config{}), "/readyz")
	if code != 200 || strings.TrimSpace(body) != `{"status":"ready","checks":{}}` {
		t.Fatalf("got %d %q", code, body)
	}
}

// U24
func TestNewStatePanics(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", name)
			}
		}()
		f()
	}
	mustPanic("duplicate", func() { health.NewState(passing("postgres"), passing("postgres")) })
	mustPanic("empty", func() { health.NewState(passing("")) })
}

// U25
func TestDrain(t *testing.T) {
	s := health.NewState()
	s.MarkStarted()
	srv := health.NewServer(health.Config{DrainDelay: 100 * time.Millisecond}, s)
	start := time.Now()
	if err := srv.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > time.Second {
		t.Fatalf("Drain took %v", d)
	}
	if code, _, _ := get(t, health.NewHandler(s, health.Config{}), "/readyz"); code != 503 {
		t.Fatalf("readyz = %d", code)
	}

	s2 := health.NewState()
	s2.MarkStarted()
	srv2 := health.NewServer(health.Config{DrainDelay: 5 * time.Second}, s2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start = time.Now()
	err := srv2.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Drain did not stop with the context")
	}
}

// U26
func TestNoSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old); _ = tp.Shutdown(context.Background()) })

	s := health.NewState(passing("postgres"))
	s.MarkStarted()
	h := health.NewHandler(s, health.Config{})
	get(t, h, "/livez")
	get(t, h, "/readyz")
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("spans = %d, want 0", n)
	}
}

// U14
func TestServerWriteTimeoutCoversCheckTimeout(t *testing.T) {
	for _, ct := range []time.Duration{0, time.Second, 8 * time.Second, 30 * time.Second, time.Hour} {
		cfg := health.Config{CheckTimeout: ct}
		eff := ct
		if eff <= 0 {
			eff = 2 * time.Second
		}
		got := health.WriteTimeout(cfg)
		if got <= eff {
			t.Fatalf("CheckTimeout %v: write timeout %v is not longer", ct, got)
		}
		if got < 10*time.Second {
			t.Fatalf("CheckTimeout %v: write timeout %v is below 10 s", ct, got)
		}
	}
}
