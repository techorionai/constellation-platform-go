package decorators_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/decorators"
	"github.com/techorionai/constellation-platform-go/executor"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

type params struct {
	Name     string
	Password string
}

var (
	errX          = errors.New("x failed")
	errValidation = errors.New("validation")
)

type rig struct {
	exp    *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
	opts   []decorators.Option
}

func newRig() *rig {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return &rig{exp: exp, reader: reader, opts: []decorators.Option{
		decorators.WithTracerProvider(tp), decorators.WithMeterProvider(mp),
	}}
}

func (r *rig) durations(t *testing.T) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "executor.duration" {
				return m.Data.(metricdata.Histogram[float64]).DataPoints
			}
		}
	}
	return nil
}

func outcomeOf(dp metricdata.HistogramDataPoint[float64]) string {
	v, _ := dp.Attributes.Value(attribute.Key("outcome"))
	return v.AsString()
}

func ok(_ context.Context, _ params) (string, error)   { return "r", nil }
func fail(_ context.Context, _ params) (string, error) { return "", errX }

// U2
func TestTracingSuccess(t *testing.T) {
	r := newRig()
	e := decorators.ApplyTracing[params, string](executor.FuncWithReturn[params, string](ok), "CreateOrder", r.opts...)
	res, err := e.Execute(t.Context(), params{})
	if res != "r" || err != nil {
		t.Fatalf("got %q, %v", res, err)
	}
	spans := r.exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "executor:CreateOrder" {
		t.Fatalf("spans: %+v", spans)
	}
	found := false
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "executor.name" && a.Value.AsString() == "CreateOrder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attributes: %+v", spans[0].Attributes)
	}
	dps := r.durations(t)
	if len(dps) != 1 || outcomeOf(dps[0]) != "ok" || dps[0].Count != 1 {
		t.Fatalf("durations: %+v", dps)
	}
}

// U3
func TestTracingError(t *testing.T) {
	r := newRig()
	e := decorators.ApplyTracing[params, string](executor.FuncWithReturn[params, string](fail), "CreateOrder", r.opts...)
	_, err := e.Execute(t.Context(), params{})
	if !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	s := r.exp.GetSpans()[0]
	if s.Status.Code != codes.Error {
		t.Fatalf("status = %v", s.Status)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "exception" {
		t.Fatalf("events: %+v", s.Events)
	}
	if dps := r.durations(t); len(dps) != 1 || outcomeOf(dps[0]) != "error" {
		t.Fatalf("durations: %+v", dps)
	}
}

func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// U4
func TestLoggingSuccessHasNoParams(t *testing.T) {
	l, buf := bufLogger()
	e := decorators.ApplyLogging[params, string](executor.FuncWithReturn[params, string](ok), "CreateOrder",
		decorators.WithLogger(l))
	if _, err := e.Execute(t.Context(), params{Name: "n", Password: "p-secret"}); err != nil {
		t.Fatal(err)
	}
	recs := records(t, buf)
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" || recs[0]["executor.name"] != "CreateOrder" {
		t.Fatalf("records: %s", buf)
	}
	if _, ok := recs[0]["duration_ms"]; !ok {
		t.Fatalf("no duration_ms: %s", buf)
	}
	if strings.Contains(buf.String(), "p-secret") {
		t.Fatalf("log has params: %s", buf)
	}
}

// U5
func TestLoggingError(t *testing.T) {
	l, buf := bufLogger()
	e := decorators.ApplyLogging[params, string](executor.FuncWithReturn[params, string](fail), "CreateOrder",
		decorators.WithLogger(l))
	if _, err := e.Execute(t.Context(), params{}); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	recs := records(t, buf)
	if len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["error"] != "x failed" {
		t.Fatalf("records: %s", buf)
	}

	l2, buf2 := bufLogger()
	e = decorators.ApplyLogging[params, string](executor.FuncWithReturn[params, string](fail), "CreateOrder",
		decorators.WithLogger(l2), decorators.WithErrorLevel(func(error) slog.Level { return slog.LevelInfo }))
	_, _ = e.Execute(t.Context(), params{})
	recs = records(t, buf2)
	if len(recs) != 1 || recs[0]["level"] != "INFO" {
		t.Fatalf("records: %s", buf2)
	}
}

// U6
func TestLogHasTraceIDOfExecutorSpan(t *testing.T) {
	// telemetry.Setup writes the JSON log to os.Stdout at the time of the call.
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = wr
	for _, k := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
		t.Setenv(k, "none")
	}
	p, err := telemetry.Setup(t.Context(), telemetry.Options{ServiceName: "svc", LogLevel: slog.LevelDebug})
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context()) //nolint:errcheck

	var inner executor.ExecutorWithReturn[params, string] = executor.FuncWithReturn[params, string](ok)
	inner = decorators.ApplyLogging(inner, "CreateOrder")
	var seen trace.SpanContext
	spy := executor.FuncWithReturn[params, string](func(ctx context.Context, p params) (string, error) {
		seen = trace.SpanContextFromContext(ctx)
		return inner.Execute(ctx, p)
	})
	e := decorators.ApplyTracing[params, string](spy, "CreateOrder")
	if _, err := e.Execute(t.Context(), params{}); err != nil {
		t.Fatal(err)
	}
	_ = wr.Close()
	data, _ := io.ReadAll(rd)
	recs := records(t, bytes.NewBuffer(data))
	if len(recs) != 1 || !seen.IsValid() || recs[0]["trace_id"] != seen.TraceID().String() ||
		recs[0]["span_id"] != seen.SpanID().String() {
		t.Fatalf("seen=%v records: %s", seen, data)
	}
}

// U7
func TestNoResultForms(t *testing.T) {
	r := newRig()
	l, buf := bufLogger()
	opts := append([]decorators.Option{decorators.WithLogger(l)}, r.opts...)

	good := executor.Func[params](func(context.Context, params) error { return nil })
	bad := executor.Func[params](func(context.Context, params) error { return errX })

	e := decorators.ApplyTracingNoResult[params](decorators.ApplyLoggingNoResult[params](good, "Cmd", opts...), "Cmd", opts...)
	if err := e.Execute(t.Context(), params{Password: "p-secret"}); err != nil {
		t.Fatal(err)
	}
	e = decorators.ApplyTracingNoResult[params](decorators.ApplyLoggingNoResult[params](bad, "Cmd", opts...), "Cmd", opts...)
	if err := e.Execute(t.Context(), params{}); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	spans := r.exp.GetSpans()
	if len(spans) != 2 || spans[0].Status.Code == codes.Error || spans[1].Status.Code != codes.Error {
		t.Fatalf("spans: %+v", spans)
	}
	got := map[string]uint64{}
	for _, dp := range r.durations(t) {
		got[outcomeOf(dp)] = dp.Count
	}
	if got["ok"] != 1 || got["error"] != 1 {
		t.Fatalf("durations: %v", got)
	}
	recs := records(t, buf)
	if len(recs) != 2 || recs[0]["level"] != "DEBUG" || recs[1]["level"] != "ERROR" || strings.Contains(buf.String(), "p-secret") {
		t.Fatalf("records: %s", buf)
	}
}

// U8
func TestInvalidNamePanics(t *testing.T) {
	base := executor.FuncWithReturn[params, string](ok)
	for _, name := range []string{"1bad", "", strings.Repeat("a", 65)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("tracing name %q: want panic", name)
				}
			}()
			decorators.ApplyTracing[params, string](base, name)
		}()
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("logging name %q: want panic", name)
				}
			}()
			decorators.ApplyLogging[params, string](base, name)
		}()
	}
	decorators.ApplyTracing[params, string](base, strings.Repeat("a", 64))
}

// U33 (executor.duration)
func TestDurationBoundaries(t *testing.T) {
	r := newRig()
	e := decorators.ApplyTracing[params, string](executor.FuncWithReturn[params, string](ok), "X", r.opts...)
	_, _ = e.Execute(t.Context(), params{})
	dps := r.durations(t)
	want := []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	if len(dps) != 1 || len(dps[0].Bounds) != len(want) {
		t.Fatalf("bounds: %+v", dps)
	}
	for i := range want {
		if dps[0].Bounds[i] != want[i] {
			t.Fatalf("bounds = %v, want %v", dps[0].Bounds, want)
		}
	}
}

// U36
func TestErrorClassifier(t *testing.T) {
	r := newRig()
	opts := append([]decorators.Option{decorators.WithErrorClassifier(func(err error) bool {
		return !errors.Is(err, errValidation)
	})}, r.opts...)
	call := func(err error) error {
		e := decorators.ApplyTracing[params, string](executor.FuncWithReturn[params, string](
			func(context.Context, params) (string, error) { return "", err }), "Cmd", opts...)
		_, got := e.Execute(t.Context(), params{})
		return got
	}
	if err := call(errValidation); !errors.Is(err, errValidation) {
		t.Fatalf("err = %v", err)
	}
	if err := call(errX); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	spans := r.exp.GetSpans()
	if spans[0].Status.Code != codes.Unset || len(spans[0].Events) != 1 {
		t.Fatalf("first span: %+v", spans[0])
	}
	if spans[1].Status.Code != codes.Error {
		t.Fatalf("second span: %+v", spans[1])
	}
	got := map[string]uint64{}
	for _, dp := range r.durations(t) {
		got[outcomeOf(dp)] = dp.Count
	}
	if got["expected_error"] != 1 || got["error"] != 1 {
		t.Fatalf("outcomes: %v", got)
	}
}
