package requestid

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/telemetry"
)

// Header is the name of the request ID header.
const Header = "X-Request-ID"

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type ctxKey struct{}

// FromContext returns the request ID of the context, or an empty string.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// ContextWithID returns a context that carries the request ID.
func ContextWithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Middleware returns the middleware that sets the request ID. See the package
// documentation.
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(Header)
			if !validID.MatchString(id) {
				id = newID()
			}
			w.Header().Set(Header, id)
			ctx := ContextWithID(r.Context(), id)
			ctx = telemetry.ContextWithAttrs(ctx, slog.String("request.id", id))
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("http.request.id", id))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
