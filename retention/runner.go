package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	scopeName     = "github.com/techorionai/constellation-platform-go/retention"
	unlockTimeout = 10 * time.Second
)

var (
	jobNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
)

// Values of the metric attribute outcome.
const (
	outcomeOK       = "ok"
	outcomeError    = "error"
	outcomeCanceled = "canceled"
	outcomeSkipped  = "skipped"
)

// RunnerOptions are the settings for [NewRunner]. The fields are for tests.
type RunnerOptions struct {
	// Clock returns the current time. The default is time.Now.
	Clock func() time.Time
	// After returns a channel that gets a value after d. The default is
	// time.After. The runner uses it for the start wait and for each interval.
	After func(d time.Duration) <-chan time.Time
}

// Runner runs the retention jobs.
type Runner struct {
	locker Locker
	jobs   []Job
	clock  func() time.Time
	after  func(d time.Duration) <-chan time.Time

	deleted     metric.Int64Counter
	duration    metric.Float64Histogram
	lastSuccess metric.Int64Gauge
}

// NewRunner checks the jobs and makes a runner. It returns [ErrInvalidJob] for a
// wrong name or a nil purger, [ErrInvalidConfig] for a wrong Config, [ErrNoPeriod]
// for a job without a period, and [ErrDuplicateJob] for two jobs with the same
// name.
func NewRunner(locker Locker, jobs []Job, opts RunnerOptions) (*Runner, error) {
	if locker == nil {
		return nil, fmt.Errorf("%w: the locker is required", ErrInvalidJob)
	}
	seen := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		if !jobNamePattern.MatchString(j.Name) {
			return nil, fmt.Errorf("%w: the name must match %s", ErrInvalidJob, jobNamePattern)
		}
		if j.Purger == nil {
			return nil, fmt.Errorf("%w: job %q has no purger", ErrInvalidJob, j.Name)
		}
		if err := j.Config.Validate(); err != nil {
			return nil, fmt.Errorf("job %q: %w", j.Name, err)
		}
		if j.DefaultPeriod < 0 || (j.Config.Period == 0 && j.DefaultPeriod == 0) {
			return nil, fmt.Errorf("%w: job %q", ErrNoPeriod, j.Name)
		}
		if _, dup := seen[j.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateJob, j.Name)
		}
		seen[j.Name] = struct{}{}
	}
	r := &Runner{locker: locker, jobs: append([]Job(nil), jobs...), clock: opts.Clock, after: opts.After}
	if r.clock == nil {
		r.clock = time.Now
	}
	if r.after == nil {
		r.after = time.After
	}
	m := otel.GetMeterProvider().Meter(scopeName)
	r.deleted, _ = m.Int64Counter("retention.deleted", metric.WithUnit("{row}"),
		metric.WithDescription("Number of rows that a tick deleted."))
	r.duration, _ = m.Float64Histogram("retention.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of one tick."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	r.lastSuccess, _ = m.Int64Gauge("retention.last_success", metric.WithUnit("s"),
		metric.WithDescription("Unix time of the end of the last tick with the outcome ok in this instance."))
	return r, nil
}

// Run runs each job in its own goroutine and returns nil when ctx ends. The jobs
// are independent. A job waits a random time from 0 to Interval/10, runs its
// first tick, and then runs the next tick Interval after the end of the
// previous tick. A tick with an error does not stop Run. A tick that finds the
// lock with another instance is skipped.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range r.jobs {
		wg.Add(1)
		go func(j *Job) {
			defer wg.Done()
			r.loop(ctx, j)
		}(&r.jobs[i])
	}
	wg.Wait()
	return nil
}

func (r *Runner) loop(ctx context.Context, j *Job) {
	wait := time.Duration(rand.Int64N(int64(j.Config.Interval/10) + 1)) //nolint:gosec // the start wait needs no secure random
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.after(wait):
		}
		if ctx.Err() != nil {
			return
		}
		_, _ = r.tick(ctx, j)
		wait = j.Config.Interval
	}
}

// RunOnce runs one tick of the job name now. It returns the number of deleted
// items. It returns [ErrUnknownJob] for an unknown name. It returns 0 and
// [ErrLocked] when another instance has the lock. 0 and nil mean that the job
// ran and found no expired items.
func (r *Runner) RunOnce(ctx context.Context, name string) (int, error) {
	for i := range r.jobs {
		if r.jobs[i].Name == name {
			return r.tick(ctx, &r.jobs[i])
		}
	}
	return 0, ErrUnknownJob
}

func (r *Runner) tick(ctx context.Context, j *Job) (int, error) {
	jobAttr := attribute.String("job", j.Name)
	ctx, span := otel.Tracer(scopeName).Start(ctx, "retention.tick", trace.WithAttributes(jobAttr))
	defer span.End()

	start := time.Now()
	outcome, total, err := r.execute(ctx, j)
	elapsed := time.Since(start)

	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome == outcomeError {
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", rootErr(err))))
		span.SetStatus(codes.Error, outcome)
	}

	mctx := context.WithoutCancel(ctx)
	r.duration.Record(mctx, elapsed.Seconds(), metric.WithAttributes(jobAttr, attribute.String("outcome", outcome)))
	if outcome != outcomeSkipped {
		r.deleted.Add(mctx, int64(total), metric.WithAttributes(jobAttr))
	}
	if outcome == outcomeOK {
		r.lastSuccess.Record(mctx, r.clock().Unix(), metric.WithAttributes(jobAttr))
	}

	switch outcome {
	case outcomeCanceled:
		slog.InfoContext(mctx, "retention: tick stopped", slog.String("job", j.Name), slog.Int("deleted", total))
	case outcomeError:
		slog.ErrorContext(mctx, "retention: tick failed", slog.String("job", j.Name),
			slog.Int("deleted", total), slog.Any("error", err))
	}
	return total, err
}

// execute runs the tick and returns the outcome, the deleted count, and the
// error.
func (r *Runner) execute(ctx context.Context, j *Job) (string, int, error) {
	unlock, acquired, err := r.locker.TryLock(ctx, "retention:"+j.Name)
	if err != nil {
		return failOutcome(ctx, err), 0, fmt.Errorf("retention: lock: %w", err)
	}
	if !acquired {
		return outcomeSkipped, 0, ErrLocked
	}
	// The release uses a new context, so that it also works after a cancel.
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if uerr := unlock(uctx); uerr != nil {
			slog.WarnContext(uctx, "retention: cannot release the lock", slog.String("job", j.Name), slog.Any("error", uerr))
		}
	}()

	before := r.clock().Add(-j.period())
	batch := j.Config.BatchSize
	total := 0
	for {
		n, err := j.Purger.Purge(ctx, PurgeParams{Before: before, Limit: batch})
		total += n
		if err != nil {
			return failOutcome(ctx, err), total, fmt.Errorf("retention: purge: %w", err)
		}
		if n < batch {
			return outcomeOK, total, nil
		}
		if err := ctx.Err(); err != nil {
			return outcomeCanceled, total, err
		}
	}
}

// failOutcome returns "canceled" for a context error of ctx, and "error" for
// another error.
func failOutcome(ctx context.Context, err error) string {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return outcomeCanceled
	}
	return outcomeError
}

func rootErr(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
