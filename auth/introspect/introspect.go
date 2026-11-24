package introspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/internal/lru"
)

const (
	scopeName = "github.com/techorionai/constellation-platform-go/auth/introspect"

	assertionType    = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	maxResponseBytes = 64 << 10
	logInterval      = 60 * time.Second

	// tokenPrefix and tokenSuffixLen give the format of a personal API token:
	// cpat_ and 43 characters of the base64url alphabet (48 bytes).
	tokenPrefix    = auth.APITokenPrefix
	tokenSuffixLen = 43

	maxPermissionKeyLen = 128
	maxScopeLen         = 4096
)

// permissionKey is the pattern of PermissionKey in constellation-contracts for
// keys with three segments.
var permissionKey = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)

// appKey is the pattern of an app key. It is the first segment of a permission
// key.
var appKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Outcomes of a verification, the values of the attribute "outcome".
const (
	outcomeActive         = "active"
	outcomeInactive       = "inactive"
	outcomeCachedActive   = "cached_active"
	outcomeCachedInactive = "cached_inactive"
	outcomeUnavailable    = "unavailable"
	outcomeClientRejected = "client_rejected"
	outcomeInvalidResp    = "invalid_response"
	outcomeInvalidFormat  = "invalid_format"
	outcomeOverloaded     = "overloaded"
)

// Config is the configuration of the verifier. Nest it in the config of the
// service with envPrefix:"AUTH__INTROSPECT__". A Config that the code builds
// must set every field that has a default, because the zero value is not in the
// range.
type Config struct {
	// Issuer is the issuer URL of the identity service.
	Issuer string `env:"ISSUER,required"`
	// IntrospectURL is the URL of the introspection endpoint. The default is
	// <Issuer>/oauth/introspect. In Compose it is the internal URL.
	IntrospectURL string `env:"URL"`
	// Audience is the app of the service. It must be the app of the service
	// client that calls introspection.
	Audience string `env:"AUDIENCE,required"`
	// CacheTTL is the longest time that an active result stays in the cache.
	// The range is 1s to 60s.
	CacheTTL time.Duration `env:"CACHE_TTL" envDefault:"30s"`
	// InactiveTTL is the time that an inactive result stays in the cache. The
	// range is 0 to 30s. 0 means no cache for inactive results.
	InactiveTTL time.Duration `env:"INACTIVE_TTL" envDefault:"5s"`
	// CacheSize is the maximum number of active entries. The range is 100 to
	// 1000000.
	CacheSize int `env:"CACHE_SIZE" envDefault:"10000"`
	// InactiveCacheSize is the maximum number of inactive entries. The range is
	// 10 to 100000. It is used only if InactiveTTL is more than 0.
	InactiveCacheSize int `env:"INACTIVE_CACHE_SIZE" envDefault:"1000"`
	// MaxConcurrent is the maximum number of identity calls at the same time.
	// The range is 1 to 256.
	MaxConcurrent int `env:"MAX_CONCURRENT" envDefault:"16"`
	// HTTPTimeout is the time limit of one introspection request. The range is
	// 1s to 30s.
	HTTPTimeout time.Duration `env:"HTTP_TIMEOUT" envDefault:"5s"`
	// AllowInsecureHTTP allows the scheme http for Issuer and IntrospectURL. Use
	// it only for local compose and tests. A service must not set it in
	// production.
	AllowInsecureHTTP bool `env:"ALLOW_INSECURE_HTTP" envDefault:"false"`
}

const envPrefix = "AUTH__INTROSPECT__"

func checkURL(name, variable, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("auth/introspect: %s (%s%s) is not a valid http or https URL", name, envPrefix, variable)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	case u.Scheme == "http":
		return fmt.Errorf("auth/introspect: %s (%s%s) must use https (set %sALLOW_INSECURE_HTTP only for local use)", name, envPrefix, variable, envPrefix)
	default:
		return fmt.Errorf("auth/introspect: %s (%s%s) is not a valid http or https URL", name, envPrefix, variable)
	}
	return nil
}

func rangeErr(name, variable, want string) error {
	return fmt.Errorf("auth/introspect: %s (%s%s) must be %s", name, envPrefix, variable, want)
}

func (c Config) resolve() (Config, error) {
	switch {
	case c.Issuer == "":
		return c, errors.New("auth/introspect: Issuer is required (" + envPrefix + "ISSUER)")
	case c.Audience == "":
		return c, errors.New("auth/introspect: Audience is required (" + envPrefix + "AUDIENCE)")
	case !appKey.MatchString(c.Audience):
		return c, errors.New("auth/introspect: Audience (" + envPrefix + "AUDIENCE) is not a valid app key")
	}
	if c.IntrospectURL == "" {
		c.IntrospectURL = strings.TrimRight(c.Issuer, "/") + "/oauth/introspect"
	}
	if err := checkURL("Issuer", "ISSUER", c.Issuer, c.AllowInsecureHTTP); err != nil {
		return c, err
	}
	if err := checkURL("IntrospectURL", "URL", c.IntrospectURL, c.AllowInsecureHTTP); err != nil {
		return c, err
	}
	switch {
	case c.CacheTTL < time.Second || c.CacheTTL > time.Minute:
		return c, rangeErr("CacheTTL", "CACHE_TTL", "from 1s to 60s")
	case c.InactiveTTL < 0 || c.InactiveTTL > 30*time.Second:
		return c, rangeErr("InactiveTTL", "INACTIVE_TTL", "from 0 to 30s")
	case c.CacheSize < 100 || c.CacheSize > 1_000_000:
		return c, rangeErr("CacheSize", "CACHE_SIZE", "from 100 to 1000000")
	case c.InactiveTTL > 0 && (c.InactiveCacheSize < 10 || c.InactiveCacheSize > 100_000):
		return c, rangeErr("InactiveCacheSize", "INACTIVE_CACHE_SIZE", "from 10 to 100000")
	case c.MaxConcurrent < 1 || c.MaxConcurrent > 256:
		return c, rangeErr("MaxConcurrent", "MAX_CONCURRENT", "from 1 to 256")
	case c.HTTPTimeout < time.Second || c.HTTPTimeout > 30*time.Second:
		return c, rangeErr("HTTPTimeout", "HTTP_TIMEOUT", "from 1s to 30s")
	}
	return c, nil
}

// Option changes the behavior of [New].
type Option func(*Verifier)

// WithHTTPClient sets the HTTP client. The verifier uses a copy that does not
// follow redirects, has the transport wrapped with otelhttp, and has the timeout
// HTTPTimeout if the client has none.
func WithHTTPClient(c *http.Client) Option { return func(v *Verifier) { v.client = c } }

// WithClock sets the clock. The default is time.Now. A test uses it to move the
// time.
func WithClock(now func() time.Time) Option { return func(v *Verifier) { v.clock = now } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(v *Verifier) { v.meterProvider = mp }
}

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(v *Verifier) { v.tracerProvider = tp }
}

type activeEntry struct {
	principal auth.Principal
}

// Verifier is an [auth.Verifier] for personal API tokens. It checks a token
// with the introspection endpoint of the identity service (RFC 7662) and keeps
// the result in memory. It is safe for concurrent use.
type Verifier struct {
	cfg       Config
	assertion auth.ClientAssertion

	client         *http.Client
	clock          func() time.Time
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	tracer         trace.Tracer
	results        metric.Int64Counter
	duration       metric.Float64Histogram

	active   *lru.Cache[string, activeEntry]
	inactive *lru.Cache[string, struct{}]
	slots    chan struct{}
	flight   singleflight.Group

	logMu   sync.Mutex
	lastLog map[string]time.Time
}

var _ auth.Verifier = (*Verifier)(nil)

// New makes a verifier. It checks the config. The assertion is the source of the
// client assertion of the service client that calls introspection.
func New(cfg Config, assertion auth.ClientAssertion, opts ...Option) (*Verifier, error) {
	rc, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	if assertion == nil {
		return nil, errors.New("auth/introspect: the client assertion is required")
	}
	v := &Verifier{
		cfg:            rc,
		assertion:      assertion,
		clock:          time.Now,
		meterProvider:  otel.GetMeterProvider(),
		tracerProvider: otel.GetTracerProvider(),
		active:         lru.New[string, activeEntry](rc.CacheSize),
		slots:          make(chan struct{}, rc.MaxConcurrent),
		lastLog:        make(map[string]time.Time),
	}
	if rc.InactiveTTL > 0 {
		v.inactive = lru.New[string, struct{}](rc.InactiveCacheSize)
	}
	for _, opt := range opts {
		opt(v)
	}
	var c http.Client
	if v.client != nil {
		c = *v.client
	}
	if c.Timeout == 0 {
		c.Timeout = rc.HTTPTimeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(v.tracerProvider),
		otelhttp.WithMeterProvider(v.meterProvider))
	v.client = &c
	v.tracer = v.tracerProvider.Tracer(scopeName)
	meter := v.meterProvider.Meter(scopeName)
	v.results, _ = meter.Int64Counter("constellation.auth.introspect",
		metric.WithUnit("{verification}"),
		metric.WithDescription("Number of API token verifications, by outcome."))
	v.duration, _ = meter.Float64Histogram("constellation.auth.introspect.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of the introspection HTTP call."))
	return v, nil
}

// validFormat reports whether the token has the format of a personal API token.
func validFormat(token string) bool {
	if len(token) != len(tokenPrefix)+tokenSuffixLen || !strings.HasPrefix(token, tokenPrefix) {
		return false
	}
	for i := len(tokenPrefix); i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

type result struct {
	principal auth.Principal
	err       error
}

// Verify checks the API token and returns the principal. Errors:
// [auth.ErrInvalidToken] for a token that is not valid, and
// [auth.ErrIntrospectionUnavailable] when the verifier cannot say if the token
// is valid. The error text never has the token.
func (v *Verifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	if !validFormat(token) {
		v.count(ctx, outcomeInvalidFormat)
		return auth.Principal{}, fmt.Errorf("auth/introspect: token format: %w", auth.ErrInvalidToken)
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	now := v.clock()
	if e, ok := v.active.Get(key, now); ok {
		v.count(ctx, outcomeCachedActive)
		return clonePrincipal(e.principal), nil
	}
	if v.inactive != nil {
		if _, ok := v.inactive.Get(key, now); ok {
			v.count(ctx, outcomeCachedInactive)
			return auth.Principal{}, fmt.Errorf("auth/introspect: token is not active: %w", auth.ErrInvalidToken)
		}
	}
	// The shared request does not stop when one caller stops. The HTTP client
	// timeout limits it.
	ch := v.flight.DoChan(key, func() (any, error) {
		select {
		case v.slots <- struct{}{}:
			defer func() { <-v.slots }()
		default:
			v.count(ctx, outcomeOverloaded)
			v.logLimited("overloaded", slog.LevelWarn, "auth/introspect: too many introspection calls at the same time, the call is refused")
			return result{err: fmt.Errorf("auth/introspect: overloaded: %w", auth.ErrIntrospectionUnavailable)}, nil
		}
		p, err := v.fetch(context.WithoutCancel(ctx), key, token)
		return result{principal: p, err: err}, nil
	})
	select {
	case <-ctx.Done():
		return auth.Principal{}, ctx.Err()
	case r := <-ch:
		res := r.Val.(result)
		if res.err != nil {
			return auth.Principal{}, res.err
		}
		return clonePrincipal(res.principal), nil
	}
}

func clonePrincipal(p auth.Principal) auth.Principal {
	p.Scopes = append([]string(nil), p.Scopes...)
	return p
}

func (v *Verifier) count(ctx context.Context, outcome string) {
	v.results.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// logLimited writes one record at most each 60 s for each kind.
func (v *Verifier) logLimited(kind string, level slog.Level, msg string, attrs ...slog.Attr) {
	now := v.clock()
	v.logMu.Lock()
	last, seen := v.lastLog[kind]
	if seen && now.Sub(last) < logInterval {
		v.logMu.Unlock()
		return
	}
	v.lastLog[kind] = now
	v.logMu.Unlock()
	slog.LogAttrs(context.Background(), level, msg, attrs...)
}

type introspectResponse struct {
	Active        *bool    `json:"active"`
	Aud           string   `json:"aud"`
	Exp           *float64 `json:"exp"`
	Sub           string   `json:"sub"`
	PrincipalType string   `json:"principal_type"`
	TokenType     string   `json:"token_type"`
	Scope         string   `json:"scope"`
	Error         string   `json:"error"`
}

func (v *Verifier) fetch(ctx context.Context, key, token string) (auth.Principal, error) {
	ctx, span := v.tracer.Start(ctx, "auth.introspect.request")
	defer span.End()

	p, outcome, err := v.request(ctx, key, token)
	v.count(ctx, outcome)
	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome != outcomeActive && outcome != outcomeInactive {
		span.SetStatus(codes.Error, outcome)
	}
	return p, err
}

func unavailable(msg string) error {
	return fmt.Errorf("auth/introspect: %s: %w", msg, auth.ErrIntrospectionUnavailable)
}

func (v *Verifier) request(ctx context.Context, key, token string) (auth.Principal, string, error) {
	assertion, err := v.assertion.Assertion(ctx)
	if err != nil {
		v.logLimited("client_rejected", slog.LevelError, "auth/introspect: cannot get the client assertion")
		return auth.Principal{}, outcomeClientRejected, unavailable("no client assertion")
	}
	form := url.Values{
		"token":                 {token},
		"token_type_hint":       {"api_token"},
		"client_id":             {v.assertion.ClientID()},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.IntrospectURL, strings.NewReader(form.Encode()))
	if err != nil {
		v.logLimited("invalid_response", slog.LevelError, "auth/introspect: IntrospectURL is not valid")
		return auth.Principal{}, outcomeInvalidResp, unavailable("IntrospectURL is not valid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := v.clock()
	resp, err := v.client.Do(req)
	v.duration.Record(ctx, v.clock().Sub(start).Seconds())
	if err != nil {
		// A url.Error holds the URL. It holds no form value.
		return auth.Principal{}, outcomeUnavailable, fmt.Errorf("auth/introspect: request failed: %w: %w", auth.ErrIntrospectionUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return auth.Principal{}, outcomeUnavailable, unavailable("cannot read the response")
	}
	if len(body) > maxResponseBytes {
		v.logLimited("invalid_response", slog.LevelError, "auth/introspect: the response is too large")
		return auth.Principal{}, outcomeInvalidResp, unavailable("the response is too large")
	}
	var ir introspectResponse
	parsed := json.Unmarshal(body, &ir) == nil

	switch {
	case resp.StatusCode >= 500 || (resp.StatusCode != http.StatusOK && parsed && ir.Error == "temporarily_unavailable"):
		return auth.Principal{}, outcomeUnavailable, unavailable(fmt.Sprintf("status %d", resp.StatusCode))
	case (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) && parsed && ir.Error != "":
		v.logLimited("client_rejected", slog.LevelError,
			"auth/introspect: the identity service rejected the client, check the client configuration",
			slog.Int("status", resp.StatusCode), slog.String("error", ir.Error))
		return auth.Principal{}, outcomeClientRejected, unavailable("the client is rejected")
	case resp.StatusCode != http.StatusOK || !parsed || ir.Active == nil:
		v.logLimited("invalid_response", slog.LevelError, "auth/introspect: the response is not valid",
			slog.Int("status", resp.StatusCode))
		return auth.Principal{}, outcomeInvalidResp, unavailable("the response is not valid")
	}
	now := v.clock()
	if !*ir.Active {
		if v.inactive != nil {
			v.inactive.Set(key, struct{}{}, now.Add(v.cfg.InactiveTTL))
		}
		return auth.Principal{}, outcomeInactive, fmt.Errorf("auth/introspect: token is not active: %w", auth.ErrInvalidToken)
	}
	p, exp, ok := v.principal(ir, now)
	if !ok {
		return auth.Principal{}, outcomeInvalidResp, fmt.Errorf("auth/introspect: the response is not valid: %w", auth.ErrInvalidToken)
	}
	ttl := min(v.cfg.CacheTTL, exp.Sub(now))
	v.active.Set(key, activeEntry{principal: clonePrincipal(p)}, now.Add(ttl))
	return p, outcomeActive, nil
}

// principal checks the fields of an active response.
func (v *Verifier) principal(ir introspectResponse, now time.Time) (auth.Principal, time.Time, bool) {
	if ir.Aud != v.cfg.Audience {
		v.logLimited("aud", slog.LevelWarn,
			"auth/introspect: the aud of the response is not the configured Audience",
			slog.String("expected_aud", v.cfg.Audience), slog.String("received_aud", ir.Aud))
		return auth.Principal{}, time.Time{}, false
	}
	if ir.TokenType != string(auth.TokenTypeAPI) || ir.Sub == "" ||
		ir.PrincipalType != string(auth.PrincipalUser) || ir.Exp == nil {
		return auth.Principal{}, time.Time{}, false
	}
	if *ir.Exp < 0 || *ir.Exp > 1<<40 {
		return auth.Principal{}, time.Time{}, false
	}
	exp := time.Unix(int64(*ir.Exp), 0)
	if !exp.After(now) {
		return auth.Principal{}, time.Time{}, false
	}
	scopes, ok := v.parseScopes(ir.Scope)
	if !ok {
		return auth.Principal{}, time.Time{}, false
	}
	return auth.Principal{
		ID: ir.Sub, Type: auth.PrincipalUser, Scopes: scopes,
		Audience: v.cfg.Audience, TokenType: auth.TokenTypeAPI,
	}, exp, true
}

// parseScopes splits the scope value. Each key must be a valid permission key of
// the app of the verifier. An empty value gives no scopes.
func (v *Verifier) parseScopes(scope string) ([]string, bool) {
	if scope == "" {
		return nil, true
	}
	if len(scope) > maxScopeLen {
		return nil, false
	}
	prefix := v.cfg.Audience + ":"
	keys := strings.Split(scope, " ")
	for _, k := range keys {
		if len(k) > maxPermissionKeyLen || !permissionKey.MatchString(k) || !strings.HasPrefix(k, prefix) {
			return nil, false
		}
	}
	return keys, true
}
