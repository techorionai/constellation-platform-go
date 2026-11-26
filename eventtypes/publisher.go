package eventtypes

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

const scopeName = "github.com/techorionai/constellation-platform-go/eventtypes"

// Outcomes of a registration attempt, the values of the attribute "outcome".
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

// Publisher registers one registration through a [Registry] until it succeeds.
type Publisher struct {
	registry      Registry
	reg           Registration
	log           *slog.Logger
	minWait       time.Duration
	maxWait       time.Duration
	meterProvider metric.MeterProvider
	attempts      metric.Int64Counter
	sleep         func(context.Context, time.Duration) error
	registered    atomic.Bool
	// invalid is the result of the check of the registration in NewPublisher.
	invalid error
}

// NewPublisher makes a publisher for the registration. It does not register.
// Call [Publisher.Run]. It checks the registration with
// [Registration.Validate]. If the registration is not valid, [Publisher.Run]
// returns the error and sends no request.
func NewPublisher(r Registry, reg Registration, opts ...Option) *Publisher {
	p := &Publisher{
		registry:      r,
		reg:           reg,
		log:           slog.Default(),
		minWait:       time.Second,
		maxWait:       time.Minute,
		meterProvider: otel.GetMeterProvider(),
		invalid:       reg.Validate(),
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
	p.attempts, _ = p.meterProvider.Meter(scopeName).Int64Counter("constellation.events.register",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Number of event type registration attempts, by outcome."))
	return p
}

// Run registers the event types and retries until it succeeds. It returns:
//
//   - nil after a successful registration.
//   - the *[InvalidError] when the events service rejects the registration. Run
//     does not retry, because the registration is wrong. The checker stays "not
//     ready".
//   - the validation error when the registration is not valid. Run sends no
//     request.
//   - [ErrRejected] (wrapped) when the registry refuses the request with a fixed
//     4xx status. Run does not retry, because the result stays the same.
//   - ctx.Err() when ctx ends.
//
// With [ErrDenied] and [ErrUnavailable], Run retries with backoff. An operator
// can fix the client config or give the role events:producer without a restart.
func (p *Publisher) Run(ctx context.Context) error {
	if p.invalid != nil {
		p.count(ctx, outcomeInvalid)
		p.log.ErrorContext(ctx, "event type registration is not valid, no request",
			slog.String("app", p.reg.App), slog.Any("error", p.invalid))
		return p.invalid
	}
	wait := p.minWait
	for {
		res, err := p.registry.Register(ctx, p.reg)
		var invalid *InvalidError
		switch {
		case err == nil:
			p.count(ctx, outcomeOK)
			p.registered.Store(true)
			p.log.InfoContext(ctx, "event types registered",
				slog.String("app", p.reg.App),
				slog.Int("version", res.Version),
				slog.Int("eventTypeCount", res.EventTypeCount))
			if res.EventTypeCount != len(p.reg.EventTypes) {
				p.log.WarnContext(ctx, "event type count of the events service differs from the file",
					slog.String("app", p.reg.App),
					slog.Int("eventTypeCount", res.EventTypeCount),
					slog.Int("fileEventTypeCount", len(p.reg.EventTypes)))
			}
			return nil
		case errors.As(err, &invalid):
			p.count(ctx, outcomeInvalid)
			p.log.ErrorContext(ctx, "event type registration rejected, no retry",
				slog.String("app", p.reg.App),
				slog.Any("codes", invalid.Codes()))
			return invalid
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrRejected):
			p.count(ctx, outcomeRejected)
			p.log.ErrorContext(ctx, "event type registration rejected, no retry",
				slog.String("app", p.reg.App), slog.Any("error", err))
			return err
		case errors.Is(err, ErrDenied):
			d := p.waitTime(err, wait)
			p.count(ctx, outcomeDenied)
			p.log.ErrorContext(ctx, "event type registration denied, will retry",
				slog.String("app", p.reg.App), slog.Any("error", err), slog.Duration("wait", d))
			if err := p.sleep(ctx, d); err != nil {
				return err
			}
			wait = min(wait*2, p.maxWait)
		default:
			d := p.waitTime(err, wait)
			p.count(ctx, outcomeUnavailable)
			p.log.WarnContext(ctx, "event type registration failed, will retry",
				slog.String("app", p.reg.App), slog.Any("error", err), slog.Duration("wait", d))
			if err := p.sleep(ctx, d); err != nil {
				return err
			}
			wait = min(wait*2, p.maxWait)
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

// Registered reports whether the registration succeeded.
func (p *Publisher) Registered() bool { return p.registered.Load() }

// Checker returns a [health.Checker] named "event_types". It reports "not
// ready" until the registration succeeds.
func (p *Publisher) Checker() health.Checker { return checker{p} }

var errNotRegistered = errors.New("eventtypes: not registered yet")

type checker struct{ p *Publisher }

func (checker) Name() string { return "event_types" }

func (c checker) Check(context.Context) error {
	if c.p.registered.Load() {
		return nil
	}
	return errNotRegistered
}
