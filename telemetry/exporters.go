package telemetry

import (
	"context"
	"errors"
	"os"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// exporters holds the selected exporter of each signal. A nil value means that
// the signal has no exporter.
type exporters struct {
	spans   sdktrace.SpanExporter
	metrics sdkmetric.Reader
	logs    sdklog.Exporter
}

// wanted returns true if the environment names an exporter for the signal.
// signal is TRACES, METRICS, or LOGS.
func wanted(signal string) bool {
	for _, name := range []string{
		"OTEL_" + signal + "_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT",
	} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// factories make the exporter of each signal. A factory returns nil if the
// environment selects no exporter. The tests of this package replace them.
type factories struct {
	spans   func(context.Context) (sdktrace.SpanExporter, error)
	metrics func(context.Context) (sdkmetric.Reader, error)
	logs    func(context.Context) (sdklog.Exporter, error)
}

var autoFactories = factories{
	spans: func(ctx context.Context) (sdktrace.SpanExporter, error) {
		s, err := autoexport.NewSpanExporter(ctx)
		if err != nil || autoexport.IsNoneSpanExporter(s) {
			return nil, err
		}
		return s, nil
	},
	metrics: func(ctx context.Context) (sdkmetric.Reader, error) {
		r, err := autoexport.NewMetricReader(ctx)
		if err != nil || autoexport.IsNoneMetricReader(r) {
			return nil, err
		}
		return r, nil
	},
	logs: func(ctx context.Context) (sdklog.Exporter, error) {
		l, err := autoexport.NewLogExporter(ctx)
		if err != nil || autoexport.IsNoneLogExporter(l) {
			return nil, err
		}
		return l, nil
	},
}

// shutdown stops the exporters that exist.
func (e exporters) shutdown(ctx context.Context) error {
	var errs []error
	if e.spans != nil {
		errs = append(errs, e.spans.Shutdown(ctx))
	}
	if e.metrics != nil {
		errs = append(errs, e.metrics.Shutdown(ctx))
	}
	if e.logs != nil {
		errs = append(errs, e.logs.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// selectExporters selects the exporter of each signal from the environment.
// autoexport defaults to OTLP when no variable is set, so this function calls
// it only for a signal that the environment names. If one exporter fails, the
// function shuts down the exporters that it already made.
func selectExporters(ctx context.Context) (exporters, error) {
	return selectWith(ctx, autoFactories)
}

func selectWith(ctx context.Context, f factories) (exporters, error) {
	var (
		e    exporters
		errs []error
	)
	if wanted("TRACES") {
		s, err := f.spans(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			e.spans = s
		}
	}
	if wanted("METRICS") {
		r, err := f.metrics(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			e.metrics = r
		}
	}
	if wanted("LOGS") {
		l, err := f.logs(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			e.logs = l
		}
	}
	if err := errors.Join(errs...); err != nil {
		return exporters{}, errors.Join(err, e.shutdown(ctx))
	}
	return e, nil
}
