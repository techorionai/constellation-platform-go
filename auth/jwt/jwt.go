package jwt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/techorionai/constellation-platform-go/auth"
)

var _ auth.Verifier = (*Verifier)(nil)

const scopeName = "github.com/techorionai/constellation-platform-go/auth/jwt"

// permissionKey is the pattern of PermissionKey in constellation-contracts.
var permissionKey = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)

const (
	maxPermissionKeyLen = 128
	maxScopeLen         = 4096
)

// Config is the configuration of the verifier. Nest it in the config of the
// service with envPrefix:"AUTH__".
type Config struct {
	// Issuer is the iss claim that the verifier requires.
	Issuer string `env:"ISSUER,required"`
	// Audience is the AppKey of this service. The aud claim must contain it.
	Audience string `env:"AUDIENCE,required"`
	// JWKSURL is the URL of the key set. The default is
	// <Issuer>/.well-known/jwks.json.
	JWKSURL string `env:"JWKS_URL"`
	// RefreshInterval is the time between the background refreshes.
	RefreshInterval time.Duration `env:"REFRESH_INTERVAL" envDefault:"10m"`
	// MinRefetchInterval is the shortest time between two fetches that an
	// unknown key ID starts.
	MinRefetchInterval time.Duration `env:"MIN_REFETCH_INTERVAL" envDefault:"30s"`
	// Leeway is the clock skew that the time claims allow.
	Leeway time.Duration `env:"LEEWAY" envDefault:"30s"`
	// HTTPTimeout is the time limit of one JWKS fetch.
	HTTPTimeout time.Duration `env:"HTTP_TIMEOUT" envDefault:"5s"`
}

func (c Config) withDefaults() (Config, error) {
	if c.Issuer == "" {
		return c, errors.New("auth/jwt: Issuer is required (AUTH__ISSUER)")
	}
	if c.Audience == "" {
		return c, errors.New("auth/jwt: Audience is required (AUTH__AUDIENCE)")
	}
	if c.JWKSURL == "" {
		c.JWKSURL = strings.TrimRight(c.Issuer, "/") + "/.well-known/jwks.json"
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 10 * time.Minute
	}
	if c.MinRefetchInterval <= 0 {
		c.MinRefetchInterval = 30 * time.Second
	}
	if c.Leeway <= 0 {
		c.Leeway = 30 * time.Second
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 5 * time.Second
	}
	return c, nil
}

// Option changes the behavior of [New].
type Option func(*Verifier)

// WithHTTPClient sets the HTTP client that fetches the JWKS. The default client
// uses HTTPTimeout.
func WithHTTPClient(c *http.Client) Option { return func(v *Verifier) { v.client = c } }

// WithClock sets the clock of the verifier. The default is time.Now. A test uses
// it to move the time.
func WithClock(now func() time.Time) Option { return func(v *Verifier) { v.clock = now } }

// WithMeterProvider sets the meter provider. The default is the global meter
// provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(v *Verifier) { v.meterProvider = mp }
}

// Verifier verifies access tokens with the keys of a JWKS endpoint. It is safe
// for concurrent use.
type Verifier struct {
	cfg           Config
	client        *http.Client
	clock         func() time.Time
	meterProvider metric.MeterProvider
	fetches       metric.Int64Counter
	group         singleflight.Group
	tick          time.Duration

	mu          sync.RWMutex
	keys        keySet
	lastAttempt time.Time

	lifeMu    sync.Mutex
	started   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New makes a verifier. It does not fetch the JWKS.
func New(cfg Config, opts ...Option) (*Verifier, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	v := &Verifier{cfg: cfg, clock: time.Now, meterProvider: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(v)
	}
	if v.client == nil {
		v.client = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	v.tick = min(cfg.RefreshInterval, time.Second)
	v.fetches, _ = v.meterProvider.Meter(scopeName).Int64Counter("constellation.auth.jwks.fetch",
		metric.WithUnit("{fetch}"),
		metric.WithDescription("Number of JWKS fetches, by outcome."))
	return v, nil
}

// Start starts the background refresh. It refreshes the key set every
// RefreshInterval until ctx ends or [Verifier.Close] runs. Start is optional.
// Without it, the cache refreshes only when a token has an unknown key ID. A
// second call does nothing.
func (v *Verifier) Start(ctx context.Context) {
	v.lifeMu.Lock()
	defer v.lifeMu.Unlock()
	if v.started {
		return
	}
	v.started = true
	ctx, v.cancel = context.WithCancel(ctx)
	v.wg.Add(1)
	go v.loop(ctx)
}

func (v *Verifier) loop(ctx context.Context) {
	defer v.wg.Done()
	t := time.NewTicker(v.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.mu.RLock()
			last := v.lastAttempt
			v.mu.RUnlock()
			if last.IsZero() || v.now().Sub(last) >= v.cfg.RefreshInterval {
				v.refresh(ctx, true)
			}
		}
	}
}

// Close stops the background refresh and waits for it. A second call does
// nothing. Verify still works after Close with the cached keys.
func (v *Verifier) Close() {
	v.closeOnce.Do(func() {
		v.lifeMu.Lock()
		cancel := v.cancel
		v.lifeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		v.wg.Wait()
	})
}

func errKeysUnavailable() error {
	return fmt.Errorf("auth/jwt: no signing keys: %w", auth.ErrKeysUnavailable)
}

func invalid(reason string) error {
	return fmt.Errorf("auth/jwt: %s: %w", reason, auth.ErrInvalidToken)
}

type claims struct {
	Iss           string          `json:"iss"`
	Sub           string          `json:"sub"`
	Aud           json.RawMessage `json:"aud"`
	Exp           *float64        `json:"exp"`
	Nbf           *float64        `json:"nbf"`
	Iat           *float64        `json:"iat"`
	ClientID      string          `json:"client_id"`
	PrincipalType string          `json:"principal_type"`
	Scope         string          `json:"scope"`
	Act           *struct {
		Sub           string `json:"sub"`
		PrincipalType string `json:"principal_type"`
	} `json:"act"`
}

// Verify checks the token and returns the principal. The error wraps one of the
// sentinel errors of the auth package.
func (v *Verifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	jws, err := jose.ParseSignedCompact(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return auth.Principal{}, invalid("parse")
	}
	if len(jws.Signatures) != 1 {
		return auth.Principal{}, invalid("signature count")
	}
	h := jws.Signatures[0].Header
	if h.Algorithm != string(jose.ES256) {
		return auth.Principal{}, invalid("alg")
	}
	typ, _ := h.ExtraHeaders[jose.HeaderType].(string)
	if !strings.EqualFold(typ, "at+jwt") && !strings.EqualFold(typ, "application/at+jwt") {
		return auth.Principal{}, invalid("typ")
	}
	if h.KeyID == "" {
		return auth.Principal{}, invalid("kid")
	}
	key, err := v.lookup(ctx, h.KeyID)
	if err != nil {
		return auth.Principal{}, err
	}
	if key == nil {
		return auth.Principal{}, invalid("unknown kid")
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return auth.Principal{}, invalid("signature")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return auth.Principal{}, invalid("claims")
	}
	return v.principal(c)
}

func (v *Verifier) principal(c claims) (auth.Principal, error) {
	if c.Iss != v.cfg.Issuer {
		return auth.Principal{}, fmt.Errorf("auth/jwt: issuer: %w", auth.ErrWrongIssuer)
	}
	if !audienceMatches(c.Aud, v.cfg.Audience) {
		return auth.Principal{}, fmt.Errorf("auth/jwt: audience: %w", auth.ErrWrongAudience)
	}
	if c.Exp == nil {
		return auth.Principal{}, invalid("exp missing")
	}
	now := v.now()
	if !unixTime(*c.Exp).After(now.Add(-v.cfg.Leeway)) {
		return auth.Principal{}, fmt.Errorf("auth/jwt: exp: %w", auth.ErrExpired)
	}
	if c.Nbf != nil && unixTime(*c.Nbf).After(now.Add(v.cfg.Leeway)) {
		return auth.Principal{}, invalid("nbf in the future")
	}
	if c.Iat != nil && unixTime(*c.Iat).After(now.Add(v.cfg.Leeway)) {
		return auth.Principal{}, invalid("iat in the future")
	}
	ptype := auth.PrincipalType(c.PrincipalType)
	if c.Sub == "" || c.ClientID == "" || !ptype.Valid() {
		return auth.Principal{}, invalid("sub, client_id, or principal_type")
	}
	scopes, ok := parseScopes(c.Scope)
	if !ok {
		return auth.Principal{}, invalid("scope")
	}
	p := auth.Principal{
		ID: c.Sub, Type: ptype, ClientID: c.ClientID, Scopes: scopes, Audience: v.cfg.Audience,
	}
	if c.Act != nil {
		atype := auth.PrincipalType(c.Act.PrincipalType)
		if c.Act.Sub == "" || !atype.Valid() {
			return auth.Principal{}, invalid("act")
		}
		p.Act = &auth.Actor{ID: c.Act.Sub, Type: atype}
	}
	return p, nil
}

func unixTime(f float64) time.Time {
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9))
}

// audienceMatches reports whether aud is the string want, or an array that
// contains want.
func audienceMatches(aud json.RawMessage, want string) bool {
	var one string
	if err := json.Unmarshal(aud, &one); err == nil {
		return one == want
	}
	var many []string
	if err := json.Unmarshal(aud, &many); err == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// parseScopes splits the scope claim. Each key must match the permission key
// pattern. An empty claim gives no scopes.
func parseScopes(scope string) ([]string, bool) {
	if scope == "" {
		return nil, true
	}
	if len(scope) > maxScopeLen {
		return nil, false
	}
	keys := strings.Split(scope, " ")
	for _, k := range keys {
		if len(k) > maxPermissionKeyLen || !permissionKey.MatchString(k) {
			return nil, false
		}
	}
	return keys, true
}
