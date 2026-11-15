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

// selectExporters selects the exporter of each signal from the environment.
// autoexport defaults to OTLP when no variable is set, so this function calls
// it only for a signal that the environment names.
func selectExporters(ctx context.Context) (exporters, error) {
	var (
		e    exporters
		errs []error
	)
	if wanted("TRACES") {
		s, err := autoexport.NewSpanExporter(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if !autoexport.IsNoneSpanExporter(s) {
			e.spans = s
		}
	}
	if wanted("METRICS") {
		r, err := autoexport.NewMetricReader(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if !autoexport.IsNoneMetricReader(r) {
			e.metrics = r
		}
	}
	if wanted("LOGS") {
		l, err := autoexport.NewLogExporter(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if !autoexport.IsNoneLogExporter(l) {
			e.logs = l
		}
	}
	if err := errors.Join(errs...); err != nil {
		return exporters{}, err
	}
	return e, nil
}
