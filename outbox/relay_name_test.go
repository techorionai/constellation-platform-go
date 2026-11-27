package outbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/outbox"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// U10 (audit outbox): two relays with different names report separate series.
func TestU10RelayName(t *testing.T) {
	tl := newTelemetry(t)
	run := func(name string, lag time.Duration, events int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := &waits{cancel: cancel, stopAfter: 1, immediate: true}
		var calls int
		store := &mock.OutboxStore{
			ProcessFunc: func(ctx context.Context, _, _ int, fn func(context.Context, []outbox.Record) error) (int, bool, error) {
				calls++
				if calls == 1 {
					return events, false, fn(ctx, records(events, "{}"))
				}
				return 0, false, nil
			},
			LagFunc: func(context.Context) (time.Duration, error) { return lag, nil },
		}
		r := newRelay(t, store, &mock.OutboxPublisher{}, tl.options(outbox.RelayOptions{Name: name, After: w.after}))
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run("events", 10*time.Second, 2)
	run("audit", 20*time.Second, 3)

	for name, want := range map[string]int64{"events": 2, "audit": 3} {
		got := tl.counter(t, "constellation.outbox.published", attribute.String("outbox", name))
		if got != want {
			t.Errorf("published for %s = %d, want %d", name, got, want)
		}
	}
	m, ok := tl.metric(t, "constellation.outbox.lag")
	if !ok {
		t.Fatal("no lag metric")
	}
	seen := map[string]float64{}
	for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
		v, _ := dp.Attributes.Value("outbox")
		seen[v.AsString()] = dp.Value
	}
	if seen["events"] != 10 || seen["audit"] != 20 {
		t.Errorf("lag series = %v, want events=10 audit=20", seen)
	}
}

func TestU10RelayNameInvalid(t *testing.T) {
	store, pub := &mock.OutboxStore{}, &mock.OutboxPublisher{}
	for _, name := range []string{"Bad", "a b", "1a", strings.Repeat("a", 33)} {
		_, err := outbox.NewRelay(store, pub, outbox.RelayOptions{Name: name})
		if !errors.Is(err, outbox.ErrInvalidConfig) || !strings.Contains(err.Error(), "Name") {
			t.Errorf("Name %q: err = %v", name, err)
		}
	}
}
