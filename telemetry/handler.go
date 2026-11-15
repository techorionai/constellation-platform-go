package telemetry

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// handler sends each record to all child handlers. It adds trace_id and
// span_id to the record when the context has an active span. The children get
// only the records at or above the level.
type handler struct {
	level    slog.Level
	children []slog.Handler
}

func newHandler(level slog.Level, children []slog.Handler) slog.Handler {
	return &handler{level: level, children: children}
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	var errs []error
	for _, c := range h.children {
		if c.Enabled(ctx, r.Level) {
			errs = append(errs, c.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, len(h.children))
	for i, c := range h.children {
		children[i] = c.WithAttrs(attrs)
	}
	return &handler{level: h.level, children: children}
}

func (h *handler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, len(h.children))
	for i, c := range h.children {
		children[i] = c.WithGroup(name)
	}
	return &handler{level: h.level, children: children}
}
