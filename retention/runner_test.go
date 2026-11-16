package retention_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/retention"
)

var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

func testConfig() retention.Config {
	return retention.Config{Period: 72 * time.Hour, Interval: time.Minute, BatchSize: 1000}
}

func job(name string, purger retention.Purger) retention.Job {
	return retention.Job{Name: name, Config: testConfig(), Purger: purger}
}

func newRunner(t testing.TB, locker retention.Locker, opts retention.RunnerOptions, jobs ...retention.Job) *retention.Runner {
	t.Helper()
	r, err := retention.NewRunner(locker, jobs, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// metricsProbe sets the global meter provider to an in-memory one.
type metricsProbe struct{ reader *sdkmetric.ManualReader }

func probe(t *testing.T) metricsProbe {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	old := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(old); _ = mp.Shutdown(context.Background()) })
	return metricsProbe{reader: reader}
}

func (p metricsProbe) collect(t *testing.T, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := p.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	return metricdata.Metrics{}
}

// outcomes returns the outcome attribute of each record of retention.duration.
func (p metricsProbe) outcomes(t *testing.T) []string {
	t.Helper()
	m := p.collect(t, "retention.duration")
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		return nil
	}
	var out []string
	for _, dp := range h.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key("outcome"))
		for i := uint64(0); i < dp.Count; i++ {
			out = append(out, v.AsString())
		}
	}
	return out
}

func (p metricsProbe) deleted(t *testing.T) (int64, bool) {
	t.Helper()
	m := p.collect(t, "retention.deleted")
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(s.DataPoints) == 0 {
		return 0, false
	}
	var total int64
	for _, dp := range s.DataPoints {
		total += dp.Value
	}
	return total, true
}

func (p metricsProbe) lastSuccess(t *testing.T) (int64, bool) {
	t.Helper()
	m := p.collect(t, "retention.last_success")
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok || len(g.DataPoints) == 0 {
		return 0, false
	}
	return g.DataPoints[0].Value, true
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// U17
func TestConfigValidate(t *testing.T) {
	bad := []retention.Config{
		{Period: -time.Hour, Interval: time.Hour, BatchSize: 10},
		{Interval: 30 * time.Second, BatchSize: 10},
		{Interval: time.Hour, BatchSize: 0},
		{Interval: time.Hour, BatchSize: 100001},
	}
	for _, c := range bad {
		if err := c.Validate(); !errors.Is(err, retention.ErrInvalidConfig) {
			t.Fatalf("%+v: err = %v", c, err)
		}
	}
	good := []retention.Config{
		{Interval: time.Minute, BatchSize: 1},
		{Interval: time.Minute, BatchSize: 100000},
		{Interval: time.Hour, BatchSize: 10},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

// U18
func TestNewRunnerBadName(t *testing.T) {
	_, err := retention.NewRunner(&mock.RetentionLocker{}, []retention.Job{job("Bad-Name", &mock.RetentionPurger{})}, retention.RunnerOptions{})
	if !errors.Is(err, retention.ErrInvalidJob) {
		t.Fatalf("err = %v", err)
	}
}

// U19
func TestRunOnceBefore(t *testing.T) {
	p := &mock.RetentionPurger{}
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{Clock: fixedClock}, job("events", p))
	if _, err := r.RunOnce(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	got := p.PurgeParams()
	if len(got) != 1 || !got[0].Before.Equal(fixedNow.Add(-72*time.Hour)) || got[0].Limit != 1000 {
		t.Fatalf("params = %+v", got)
	}
}

// U20
func TestRunOnceBatches(t *testing.T) {
	counts := []int{1000, 1000, 300}
	p := &mock.RetentionPurger{}
	p.PurgeFunc = func(context.Context, retention.PurgeParams) (int, error) {
		n := counts[p.PurgeCalls()-1]
		return n, nil
	}
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{Clock: fixedClock}, job("events", p))
	n, err := r.RunOnce(context.Background(), "events")
	if err != nil || n != 2300 || p.PurgeCalls() != 3 {
		t.Fatalf("n %d, err %v, calls %d", n, err, p.PurgeCalls())
	}
}

// U21
func TestRunOnceLocked(t *testing.T) {
	m := probe(t)
	p := &mock.RetentionPurger{}
	locker := &mock.RetentionLocker{Busy: true}
	r := newRunner(t, locker, retention.RunnerOptions{Clock: fixedClock}, job("events", p))
	n, err := r.RunOnce(context.Background(), "events")
	if n != 0 || !errors.Is(err, retention.ErrLocked) {
		t.Fatalf("n %d, err %v", n, err)
	}
	if p.PurgeCalls() != 0 {
		t.Fatal("the purger was called")
	}
	if got := m.outcomes(t); len(got) != 1 || got[0] != "skipped" {
		t.Fatalf("outcomes = %v", got)
	}
	if _, ok := m.deleted(t); ok {
		t.Fatal("retention.deleted has a record")
	}
	if _, ok := m.lastSuccess(t); ok {
		t.Fatal("retention.last_success has a record")
	}

	locker.Busy = false
	if n, err := r.RunOnce(context.Background(), "events"); n != 0 || err != nil {
		t.Fatalf("n %d, err %v", n, err)
	}
	if names := locker.LockNames(); len(names) != 2 || names[0] != "retention:events" {
		t.Fatalf("lock names = %v", names)
	}
}

// U22
func TestRunOncePurgeError(t *testing.T) {
	boom := errors.New("boom")
	p := &mock.RetentionPurger{PurgeFunc: func(context.Context, retention.PurgeParams) (int, error) { return 0, boom }}
	locker := &mock.RetentionLocker{}
	r := newRunner(t, locker, retention.RunnerOptions{}, job("events", p))
	if _, err := r.RunOnce(context.Background(), "events"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if locker.UnlockCalls() != 1 {
		t.Fatalf("unlock calls = %d", locker.UnlockCalls())
	}
}

// U23
func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{}, job("events", &mock.RetentionPurger{}))
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return in 1 second")
	}
}

// U24
func TestRunOnceMetrics(t *testing.T) {
	m := probe(t)
	fail := false
	p := &mock.RetentionPurger{PurgeFunc: func(context.Context, retention.PurgeParams) (int, error) {
		if fail {
			return 0, errors.New("boom")
		}
		return 7, nil
	}}
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{Clock: fixedClock}, job("events", p))
	if _, err := r.RunOnce(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	if n, ok := m.deleted(t); !ok || n != 7 {
		t.Fatalf("deleted = %d, %v", n, ok)
	}
	if got := m.outcomes(t); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("outcomes = %v", got)
	}
	if v, ok := m.lastSuccess(t); !ok || v != fixedNow.Unix() {
		t.Fatalf("last_success = %d, %v", v, ok)
	}

	fail = true
	if _, err := r.RunOnce(context.Background(), "events"); err == nil {
		t.Fatal("no error")
	}
	if got := m.outcomes(t); len(got) != 2 {
		t.Fatalf("outcomes = %v", got)
	}
	if v, _ := m.lastSuccess(t); v != fixedNow.Unix() {
		t.Fatalf("last_success changed to %d", v)
	}
}

// U25
func TestRunOnceCanceled(t *testing.T) {
	m := probe(t)
	logs := captureLogs(t)
	entered := make(chan struct{})
	p := &mock.RetentionPurger{PurgeFunc: func(ctx context.Context, _ retention.PurgeParams) (int, error) {
		close(entered)
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	locker := &mock.RetentionLocker{}
	r := newRunner(t, locker, retention.RunnerOptions{}, job("events", p))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-entered; cancel() }()
	_, err := r.RunOnce(ctx, "events")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if locker.UnlockCalls() != 1 {
		t.Fatalf("unlock calls = %d", locker.UnlockCalls())
	}
	if got := m.outcomes(t); len(got) != 1 || got[0] != "canceled" {
		t.Fatalf("outcomes = %v", got)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("log has an error record: %s", logs.String())
	}
}

// U26
func TestConfigLoad(t *testing.T) {
	type svcConfig struct {
		Audit retention.Config `envPrefix:"RETENTION__AUDIT__"`
	}
	t.Run("period", func(t *testing.T) {
		t.Setenv("SVC_RETENTION__AUDIT__PERIOD", "1440h")
		var cfg svcConfig
		if err := config.Load("SVC_", &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Audit.Period != 1440*time.Hour || cfg.Audit.Interval != time.Hour || cfg.Audit.BatchSize != 1000 {
			t.Fatalf("config = %+v", cfg.Audit)
		}
	})
	t.Run("none", func(t *testing.T) {
		var cfg svcConfig
		if err := config.Load("SVC_", &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Audit.Period != 0 {
			t.Fatalf("period = %v", cfg.Audit.Period)
		}
	})
	t.Run("interval and batch", func(t *testing.T) {
		t.Setenv("SVC_RETENTION__AUDIT__INTERVAL", "2h")
		t.Setenv("SVC_RETENTION__AUDIT__BATCH_SIZE", "500")
		var cfg svcConfig
		if err := config.Load("SVC_", &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Audit.Interval != 2*time.Hour || cfg.Audit.BatchSize != 500 {
			t.Fatalf("config = %+v", cfg.Audit)
		}
	})
}

// U35
func TestDefaultPeriod(t *testing.T) {
	p := &mock.RetentionPurger{}
	j := job("events", p)
	j.Config.Period = 0
	j.DefaultPeriod = 1440 * time.Hour
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{Clock: fixedClock}, j)
	if _, err := r.RunOnce(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	if got := p.PurgeParams()[0].Before; !got.Equal(fixedNow.Add(-1440 * time.Hour)) {
		t.Fatalf("before = %v", got)
	}
}

// U36
func TestConfigPeriodWins(t *testing.T) {
	p := &mock.RetentionPurger{}
	j := job("events", p)
	j.Config.Period = 720 * time.Hour
	j.DefaultPeriod = 1440 * time.Hour
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{Clock: fixedClock}, j)
	if _, err := r.RunOnce(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	if got := p.PurgeParams()[0].Before; !got.Equal(fixedNow.Add(-720 * time.Hour)) {
		t.Fatalf("before = %v", got)
	}
}

// U37
func TestNewRunnerPeriodAndDuplicates(t *testing.T) {
	locker := &mock.RetentionLocker{}
	p := &mock.RetentionPurger{}
	none := job("events", p)
	none.Config.Period = 0
	_, err := retention.NewRunner(locker, []retention.Job{none}, retention.RunnerOptions{})
	if !errors.Is(err, retention.ErrNoPeriod) {
		t.Fatalf("no period: err = %v", err)
	}
	negative := none
	negative.DefaultPeriod = -time.Hour
	if _, err := retention.NewRunner(locker, []retention.Job{negative}, retention.RunnerOptions{}); !errors.Is(err, retention.ErrNoPeriod) {
		t.Fatalf("negative: err = %v", err)
	}
	if _, err := retention.NewRunner(locker, []retention.Job{job("events", p), job("events", p)}, retention.RunnerOptions{}); !errors.Is(err, retention.ErrDuplicateJob) {
		t.Fatalf("duplicate: err = %v", err)
	}
}

// U38
func TestRunOnceUnknownJob(t *testing.T) {
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{}, job("events", &mock.RetentionPurger{}))
	if _, err := r.RunOnce(context.Background(), "unknown"); !errors.Is(err, retention.ErrUnknownJob) {
		t.Fatalf("err = %v", err)
	}
}

// afterRequest is one call of the After function of a runner.
type afterRequest struct {
	d  time.Duration
	ch chan time.Time
}

// controlledAfter returns an After function. The test reads the requests and
// sends on the channel of a request to end the wait.
func controlledAfter() (func(time.Duration) <-chan time.Time, chan afterRequest) {
	reqs := make(chan afterRequest, 100)
	return func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		reqs <- afterRequest{d: d, ch: ch}
		return ch
	}, reqs
}

func nextRequest(t *testing.T, reqs chan afterRequest) afterRequest {
	t.Helper()
	select {
	case r := <-reqs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: no call of After")
		return afterRequest{}
	}
}

func runInBackground(r *retention.Runner, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	return done
}

// U40
func TestRunSchedule(t *testing.T) {
	after, reqs := controlledAfter()
	p := &mock.RetentionPurger{}
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{After: after}, job("events", p))
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(r, ctx)

	first := nextRequest(t, reqs)
	if first.d < 0 || first.d > 6*time.Second {
		t.Fatalf("start wait = %v, want 0 to 6s", first.d)
	}
	time.Sleep(20 * time.Millisecond)
	if p.PurgeCalls() != 0 {
		t.Fatal("the purger ran before the start wait ended")
	}
	first.ch <- time.Now()
	waitFor(t, "first tick", func() bool { return p.PurgeCalls() == 1 })
	for tick := 2; tick <= 3; tick++ {
		next := nextRequest(t, reqs)
		if next.d != time.Minute {
			t.Fatalf("interval = %v, want 1m", next.d)
		}
		next.ch <- time.Now()
		want := tick
		waitFor(t, "tick", func() bool { return p.PurgeCalls() == want })
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the cancel")
	}
	if p.PurgeCalls() != 3 {
		t.Fatalf("ticks = %d, want 3", p.PurgeCalls())
	}
}

// U41
func TestRunContinuesAfterError(t *testing.T) {
	logs := captureLogs(t)
	after, reqs := controlledAfter()
	p := &mock.RetentionPurger{}
	p.PurgeFunc = func(context.Context, retention.PurgeParams) (int, error) {
		if p.PurgeCalls() == 1 {
			return 0, errors.New("boom")
		}
		return 0, nil
	}
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{After: after}, job("events", p))
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(r, ctx)

	nextRequest(t, reqs).ch <- time.Now()
	waitFor(t, "first tick", func() bool { return p.PurgeCalls() == 1 })
	nextRequest(t, reqs).ch <- time.Now()
	waitFor(t, "second tick", func() bool { return p.PurgeCalls() == 2 })
	select {
	case <-done:
		t.Fatal("Run returned after an error")
	default:
	}
	cancel()
	<-done
	if n := strings.Count(logs.String(), `"level":"ERROR"`); n != 1 {
		t.Fatalf("%d error records, want 1: %s", n, logs.String())
	}
}

// U42
func TestRunJobsAreIndependent(t *testing.T) {
	after, reqs := controlledAfter()
	release := make(chan struct{})
	pa := &mock.RetentionPurger{PurgeFunc: func(ctx context.Context, _ retention.PurgeParams) (int, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 0, nil
	}}
	pb := &mock.RetentionPurger{}
	a := job("a", pa)
	a.Config.Interval = 3 * time.Minute
	b := job("b", pb)
	r := newRunner(t, &mock.RetentionLocker{}, retention.RunnerOptions{After: after}, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(r, ctx)

	// Both jobs ask for a start wait. The test ends both.
	nextRequest(t, reqs).ch <- time.Now()
	nextRequest(t, reqs).ch <- time.Now()
	waitFor(t, "a blocks", func() bool { return pa.PurgeCalls() == 1 })
	// b runs ticks while a blocks. Its interval requests have d = 1m.
	for tick := 1; tick <= 2; tick++ {
		req := nextRequest(t, reqs)
		if req.d != time.Minute {
			t.Fatalf("request = %v, want the interval of b", req.d)
		}
		req.ch <- time.Now()
		want := tick + 1
		waitFor(t, "b tick", func() bool { return pb.PurgeCalls() == want })
	}
	if pa.PurgeCalls() != 1 {
		t.Fatal("a ran again while blocked")
	}
	close(release)
	// When a completes its tick, it asks for its interval of 3 minutes.
	for {
		req := nextRequest(t, reqs)
		if req.d == 3*time.Minute {
			break
		}
	}
	cancel()
	<-done
}

func TestRunSkipsLockedTickWithoutErrorLog(t *testing.T) {
	logs := captureLogs(t)
	after, reqs := controlledAfter()
	locker := &mock.RetentionLocker{Busy: true}
	r := newRunner(t, locker, retention.RunnerOptions{After: after}, job("events", &mock.RetentionPurger{}))
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(r, ctx)
	nextRequest(t, reqs).ch <- time.Now()
	waitFor(t, "lock call", func() bool { return locker.TryLockCalls() == 1 })
	nextRequest(t, reqs) // the next interval: the tick ended
	cancel()
	<-done
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("log = %s", logs.String())
	}
}
