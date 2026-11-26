package streams_test

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
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/streams"
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

type telemetry struct {
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
	spans  *tracetest.SpanRecorder
	tp     *sdktrace.TracerProvider
	logs   *syncBuffer
}

func newTelemetry(t *testing.T) *telemetry {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()); _ = tp.Shutdown(context.Background()) })
	return &telemetry{reader: reader, mp: mp, spans: spans, tp: tp, logs: &syncBuffer{}}
}

func (tl *telemetry) options(o streams.ConsumerOptions) streams.ConsumerOptions {
	o.MeterProvider = tl.mp
	o.TracerProvider = tl.tp
	o.Logger = slog.New(slog.NewJSONHandler(tl.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return o
}

func (tl *telemetry) counter(t *testing.T, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tl.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				match := true
				for _, a := range attrs {
					if v, ok := dp.Attributes.Value(a.Key); !ok || v != a.Value {
						match = false
					}
				}
				if match {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// once returns a Read function that returns msgs on the first call and waits
// for the end of the context after that.
func once(msgs ...streams.Message) func(context.Context, int, time.Duration) ([]streams.Message, error) {
	var mu sync.Mutex
	done := false
	return func(ctx context.Context, _ int, _ time.Duration) ([]streams.Message, error) {
		mu.Lock()
		first := !done
		done = true
		mu.Unlock()
		if first {
			return msgs, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// runUntil runs the consumer and stops it when cond is true.
func runUntil(t *testing.T, c *streams.Consumer, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
			t.Fatal("the condition did not become true")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func newConsumer(t *testing.T, g streams.Group, h streams.Handler, opts streams.ConsumerOptions) *streams.Consumer {
	t.Helper()
	c, err := streams.NewConsumer(g, h, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const event = `{"specversion":"1.0","id":"1","source":"/identity","type":"identity.user.created","data":{"secret":"SECRET-DATA"}}`

// U16: the options of the consumer.
func TestU16ConsumerOptions(t *testing.T) {
	g := &mock.StreamGroup{}
	h := func(context.Context, streams.Message) error { return nil }
	bad := map[string]streams.ConsumerOptions{
		"Count":         {Count: -1},
		"Block":         {Block: 99 * time.Millisecond},
		"MaxDeliveries": {MaxDeliveries: -1},
		"ClaimIdle":     {ClaimIdle: time.Millisecond},
	}
	for field, o := range bad {
		if _, err := streams.NewConsumer(g, h, o); !errors.Is(err, streams.ErrInvalidConfig) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	for _, o := range []streams.ConsumerOptions{{Count: 1001}, {MaxDeliveries: 1001}, {Block: 61 * time.Second}} {
		if _, err := streams.NewConsumer(g, h, o); !errors.Is(err, streams.ErrInvalidConfig) {
			t.Errorf("%+v: err = %v", o, err)
		}
	}
	if _, err := streams.NewConsumer(g, h, streams.ConsumerOptions{}); err != nil {
		t.Errorf("defaults: %v", err)
	}
}

// U17: the handler result sets the ack.
func TestU17Ack(t *testing.T) {
	tl := newTelemetry(t)
	g := &mock.StreamGroup{ReadFunc: once(
		streams.Message{ID: "1-0", Event: []byte(event), Deliveries: 1},
		streams.Message{ID: "2-0", Event: []byte(event), Deliveries: 1},
	)}
	var mu sync.Mutex
	var handled []string
	h := func(_ context.Context, m streams.Message) error {
		mu.Lock()
		handled = append(handled, m.ID)
		mu.Unlock()
		if m.ID == "2-0" {
			return errors.New("handler failed SECRET-DATA")
		}
		return nil
	}
	c := newConsumer(t, g, h, tl.options(streams.ConsumerOptions{}))
	runUntil(t, c, func() bool { mu.Lock(); defer mu.Unlock(); return len(handled) == 2 })
	if acked := g.Acked(); len(acked) != 1 || acked[0] != "1-0" {
		t.Errorf("acked = %v, want [1-0]", acked)
	}
	if g.LeaveCalls() != 1 {
		t.Errorf("Leave calls = %d, want 1", g.LeaveCalls())
	}
	if n := tl.counter(t, "constellation.events.consumer.handled", attribute.String("outcome", "ok")); n != 1 {
		t.Errorf("ok = %d, want 1", n)
	}
	if n := tl.counter(t, "constellation.events.consumer.handled", attribute.String("outcome", "error")); n != 1 {
		t.Errorf("error = %d, want 1", n)
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

// U18: a message with too many deliveries is dropped.
func TestU18Dropped(t *testing.T) {
	tl := newTelemetry(t)
	g := &mock.StreamGroup{ReadFunc: once(streams.Message{ID: "1-0", Event: []byte(event), Deliveries: 4})}
	called := false
	h := func(context.Context, streams.Message) error { called = true; return nil }
	c := newConsumer(t, g, h, tl.options(streams.ConsumerOptions{MaxDeliveries: 3}))
	runUntil(t, c, func() bool { return len(g.Acked()) == 1 })
	if called {
		t.Error("the handler ran for a dropped message")
	}
	if n := tl.counter(t, "constellation.events.consumer.handled", attribute.String("outcome", "dropped")); n != 1 {
		t.Errorf("dropped = %d, want 1", n)
	}
	logs := tl.logs.String()
	for _, want := range []string{`"level":"ERROR"`, `"stream":"identity"`, `"group":"helpdesk"`, `"id":"1-0"`, `"type":"identity.user.created"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the error log lacks %s: %s", want, logs)
		}
	}
}

// U19: IDs that the trim deleted count as lost.
func TestU19Lost(t *testing.T) {
	tl := newTelemetry(t)
	var mu sync.Mutex
	claims := 0
	g := &mock.StreamGroup{ClaimFunc: func(ctx context.Context, _ time.Duration, _ int) ([]streams.Message, []string, error) {
		mu.Lock()
		defer mu.Unlock()
		claims++
		if claims == 1 {
			return nil, []string{"1-0", "2-0"}, nil
		}
		return nil, nil, nil
	}}
	c := newConsumer(t, g, func(context.Context, streams.Message) error { return nil }, tl.options(streams.ConsumerOptions{Block: 100 * time.Millisecond}))
	runUntil(t, c, func() bool {
		return tl.counter(t, "constellation.events.consumer.lost", attribute.String("stream", "identity"), attribute.String("group", "helpdesk")) == 2
	})
	if !strings.Contains(tl.logs.String(), `"level":"WARN"`) {
		t.Errorf("no warn log record: %s", tl.logs.String())
	}
}

// U20: the span of one message.
func TestU20Span(t *testing.T) {
	tl := newTelemetry(t)
	traceparent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	withTP := `{"type":"identity.user.created","traceparent":"` + traceparent + `"}`
	noTP := `{"type":"identity.user.created"}`
	badTP := `{"type":"identity.user.created","traceparent":"00-zz-b7ad6b7169203331-01"}`
	g := &mock.StreamGroup{ReadFunc: once(
		streams.Message{ID: "1-0", Event: []byte(withTP), Deliveries: 1},
		streams.Message{ID: "2-0", Event: []byte(noTP), Deliveries: 2},
		streams.Message{ID: "3-0", Event: []byte(badTP), Deliveries: 1},
	)}
	var mu sync.Mutex
	handlerSpans := map[string]bool{}
	h := func(ctx context.Context, m streams.Message) error {
		mu.Lock()
		handlerSpans[m.ID] = trace.SpanContextFromContext(ctx).IsValid()
		mu.Unlock()
		return nil
	}
	c := newConsumer(t, g, h, tl.options(streams.ConsumerOptions{}))
	runUntil(t, c, func() bool { return len(g.Acked()) == 3 })
	spans := tl.spans.Ended()
	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(spans))
	}
	for i, s := range spans {
		if s.Name() != "events.consume" || s.SpanKind() != trace.SpanKindConsumer {
			t.Errorf("span %d: %s %v", i, s.Name(), s.SpanKind())
		}
		if s.Parent().IsValid() {
			t.Errorf("span %d is not a root span", i)
		}
		attrs := map[string]attribute.Value{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a.Value
		}
		if attrs["constellation.events.stream"].AsString() != "identity" ||
			attrs["constellation.events.group"].AsString() != "helpdesk" ||
			attrs["constellation.events.type"].AsString() != "identity.user.created" ||
			attrs["outcome"].AsString() != "ok" {
			t.Errorf("span %d attributes = %v", i, attrs)
		}
	}
	if len(spans[0].Links()) != 1 || spans[0].Links()[0].SpanContext.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("span 0 links = %v", spans[0].Links())
	}
	if len(spans[1].Links()) != 0 || len(spans[2].Links()) != 0 {
		t.Error("a message with no valid traceparent must not have a link")
	}
	if spans[1].Attributes()[3].Value.AsInt64() != 2 {
		t.Errorf("deliveries attribute = %v", spans[1].Attributes())
	}
	for id, ok := range handlerSpans {
		if !ok {
			t.Errorf("the handler context of %s has no span", id)
		}
	}
}

// U21: the names of streams and groups.
func TestU21Names(t *testing.T) {
	if got := streams.StreamKey("helpdesk"); got != "events.stream:helpdesk" {
		t.Errorf("StreamKey = %q", got)
	}
	long := strings.Repeat("a", 33)
	for _, c := range []struct {
		app, name, want string
		ok              bool
	}{
		{"helpdesk", "", "helpdesk", true},
		{"helpdesk", "sla", "helpdesk.sla", true},
		{"helpdesk", long, "", false},
		{"helpdesk", strings.Repeat("a", 32), "helpdesk." + strings.Repeat("a", 32), true},
		{"Helpdesk", "", "", false},
		{"helpdesk", "Sla", "", false},
		{"helpdesk", "1sla", "", false},
	} {
		got, err := streams.GroupName(c.app, c.name)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("GroupName(%q, %q) = %q, %v", c.app, c.name, got, err)
		}
		if err != nil && !errors.Is(err, streams.ErrInvalidGroup) {
			t.Errorf("GroupName(%q, %q): err = %v, want ErrInvalidGroup", c.app, c.name, err)
		}
	}
	for _, c := range []struct {
		app, group string
		ok         bool
	}{
		{"helpdesk", "helpdesk", true},
		{"helpdesk", "helpdesk.sla", true},
		{"helpdesk", "identity", false},
		{"helpdesk", "helpdesk.", false},
		{"helpdesk", "helpdesk." + long, false},
		{"helpdesk", "helpdeskx", false},
		{"helpdesk", "helpdesk.Sla", false},
	} {
		if err := streams.ValidGroup(c.app, c.group); (err == nil) != c.ok {
			t.Errorf("ValidGroup(%q, %q) = %v", c.app, c.group, err)
		}
	}
}

// The cleanup runs after CleanupInterval.
func TestConsumerCleanup(t *testing.T) {
	g := &mock.StreamGroup{}
	c := newConsumer(t, g, func(context.Context, streams.Message) error { return nil },
		streams.ConsumerOptions{Block: 100 * time.Millisecond, CleanupInterval: time.Nanosecond})
	runUntil(t, c, func() bool { return g.CleanupCalls() >= 1 })
}
