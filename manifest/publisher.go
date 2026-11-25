package manifest

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/techorionai/constellation-platform-go/health"
)

const scopeName = "github.com/techorionai/constellation-platform-go/manifest"

// Outcomes of a publish attempt, the values of the attribute "outcome".
const (
	outcomeOK          = "ok"
	outcomeInvalid     = "invalid"
	outcomeRejected    = "rejected"
	outcomeDenied      = "denied"
	outcomeUnavailable = "unavailable"
)

// Option changes the behavior of [NewPublisher].
type Option func(*Publisher)

// WithLogger sets the logger. The default is [slog.Default].
func WithLogger(l *slog.Logger) Option { return func(p *Publisher) { p.log = l } }

// WithBackoff sets the shortest and the longest wait between two attempts. The
// default is 1 second and 1 minute. The wait grows by a factor of 2 and has
// jitter.
func WithBackoff(minWait, maxWait time.Duration) Option {
	return func(p *Publisher) { p.minWait, p.maxWait = minWait, maxWait }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *Publisher) { p.meterProvider = mp }
}

// Publisher publishes one manifest through a [Registry] until it succeeds.
type Publisher struct {
	registry      Registry
	m             PermissionManifest
	log           *slog.Logger
	minWait       time.Duration
	maxWait       time.Duration
	meterProvider metric.MeterProvider
	attempts      metric.Int64Counter
	sleep         func(context.Context, time.Duration) error
	published     atomic.Bool
	// invalid is the result of the check of the manifest in NewPublisher.
	invalid error
}

// NewPublisher makes a publisher for the manifest. It does not publish. Call
// [Publisher.Run]. It checks the manifest with [PermissionManifest.Validate].
// If the manifest is not valid, [Publisher.Run] returns the error and sends no
// request.
func NewPublisher(r Registry, m PermissionManifest, opts ...Option) *Publisher {
	p := &Publisher{
		registry:      r,
		m:             m,
		log:           slog.Default(),
		minWait:       time.Second,
		maxWait:       time.Minute,
		meterProvider: otel.GetMeterProvider(),
		invalid:       m.Validate(),
		sleep:         sleep,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.minWait <= 0 {
		p.minWait = time.Second
	}
	if p.maxWait < p.minWait {
		p.maxWait = p.minWait
	}
	p.attempts, _ = p.meterProvider.Meter(scopeName).Int64Counter("constellation.manifest.publish",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Number of manifest publish attempts, by outcome."))
	return p
}

// Run publishes the manifest and retries until it succeeds. It returns:
//
//   - nil after a successful publish.
//   - the *[InvalidError] when the identity service rejects the manifest. Run does
//     not retry, because the manifest is wrong. The checker stays "not ready".
//   - the validation error when the manifest is not valid. Run sends no request.
//   - [ErrRejected] (wrapped) when the registry refuses the request with a fixed
//     4xx status. Run does not retry, because the result stays the same.
//   - ctx.Err() when ctx ends.
//
// With [ErrDenied] and [ErrUnavailable], Run retries with backoff. An operator
// can fix the client config without a restart.
func (p *Publisher) Run(ctx context.Context) error {
	if p.invalid != nil {
		p.count(ctx, outcomeInvalid)
		p.log.ErrorContext(ctx, "manifest is not valid, no publish",
			slog.String("app", p.m.App), slog.Any("error", p.invalid))
		return p.invalid
	}
	wait := p.minWait
	for {
		err := p.registry.Publish(ctx, p.m)
		var invalid *InvalidError
		switch {
		case err == nil:
			p.count(ctx, outcomeOK)
			p.published.Store(true)
			p.log.InfoContext(ctx, "manifest published",
				slog.String("app", p.m.App),
				slog.Int("permissions", len(p.m.Permissions)),
				slog.Int("roles", len(p.m.DefaultRoles)))
			return nil
		case errors.As(err, &invalid):
			p.count(ctx, outcomeInvalid)
			p.log.ErrorContext(ctx, "manifest rejected, no retry",
				slog.String("app", p.m.App),
				slog.Any("codes", invalid.Codes()))
			return invalid
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrRejected):
			p.count(ctx, outcomeRejected)
			p.log.ErrorContext(ctx, "manifest publish rejected, no retry",
				slog.String("app", p.m.App), slog.Any("error", err))
			return err
		case errors.Is(err, ErrDenied):
			d := p.waitTime(err, wait)
			p.count(ctx, outcomeDenied)
			p.log.ErrorContext(ctx, "manifest publish denied, will retry",
				slog.String("app", p.m.App), slog.Any("error", err), slog.Duration("wait", d))
			if err := p.sleep(ctx, d); err != nil {
				return err
			}
			wait = min(wait*2, p.maxWait)
			continue
		default:
			d := p.waitTime(err, wait)
			p.count(ctx, outcomeUnavailable)
			p.log.WarnContext(ctx, "manifest publish failed, will retry",
				slog.String("app", p.m.App), slog.Any("error", err), slog.Duration("wait", d))
			if err := p.sleep(ctx, d); err != nil {
				return err
			}
			wait = min(wait*2, p.maxWait)
			continue
		}
	}
}

// waitTime returns the wait before the next attempt. It is the larger value of
// the backoff with jitter and the wait time of the server, up to 5 minutes.
func (p *Publisher) waitTime(err error, backoff time.Duration) time.Duration {
	d := jitter(backoff)
	if ra, ok := RetryAfter(err); ok {
		d = max(d, min(ra, maxRetryAfter))
	}
	return d
}

func (p *Publisher) count(ctx context.Context, outcome string) {
	p.attempts.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// jitter returns a random wait between d/2 and d.
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1) //nolint:gosec // jitter needs no secure random numbers
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Published reports whether the publish succeeded.
func (p *Publisher) Published() bool { return p.published.Load() }

// Checker returns a [health.Checker] named "manifest". It reports "not ready"
// until the publish succeeds.
func (p *Publisher) Checker() health.Checker { return checker{p} }

var errNotPublished = errors.New("manifest: not published yet")

type checker struct{ p *Publisher }

func (checker) Name() string { return "manifest" }

func (c checker) Check(context.Context) error {
	if c.p.published.Load() {
		return nil
	}
	return errNotPublished
}
