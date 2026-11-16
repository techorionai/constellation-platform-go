package cache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/cache"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

type user struct {
	Name string `json:"name"`
}

type point struct {
	attrs  string
	count  int64
	sum    float64
	bounds []float64
}

type obs struct {
	reader *sdkmetric.ManualReader
	exp    *tracetest.InMemoryExporter
	logs   *bytes.Buffer
}

// setup sets the global providers and the default logger for one test.
func setup(t *testing.T) *obs {
	t.Helper()
	oldMP, oldTP, oldLog := otel.GetMeterProvider(), otel.GetTracerProvider(), slog.Default()
	o := &obs{reader: sdkmetric.NewManualReader(), exp: tracetest.NewInMemoryExporter(), logs: &bytes.Buffer{}}
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(o.reader)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(o.exp)))
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: o.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		otel.SetMeterProvider(oldMP)
		otel.SetTracerProvider(oldTP)
		slog.SetDefault(oldLog)
	})
	return o
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (o *obs) metrics(t *testing.T) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := o.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{attrs: attrString(dp.Attributes), count: dp.Value})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{attrs: attrString(dp.Attributes), count: int64(dp.Count), sum: dp.Sum, bounds: dp.Bounds}) //nolint:gosec // test data is small
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{attrs: attrString(dp.Attributes), count: int64(dp.Count), sum: float64(dp.Sum), bounds: dp.Bounds}) //nolint:gosec // test data is small
				}
			}
		}
	}
	return out
}

func attrString(s attribute.Set) string {
	var parts []string
	for _, kv := range s.ToSlice() {
		parts = append(parts, string(kv.Key)+"="+kv.Value.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (o *obs) logText() string { return o.logs.String() }

func total(pts []point) (n int64) {
	for _, p := range pts {
		n += p.count
	}
	return n
}

func find(pts []point, attrs string) int64 {
	for _, p := range pts {
		if p.attrs == attrs {
			return p.count
		}
	}
	return 0
}

const ttl = time.Minute

// U9
func TestSetGet(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	if err := c.Set(t.Context(), "k", user{"Ada"}, ttl); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), "k")
	if err != nil || got.Name != "Ada" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, ok := store.Value("ns:k"); !ok {
		t.Fatal("the store has no key ns:k")
	}
}

// U10
func TestGetMiss(t *testing.T) {
	setup(t)
	c := cache.New[user](&mock.CacheStore{}, "ns")
	if _, err := c.Get(t.Context(), "none"); !errors.Is(err, cache.ErrMiss) {
		t.Fatalf("err = %v", err)
	}
}

// U11
func TestGetOrLoadLoadsOnce(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	var loads int
	load := func(context.Context) (user, error) { loads++; return user{"Ada"}, nil }
	for range 2 {
		v, err := c.GetOrLoad(t.Context(), "k", ttl, load)
		if err != nil || v.Name != "Ada" {
			t.Fatalf("got %+v, %v", v, err)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d", loads)
	}
	if store.TTL("ns:k") != ttl {
		t.Fatalf("ttl = %v", store.TTL("ns:k"))
	}
}

// blocked is a set of GetOrLoad calls at the same time on one missing key.
type blocked struct {
	all     chan struct{} // closed when all callers wait in the group
	results []error
	values  []user
	wg      sync.WaitGroup
}

// startBlockedLoads starts n GetOrLoad calls on the key "k". The load function
// blocks until all n callers wait in the group, and then until hold is closed
// (if hold is not nil).
func startBlockedLoads(t *testing.T, c *cache.Cache[user], n int, hold chan struct{}, ctxs []context.Context,
	loads *atomic.Int32) *blocked {
	t.Helper()
	b := &blocked{all: make(chan struct{}), results: make([]error, n), values: make([]user, n)}
	var waiting atomic.Int32
	cache.SetWaitHook(c, func() {
		if int(waiting.Add(1)) == n {
			close(b.all)
		}
	})
	for i := range n {
		ctx := t.Context()
		if ctxs != nil {
			ctx = ctxs[i]
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.values[i], b.results[i] = c.GetOrLoad(ctx, "k", ttl, func(context.Context) (user, error) {
				loads.Add(1)
				<-b.all
				if hold != nil {
					<-hold
				}
				return user{"Ada"}, nil
			})
		}()
	}
	return b
}

// wait waits for all calls to return.
func (b *blocked) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout while the calls wait")
	}
}

// U12, U25
func TestGetOrLoadSharesOneLoad(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	var loads atomic.Int32
	b := startBlockedLoads(t, c, 10, nil, nil, &loads)
	b.wait(t)
	if loads.Load() != 1 {
		t.Fatalf("loads = %d", loads.Load())
	}
	for i := range b.results {
		if b.results[i] != nil || b.values[i].Name != "Ada" {
			t.Fatalf("call %d: %+v, %v", i, b.values[i], b.results[i])
		}
	}
	if store.SetCalls() != 1 {
		t.Fatalf("set calls = %d", store.SetCalls())
	}
	m := o.metrics(t)
	if total(m["cache.load.shared"]) != 9 {
		t.Fatalf("cache.load.shared = %v", m["cache.load.shared"])
	}
	if total(m["cache.load.duration"]) != 1 {
		t.Fatalf("cache.load.duration = %v", m["cache.load.duration"])
	}
	if find(m["cache.requests"], "cache.namespace=ns,cache.operation=set,result=ok") != 1 {
		t.Fatalf("cache.requests = %v", m["cache.requests"])
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// U13
func TestGetOrLoadLoadError(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	errY := errors.New("y")
	_, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) { return user{}, errY })
	if !errors.Is(err, errY) {
		t.Fatalf("err = %v", err)
	}
	if store.SetCalls() != 0 {
		t.Fatalf("set calls = %d", store.SetCalls())
	}
}

// R1: a panic in load must not stop the process. All callers get ErrLoadPanic.
func TestGetOrLoadPanicInLoad(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	_, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) {
		panic("secret-panic-value")
	})
	if !errors.Is(err, cache.ErrLoadPanic) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret-panic-value") {
		t.Fatalf("the error has the panic value: %v", err)
	}
	if store.SetCalls() != 0 {
		t.Fatalf("set calls = %d", store.SetCalls())
	}
	if strings.Contains(o.logText(), "secret-panic-value") || !strings.Contains(o.logText(), "level=ERROR") {
		t.Fatalf("log: %s", o.logText())
	}
	// The cache still works after the panic.
	v, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) { return user{"Ada"}, nil })
	if err != nil || v.Name != "Ada" {
		t.Fatalf("got %+v, %v", v, err)
	}
}

// R1: callers that wait for the same load all get ErrLoadPanic.
func TestGetOrLoadPanicSharedByCallers(t *testing.T) {
	setup(t)
	c := cache.New[user](&mock.CacheStore{}, "ns")
	release := make(chan struct{})
	var started atomic.Int32
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) {
				started.Add(1)
				<-release
				panic("boom")
			})
		}()
	}
	waitFor(t, func() bool { return started.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, cache.ErrLoadPanic) {
			t.Fatalf("call %d: err = %v", i, err)
		}
	}
}

// R16: the spans of Set and Delete have the attribute cache.hit.
func TestSpansHaveCacheHit(t *testing.T) {
	o := setup(t)
	c := cache.New[user](&mock.CacheStore{}, "ns")
	_ = c.Set(t.Context(), "k", user{"Ada"}, ttl)
	_ = c.Delete(t.Context(), "k")
	_, _ = c.Get(t.Context(), "k")
	found := map[string]bool{}
	for _, s := range o.exp.GetSpans() {
		for _, a := range s.Attributes {
			if a.Key == "cache.hit" {
				found[s.Name] = a.Value.AsBool()
			}
		}
	}
	for _, name := range []string{"cache.Set", "cache.Delete", "cache.Get"} {
		if hit, ok := found[name]; !ok || hit {
			t.Fatalf("span %s: cache.hit present = %v, value = %v", name, ok, hit)
		}
	}
}

// U14
func TestGetOrLoadStoreGetError(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{GetFunc: func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }}
	c := cache.New[user](store, "ns")
	var loads int
	v, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) { loads++; return user{"Ada"}, nil })
	if err != nil || v.Name != "Ada" || loads != 1 {
		t.Fatalf("got %+v, %v, loads %d", v, err, loads)
	}
	if n := strings.Count(o.logText(), "level=WARN"); n != 1 {
		t.Fatalf("warn records = %d: %s", n, o.logText())
	}
}

// U15
func TestGetOrLoadStoreSetError(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{SetFunc: func(context.Context, string, []byte, time.Duration) error { return errors.New("boom") }}
	c := cache.New[user](store, "ns")
	v, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) { return user{"Ada"}, nil })
	if err != nil || v.Name != "Ada" {
		t.Fatalf("got %+v, %v", v, err)
	}
	if n := strings.Count(o.logText(), "level=WARN"); n != 1 {
		t.Fatalf("warn records = %d: %s", n, o.logText())
	}
}

// U16, U26
func TestGetOrLoadBadBytes(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{}
	store.Put("ns:k", []byte("not json"))
	c := cache.New[user](store, "ns")
	var loads int
	v, err := c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (user, error) { loads++; return user{"Ada"}, nil })
	if err != nil || v.Name != "Ada" || loads != 1 {
		t.Fatalf("got %+v, %v, loads %d", v, err, loads)
	}
	if b, _ := store.Value("ns:k"); string(b) != `{"name":"Ada"}` {
		t.Fatalf("stored = %s", b)
	}
	if n := total(o.metrics(t)["cache.decode.errors"]); n != 1 {
		t.Fatalf("cache.decode.errors = %d", n)
	}
}

// U17
func TestSetLimits(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[string](store, "ns")
	ctx := t.Context()
	for _, d := range []time.Duration{0, -time.Second, 500 * time.Microsecond, 31 * 24 * time.Hour} {
		if err := c.Set(ctx, "k", "v", d); !errors.Is(err, cache.ErrInvalidTTL) {
			t.Fatalf("ttl %v: err = %v", d, err)
		}
	}
	// "ns:" has 3 bytes, so a key of 254 bytes gives a store key of 257 bytes.
	if err := c.Set(ctx, strings.Repeat("a", 254), "v", ttl); !errors.Is(err, cache.ErrInvalidKey) {
		t.Fatalf("long key: err = %v", err)
	}
	if err := c.Set(ctx, "", "v", ttl); !errors.Is(err, cache.ErrInvalidKey) {
		t.Fatalf("empty key: err = %v", err)
	}
	// Value of 1 MiB plus 1 byte. The JSON codec adds 2 quote bytes.
	big := cache.New[string](store, "ns")
	if err := big.Set(ctx, "k", strings.Repeat("a", cache.MaxValueSize-1), ttl); !errors.Is(err, cache.ErrTooLarge) {
		t.Fatalf("big value: err = %v", err)
	}
	for _, d := range []time.Duration{time.Millisecond, 30 * 24 * time.Hour} {
		if err := c.Set(ctx, "k", "v", d); err != nil {
			t.Fatalf("limit ttl %v: err = %v", d, err)
		}
	}
	if err := c.Set(ctx, strings.Repeat("a", 253), "v", ttl); err != nil {
		t.Fatalf("limit key: err = %v", err)
	}
	if err := big.Set(ctx, "k", strings.Repeat("a", cache.MaxValueSize-2), ttl); err != nil {
		t.Fatalf("limit value: err = %v", err)
	}
	// The store itself refuses an empty key and a key of 257 bytes.
	if err := store.Set(ctx, "", []byte("v"), ttl); !errors.Is(err, cache.ErrInvalidKey) {
		t.Fatalf("empty store key: err = %v", err)
	}
}

// U18
func TestNoKeyOrValueInTelemetry(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{GetFunc: func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }}
	store.SetFunc = func(context.Context, string, []byte, time.Duration) error { return errors.New("boom") }
	c := cache.New[user](store, "ns")
	_, _ = c.GetOrLoad(t.Context(), "user:alice@example.com", ttl, func(context.Context) (user, error) {
		return user{"Alice Secret Name"}, nil
	})
	_ = c.Delete(t.Context(), "user:alice@example.com")
	for _, s := range o.exp.GetSpans() {
		for _, a := range s.Attributes {
			if v := a.Value.String(); strings.Contains(v, "alice") || strings.Contains(v, "Alice") {
				t.Fatalf("span %s attribute %s=%s", s.Name, a.Key, v)
			}
		}
		for _, e := range s.Events {
			t.Fatalf("span %s has event %s", s.Name, e.Name)
		}
		if strings.Contains(s.Status.Description, "alice") {
			t.Fatalf("span status: %s", s.Status.Description)
		}
	}
	if strings.Contains(o.logText(), "alice") || strings.Contains(o.logText(), "Alice") {
		t.Fatalf("log: %s", o.logText())
	}
	for name, pts := range o.metrics(t) {
		for _, p := range pts {
			if strings.Contains(p.attrs, "alice") || strings.Contains(p.attrs, "Alice") {
				t.Fatalf("metric %s attrs %s", name, p.attrs)
			}
		}
	}
	names := map[string]bool{}
	for _, s := range o.exp.GetSpans() {
		names[s.Name] = true
	}
	if !names["cache.GetOrLoad"] || !names["cache.Delete"] {
		t.Fatalf("spans: %v", names)
	}
}

// U23
func TestMetrics(t *testing.T) {
	o := setup(t)
	getErr := false
	store := &mock.CacheStore{}
	store.GetFunc = nil
	c := cache.New[string](store, "ns")
	ctx := t.Context()
	_, _ = c.Get(ctx, "k")                // miss
	_ = c.Set(ctx, "k", "123456789", ttl) // 9 chars plus 2 quotes = 11 bytes
	_, _ = c.Get(ctx, "k")                // hit
	_ = c.Delete(ctx, "k")
	store.GetFunc = func(context.Context, string) ([]byte, error) { getErr = true; return nil, errors.New("boom") }
	_, _ = c.Get(ctx, "k") // error
	if !getErr {
		t.Fatal("the error path did not run")
	}
	m := o.metrics(t)
	req := m["cache.requests"]
	for _, a := range []string{"get,result=miss", "set,result=ok", "get,result=hit", "delete,result=ok", "get,result=error"} {
		op, res, _ := strings.Cut(a, ",")
		key := "cache.namespace=ns,cache.operation=" + op + "," + res
		if find(req, key) != 1 {
			t.Fatalf("cache.requests %s = %d: %v", key, find(req, key), req)
		}
	}
	if total(m["cache.duration"]) != 5 {
		t.Fatalf("cache.duration = %v", m["cache.duration"])
	}
	size := m["cache.value.size"]
	if total(size) != 2 {
		t.Fatalf("cache.value.size = %v", size)
	}
	for _, p := range size {
		if p.sum != float64(p.count)*11 {
			t.Fatalf("cache.value.size point %+v", p)
		}
	}
}

// U24
func TestLoadDurationMetric(t *testing.T) {
	o := setup(t)
	c := cache.New[string](&mock.CacheStore{}, "ns")
	_, _ = c.GetOrLoad(t.Context(), "a", ttl, func(context.Context) (string, error) { return "v", nil })
	_, _ = c.GetOrLoad(t.Context(), "b", ttl, func(context.Context) (string, error) { return "", errors.New("x") })
	d := o.metrics(t)["cache.load.duration"]
	if find(d, "cache.namespace=ns,outcome=ok") != 1 || find(d, "cache.namespace=ns,outcome=error") != 1 {
		t.Fatalf("cache.load.duration = %v", d)
	}
}

// U27
func TestInstrument(t *testing.T) {
	o := setup(t)
	store := cache.Instrument(&mock.CacheStore{}, "raw")
	if err := store.Set(t.Context(), "k", []byte("v"), ttl); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}
	req := o.metrics(t)["cache.requests"]
	if find(req, "cache.namespace=raw,cache.operation=get,result=hit") != 1 ||
		find(req, "cache.namespace=raw,cache.operation=set,result=ok") != 1 {
		t.Fatalf("cache.requests = %v", req)
	}
}

// U29
func TestCancelOfOneCallerDoesNotStopLoad(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns")
	var loads atomic.Int32
	ctxs := make([]context.Context, 10)
	var cancelFirst context.CancelFunc
	for i := range ctxs {
		if i == 0 {
			ctxs[i], cancelFirst = context.WithCancel(t.Context())
		} else {
			ctxs[i] = t.Context()
		}
	}
	hold := make(chan struct{})
	b := startBlockedLoads(t, c, 10, hold, ctxs, &loads)
	// All callers wait in the group. The load function blocks on hold.
	select {
	case <-b.all:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout while the callers join the group")
	}
	cancelFirst()
	// Caller 0 returns the context error while the load continues.
	waitFor(t, func() bool { return loads.Load() == 1 })
	close(hold)
	b.wait(t)
	if !errors.Is(b.results[0], context.Canceled) {
		t.Fatalf("first caller: %+v, %v", b.values[0], b.results[0])
	}
	for i := 1; i < 10; i++ {
		if b.results[i] != nil || b.values[i].Name != "Ada" {
			t.Fatalf("call %d: %+v, %v", i, b.values[i], b.results[i])
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d", loads.Load())
	}
	if _, ok := store.Value("ns:k"); !ok {
		t.Fatal("the store has no value")
	}
}

// U30
func TestLoadTimeout(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[user](store, "ns", cache.WithLoadTimeout(50*time.Millisecond))
	start := time.Now()
	_, err := c.GetOrLoad(t.Context(), "k", ttl, func(ctx context.Context) (user, error) {
		<-ctx.Done()
		return user{}, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 5*time.Second {
		t.Fatalf("duration = %v", d)
	}
	if store.SetCalls() != 0 {
		t.Fatalf("set calls = %d", store.SetCalls())
	}
}

// U31
func TestInvalidNamespacePanics(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	bad := []string{"Profile", "1abc", strings.Repeat("a", 33), ""}
	for _, ns := range bad {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("New(%q): want panic", ns)
				}
			}()
			cache.New[string](store, ns)
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Instrument(Profile): want panic")
			}
		}()
		cache.Instrument(store, "Profile")
	}()
	cache.New[string](store, strings.Repeat("a", 32))
	cache.Instrument(store, strings.Repeat("a", 32))
}

// U32
func TestGetDecodeError(t *testing.T) {
	o := setup(t)
	store := &mock.CacheStore{}
	store.Put("ns:k", []byte("not json"))
	c := cache.New[user](store, "ns")
	_, err := c.Get(t.Context(), "k")
	if !errors.Is(err, cache.ErrDecode) {
		t.Fatalf("err = %v", err)
	}
	if n := total(o.metrics(t)["cache.decode.errors"]); n != 1 {
		t.Fatalf("cache.decode.errors = %d", n)
	}
}

// U33 (cache metrics)
func TestHistogramBoundaries(t *testing.T) {
	o := setup(t)
	c := cache.New[string](&mock.CacheStore{}, "ns")
	_, _ = c.GetOrLoad(t.Context(), "k", ttl, func(context.Context) (string, error) { return "v", nil })
	m := o.metrics(t)
	seconds := []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	bytesB := []float64{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576}
	for name, want := range map[string][]float64{
		"cache.duration": seconds, "cache.load.duration": seconds, "cache.value.size": bytesB,
	} {
		pts := m[name]
		if len(pts) == 0 {
			t.Fatalf("%s: no data", name)
		}
		if fmt.Sprint(pts[0].bounds) != fmt.Sprint(want) {
			t.Fatalf("%s bounds = %v, want %v", name, pts[0].bounds, want)
		}
	}
}

// U35
func TestGetOrLoadChecksFirst(t *testing.T) {
	setup(t)
	store := &mock.CacheStore{}
	c := cache.New[string](store, "ns")
	called := false
	load := func(context.Context) (string, error) { called = true; return "v", nil }
	if _, err := c.GetOrLoad(t.Context(), strings.Repeat("a", 254), ttl, load); !errors.Is(err, cache.ErrInvalidKey) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.GetOrLoad(t.Context(), "k", 0, load); !errors.Is(err, cache.ErrInvalidTTL) {
		t.Fatalf("err = %v", err)
	}
	if called || store.GetCalls() != 0 || store.SetCalls() != 0 {
		t.Fatalf("load called %v, get %d, set %d", called, store.GetCalls(), store.SetCalls())
	}
}
