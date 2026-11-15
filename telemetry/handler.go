package telemetry

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// handler sends each record to all child handlers. It adds trace_id and
// span_id at the top level of the record when the context has an active span,
// also for a logger that used WithGroup. The children get only the records at
// or above the level.
//
// The children never get WithAttrs or WithGroup calls. The handler keeps these
// calls in ops and builds the nested attributes for each record, so that the
// trace fields stay outside the groups.
type handler struct {
	level    slog.Level
	children []slog.Handler
	ops      []op
}

// op is one WithAttrs call (attrs) or one WithGroup call (group).
type op struct {
	attrs []slog.Attr
	group string
}

func newHandler(level slog.Level, children []slog.Handler) slog.Handler {
	return &handler{level: level, children: children}
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if len(h.ops) > 0 || sc.IsValid() {
		nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		var top []slog.Attr
		if sc.IsValid() {
			top = append(top,
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()),
			)
		}
		nr.AddAttrs(append(top, h.nest(r)...)...)
		r = nr
	}
	var errs []error
	for _, c := range h.children {
		if c.Enabled(ctx, r.Level) {
			errs = append(errs, c.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

// nest returns the attributes of the record, wrapped in the groups and
// preceded by the attributes of the WithAttrs calls, as slog defines.
func (h *handler) nest(r slog.Record) []slog.Attr {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	for i := len(h.ops) - 1; i >= 0; i-- {
		o := h.ops[i]
		if o.group != "" {
			if len(attrs) == 0 {
				continue
			}
			args := make([]any, len(attrs))
			for j, a := range attrs {
				args[j] = a
			}
			attrs = []slog.Attr{slog.Group(o.group, args...)}
			continue
		}
		attrs = append(append([]slog.Attr{}, o.attrs...), attrs...)
	}
	return attrs
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(op{attrs: attrs})
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(op{group: name})
}

func (h *handler) with(o op) slog.Handler {
	ops := make([]op, len(h.ops), len(h.ops)+1)
	copy(ops, h.ops)
	return &handler{level: h.level, children: h.children, ops: append(ops, o)}
}
