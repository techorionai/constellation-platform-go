package telemetry

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
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	olog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// clearOTELEnv removes all OTEL_* variables that the tests can see.
func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_TRACES_EXPORTER",
		"OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER", "OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES", "OTEL_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", v)
}

// U12
func TestNoExporterWithoutEnv(t *testing.T) {
	clearOTELEnv(t)
	e, err := selectExporters(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if e.spans != nil || e.metrics != nil || e.logs != nil {
		t.Fatalf("got exporters: %+v", e)
	}
	p, err := Setup(t.Context(), Options{ServiceName: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// U35
func TestExporterSelection(t *testing.T) {
	t.Run("endpoint", func(t *testing.T) {
		clearOTELEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
		e, err := selectExporters(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.spans == nil || e.metrics == nil || e.logs == nil {
			t.Fatalf("want 3 exporters, got %s %s %s", typeName(e.spans), typeName(e.metrics), typeName(e.logs))
		}
		if !strings.Contains(typeName(e.spans), "otlptrace") {
			t.Errorf("spans: %s", typeName(e.spans))
		}
		if !strings.Contains(typeName(e.logs), "otlploghttp") {
			t.Errorf("logs: %s", typeName(e.logs))
		}
	})
	t.Run("console traces", func(t *testing.T) {
		clearOTELEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		e, err := selectExporters(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.spans == nil || !strings.Contains(typeName(e.spans), "stdouttrace") {
			t.Errorf("spans: %s", typeName(e.spans))
		}
		if e.metrics != nil || e.logs != nil {
			t.Errorf("want no metrics and no logs exporter")
		}
	})
	t.Run("traces endpoint", func(t *testing.T) {
		clearOTELEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")
		e, err := selectExporters(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.spans == nil || !strings.Contains(typeName(e.spans), "otlptrace") {
			t.Errorf("spans: %s", typeName(e.spans))
		}
		if e.metrics != nil || e.logs != nil {
			t.Errorf("want no metrics and no logs exporter")
		}
	})
}

type receiver struct {
	mu      sync.Mutex
	traces  []*coltrace.ExportTraceServiceRequest
	metrics []*colmetrics.ExportMetricsServiceRequest
	logs    []*collogs.ExportLogsServiceRequest
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	var (
		msg  proto.Message
		resp proto.Message
	)
	switch req.URL.Path {
	case "/v1/traces":
		m := &coltrace.ExportTraceServiceRequest{}
		r.traces = append(r.traces, m)
		msg, resp = m, &coltrace.ExportTraceServiceResponse{}
	case "/v1/metrics":
		m := &colmetrics.ExportMetricsServiceRequest{}
		r.metrics = append(r.metrics, m)
		msg, resp = m, &colmetrics.ExportMetricsServiceResponse{}
	case "/v1/logs":
		m := &collogs.ExportLogsServiceRequest{}
		r.logs = append(r.logs, m)
		msg, resp = m, &collogs.ExportLogsServiceResponse{}
	default:
		http.NotFound(w, req)
		return
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, _ := proto.Marshal(resp)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func attr(res *resourcepb.Resource, key string) string {
	for _, kv := range res.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// U13
func TestSetupSendsToOTLP(t *testing.T) {
	clearOTELEnv(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	var out bytes.Buffer
	p, err := setup(t.Context(), Options{ServiceName: "svc", ServiceVersion: "1.2.3"}, hooks{stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := otel.Tracer("t").Start(t.Context(), "op")
	c, _ := otel.Meter("t").Int64Counter("test.counter")
	c.Add(ctx, 1)
	slog.InfoContext(ctx, "hello")
	span.End()
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	if len(rcv.traces) == 0 || len(rcv.metrics) == 0 || len(rcv.logs) == 0 {
		t.Fatalf("traces=%d metrics=%d logs=%d", len(rcv.traces), len(rcv.metrics), len(rcv.logs))
	}
	check := func(name string, res *resourcepb.Resource) {
		if attr(res, "service.name") != "svc" || attr(res, "service.version") != "1.2.3" {
			t.Errorf("%s: resource %v", name, res)
		}
	}
	check("trace", rcv.traces[0].GetResourceSpans()[0].GetResource())
	check("metric", rcv.metrics[0].GetResourceMetrics()[0].GetResource())
	check("log", rcv.logs[0].GetResourceLogs()[0].GetResource())
}

func collect(t *testing.T, r *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

// U14
func TestOptionsWinOverServiceNameEnv(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "x")
	r := sdkmetric.NewManualReader()
	p, err := setup(t.Context(), Options{ServiceName: "y"}, hooks{readers: []sdkmetric.Reader{r}, stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	v, _ := collect(t, r).Resource.Set().Value("service.name")
	if v.AsString() != "y" {
		t.Fatalf("got %q", v.AsString())
	}
}

// U15
func TestPropagator(t *testing.T) {
	clearOTELEnv(t)
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	fields := strings.Join(otel.GetTextMapPropagator().Fields(), ",")
	if !strings.Contains(fields, "traceparent") || !strings.Contains(fields, "baggage") {
		t.Fatalf("fields: %s", fields)
	}
}

// U16
func TestLogHasTraceFields(t *testing.T) {
	clearOTELEnv(t)
	var out bytes.Buffer
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	ctx, span := otel.Tracer("t").Start(t.Context(), "op")
	slog.InfoContext(ctx, "in span")
	span.End()
	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if rec["trace_id"] != span.SpanContext().TraceID().String() || rec["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("record: %v", rec)
	}
}

// U17
func TestLogLevel(t *testing.T) {
	clearOTELEnv(t)
	var out bytes.Buffer
	p, err := setup(t.Context(), Options{ServiceName: "svc", LogLevel: slog.LevelWarn}, hooks{stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("info message")
	slog.Warn("warn message")
	_ = p.Shutdown(t.Context())
	if strings.Contains(out.String(), "info message") || !strings.Contains(out.String(), "warn message") {
		t.Fatalf("first case: %s", out.String())
	}

	t.Setenv("OTEL_LOG_LEVEL", "debug")
	out.Reset()
	p, err = setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	slog.Debug("debug message")
	slog.Info("info message")
	if strings.Contains(out.String(), "debug message") || !strings.Contains(out.String(), "info message") {
		t.Fatalf("second case: %s", out.String())
	}
}

// U18
func TestShutdownTwice(t *testing.T) {
	clearOTELEnv(t)
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// U32
func TestMissingServiceName(t *testing.T) {
	_, err := Setup(t.Context(), Options{})
	if !errors.Is(err, ErrMissingServiceName) {
		t.Fatalf("got %v", err)
	}
}

// U33
func TestInstanceID(t *testing.T) {
	clearOTELEnv(t)
	id := func() string {
		r := sdkmetric.NewManualReader()
		p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{readers: []sdkmetric.Reader{r}, stdout: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Shutdown(t.Context()) //nolint:errcheck
		v, _ := collect(t, r).Resource.Set().Value("service.instance.id")
		return v.AsString()
	}
	a, b := id(), id()
	if len(a) != 36 || len(b) != 36 || a == b {
		t.Fatalf("ids: %q %q", a, b)
	}
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=abc")
	if c := id(); c != "abc" {
		t.Fatalf("got %q", c)
	}
}

// U34
func TestRuntimeMetrics(t *testing.T) {
	clearOTELEnv(t)
	r := sdkmetric.NewManualReader()
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{readers: []sdkmetric.Reader{r}, stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, sm := range collect(t, r).ScopeMetrics {
			for _, m := range sm.Metrics {
				if strings.HasPrefix(m.Name, "go.goroutine.count") {
					found = true
				}
			}
		}
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime metric go.goroutine.count not found")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type spySpanExporter struct{ shutdowns int }

func (s *spySpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }

func (s *spySpanExporter) Shutdown(context.Context) error {
	s.shutdowns++
	return nil
}

// U45 (finding R7 of the review of PR 5)
func TestSelectExportersShutsDownOnError(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	spy := &spySpanExporter{}
	f := factories{
		spans:   func(context.Context) (sdktrace.SpanExporter, error) { return spy, nil },
		metrics: func(context.Context) (sdkmetric.Reader, error) { return nil, errors.New("metrics failed") },
		logs:    autoFactories.logs,
	}
	e, err := selectWith(t.Context(), f)
	if err == nil || !strings.Contains(err.Error(), "metrics failed") {
		t.Fatalf("got %v", err)
	}
	if e.spans != nil || e.metrics != nil || e.logs != nil {
		t.Fatalf("got exporters: %+v", e)
	}
	if spy.shutdowns != 1 {
		t.Fatalf("the span exporter was shut down %d times, want 1", spy.shutdowns)
	}
}

// U46 (finding R11 of the review of PR 5)
func TestTraceFieldsStayOutsideGroups(t *testing.T) {
	clearOTELEnv(t)
	var out bytes.Buffer
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck
	ctx, span := otel.Tracer("t").Start(t.Context(), "op")
	defer span.End()
	slog.Default().With("a", 1).WithGroup("g").With("b", 2).InfoContext(ctx, "msg", "c", 3)
	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if rec["trace_id"] != span.SpanContext().TraceID().String() || rec["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("trace fields are not at the top level: %v", rec)
	}
	g, ok := rec["g"].(map[string]any)
	if !ok || rec["a"] != float64(1) || g["b"] != float64(2) || g["c"] != float64(3) || len(g) != 2 {
		t.Fatalf("record: %v", rec)
	}
	out.Reset()
	slog.Default().WithGroup("g").InfoContext(ctx, "no attrs")
	rec = nil
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, has := rec["g"]; has {
		t.Fatalf("empty group in the record: %v", rec)
	}
}

// U47 (finding R12 of the review of PR 5)
func TestShutdownResetsGlobals(t *testing.T) {
	clearOTELEnv(t)
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	p, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != trace.TracerProvider(p.tracer) || slog.Default() == before {
		t.Fatal("setup did not set the global state")
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() == trace.TracerProvider(p.tracer) {
		t.Error("the global tracer provider is still the shut down provider")
	}
	if otel.GetMeterProvider() == metric.MeterProvider(p.meter) {
		t.Error("the global meter provider is still the shut down provider")
	}
	if otel.GetLoggerProvider() == olog.LoggerProvider(p.logger) {
		t.Error("the global logger provider is still the shut down provider")
	}
	if f := otel.GetTextMapPropagator().Fields(); len(f) != 0 {
		t.Errorf("propagator fields: %v", f)
	}
	if slog.Default() != before {
		t.Error("the default slog logger is not the logger from before Setup")
	}

	// An old Provider does not reset the state of a newer Provider.
	p1, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := setup(t.Context(), Options{ServiceName: "svc"}, hooks{stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Shutdown(t.Context()) //nolint:errcheck
	_ = p1.Shutdown(t.Context())
	if otel.GetTracerProvider() != trace.TracerProvider(p2.tracer) {
		t.Error("an old Provider reset the global state of a newer Provider")
	}
}
