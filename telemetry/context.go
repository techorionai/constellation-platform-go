package telemetry

import (
	"context"
	"log/slog"
)

type attrsKey struct{}

// ContextWithAttrs returns a context that carries log attributes. The log
// handler of [Setup] adds these attributes at the top level of each record that
// a caller writes with that context, for example with [slog.InfoContext].
//
// The new attributes go after the attributes that the parent context already
// has. A middleware uses this hook to put the request ID and the principal on
// the log records of a request. The telemetry package does not import the
// packages of these middleware.
func ContextWithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	old, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	all := make([]slog.Attr, 0, len(old)+len(attrs))
	all = append(all, old...)
	all = append(all, attrs...)
	return context.WithValue(ctx, attrsKey{}, all)
}

func attrsFromContext(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	return attrs
}
