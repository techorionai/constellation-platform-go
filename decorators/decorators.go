package decorators

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/executor"
)

const scopeName = "github.com/techorionai/constellation-platform-go/decorators"

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)

// durationBuckets are the bucket boundaries of the histograms with the unit s.
var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type settings struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	logger         *slog.Logger
	errorLevel     func(error) slog.Level
	classifier     func(error) bool
}

// Option changes the settings of a decorator.
type Option func(*settings)

// WithTracerProvider sets the tracer provider. The default is the global
// tracer provider at the time of the call of the constructor.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(s *settings) { s.tracerProvider = tp }
}

// WithMeterProvider sets the meter provider. The default is the global meter
// provider at the time of the call of the constructor.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(s *settings) { s.meterProvider = mp }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *settings) { s.logger = l }
}

// WithErrorLevel sets the log level of a failed call. The default level is
// slog.LevelError. Use it to log an expected error, for example a validation
// error, at the info level. It applies to the logging decorators.
func WithErrorLevel(f func(error) slog.Level) Option {
	return func(s *settings) { s.errorLevel = f }
}

// WithErrorClassifier selects the errors that are failures. The function
// returns false for an expected error, for example a validation error or "not
// found". For such an error, the span has the error event and keeps the status
// Unset, and outcome is expected_error. Without this option, each error is a
// failure. It applies to the tracing decorators.
func WithErrorClassifier(f func(error) bool) Option {
	return func(s *settings) { s.classifier = f }
}

func newSettings(name string, opts []Option) settings {
	if !namePattern.MatchString(name) {
		panic(fmt.Sprintf("decorators: invalid executor name %q, it must match %s", name, namePattern))
	}
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.tracerProvider == nil {
		s.tracerProvider = otel.GetTracerProvider()
	}
	if s.meterProvider == nil {
		s.meterProvider = otel.GetMeterProvider()
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

type tracing[P, R any] struct {
	base     executor.ExecutorWithReturn[P, R]
	name     string
	nameAttr attribute.KeyValue
	tracer   trace.Tracer
	duration metric.Float64Histogram
	classify func(error) bool
}

// ApplyTracing wraps base. Each call makes the span executor:<name> with the
// attribute executor.name, and records the metric executor.duration (histogram,
// unit s) with the attributes executor.name and outcome (ok, error, or
// expected_error). It returns the error of base without a change. It panics if
// name does not match ^[A-Za-z][A-Za-z0-9_.]{0,63}$.
func ApplyTracing[P, R any](base executor.ExecutorWithReturn[P, R], name string, opts ...Option) executor.ExecutorWithReturn[P, R] {
	s := newSettings(name, opts)
	// A failed instrument creation gives a no-op instrument in the SDK.
	hist, _ := s.meterProvider.Meter(scopeName).Float64Histogram("executor.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one executor call."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	return &tracing[P, R]{
		base:     base,
		name:     name,
		nameAttr: attribute.String("executor.name", name),
		tracer:   s.tracerProvider.Tracer(scopeName),
		duration: hist,
		classify: s.classifier,
	}
}

func (t *tracing[P, R]) Execute(ctx context.Context, params P) (R, error) {
	ctx, span := t.tracer.Start(ctx, "executor:"+t.name, trace.WithAttributes(t.nameAttr))
	defer span.End()
	start := time.Now()
	res, err := t.base.Execute(ctx, params)
	outcome := "ok"
	if err != nil {
		span.RecordError(err)
		if t.classify == nil || t.classify(err) {
			outcome = "error"
			span.SetStatus(codes.Error, err.Error())
		} else {
			outcome = "expected_error"
		}
	}
	if t.duration != nil {
		t.duration.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(),
			metric.WithAttributes(t.nameAttr, attribute.String("outcome", outcome)))
	}
	return res, err
}

type logging[P, R any] struct {
	base   executor.ExecutorWithReturn[P, R]
	name   string
	logger *slog.Logger
	level  func(error) slog.Level
}

// ApplyLogging wraps base. On success it writes a debug record "executor
// completed" with executor.name and duration_ms. On an error it writes a record
// "executor failed" with executor.name, duration_ms, and error. The level is
// error, or the level that [WithErrorLevel] gives. It does not log the params or
// the result. It returns the error of base without a change. It panics if name
// does not match ^[A-Za-z][A-Za-z0-9_.]{0,63}$.
func ApplyLogging[P, R any](base executor.ExecutorWithReturn[P, R], name string, opts ...Option) executor.ExecutorWithReturn[P, R] {
	s := newSettings(name, opts)
	return &logging[P, R]{base: base, name: name, logger: s.logger, level: s.errorLevel}
}

func (l *logging[P, R]) Execute(ctx context.Context, params P) (R, error) {
	start := time.Now()
	res, err := l.base.Execute(ctx, params)
	ms := float64(time.Since(start)) / float64(time.Millisecond)
	if err == nil {
		l.logger.LogAttrs(ctx, slog.LevelDebug, "executor completed",
			slog.String("executor.name", l.name), slog.Float64("duration_ms", ms))
		return res, nil
	}
	level := slog.LevelError
	if l.level != nil {
		level = l.level(err)
	}
	l.logger.LogAttrs(ctx, level, "executor failed",
		slog.String("executor.name", l.name), slog.Float64("duration_ms", ms), slog.Any("error", err))
	return res, err
}

// noResult adapts an [executor.Executor] to [executor.ExecutorWithReturn].
type noResult[P any] struct{ base executor.Executor[P] }

func (n noResult[P]) Execute(ctx context.Context, params P) (struct{}, error) {
	return struct{}{}, n.base.Execute(ctx, params)
}

// withoutResult adapts [executor.ExecutorWithReturn] back to [executor.Executor].
type withoutResult[P any] struct {
	base executor.ExecutorWithReturn[P, struct{}]
}

func (w withoutResult[P]) Execute(ctx context.Context, params P) error {
	_, err := w.base.Execute(ctx, params)
	return err
}

// ApplyTracingNoResult is [ApplyTracing] for an [executor.Executor].
func ApplyTracingNoResult[P any](base executor.Executor[P], name string, opts ...Option) executor.Executor[P] {
	return withoutResult[P]{ApplyTracing[P, struct{}](noResult[P]{base}, name, opts...)}
}

// ApplyLoggingNoResult is [ApplyLogging] for an [executor.Executor].
func ApplyLoggingNoResult[P any](base executor.Executor[P], name string, opts ...Option) executor.Executor[P] {
	return withoutResult[P]{ApplyLogging[P, struct{}](noResult[P]{base}, name, opts...)}
}
