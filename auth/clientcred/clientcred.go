package clientcred

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/internal/lru"
)

var (
	_ auth.TokenSource     = (*Source)(nil)
	_ auth.TokenExchanger  = (*Source)(nil)
	_ auth.ClientAssertion = (*Source)(nil)
)

const (
	scopeName = "github.com/techorionai/constellation-platform-go/auth/clientcred"

	assertionType     = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	assertionLifetime = 60 * time.Second
	maxResponseBytes  = 64 << 10
	refreshFraction   = 0.8
	// maxExpiresIn is the longest token lifetime that the source accepts.
	maxExpiresIn = 24 * time.Hour
)

// Outcomes of a token request, the values of the attribute "outcome".
const (
	outcomeOK           = "ok"
	outcomeOAuthError   = "oauth_error"
	outcomeUnavailable  = "unavailable"
	outcomeBadResponse  = "bad_response"
	outcomeNetworkError = "network_error"
)

// Config is the configuration of the token source. Nest it in the config of the
// service with envPrefix:"CLIENT__".
type Config struct {
	// ID is the client ID. It is the iss and the sub of the assertion.
	ID string `env:"ID,required"`
	// KeyID is the kid of the signing key. The identity service knows the public
	// key with this ID.
	KeyID string `env:"KEY_ID,required"`
	// PrivateKey is the PEM encoded EC P-256 private key. CLIENT__PRIVATE_KEY_FILE
	// reads it from a file.
	PrivateKey config.Secret `env:"PRIVATE_KEY,required" envFile:"true"`
	// Issuer is the issuer URL of the identity service. It is the aud of the
	// assertion.
	Issuer string `env:"ISSUER,required"`
	// TokenURL is the URL of the token endpoint that the service calls. The
	// default is <Issuer>/oauth/token. In Compose it is the internal URL.
	TokenURL string `env:"TOKEN_URL"`
	// HTTPTimeout is the time limit of one token request.
	HTTPTimeout time.Duration `env:"HTTP_TIMEOUT" envDefault:"10s"`
	// AllowInsecureHTTP allows the scheme http for Issuer and TokenURL. Use it
	// only for local compose and tests. A service must not set it in production.
	AllowInsecureHTTP bool `env:"ALLOW_INSECURE_HTTP" envDefault:"false"`
}

// checkURL returns an error if the URL does not use https. With allowHTTP it
// also accepts http.
func checkURL(name, variable, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("auth/clientcred: %s (%s) is not a valid http or https URL", name, variable)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	case u.Scheme == "http":
		return fmt.Errorf("auth/clientcred: %s (%s) must use https (set CLIENT__ALLOW_INSECURE_HTTP only for local use)", name, variable)
	default:
		return fmt.Errorf("auth/clientcred: %s (%s) is not a valid http or https URL", name, variable)
	}
	return nil
}

type resolved struct {
	Config
	key *ecdsa.PrivateKey
}

func (c Config) resolve() (resolved, error) {
	switch {
	case c.ID == "":
		return resolved{}, errors.New("auth/clientcred: ID is required (CLIENT__ID)")
	case c.KeyID == "":
		return resolved{}, errors.New("auth/clientcred: KeyID is required (CLIENT__KEY_ID)")
	case c.Issuer == "":
		return resolved{}, errors.New("auth/clientcred: Issuer is required (CLIENT__ISSUER)")
	case c.PrivateKey == "":
		return resolved{}, errors.New("auth/clientcred: PrivateKey is required (CLIENT__PRIVATE_KEY)")
	}
	if c.TokenURL == "" {
		c.TokenURL = strings.TrimRight(c.Issuer, "/") + "/oauth/token"
	}
	if err := checkURL("Issuer", "CLIENT__ISSUER", c.Issuer, c.AllowInsecureHTTP); err != nil {
		return resolved{}, err
	}
	if err := checkURL("TokenURL", "CLIENT__TOKEN_URL", c.TokenURL, c.AllowInsecureHTTP); err != nil {
		return resolved{}, err
	}
	if c.HTTPTimeout <= 0 {
		return resolved{}, errors.New("auth/clientcred: HTTPTimeout (CLIENT__HTTP_TIMEOUT) must be more than 0")
	}
	key, err := parseKey(c.PrivateKey.Reveal())
	if err != nil {
		return resolved{}, err
	}
	return resolved{Config: c, key: key}, nil
}

// parseKey reads a PEM EC P-256 private key (PKCS #8 or SEC 1). The errors never
// contain key material.
func parseKey(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("auth/clientcred: PrivateKey (CLIENT__PRIVATE_KEY) is not PEM")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("auth/clientcred: PrivateKey (CLIENT__PRIVATE_KEY) must be an EC P-256 key")
		}
		key = ek
	} else if ek, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = ek
	} else {
		return nil, errors.New("auth/clientcred: PrivateKey (CLIENT__PRIVATE_KEY) cannot be parsed")
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("auth/clientcred: PrivateKey (CLIENT__PRIVATE_KEY) must be an EC P-256 key")
	}
	return key, nil
}

// Option changes the behavior of [New].
type Option func(*Source)

// WithHTTPClient sets the HTTP client of the token request. The source uses a
// copy that does not follow redirects, has the transport wrapped with otelhttp,
// and has the timeout HTTPTimeout if the client has none.
func WithHTTPClient(c *http.Client) Option { return func(s *Source) { s.client = c } }

// WithClock sets the clock. The default is time.Now. A test uses it to move the
// time.
func WithClock(now func() time.Time) Option { return func(s *Source) { s.clock = now } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(s *Source) { s.meterProvider = mp }
}

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(s *Source) { s.tracerProvider = tp }
}

type entry struct {
	token     string
	refreshAt time.Time
	expiresAt time.Time
}

// Source is an [auth.TokenSource] for the client credentials grant. It is safe
// for concurrent use.
type Source struct {
	cfg            resolved
	signer         jose.Signer
	client         *http.Client
	clock          func() time.Time
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	tracer         trace.Tracer
	requests       metric.Int64Counter
	exchanges      metric.Int64Counter

	mu     sync.Mutex
	cache  map[string]entry
	flight singleflight.Group

	exchangeCache  *lru.Cache[string, entry]
	exchangeFlight singleflight.Group
}

// New makes a token source. It checks the config and parses the private key.
func New(cfg Config, opts ...Option) (*Source, error) {
	rc, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	s := &Source{
		cfg:            rc,
		clock:          time.Now,
		meterProvider:  otel.GetMeterProvider(),
		tracerProvider: otel.GetTracerProvider(),
		cache:          make(map[string]entry),
		exchangeCache:  lru.New[string, entry](exchangeCacheSize),
	}
	for _, opt := range opts {
		opt(s)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: rc.key, KeyID: rc.KeyID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, errors.New("auth/clientcred: cannot make the signer")
	}
	s.signer = signer
	var c http.Client
	if s.client != nil {
		c = *s.client
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
		otelhttp.WithTracerProvider(s.tracerProvider),
		otelhttp.WithMeterProvider(s.meterProvider))
	s.client = &c
	s.tracer = s.tracerProvider.Tracer(scopeName)
	s.requests, _ = s.meterProvider.Meter(scopeName).Int64Counter("constellation.auth.token.request",
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of token requests, by outcome."))
	s.exchanges, _ = s.meterProvider.Meter(scopeName).Int64Counter("constellation.auth.token.exchange",
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of token exchange requests, by outcome."))
	return s, nil
}

// Token returns an access token for the audience and the scopes. It uses the
// cache when it can. It does not retry.
//
// Errors: [auth.ErrNoScopes], [auth.ErrBadTokenResponse],
// [auth.ErrTokenUnavailable], and *[auth.OAuthError].
func (s *Source) Token(ctx context.Context, audience string, scopes ...string) (string, error) {
	scopes = normalize(scopes)
	if len(scopes) == 0 {
		return "", auth.ErrNoScopes
	}
	key := audience + "\x00" + strings.Join(scopes, " ")
	if tok, ok := s.cached(key); ok {
		return tok, nil
	}
	// The shared request does not stop when one caller stops. The HTTP client
	// timeout limits it.
	ch := s.flight.DoChan(key, func() (any, error) {
		if tok, ok := s.cached(key); ok {
			return tok, nil
		}
		tok, err := s.fetch(context.WithoutCancel(ctx), key, audience, scopes)
		if err != nil {
			// A failed refresh before the expiry keeps the cached token. The
			// next call tries to refresh again.
			if old, valid := s.unexpired(key); valid {
				return old, nil
			}
		}
		return tok, err
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

// normalize sorts the scopes, and removes empty values and duplicates.
func normalize(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if sc != "" {
			out = append(out, sc)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (s *Source) cached(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok {
		return "", false
	}
	now := s.clock()
	if !now.Before(e.refreshAt) || !now.Before(e.expiresAt) {
		return "", false
	}
	return e.token, true
}

// unexpired returns the cached token if it is before its expiry.
func (s *Source) unexpired(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || !s.clock().Before(e.expiresAt) {
		return "", false
	}
	return e.token, true
}

// ClientID returns the client ID of the service. It implements
// [auth.ClientAssertion].
func (s *Source) ClientID() string { return s.cfg.ID }

// Assertion returns a new private_key_jwt client assertion. The issuer is the
// audience. Each call has a new jti. It implements [auth.ClientAssertion].
func (s *Source) Assertion(context.Context) (string, error) {
	a, err := s.assertion()
	if err != nil {
		return "", errors.New("auth/clientcred: cannot sign the assertion")
	}
	return a, nil
}

func (s *Source) assertion() (string, error) {
	now := s.clock()
	claims := map[string]any{
		"iss": s.cfg.ID,
		"sub": s.cfg.ID,
		"aud": s.cfg.Issuer,
		"jti": uuid.NewString(),
		"iat": now.Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := s.signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

type tokenResponse struct {
	AccessToken string  `json:"access_token"`
	ExpiresIn   float64 `json:"expires_in"`
	Error       string  `json:"error"`
}

func (s *Source) fetch(ctx context.Context, key, audience string, scopes []string) (string, error) {
	ctx, span := s.tracer.Start(ctx, "auth.clientcred.token", trace.WithAttributes(
		attribute.String("constellation.client.id", s.cfg.ID),
		attribute.String("constellation.token.audience", audience)))
	defer span.End()

	tok, outcome, err := s.request(ctx, key, audience, scopes)
	s.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	span.SetAttributes(attribute.String("outcome", outcome))
	if err != nil {
		span.SetStatus(codes.Error, outcome)
		return "", err
	}
	return tok, nil
}

func (s *Source) request(ctx context.Context, key, audience string, scopes []string) (token, outcome string, err error) {
	assertion, err := s.assertion()
	if err != nil {
		return "", outcomeBadResponse, errors.New("auth/clientcred: cannot sign the assertion")
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
		"audience":              {audience},
		"scope":                 {strings.Join(scopes, " ")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", outcomeBadResponse, errors.New("auth/clientcred: TokenURL is not valid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := s.clock()
	resp, err := s.client.Do(req)
	if err != nil {
		// A url.Error holds the URL. It holds no form value.
		return "", outcomeNetworkError, fmt.Errorf("%w: %w", auth.ErrTokenUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", outcomeNetworkError, fmt.Errorf("%w: cannot read the response", auth.ErrTokenUnavailable)
	}
	if len(body) > maxResponseBytes {
		return "", outcomeBadResponse, fmt.Errorf("%w: body is too large", auth.ErrBadTokenResponse)
	}
	var tr tokenResponse
	parsed := json.Unmarshal(body, &tr) == nil

	switch {
	case resp.StatusCode == http.StatusOK:
		if !parsed || tr.AccessToken == "" {
			return "", outcomeBadResponse, fmt.Errorf("%w: no access_token", auth.ErrBadTokenResponse)
		}
		if !(tr.ExpiresIn > 0) {
			return "", outcomeBadResponse, fmt.Errorf("%w: expires_in is not more than 0", auth.ErrBadTokenResponse)
		}
		if tr.ExpiresIn > maxExpiresIn.Seconds() {
			return "", outcomeBadResponse, fmt.Errorf("%w: expires_in is more than %s", auth.ErrBadTokenResponse, maxExpiresIn)
		}
		life := time.Duration(tr.ExpiresIn * float64(time.Second))
		s.mu.Lock()
		s.cache[key] = entry{
			token:     tr.AccessToken,
			refreshAt: start.Add(time.Duration(float64(life) * refreshFraction)),
			expiresAt: start.Add(life),
		}
		s.mu.Unlock()
		return tr.AccessToken, outcomeOK, nil
	case resp.StatusCode >= 500 || (parsed && tr.Error == "temporarily_unavailable"):
		return "", outcomeUnavailable, fmt.Errorf("%w: status %d", auth.ErrTokenUnavailable, resp.StatusCode)
	case parsed && tr.Error != "":
		return "", outcomeOAuthError, &auth.OAuthError{Code: tr.Error}
	default:
		return "", outcomeBadResponse, fmt.Errorf("%w: status %d", auth.ErrBadTokenResponse, resp.StatusCode)
	}
}
