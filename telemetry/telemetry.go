package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ErrMissingServiceName is returned by [Setup] when Options.ServiceName is
// empty.
var ErrMissingServiceName = errors.New("telemetry: service name is required")

// Options are the settings for [Setup].
type Options struct {
	// ServiceName is the value of the resource attribute service.name. It is
	// required. It wins over OTEL_SERVICE_NAME.
	ServiceName string
	// ServiceVersion is the value of the resource attribute service.version.
	ServiceVersion string
	// LogLevel is the minimum level of the application logger. The zero value
	// is slog.LevelInfo. OTEL_LOG_LEVEL does not change it.
	LogLevel slog.Level
}

// Provider holds the OpenTelemetry providers that [Setup] creates.
type Provider struct {
	tracer *sdktrace.TracerProvider
	meter  *sdkmetric.MeterProvider
	logger *sdklog.LoggerProvider
	once   sync.Once
}

// Shutdown flushes and stops all providers. A second call returns nil.
func (p *Provider) Shutdown(ctx context.Context) error {
	var err error
	p.once.Do(func() {
		err = errors.Join(
			p.tracer.Shutdown(ctx),
			p.meter.Shutdown(ctx),
			p.logger.Shutdown(ctx),
		)
	})
	return err
}

// hooks lets the tests of this package add readers and change the log output.
type hooks struct {
	readers []sdkmetric.Reader
	stdout  io.Writer
}

// Setup creates the OpenTelemetry providers and sets the global state. See the
// package documentation.
func Setup(ctx context.Context, opts Options) (*Provider, error) {
	return setup(ctx, opts, hooks{})
}

func setup(ctx context.Context, opts Options, h hooks) (*Provider, error) {
	if opts.ServiceName == "" {
		return nil, ErrMissingServiceName
	}
	res, err := newResource(opts)
	if err != nil {
		return nil, err
	}
	exp, err := selectExporters(ctx)
	if err != nil {
		return nil, err
	}

	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if exp.spans != nil {
		traceOpts = append(traceOpts, sdktrace.WithBatcher(exp.spans))
	}
	tp := sdktrace.NewTracerProvider(traceOpts...)

	metricOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if exp.metrics != nil {
		metricOpts = append(metricOpts, sdkmetric.WithReader(exp.metrics))
	}
	for _, r := range h.readers {
		metricOpts = append(metricOpts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(metricOpts...)

	logOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	if exp.logs != nil {
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exp.logs)))
	}
	lp := sdklog.NewLoggerProvider(logOpts...)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, errors.Join(err, tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	stdout := h.stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	handlers := []slog.Handler{slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: opts.LogLevel})}
	if exp.logs != nil {
		handlers = append(handlers, otelslog.NewHandler(opts.ServiceName, otelslog.WithLoggerProvider(lp)))
	}
	slog.SetDefault(slog.New(newHandler(opts.LogLevel, handlers)))

	return &Provider{tracer: tp, meter: mp, logger: lp}, nil
}

// newResource builds the resource. The order of the merges gives this
// priority, from low to high: the defaults, a random service.instance.id, the
// environment (OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME), the options.
func newResource(opts Options) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.instance.id", uuid.NewString()),
	))
	if err != nil {
		return nil, err
	}
	res, err = resource.Merge(res, resource.Environment())
	if err != nil {
		return nil, err
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", opts.ServiceName)}
	if opts.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", opts.ServiceVersion))
	}
	return resource.Merge(res, resource.NewSchemaless(attrs...))
}
