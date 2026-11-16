package cache

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/techorionai/constellation-platform-go/cache"

var (
	durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	sizeBuckets     = []float64{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576}
)

// metrics holds the instruments of one namespace. A failed instrument creation
// gives a no-op instrument in the SDK, so the fields are never nil.
type metrics struct {
	namespace  attribute.KeyValue
	requests   metric.Int64Counter
	duration   metric.Float64Histogram
	size       metric.Int64Histogram
	loadDur    metric.Float64Histogram
	loadShared metric.Int64Counter
	decodeErrs metric.Int64Counter
}

func newMetrics(namespace string) *metrics {
	m := otel.GetMeterProvider().Meter(scopeName)
	req, _ := m.Int64Counter("cache.requests", metric.WithUnit("{request}"),
		metric.WithDescription("Number of store calls, by operation and result."))
	dur, _ := m.Float64Histogram("cache.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of the store calls."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	size, _ := m.Int64Histogram("cache.value.size", metric.WithUnit("By"),
		metric.WithDescription("Size of the encoded value on a hit and on a set."),
		metric.WithExplicitBucketBoundaries(sizeBuckets...))
	loadDur, _ := m.Float64Histogram("cache.load.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of the load function of GetOrLoad."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	shared, _ := m.Int64Counter("cache.load.shared", metric.WithUnit("{call}"),
		metric.WithDescription("Number of GetOrLoad calls that used the load result of another call."))
	decode, _ := m.Int64Counter("cache.decode.errors", metric.WithUnit("{error}"),
		metric.WithDescription("Number of stored values that do not decode."))
	return &metrics{
		namespace: attribute.String("cache.namespace", namespace),
		requests:  req, duration: dur, size: size, loadDur: loadDur, loadShared: shared, decodeErrs: decode,
	}
}

// record records one store call. The size is negative when there is none.
func (m *metrics) record(ctx context.Context, op, result string, start time.Time, size int) {
	ctx = context.WithoutCancel(ctx)
	attrs := metric.WithAttributes(m.namespace, attribute.String("cache.operation", op), attribute.String("result", result))
	m.requests.Add(ctx, 1, attrs)
	m.duration.Record(ctx, time.Since(start).Seconds(), attrs)
	if size >= 0 {
		m.size.Record(ctx, int64(size), metric.WithAttributes(m.namespace, attribute.String("cache.operation", op)))
	}
}

// Instrument returns a Store that records cache.requests, cache.duration, and
// cache.value.size for the calls to store. Use it for code that uses a Store
// without [Cache]. [Cache] records the same metrics itself, so do not give an
// instrumented Store to [New]. It panics if namespace does not match
// ^[a-z][a-z0-9_]{0,31}$.
func Instrument(store Store, namespace string) Store {
	checkNamespace(namespace)
	return &instrumented{store: store, m: newMetrics(namespace)}
}

type instrumented struct {
	store Store
	m     *metrics
}

func (i *instrumented) Get(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	v, err := i.store.Get(ctx, key)
	switch {
	case err == nil:
		i.m.record(ctx, "get", "hit", start, len(v))
	case isMiss(err):
		i.m.record(ctx, "get", "miss", start, -1)
	default:
		i.m.record(ctx, "get", "error", start, -1)
	}
	return v, err
}

func (i *instrumented) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	start := time.Now()
	err := i.store.Set(ctx, key, value, ttl)
	if err != nil {
		i.m.record(ctx, "set", "error", start, -1)
		return err
	}
	i.m.record(ctx, "set", "ok", start, len(value))
	return nil
}

func (i *instrumented) Delete(ctx context.Context, keys ...string) error {
	start := time.Now()
	err := i.store.Delete(ctx, keys...)
	if err != nil {
		i.m.record(ctx, "delete", "error", start, -1)
		return err
	}
	i.m.record(ctx, "delete", "ok", start, -1)
	return nil
}
