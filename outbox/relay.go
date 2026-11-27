package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/techorionai/constellation-platform-go/outbox"

// MaxBatchBytes is the maximum payload size of one relay batch.
const MaxBatchBytes = 16 << 20

// Defaults and limits of [RelayOptions].
const (
	DefaultInterval       = time.Second
	MinInterval           = 100 * time.Millisecond
	DefaultBatchSize      = 100
	MaxBatchSize          = 500
	DefaultPublishTimeout = 10 * time.Second
	MinPublishTimeout     = time.Second
	MaxPublishTimeout     = time.Minute
	// DefaultLagTimeout is the time limit of the lag gauge callback.
	DefaultLagTimeout = 5 * time.Second

	minBackoff = time.Second
	maxBackoff = time.Minute
)

// Record is one unpublished outbox row.
type Record struct {
	// ID is the database ID of the row.
	ID int64
	// EventID is the id of the CloudEvent.
	EventID string
	// Payload is the CloudEvent in JSON.
	Payload []byte
}

// Store is the port to the unpublished rows.
type Store interface {
	// Process locks up to limit unpublished rows in the order of ID, calls fn
	// with them, and marks them as published when fn returns nil. If fn returns
	// an error, nothing changes. It gives fn rows until the payload bytes reach
	// maxBytes, but the first row always goes to fn. It does not call fn when
	// there are no rows. n is the count of the published rows. full is true when
	// the batch was full by count or by bytes.
	//
	// Delivery is at least once. The rows stay locked while fn runs. If fn
	// publishes the events and the commit then fails (for example, the database
	// connection ends), the rows stay unpublished and the next call publishes
	// them again. Consumers must handle a duplicate event by its source and id.
	Process(ctx context.Context, limit, maxBytes int, fn func(ctx context.Context, events []Record) error) (n int, full bool, err error)
	// Lag returns the age of the oldest unpublished row, or 0.
	Lag(ctx context.Context) (time.Duration, error)
}

// Publisher is the port to the event stream.
type Publisher interface {
	// Publish adds the payloads to the stream of the service.
	Publish(ctx context.Context, payloads [][]byte) error
}

// DefaultRelayName is the default of [RelayOptions.Name].
const DefaultRelayName = "events"

var relayNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// RelayOptions are the settings of the [Relay]. A zero value uses the default.
type RelayOptions struct {
	// Name tells two relays in one process apart. It is the value of the
	// attribute "outbox" of the metrics. Default "events". It must match
	// ^[a-z][a-z0-9_]{0,31}$. An audit relay uses "audit".
	Name string
	// Interval is the wait after an empty batch. Default 1s, minimum 100ms.
	Interval time.Duration
	// BatchSize is the maximum number of rows in one batch. Default 100, from 1
	// to 500.
	BatchSize int
	// PublishTimeout is the time limit of one publish. Default 10s, from 1s to
	// 1m.
	PublishTimeout time.Duration
	// LagTimeout is the time limit of the lag gauge callback. Default 5s.
	LagTimeout time.Duration

	// Clock returns the current time. The default is time.Now. It is for tests.
	Clock func() time.Time
	// After returns a channel that gets a value after d. The default is
	// time.After. It is for tests.
	After func(d time.Duration) <-chan time.Time

	// MeterProvider is the meter provider. The default is the global one.
	MeterProvider metric.MeterProvider
	// TracerProvider is the tracer provider. The default is the global one.
	TracerProvider trace.TracerProvider
	// Logger is the logger. The default is slog.Default().
	Logger *slog.Logger
}

func (o RelayOptions) resolve() (RelayOptions, error) {
	if o.Name == "" {
		o.Name = DefaultRelayName
	}
	if !relayNamePattern.MatchString(o.Name) {
		return o, fmt.Errorf("%w: Name must match %s", ErrInvalidConfig, relayNamePattern)
	}
	if o.Interval == 0 {
		o.Interval = DefaultInterval
	}
	if o.BatchSize == 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.PublishTimeout == 0 {
		o.PublishTimeout = DefaultPublishTimeout
	}
	if o.LagTimeout == 0 {
		o.LagTimeout = DefaultLagTimeout
	}
	switch {
	case o.Interval < MinInterval:
		return o, fmt.Errorf("%w: Interval must be %s or more", ErrInvalidConfig, MinInterval)
	case o.BatchSize < 1 || o.BatchSize > MaxBatchSize:
		return o, fmt.Errorf("%w: BatchSize must be from 1 to %d", ErrInvalidConfig, MaxBatchSize)
	case o.PublishTimeout < MinPublishTimeout || o.PublishTimeout > MaxPublishTimeout:
		return o, fmt.Errorf("%w: PublishTimeout must be from %s to %s", ErrInvalidConfig, MinPublishTimeout, MaxPublishTimeout)
	case o.LagTimeout < 0:
		return o, fmt.Errorf("%w: LagTimeout must not be negative", ErrInvalidConfig)
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.MeterProvider == nil {
		o.MeterProvider = otel.GetMeterProvider()
	}
	if o.TracerProvider == nil {
		o.TracerProvider = otel.GetTracerProvider()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o, nil
}

// Relay copies the unpublished outbox rows to the event stream.
type Relay struct {
	store  Store
	pub    Publisher
	opts   RelayOptions
	notify chan struct{}
	tracer trace.Tracer

	published metric.Int64Counter
	lag       metric.Float64ObservableGauge
	nameAttr  attribute.KeyValue
}

// NewRelay checks the options and makes a relay. It returns an error that wraps
// [ErrInvalidConfig] and names the field.
func NewRelay(store Store, pub Publisher, opts RelayOptions) (*Relay, error) {
	if store == nil || pub == nil {
		return nil, fmt.Errorf("%w: the store and the publisher are required", ErrInvalidConfig)
	}
	opts, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	r := &Relay{
		store:  store,
		pub:    pub,
		opts:   opts,
		notify: make(chan struct{}, 1),
		tracer: opts.TracerProvider.Tracer(scopeName),

		nameAttr: attribute.String("outbox", opts.Name),
	}
	m := opts.MeterProvider.Meter(scopeName)
	r.published, err = m.Int64Counter("constellation.outbox.published", metric.WithUnit("{event}"),
		metric.WithDescription("Number of outbox events that the relay published, by outcome."))
	if err != nil {
		return nil, fmt.Errorf("outbox: counter: %w", err)
	}
	r.lag, err = m.Float64ObservableGauge("constellation.outbox.lag", metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest unpublished outbox row. 0 if there is none."))
	if err != nil {
		return nil, fmt.Errorf("outbox: gauge: %w", err)
	}
	if _, err := m.RegisterCallback(r.observeLag, r.lag); err != nil {
		return nil, fmt.Errorf("outbox: register callback: %w", err)
	}
	return r, nil
}

func (r *Relay) observeLag(ctx context.Context, o metric.Observer) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.LagTimeout)
	defer cancel()
	type result struct {
		d   time.Duration
		err error
	}
	// The store call can ignore the context, so the callback does not wait for
	// it after the time limit.
	ch := make(chan result, 1)
	go func() {
		d, err := r.store.Lag(ctx)
		ch <- result{d, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			r.opts.Logger.WarnContext(ctx, "outbox: cannot read the lag", slog.String("error.type", fmt.Sprintf("%T", rootErr(res.err))))
			return nil
		}
		o.ObserveFloat64(r.lag, res.d.Seconds(), metric.WithAttributes(r.nameAttr))
	case <-ctx.Done():
		r.opts.Logger.WarnContext(context.WithoutCancel(ctx), "outbox: the lag query took too long")
	}
	return nil
}

// Notify wakes the relay loop. It does not wait. Call it in
// transaction.Transactor.AfterCommit after a write.
func (r *Relay) Notify() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Run copies rows to the stream until ctx ends. It returns nil then. A failed
// tick does not stop Run. The relay waits with a backoff from 1s to 1m after an
// error.
func (r *Relay) Run(ctx context.Context) error {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		_, full, err := r.tick(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			backoff = nextBackoff(backoff)
			r.opts.Logger.ErrorContext(ctx, "outbox: relay tick failed",
				slog.String("error.type", fmt.Sprintf("%T", rootErr(err))),
				slog.Duration("backoff", backoff))
			// A Notify call does not cut the backoff.
			select {
			case <-ctx.Done():
			case <-r.opts.After(jitter(backoff)):
			}
		case full:
			backoff = 0
		default:
			backoff = 0
			select {
			case <-ctx.Done():
			case <-r.notify:
			case <-r.opts.After(r.opts.Interval):
			}
		}
	}
	return nil
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur < minBackoff {
		return minBackoff
	}
	return min(cur*2, maxBackoff)
}

// jitter returns a duration from d/2 to d.
func jitter(d time.Duration) time.Duration {
	half := int64(d / 2)
	return time.Duration(half + rand.Int64N(half+1)) //nolint:gosec // the jitter needs no secure random
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

// tick processes one batch.
func (r *Relay) tick(ctx context.Context) (int, bool, error) {
	batchLen := 0
	publishFailed := false
	n, full, err := r.store.Process(ctx, r.opts.BatchSize, MaxBatchBytes, func(ctx context.Context, recs []Record) error {
		batchLen = len(recs)
		if err := r.publish(ctx, recs); err != nil {
			publishFailed = true
			return err
		}
		return nil
	})
	mctx := context.WithoutCancel(ctx)
	if err != nil {
		// A publish error counts each event of the batch. A database error
		// has its own outcome and counts 1.
		outcome, count := "error", batchLen
		if !publishFailed {
			outcome, count = "db_error", 1
		}
		r.published.Add(mctx, int64(count), metric.WithAttributes(r.nameAttr, attribute.String("outcome", outcome)))
		return 0, false, err
	}
	if n > 0 {
		r.published.Add(mctx, int64(n), metric.WithAttributes(r.nameAttr, attribute.String("outcome", "ok")))
	}
	return n, full, nil
}

func (r *Relay) publish(ctx context.Context, recs []Record) (err error) {
	ctx, span := r.tracer.Start(ctx, "outbox.relay.publish",
		trace.WithAttributes(attribute.Int("constellation.outbox.events", len(recs))))
	defer func() {
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "error"),
				attribute.String("error.type", fmt.Sprintf("%T", rootErr(err))))
			span.SetStatus(codes.Error, "publish failed")
		} else {
			span.SetAttributes(attribute.String("outcome", "ok"))
		}
		span.End()
	}()
	ctx, cancel := context.WithTimeout(ctx, r.opts.PublishTimeout)
	defer cancel()
	payloads := make([][]byte, len(recs))
	for i := range recs {
		payloads[i] = recs[i].Payload
	}
	return r.pub.Publish(ctx, payloads)
}
