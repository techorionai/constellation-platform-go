package identity

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
)

// Audience is the token audience of the identity service.
const Audience = "identity"

// Config is the configuration of the identity client. Nest it in the config of
// the service with envPrefix:"IDENTITY__".
type Config struct {
	// URL is the base URL of the identity service, for example the internal URL
	// in Compose.
	URL string `env:"URL,required"`
	// Timeout is the time limit of one request. It includes the token request.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
}

// Option changes the behavior of [New].
type Option func(*options)

type options struct {
	base           http.RoundTripper
	tracerProvider trace.TracerProvider
	propagators    propagation.TextMapPropagator
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

// New makes a client of the identity service. The client adds a bearer token for
// the audience [Audience] and the given scopes to each request. The caller gives
// the scopes, and at least one is necessary.
//
// The client does not follow redirects, because a redirect target must not get
// the token.
func New(cfg Config, ts auth.TokenSource, scopes []string, opts ...Option) (*ClientWithResponses, error) {
	if cfg.URL == "" {
		return nil, errors.New("clients/identity: URL is required (IDENTITY__URL)")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("clients/identity: URL (IDENTITY__URL) is not a valid http or https URL")
	}
	if ts == nil {
		return nil, errors.New("clients/identity: token source is required")
	}
	scopes = slices.DeleteFunc(slices.Clone(scopes), func(s string) bool { return s == "" })
	if len(scopes) == 0 {
		return nil, errors.New("clients/identity: at least one scope is required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("clients/identity: Timeout (IDENTITY__TIMEOUT) must be more than 0")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	otelOpts := []otelhttp.Option{}
	if o.tracerProvider != nil {
		otelOpts = append(otelOpts, otelhttp.WithTracerProvider(o.tracerProvider))
	}
	if o.propagators != nil {
		otelOpts = append(otelOpts, otelhttp.WithPropagators(o.propagators))
	}
	rt := otelhttp.NewTransport(auth.BearerTransport(o.base, ts, Audience, scopes...), otelOpts...)
	httpClient := &http.Client{
		Transport: rt,
		Timeout:   cfg.Timeout,
		// The token must not go to a redirect target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c, err := NewClientWithResponses(cfg.URL, WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("clients/identity: %w", err)
	}
	return c, nil
}
