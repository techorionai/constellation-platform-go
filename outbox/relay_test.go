package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/outbox"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// telemetry holds the in-memory providers and the log buffer of one test.
type telemetry struct {
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
	spans  *tracetest.SpanRecorder
	tp     *sdktrace.TracerProvider
	logs   *syncBuffer
	logger *slog.Logger
}

func newTelemetry(t *testing.T) *telemetry {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	logs := &syncBuffer{}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()); _ = tp.Shutdown(context.Background()) })
	return &telemetry{reader: reader, mp: mp, spans: spans, tp: tp, logs: logs,
		logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
}

func (tl *telemetry) options(o outbox.RelayOptions) outbox.RelayOptions {
	o.MeterProvider = tl.mp
	o.TracerProvider = tl.tp
	o.Logger = tl.logger
	return o
}

// metric returns the collected metric with the name, or false.
func (tl *telemetry) metric(t *testing.T, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tl.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// counter returns the sum of the counter points that have the attribute.
func (tl *telemetry) counter(t *testing.T, name string, attr attribute.KeyValue) int64 {
	t.Helper()
	m, ok := tl.metric(t, name)
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		if v, ok := dp.Attributes.Value(attr.Key); ok && v == attr.Value {
			total += dp.Value
		}
	}
	return total
}

func records(n int, payload string) []outbox.Record {
	recs := make([]outbox.Record, n)
	for i := range recs {
		recs[i] = outbox.Record{ID: int64(i + 1), EventID: "e", Payload: []byte(payload)}
	}
	return recs
}

func newRelay(t *testing.T, store outbox.Store, pub outbox.Publisher, opts outbox.RelayOptions) *outbox.Relay {
	t.Helper()
	r, err := outbox.NewRelay(store, pub, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// U10: the options of the relay.
func TestU10RelayOptions(t *testing.T) {
	store, pub := &mock.OutboxStore{}, &mock.OutboxPublisher{}
	bad := map[string]outbox.RelayOptions{
		"Interval":       {Interval: 99 * time.Millisecond},
		"BatchSize":      {BatchSize: -1},
		"PublishTimeout": {PublishTimeout: 999 * time.Millisecond},
	}
	for field, o := range bad {
		if _, err := outbox.NewRelay(store, pub, o); !errors.Is(err, outbox.ErrInvalidConfig) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	for _, o := range []outbox.RelayOptions{{BatchSize: 501}, {PublishTimeout: 61 * time.Second}} {
		if _, err := outbox.NewRelay(store, pub, o); !errors.Is(err, outbox.ErrInvalidConfig) {
			t.Errorf("%+v: err = %v", o, err)
		}
	}
	if _, err := outbox.NewRelay(store, pub, outbox.RelayOptions{}); err != nil {
		t.Errorf("defaults: %v", err)
	}
	if _, err := outbox.NewRelay(store, pub, outbox.RelayOptions{Interval: 100 * time.Millisecond, BatchSize: 500, PublishTimeout: time.Minute}); err != nil {
		t.Errorf("limits: %v", err)
	}
}

// waits is an After hook that records the wait times.
type waits struct {
	mu     sync.Mutex
	d      []time.Duration
	cancel context.CancelFunc
	// stopAfter cancels the context after this number of waits. 0 means never.
	stopAfter int
	// immediate makes the channel fire at once.
	immediate bool
	called    chan struct{}
}

func (w *waits) after(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	w.d = append(w.d, d)
	n := len(w.d)
	w.mu.Unlock()
	if w.called != nil {
		select {
		case w.called <- struct{}{}:
		default:
		}
	}
	if w.stopAfter > 0 && n >= w.stopAfter {
		w.cancel()
	}
	if w.immediate {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	return nil
}

func (w *waits) durations() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

// U11: a full batch has no wait. An empty batch waits for the interval.
func TestU11FullBatchNoWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &waits{cancel: cancel, stopAfter: 1}
	var calls int
	store := &mock.OutboxStore{ProcessFunc: func(ctx context.Context, _, _ int, fn func(context.Context, []outbox.Record) error) (int, bool, error) {
		calls++
		if calls == 1 {
			return 2, true, fn(ctx, records(2, "{}"))
		}
		return 0, false, nil
	}}
	r := newRelay(t, store, &mock.OutboxPublisher{}, outbox.RelayOptions{Interval: 250 * time.Millisecond, After: w.after})
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if store.ProcessCalls() != 2 {
		t.Errorf("Process calls = %d, want 2", store.ProcessCalls())
	}
	if d := w.durations(); len(d) != 1 || d[0] != 250*time.Millisecond {
		t.Errorf("waits = %v, want [250ms]", d)
	}
}

// U12: a publish error rolls back, backs off, and counts an error.
func TestU12PublishError(t *testing.T) {
	tl := newTelemetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &waits{cancel: cancel, stopAfter: 3, immediate: true}
	store := &mock.OutboxStore{ProcessFunc: func(ctx context.Context, _, _ int, fn func(context.Context, []outbox.Record) error) (int, bool, error) {
		return 0, false, fn(ctx, records(2, `{"secret":"SECRET-DATA"}`))
	}}
	pub := &mock.OutboxPublisher{PublishFunc: func(context.Context, [][]byte) error {
		return errors.New("valkey down SECRET-DATA")
	}}
	r := newRelay(t, store, pub, tl.options(outbox.RelayOptions{After: w.after}))
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	d := w.durations()
	if len(d) != 3 {
		t.Fatalf("waits = %v, want 3", d)
	}
	for i, max := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if d[i] < max/2 || d[i] > max {
			t.Errorf("wait %d = %v, want from %v to %v", i, d[i], max/2, max)
		}
	}
	if n := tl.counter(t, "constellation.outbox.published", attribute.String("outcome", "error")); n != 6 {
		t.Errorf("error count = %d, want 6", n)
	}
	// U23: the logs and the spans do not have the event data or the error text.
	if strings.Contains(tl.logs.String(), "SECRET-DATA") {
		t.Errorf("a log record has the event data: %s", tl.logs.String())
	}
	for _, s := range tl.spans.Ended() {
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.String(), "SECRET-DATA") {
				t.Errorf("span attribute %s has the event data", a.Key)
			}
		}
	}
}

// U13: Notify during a wait starts the next tick at once.
func TestU13Notify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &waits{called: make(chan struct{}, 1)}
	store := &mock.OutboxStore{}
	store.ProcessFunc = func(context.Context, int, int, func(context.Context, []outbox.Record) error) (int, bool, error) {
		if store.ProcessCalls() >= 2 {
			cancel()
		}
		return 0, false, nil
	}
	r := newRelay(t, store, &mock.OutboxPublisher{}, outbox.RelayOptions{After: w.after})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-w.called:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not wait")
	}
	r.Notify()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify did not wake the relay")
	}
	if store.ProcessCalls() != 2 {
		t.Errorf("Process calls = %d, want 2", store.ProcessCalls())
	}
}

// U14: the publish context ends at PublishTimeout.
func TestU14PublishTimeout(t *testing.T) {
	tl := newTelemetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &waits{cancel: cancel, stopAfter: 1, immediate: true}
	var deadline time.Duration
	store := &mock.OutboxStore{ProcessFunc: func(ctx context.Context, _, _ int, fn func(context.Context, []outbox.Record) error) (int, bool, error) {
		return 0, false, fn(ctx, records(1, "{}"))
	}}
	pub := &mock.OutboxPublisher{PublishFunc: func(ctx context.Context, _ [][]byte) error {
		dl, ok := ctx.Deadline()
		if !ok {
			return errors.New("no deadline")
		}
		deadline = time.Until(dl)
		return context.DeadlineExceeded
	}}
	r := newRelay(t, store, pub, tl.options(outbox.RelayOptions{PublishTimeout: 2 * time.Second, After: w.after}))
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if deadline <= time.Second || deadline > 2*time.Second {
		t.Errorf("time to the deadline = %v, want about 2s", deadline)
	}
	if n := tl.counter(t, "constellation.outbox.published", attribute.String("outcome", "error")); n != 1 {
		t.Errorf("error count = %d, want 1", n)
	}
}

// U15: the relay span and the lag gauge.
func TestU15SpanAndLag(t *testing.T) {
	tl := newTelemetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &waits{cancel: cancel, stopAfter: 2, immediate: true}
	var calls int
	var lagMode int
	store := &mock.OutboxStore{
		ProcessFunc: func(ctx context.Context, _, _ int, fn func(context.Context, []outbox.Record) error) (int, bool, error) {
			calls++
			if calls == 1 {
				return 3, false, fn(ctx, records(3, "{}"))
			}
			return 0, false, nil
		},
		LagFunc: func(ctx context.Context) (time.Duration, error) {
			switch lagMode {
			case 0:
				return 90 * time.Second, nil
			case 1:
				return 0, errors.New("db down")
			default:
				<-ctx.Done()
				return 0, ctx.Err()
			}
		},
	}
	r := newRelay(t, store, &mock.OutboxPublisher{}, tl.options(outbox.RelayOptions{After: w.after, LagTimeout: 50 * time.Millisecond}))
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	spans := tl.spans.Ended()
	if len(spans) != 1 || spans[0].Name() != "outbox.relay.publish" {
		t.Fatalf("spans = %d, want 1 outbox.relay.publish", len(spans))
	}
	attrs := map[string]attribute.Value{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value
	}
	if attrs["constellation.outbox.events"].AsInt64() != 3 || attrs["outcome"].AsString() != "ok" {
		t.Errorf("attributes = %v", attrs)
	}
	if n := tl.counter(t, "constellation.outbox.published", attribute.String("outcome", "ok")); n != 3 {
		t.Errorf("ok count = %d, want 3", n)
	}

	lagValue := func() (float64, bool) {
		m, ok := tl.metric(t, "constellation.outbox.lag")
		if !ok {
			return 0, false
		}
		dps := m.Data.(metricdata.Gauge[float64]).DataPoints
		if len(dps) == 0 {
			return 0, false
		}
		return dps[0].Value, true
	}
	if v, ok := lagValue(); !ok || v != 90 {
		t.Errorf("lag = %v, %v, want 90", v, ok)
	}
	for _, mode := range []int{1, 2} {
		lagMode = mode
		before := strings.Count(tl.logs.String(), `"level":"WARN"`)
		if v, ok := lagValue(); ok && v != 90 {
			t.Errorf("mode %d: lag = %v, want no new value", mode, v)
		}
		if strings.Count(tl.logs.String(), `"level":"WARN"`) != before+1 {
			t.Errorf("mode %d: no warn log record", mode)
		}
	}
}
