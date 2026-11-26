package streams

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/techorionai/constellation-platform-go/streams"

// Defaults and limits of [ConsumerOptions].
const (
	DefaultCount           = 50
	MaxCount               = 1000
	DefaultBlock           = 5 * time.Second
	MinBlock               = 100 * time.Millisecond
	MaxBlock               = time.Minute
	DefaultClaimIdle       = 5 * time.Minute
	MinClaimIdle           = 100 * time.Millisecond
	DefaultMaxDeliveries   = 10
	MaxMaxDeliveries       = 1000
	DefaultCleanupInterval = time.Hour
	DefaultCleanupIdle     = 24 * time.Hour

	leaveTimeout = 5 * time.Second
	minBackoff   = 100 * time.Millisecond
	maxBackoff   = 30 * time.Second
)

// ConsumerOptions are the settings of a [Consumer]. A zero value uses the
// default.
type ConsumerOptions struct {
	// Count is the maximum number of messages for one read or claim. Default
	// 50, from 1 to 1000.
	Count int
	// Block is the time that a read waits for new messages. Default 5s, from
	// 100ms to 1m.
	Block time.Duration
	// ClaimIdle is the idle time after which a pending message can be claimed.
	// Default 5m, 100ms or more.
	ClaimIdle time.Duration
	// MaxDeliveries is the number of deliveries after which a message is
	// dropped. Default 10, from 1 to 1000.
	MaxDeliveries int
	// CleanupInterval is the time between two cleanups of old consumers.
	// Default 1h.
	CleanupInterval time.Duration
	// CleanupIdle is the idle time after which a consumer with no pending
	// messages is deleted. Default 24h.
	CleanupIdle time.Duration

	// Clock returns the current time. The default is time.Now. It is for tests.
	Clock func() time.Time
	// MeterProvider is the meter provider. The default is the global one.
	MeterProvider metric.MeterProvider
	// TracerProvider is the tracer provider. The default is the global one.
	TracerProvider trace.TracerProvider
	// Logger is the logger. The default is slog.Default().
	Logger *slog.Logger
}

func (o ConsumerOptions) resolve() (ConsumerOptions, error) {
	if o.Count == 0 {
		o.Count = DefaultCount
	}
	if o.Block == 0 {
		o.Block = DefaultBlock
	}
	if o.ClaimIdle == 0 {
		o.ClaimIdle = DefaultClaimIdle
	}
	if o.MaxDeliveries == 0 {
		o.MaxDeliveries = DefaultMaxDeliveries
	}
	if o.CleanupInterval == 0 {
		o.CleanupInterval = DefaultCleanupInterval
	}
	if o.CleanupIdle == 0 {
		o.CleanupIdle = DefaultCleanupIdle
	}
	switch {
	case o.Count < 1 || o.Count > MaxCount:
		return o, fmt.Errorf("%w: Count must be from 1 to %d", ErrInvalidConfig, MaxCount)
	case o.Block < MinBlock || o.Block > MaxBlock:
		return o, fmt.Errorf("%w: Block must be from %s to %s", ErrInvalidConfig, MinBlock, MaxBlock)
	case o.ClaimIdle < MinClaimIdle:
		return o, fmt.Errorf("%w: ClaimIdle must be %s or more", ErrInvalidConfig, MinClaimIdle)
	case o.MaxDeliveries < 1 || o.MaxDeliveries > MaxMaxDeliveries:
		return o, fmt.Errorf("%w: MaxDeliveries must be from 1 to %d", ErrInvalidConfig, MaxMaxDeliveries)
	case o.CleanupInterval < 0:
		return o, fmt.Errorf("%w: CleanupInterval must not be negative", ErrInvalidConfig)
	case o.CleanupIdle < 0:
		return o, fmt.Errorf("%w: CleanupIdle must not be negative", ErrInvalidConfig)
	}
	if o.Clock == nil {
		o.Clock = time.Now
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

// Consumer reads one stream with one consumer group.
type Consumer struct {
	group   Group
	handler Handler
	opts    ConsumerOptions
	tracer  trace.Tracer

	handled metric.Int64Counter
	lost    metric.Int64Counter
}

// NewConsumer checks the options and makes a consumer. It returns an error that
// wraps [ErrInvalidConfig] and names the field.
func NewConsumer(group Group, h Handler, opts ConsumerOptions) (*Consumer, error) {
	if group == nil || h == nil {
		return nil, fmt.Errorf("%w: the group and the handler are required", ErrInvalidConfig)
	}
	opts, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	m := opts.MeterProvider.Meter(scopeName)
	c := &Consumer{group: group, handler: h, opts: opts, tracer: opts.TracerProvider.Tracer(scopeName)}
	c.handled, err = m.Int64Counter("constellation.events.consumer.handled", metric.WithUnit("{message}"),
		metric.WithDescription("Number of stream messages that the consumer handled, by outcome."))
	if err != nil {
		return nil, fmt.Errorf("streams: counter: %w", err)
	}
	c.lost, err = m.Int64Counter("constellation.events.consumer.lost", metric.WithUnit("{message}"),
		metric.WithDescription("Number of pending messages that the trim deleted before the consumer handled them."))
	if err != nil {
		return nil, fmt.Errorf("streams: counter: %w", err)
	}
	return c, nil
}

// Run handles messages until ctx ends. It returns nil then. A failed call to
// the stream does not stop Run. It waits with a backoff and tries again. At the
// end, Run deletes its own consumer if it has no pending messages.
func (c *Consumer) Run(ctx context.Context) error {
	defer c.leave(ctx)
	backoff := time.Duration(0)
	ensured := false
	lastCleanup := c.opts.Clock()
	for ctx.Err() == nil {
		var err error
		if !ensured {
			if err = c.group.Ensure(ctx); err == nil {
				ensured = true
			}
		}
		if err == nil {
			err = c.iterate(ctx, &lastCleanup)
		}
		if err == nil {
			backoff = 0
			continue
		}
		if ctx.Err() != nil {
			break
		}
		backoff = min(max(backoff*2, minBackoff), maxBackoff)
		c.opts.Logger.ErrorContext(ctx, "streams: consumer call failed",
			slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()),
			slog.Any("error", err), slog.Duration("backoff", backoff))
		sleep(ctx, backoff)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (c *Consumer) leave(ctx context.Context) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveTimeout)
	defer cancel()
	if err := c.group.Leave(lctx); err != nil {
		c.opts.Logger.WarnContext(lctx, "streams: cannot delete the consumer",
			slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()), slog.Any("error", err))
	}
}

// iterate does the cleanup, one claim, and one read.
func (c *Consumer) iterate(ctx context.Context, lastCleanup *time.Time) error {
	if now := c.opts.Clock(); now.Sub(*lastCleanup) >= c.opts.CleanupInterval {
		*lastCleanup = now
		if n, err := c.group.Cleanup(ctx, c.opts.CleanupIdle); err != nil {
			if ctx.Err() != nil {
				return err
			}
			c.opts.Logger.WarnContext(ctx, "streams: cleanup of consumers failed",
				slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()), slog.Any("error", err))
		} else if n > 0 {
			c.opts.Logger.InfoContext(ctx, "streams: deleted old consumers",
				slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()), slog.Int("count", n))
		}
	}
	msgs, deleted, err := c.group.Claim(ctx, c.opts.ClaimIdle, c.opts.Count)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	c.reportLost(ctx, deleted)
	c.handleAll(ctx, msgs)
	msgs, err = c.group.Read(ctx, c.opts.Count, c.opts.Block)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	c.handleAll(ctx, msgs)
	return nil
}

func (c *Consumer) reportLost(ctx context.Context, deleted []string) {
	if len(deleted) == 0 {
		return
	}
	attrs := c.attrs()
	c.lost.Add(ctx, int64(len(deleted)), metric.WithAttributes(attrs...))
	c.opts.Logger.WarnContext(ctx, "streams: the trim deleted pending messages",
		slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()), slog.Int("count", len(deleted)))
}

func (c *Consumer) attrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("stream", c.group.Stream()),
		attribute.String("group", c.group.Name()),
	}
}

func (c *Consumer) handleAll(ctx context.Context, msgs []Message) {
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		c.handleOne(ctx, m)
	}
}

// header is the part of the CloudEvent that the consumer reads.
type header struct {
	Type        string `json:"type"`
	Traceparent string `json:"traceparent"`
}

func (c *Consumer) handleOne(ctx context.Context, m Message) {
	var h header
	_ = json.Unmarshal(m.Event, &h)

	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("constellation.events.stream", c.group.Stream()),
			attribute.String("constellation.events.group", c.group.Name()),
			attribute.String("constellation.events.type", h.Type),
			attribute.Int64("constellation.events.deliveries", m.Deliveries),
		),
	}
	if sc, ok := parseTraceparent(h.Traceparent); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	hctx, span := c.tracer.Start(ctx, "events.consume", opts...)
	defer span.End()

	outcome := "ok"
	defer func() {
		span.SetAttributes(attribute.String("outcome", outcome))
		c.handled.Add(context.WithoutCancel(ctx), 1,
			metric.WithAttributes(append(c.attrs(), attribute.String("outcome", outcome))...))
	}()

	if m.Deliveries > int64(c.opts.MaxDeliveries) {
		outcome = "dropped"
		c.opts.Logger.ErrorContext(hctx, "streams: dropped a message after too many deliveries",
			slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()),
			slog.String("id", m.ID), slog.String("type", h.Type), slog.Int64("deliveries", m.Deliveries))
		c.ack(hctx, ctx, m.ID)
		return
	}
	if err := c.handler(hctx, m); err != nil {
		outcome = "error"
		span.SetStatus(codes.Error, "handler failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		c.opts.Logger.WarnContext(hctx, "streams: the handler failed",
			slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()),
			slog.String("id", m.ID), slog.String("type", h.Type), slog.Int64("deliveries", m.Deliveries),
			slog.String("error.type", fmt.Sprintf("%T", err)))
		return
	}
	c.ack(hctx, ctx, m.ID)
}

func (c *Consumer) ack(logCtx, ctx context.Context, id string) {
	// The ack uses a context that a stop does not cancel, so that a handled
	// message is not delivered again.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveTimeout)
	defer cancel()
	if err := c.group.Ack(actx, id); err != nil {
		c.opts.Logger.WarnContext(logCtx, "streams: cannot acknowledge a message",
			slog.String("stream", c.group.Stream()), slog.String("group", c.group.Name()),
			slog.String("id", id), slog.Any("error", err))
	}
}

// parseTraceparent reads a W3C traceparent of version 00. It returns false for
// a missing or invalid value.
func parseTraceparent(s string) (trace.SpanContext, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return trace.SpanContext{}, false
	}
	var cfg trace.SpanContextConfig
	if _, err := hex.Decode(cfg.TraceID[:], []byte(parts[1])); err != nil {
		return trace.SpanContext{}, false
	}
	if _, err := hex.Decode(cfg.SpanID[:], []byte(parts[2])); err != nil {
		return trace.SpanContext{}, false
	}
	var flags [1]byte
	if _, err := hex.Decode(flags[:], []byte(parts[3])); err != nil {
		return trace.SpanContext{}, false
	}
	cfg.TraceFlags = trace.TraceFlags(flags[0])
	cfg.Remote = true
	sc := trace.NewSpanContext(cfg)
	return sc, sc.IsValid()
}
