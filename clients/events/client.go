package events

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/internal/limithttp"
)

// Audience is the token audience of the events service.
const Audience = "events"

// Config is the configuration of the events client. Nest it in the config of
// the service with envPrefix:"EVENTS__".
type Config struct {
	// URL is the base URL of the events service, for example the internal URL
	// in Compose.
	URL string `env:"URL,required"`
	// Timeout is the time limit of one request. It includes the token request.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
	// AllowInsecureHTTP allows the scheme http for URL. Use it only for local
	// compose and tests. A service must not set it in production.
	AllowInsecureHTTP bool `env:"ALLOW_INSECURE_HTTP" envDefault:"false"`
}

// Limits of the response body. The default is 1 MiB. A catalog reader can set a
// larger value with [WithMaxResponseBytes].
const (
	defaultMaxResponseBytes = 1 << 20
	minMaxResponseBytes     = 1 << 10
	maxMaxResponseBytes     = 64 << 20
)

// ErrResponseTooLarge is the error of a response body above the limit.
var ErrResponseTooLarge = errors.New("clients/events: response body is too large")

// Option changes the behavior of [New].
type Option func(*options)

type options struct {
	base           http.RoundTripper
	tracerProvider trace.TracerProvider
	propagators    propagation.TextMapPropagator
	maxResponse    int64
}

// WithTransport sets the base round tripper. The default is
// [http.DefaultTransport]. The client wraps it with the bearer token and with
// otelhttp.
func WithTransport(rt http.RoundTripper) Option { return func(o *options) { o.base = rt } }

// WithTracerProvider sets the tracer provider of the client. The default is the
// global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

// WithPropagators sets the propagator that adds the trace headers. The default
// is the global one.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// WithMaxResponseBytes sets the largest response body that the client reads. The
// default is 1 MiB. The range is 1 KiB to 64 MiB. The register response is
// small. A catalog page can be large, so a catalog reader sets a larger value or
// a small pageSize.
func WithMaxResponseBytes(n int64) Option { return func(o *options) { o.maxResponse = n } }

// New makes a client of the events service. The client adds a bearer token for
// the audience [Audience] and the given scopes to each request. The caller gives
// the scopes, and at least one is necessary.
//
// The client does not follow redirects, because a redirect target must not get
// the token.
func New(cfg Config, ts auth.TokenSource, scopes []string, opts ...Option) (*ClientWithResponses, error) {
	if cfg.URL == "" {
		return nil, errors.New("clients/events: URL is required (EVENTS__URL)")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("clients/events: URL (EVENTS__URL) is not a valid http or https URL")
	}
	if u.Scheme == "http" && !cfg.AllowInsecureHTTP {
		return nil, errors.New("clients/events: URL (EVENTS__URL) must use https (set EVENTS__ALLOW_INSECURE_HTTP only for local use)")
	}
	if ts == nil {
		return nil, errors.New("clients/events: token source is required")
	}
	scopes = slices.DeleteFunc(slices.Clone(scopes), func(s string) bool { return s == "" })
	if len(scopes) == 0 {
		return nil, errors.New("clients/events: at least one scope is required")
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("clients/events: Timeout (EVENTS__TIMEOUT) must be more than 0")
	}
	o := options{maxResponse: defaultMaxResponseBytes}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxResponse < minMaxResponseBytes || o.maxResponse > maxMaxResponseBytes {
		return nil, fmt.Errorf("clients/events: WithMaxResponseBytes must be from %d to %d, got %d", minMaxResponseBytes, maxMaxResponseBytes, o.maxResponse)
	}
	otelOpts := []otelhttp.Option{}
	if o.tracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(o.tracerProvider))
	}
	if o.propagators != nil {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(o.propagators))
	}
	rt := limithttp.Transport{Base: otelhttp.NewTransport(auth.BearerTransport(o.base, ts, Audience, scopes...), otelOpts...), Max: o.maxResponse, Err: ErrResponseTooLarge}
	httpClient := &http.Client{
		Transport: rt,
		Timeout:   cfg.Timeout,
		// The token must not go to a redirect target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c, err := NewClientWithResponses(cfg.URL, WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("clients/events: %w", err)
	}
	return c, nil
}
