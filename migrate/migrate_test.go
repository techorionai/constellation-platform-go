package migrate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/migrate"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// blockingLocker waits until the context ends.
func blockingLocker() *mock.Locker {
	return &mock.Locker{LockFunc: func(ctx context.Context) (func(context.Context) error, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

// U1
func TestRunApplies(t *testing.T) {
	l := &mock.Locker{}
	m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) {
		return migrate.Result{FromVersion: 0, ToVersion: 2, Applied: 2}, nil
	}}
	res, err := migrate.Run(context.Background(), l, m, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 || res.ToVersion != 2 {
		t.Fatalf("result = %+v", res)
	}
	if l.UnlockCalls() != 1 {
		t.Fatalf("unlock calls = %d, want 1", l.UnlockCalls())
	}
}

// U2
func TestRunMigratorFails(t *testing.T) {
	boom := errors.New("boom")
	l := &mock.Locker{}
	m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) { return migrate.Result{}, boom }}
	_, err := migrate.Run(context.Background(), l, m, migrate.Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if l.UnlockCalls() != 1 {
		t.Fatalf("unlock calls = %d, want 1", l.UnlockCalls())
	}
}

// U3
func TestRunLockTimeout(t *testing.T) {
	m := &mock.Migrator{}
	_, err := migrate.Run(context.Background(), blockingLocker(), m, migrate.Options{LockTimeout: 50 * time.Millisecond})
	if !errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if m.UpCalls() != 0 {
		t.Fatal("Up was called")
	}
}

// U4
func TestRunLockerFails(t *testing.T) {
	boom := errors.New("boom")
	l := &mock.Locker{LockFunc: func(context.Context) (func(context.Context) error, error) { return nil, boom }}
	m := &mock.Migrator{}
	_, err := migrate.Run(context.Background(), l, m, migrate.Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatal("err is ErrLockTimeout")
	}
	if m.UpCalls() != 0 {
		t.Fatal("Up was called")
	}
}

// U5
func TestRunDirty(t *testing.T) {
	m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) { return migrate.Result{}, migrate.ErrDirty }}
	_, err := migrate.Run(context.Background(), &mock.Locker{}, m, migrate.Options{})
	if !errors.Is(err, migrate.ErrDirty) {
		t.Fatalf("err = %v, want ErrDirty", err)
	}
}

type telemetryProbe struct {
	spans   *tracetest.InMemoryExporter
	metrics *sdkmetric.ManualReader
}

// probe sets the global providers to in-memory ones, and restores them at cleanup.
func probe(t *testing.T) telemetryProbe {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	oldT, oldM := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(oldT)
		otel.SetMeterProvider(oldM)
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return telemetryProbe{spans: exp, metrics: reader}
}

type histPoint struct {
	name    string
	outcome string
}

func (p telemetryProbe) durations(t *testing.T) []histPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := p.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []histPoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "migrate.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("migrate.duration has type %T", m.Data)
			}
			if m.Unit != "s" {
				t.Fatalf("unit = %q, want s", m.Unit)
			}
			for _, dp := range h.DataPoints {
				n, _ := dp.Attributes.Value(attribute.Key("migrate.name"))
				o, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out = append(out, histPoint{n.AsString(), o.AsString()})
			}
		}
	}
	return out
}

// U6
func TestRunTelemetryOutcomes(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		outcome string
		run     func(t *testing.T) error
	}{
		{"ok", func(t *testing.T) error {
			_, err := migrate.Run(context.Background(), &mock.Locker{}, &mock.Migrator{}, migrate.Options{})
			return err
		}},
		{"ahead", func(t *testing.T) error {
			m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) {
				return migrate.Result{FromVersion: 3, ToVersion: 3, Ahead: true}, nil
			}}
			_, err := migrate.Run(context.Background(), &mock.Locker{}, m, migrate.Options{})
			return err
		}},
		{"lock_timeout", func(t *testing.T) error {
			_, err := migrate.Run(context.Background(), blockingLocker(), &mock.Migrator{}, migrate.Options{LockTimeout: 20 * time.Millisecond})
			return err
		}},
		{"dirty", func(t *testing.T) error {
			m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) { return migrate.Result{}, migrate.ErrDirty }}
			_, err := migrate.Run(context.Background(), &mock.Locker{}, m, migrate.Options{})
			return err
		}},
		{"canceled", func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := migrate.Run(ctx, blockingLocker(), &mock.Migrator{}, migrate.Options{})
			return err
		}},
		{"error", func(t *testing.T) error {
			m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) { return migrate.Result{}, boom }}
			_, err := migrate.Run(context.Background(), &mock.Locker{}, m, migrate.Options{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			p := probe(t)
			err := tc.run(t)
			if (tc.outcome == "ok" || tc.outcome == "ahead") != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			spans := p.spans.GetSpans()
			if len(spans) != 1 || spans[0].Name != "migrate.run" {
				t.Fatalf("spans = %v", spans)
			}
			var gotName string
			for _, a := range spans[0].Attributes {
				if a.Key == "migrate.name" {
					gotName = a.Value.AsString()
				}
			}
			if gotName != "schema_migrations" {
				t.Fatalf("span migrate.name = %q", gotName)
			}
			got := p.durations(t)
			if len(got) != 1 || got[0] != (histPoint{"schema_migrations", tc.outcome}) {
				t.Fatalf("metric points = %v, want outcome %s", got, tc.outcome)
			}
		})
	}

	t.Run("two names", func(t *testing.T) {
		p := probe(t)
		for _, name := range []string{"schema_migrations", "platform_secrets_schema_migrations"} {
			if _, err := migrate.Run(context.Background(), &mock.Locker{}, &mock.Migrator{}, migrate.Options{Name: name}); err != nil {
				t.Fatal(err)
			}
		}
		got := p.durations(t)
		if len(got) != 2 || got[0].name == got[1].name {
			t.Fatalf("metric points = %v, want two names", got)
		}
	})
}

// U20
func TestRunLockTimeoutOptions(t *testing.T) {
	l := &mock.Locker{}
	if _, err := migrate.Run(context.Background(), l, &mock.Migrator{}, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	deadline, ok := l.LockContext().Deadline()
	if !ok {
		t.Fatal("the context of Lock has no deadline")
	}
	if d := time.Until(deadline); d < 9*time.Minute || d > 10*time.Minute+time.Second {
		t.Fatalf("deadline in %v, want about 10 minutes", d)
	}

	l2 := &mock.Locker{}
	_, err := migrate.Run(context.Background(), l2, &mock.Migrator{}, migrate.Options{LockTimeout: -time.Second})
	if !errors.Is(err, migrate.ErrInvalidOptions) {
		t.Fatalf("err = %v, want ErrInvalidOptions", err)
	}
	if l2.LockCalls() != 0 {
		t.Fatal("Lock was called")
	}
}

// captureLog sets the default logger to a JSON logger on a buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// U21
func TestRunAheadWarns(t *testing.T) {
	buf := captureLog(t)
	m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) {
		return migrate.Result{FromVersion: 3, ToVersion: 3, Ahead: true}, nil
	}}
	if _, err := migrate.Run(context.Background(), &mock.Locker{}, m, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	var warns int
	for _, r := range records(t, buf) {
		if r["level"] == "WARN" {
			warns++
			if r["from_version"] != float64(3) || r["to_version"] != float64(3) {
				t.Fatalf("warn record = %v", r)
			}
		}
	}
	if warns != 1 {
		t.Fatalf("warn records = %d, want 1", warns)
	}
}

// U22
func TestRunParentCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	m := &mock.Migrator{}
	_, err := migrate.Run(ctx, blockingLocker(), m, migrate.Options{LockTimeout: 10 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatal("err is ErrLockTimeout")
	}
	if m.UpCalls() != 0 {
		t.Fatal("Up was called")
	}
}

// U27
func TestRunInvalidName(t *testing.T) {
	l := &mock.Locker{}
	_, err := migrate.Run(context.Background(), l, &mock.Migrator{}, migrate.Options{Name: "Bad-Name"})
	if !errors.Is(err, migrate.ErrInvalidOptions) {
		t.Fatalf("err = %v, want ErrInvalidOptions", err)
	}
	if l.LockCalls() != 0 {
		t.Fatal("Lock was called")
	}

	buf := captureLog(t)
	if _, err := migrate.Run(context.Background(), &mock.Locker{}, &mock.Migrator{}, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	recs := records(t, buf)
	if len(recs) == 0 {
		t.Fatal("no log record")
	}
	for _, r := range recs {
		if r["migrate.name"] != "schema_migrations" {
			t.Fatalf("record = %v", r)
		}
	}
}
