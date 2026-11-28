package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

const scopeName = "github.com/techorionai/constellation-platform-go/auth"

type options struct {
	meterProvider    metric.MeterProvider
	resourceMetadata string
}

// WithResourceMetadata sets the URL of the protected resource metadata of
// RFC 9728. [Middleware] adds the parameter resource_metadata="<url>" to the
// WWW-Authenticate header of each 401 response. A 503 response does not change.
// With no option, the header does not change.
func WithResourceMetadata(url string) Option {
	return func(o *options) { o.resourceMetadata = url }
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
				writeVerifyError(ctx, w, err, o.resourceMetadata)
				return
			}
			ctx = ContextWithPrincipal(ctx, p)
			spanAttrs := []attribute.KeyValue{
				attribute.String("enduser.id", p.ID),
				attribute.String("constellation.principal.type", string(p.Type)),
				attribute.String("constellation.client.id", p.ClientID),
				attribute.String("constellation.principal.token_type", string(p.TokenType.OrAccess())),
			}
			logAttrs := []slog.Attr{
				slog.String("principal.id", p.ID),
				slog.String("principal.type", string(p.Type)),
				slog.String("client.id", p.ClientID),
				slog.String("principal.token_type", string(p.TokenType.OrAccess())),
			}
			if p.Act != nil {
				spanAttrs = append(spanAttrs,
					attribute.String("constellation.principal.act.id", p.Act.ID),
					attribute.String("constellation.principal.act.type", string(p.Act.Type)))
				logAttrs = append(logAttrs,
					slog.String("principal.act.id", p.Act.ID),
					slog.String("principal.act.type", string(p.Act.Type)))
			}
			trace.SpanFromContext(ctx).SetAttributes(spanAttrs...)
			ctx = telemetry.ContextWithAttrs(ctx, logAttrs...)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func verifyRequest(ctx context.Context, v Verifier, r *http.Request) (Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) > 1 {
		// Another component may read another header. Do not guess.
		return Principal{}, fmt.Errorf("auth: more than one Authorization header: %w", ErrInvalidToken)
	}
	if len(values) == 0 {
		return Principal{}, ErrNoToken
	}
	token, ok := bearerToken(values[0])
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

func writeVerifyError(ctx context.Context, w http.ResponseWriter, err error, resourceMetadata string) {
	param := ""
	if resourceMetadata != "" {
		param = `resource_metadata="` + resourceMetadata + `"`
	}
	switch {
	case errors.Is(err, ErrNoToken):
		h := "Bearer"
		if param != "" {
			h += " " + param
		}
		w.Header().Set("WWW-Authenticate", h)
		writeBody(ctx, w, problem{Title: "Authentication required", Status: http.StatusUnauthorized, Code: "unauthorized"})
	case errors.Is(err, ErrKeysUnavailable), errors.Is(err, ErrIntrospectionUnavailable):
		slog.WarnContext(ctx, "auth: verification unavailable", slog.Any("error", err))
		w.Header().Set("Retry-After", retryAfterSeconds(err))
		writeBody(ctx, w, problem{Title: "Authentication unavailable", Status: http.StatusServiceUnavailable, Code: "auth_unavailable"})
	default:
		h := `Bearer error="invalid_token"`
		if param != "" {
			h += ", " + param
		}
		w.Header().Set("WWW-Authenticate", h)
		writeBody(ctx, w, problem{Title: "Authentication required", Status: http.StatusUnauthorized, Code: "unauthorized"})
	}
}

// retryAfterSeconds returns the Retry-After header of a 503. It is the wait time
// of the error in whole seconds, rounded up, from 1 to 60. Without a wait time
// it is 5.
func retryAfterSeconds(err error) string {
	d, ok := RetryAfter(err)
	if !ok {
		return "5"
	}
	secs := int((d + time.Second - 1) / time.Second)
	return strconv.Itoa(min(max(secs, 1), 60))
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
				WriteInsufficientScope(ctx, w, scopes, missing, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteInsufficientScope writes the 403 response for a missing scope. The
// WWW-Authenticate header has error="insufficient_scope" and the required
// scopes. It has the resource_metadata parameter if resourceMetadata is not
// empty. The body is a problem document with the code insufficient_scope and
// the missing scopes.
func WriteInsufficientScope(ctx context.Context, w http.ResponseWriter, required, missing []string, resourceMetadata string) {
	h := `Bearer error="insufficient_scope", scope="` + strings.Join(required, " ") + `"`
	if resourceMetadata != "" {
		h += `, resource_metadata="` + resourceMetadata + `"`
	}
	w.Header().Set("WWW-Authenticate", h)
	writeBody(ctx, w, problem{
		Title: "Missing scopes", Status: http.StatusForbidden,
		Code: "insufficient_scope", MissingScopes: missing,
	})
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
