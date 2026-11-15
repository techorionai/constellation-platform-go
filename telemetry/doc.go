// Package telemetry sets up OpenTelemetry traces, metrics, and logs for a
// service.
//
// [Setup] creates the providers, sets the global tracer provider, meter
// provider, logger provider, and propagators, and sets the default slog logger.
// The exporters come from the standard OTEL_* environment variables. A signal
// gets an exporter only if the environment names one for it. Without an
// OTEL_* exporter variable, the service makes no outbound connection.
//
// The slog logger writes JSON to stdout. Each record in a span has the fields
// trace_id and span_id. When a log exporter is active, each record also goes to
// the OpenTelemetry log bridge.
//
// Call [Provider.Shutdown] before the process stops, to flush the data.
package telemetry
