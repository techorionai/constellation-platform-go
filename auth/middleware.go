package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

const scopeName = "github.com/techorionai/constellation-platform-go/auth"

type options struct {
	meterProvider metric.MeterProvider
}

// Option changes the behavior of [Middleware].
type Option func(*options)

// WithMeterProvider sets the meter provider. The default is the global meter
// provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// Middleware returns the middleware that verifies the bearer token of each
// request. On success it puts the [Principal] in the request context, adds the
// principal attributes to the active span, and adds the principal fields to the
// log records of the request. On failure it writes a problem document and does
// not call the next handler. It counts each result in the metric
// constellation.auth.verify.
func Middleware(v Verifier, opts ...Option) func(http.Handler) http.Handler {
	o := options{meterProvider: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(&o)
	}
	counter, _ := o.meterProvider.Meter(scopeName).Int64Counter("constellation.auth.verify",
		metric.WithUnit("{verification}"),
		metric.WithDescription("Number of token verifications, by outcome."))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, err := verifyRequest(ctx, v, r)
			counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", Outcome(err))))
			if err != nil {
				writeVerifyError(ctx, w, err)
				return
			}
			ctx = ContextWithPrincipal(ctx, p)
			trace.SpanFromContext(ctx).SetAttributes(
				attribute.String("enduser.id", p.ID),
				attribute.String("constellation.principal.type", string(p.Type)),
				attribute.String("constellation.client.id", p.ClientID),
			)
			ctx = telemetry.ContextWithAttrs(ctx,
				slog.String("principal.id", p.ID),
				slog.String("principal.type", string(p.Type)),
				slog.String("client.id", p.ClientID),
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func verifyRequest(ctx context.Context, v Verifier, r *http.Request) (Principal, error) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return Principal{}, ErrNoToken
	}
	return v.Verify(ctx, token)
}

// bearerToken returns the token of an Authorization header. The scheme is case
// insensitive (RFC 7235).
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func writeVerifyError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoToken):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeBody(ctx, w, problem{Title: "Authentication required", Status: http.StatusUnauthorized, Code: "unauthorized"})
	case errors.Is(err, ErrKeysUnavailable):
		slog.WarnContext(ctx, "auth: signing keys unavailable", slog.Any("error", err))
		w.Header().Set("Retry-After", "5")
		writeBody(ctx, w, problem{Title: "Authentication unavailable", Status: http.StatusServiceUnavailable, Code: "auth_unavailable"})
	default:
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeBody(ctx, w, problem{Title: "Authentication required", Status: http.StatusUnauthorized, Code: "unauthorized"})
	}
}

// RequireScopes returns the middleware that lets a request pass only if the
// principal has all the scopes. It fails closed: a context with no principal
// gives a 500 problem with the code auth_not_configured, because the chain has
// no [Middleware].
func RequireScopes(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := PrincipalFromContext(ctx)
			if !ok {
				slog.ErrorContext(ctx, "auth: RequireScopes has no principal, the chain has no auth.Middleware")
				writeBody(ctx, w, problem{Title: "Authentication not configured", Status: http.StatusInternalServerError, Code: "auth_not_configured"})
				return
			}
			if missing := p.MissingScopes(scopes...); len(missing) > 0 {
				w.Header().Set("WWW-Authenticate",
					`Bearer error="insufficient_scope", scope="`+strings.Join(scopes, " ")+`"`)
				writeBody(ctx, w, problem{
					Title: "Missing scopes", Status: http.StatusForbidden,
					Code: "insufficient_scope", MissingScopes: missing,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// problem has the field names of the Problem model of constellation-contracts.
// Detail is never set from an error text.
type problem struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Status        int      `json:"status"`
	Code          string   `json:"code"`
	RequestID     string   `json:"requestId,omitempty"`
	TraceID       string   `json:"traceId,omitempty"`
	MissingScopes []string `json:"missingScopes,omitempty"`
}

func writeBody(ctx context.Context, w http.ResponseWriter, p problem) {
	p.Type = "about:blank"
	p.RequestID = requestid.FromContext(ctx)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		p.TraceID = sc.TraceID().String()
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.WarnContext(ctx, "auth: write problem failed", slog.Any("error", err))
	}
}
