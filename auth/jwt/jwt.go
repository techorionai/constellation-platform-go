package jwt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
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

// adminScope is the one permission key with two segments that
// constellation-contracts allows.
const adminScope = "identity:admin"

// permissionKey is the pattern of PermissionKey in constellation-contracts
// for keys with three segments.
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
	// AllowInsecureHTTP allows the scheme http for Issuer and JWKSURL. Use it
	// only for local compose and tests. A service must not set it in production.
	AllowInsecureHTTP bool `env:"ALLOW_INSECURE_HTTP" envDefault:"false"`
}

// maxLeeway is the largest Leeway that New accepts.
const maxLeeway = 2 * time.Minute

// checkURL returns an error if the URL does not use https. With allowHTTP it
// also accepts http.
func checkURL(name, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("auth/jwt: %s is not a valid URL: %w", name, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	default:
		return fmt.Errorf("auth/jwt: %s must use https (set AUTH__ALLOW_INSECURE_HTTP only for local use)", name)
	}
	if u.Host == "" {
		return fmt.Errorf("auth/jwt: %s has no host", name)
	}
	return nil
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
	if err := checkURL("Issuer (AUTH__ISSUER)", c.Issuer, c.AllowInsecureHTTP); err != nil {
		return c, err
	}
	if err := checkURL("JWKSURL (AUTH__JWKS_URL)", c.JWKSURL, c.AllowInsecureHTTP); err != nil {
		return c, err
	}
	if c.Leeway < 0 || c.Leeway > maxLeeway {
		return c, errors.New("auth/jwt: Leeway (AUTH__LEEWAY) must be between 0 and 2m")
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 10 * time.Minute
	}
	if c.MinRefetchInterval <= 0 {
		c.MinRefetchInterval = 30 * time.Second
	}
	if c.Leeway == 0 {
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
// uses HTTPTimeout. The verifier uses a copy of the client that does not follow
// redirects.
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

	lifeMu  sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
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
	} else {
		c := *v.client
		v.client = &c
	}
	// The fetch never follows a redirect. A redirect response is a failed fetch.
	v.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v.tick = min(cfg.RefreshInterval, time.Second)
	v.fetches, _ = v.meterProvider.Meter(scopeName).Int64Counter("constellation.auth.jwks.fetch",
		metric.WithUnit("{fetch}"),
		metric.WithDescription("Number of JWKS fetches, by outcome."))
	return v, nil
}

// Start starts the background refresh. It refreshes the key set every
// RefreshInterval until ctx ends or [Verifier.Close] runs. Start is optional.
// Without it, the cache refreshes only when a token has an unknown key ID. A
// second call does nothing. Start after [Verifier.Close] does nothing.
func (v *Verifier) Start(ctx context.Context) {
	v.lifeMu.Lock()
	defer v.lifeMu.Unlock()
	if v.started || v.closed {
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
				_, _ = v.refresh(ctx, true, false)
			}
		}
	}
}

// Close stops the background refresh and waits for it. A second call does
// nothing. Verify still works after Close with the cached keys.
func (v *Verifier) Close() {
	v.lifeMu.Lock()
	v.closed = true
	cancel := v.cancel
	v.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	v.wg.Wait()
}

func errKeysUnavailable() error {
	return fmt.Errorf("auth/jwt: no signing keys: %w", auth.ErrKeysUnavailable)
}

func invalid(reason string) error {
	return fmt.Errorf("auth/jwt: %s: %w", reason, auth.ErrInvalidToken)
}

type claims struct {
	Iss           string
	Sub           string
	Aud           json.RawMessage
	Exp           *float64
	Nbf           *float64
	Iat           *float64
	ClientID      string
	PrincipalType string
	Scope         string
	Act           *actClaim
	Amr           []string
	AuthTime      *float64
}

type actClaim struct {
	Sub           string
	PrincipalType string
}

// claimReader reads claims by their exact names. The decoder of encoding/json
// matches names without case, so the verifier does not use it on a struct.
type claimReader struct {
	m   map[string]json.RawMessage
	bad bool
}

func newClaimReader(payload []byte) (*claimReader, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil || m == nil {
		return nil, invalid("claims")
	}
	return &claimReader{m: m}, nil
}

func (r *claimReader) raw(name string) (json.RawMessage, bool) {
	raw, ok := r.m[name]
	if !ok || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}

func (r *claimReader) str(name string) string {
	raw, ok := r.raw(name)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		r.bad = true
	}
	return s
}

func (r *claimReader) num(name string) *float64 {
	raw, ok := r.raw(name)
	if !ok {
		return nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		r.bad = true
		return nil
	}
	return &n
}

func parseClaims(payload []byte) (claims, error) {
	r, err := newClaimReader(payload)
	if err != nil {
		return claims{}, err
	}
	c := claims{
		Iss: r.str("iss"), Sub: r.str("sub"), Exp: r.num("exp"), Nbf: r.num("nbf"), Iat: r.num("iat"),
		ClientID: r.str("client_id"), PrincipalType: r.str("principal_type"), Scope: r.str("scope"),
	}
	c.Aud, _ = r.raw("aud")
	c.AuthTime = r.num("auth_time")
	if raw, ok := r.raw("amr"); ok {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			r.bad = true
		}
		for _, it := range items {
			var s string
			if string(it) == "null" || json.Unmarshal(it, &s) != nil {
				r.bad = true
				break
			}
			c.Amr = append(c.Amr, s)
		}
	}
	if raw, ok := r.raw("act"); ok {
		a, err := newClaimReader(raw)
		if err != nil {
			return claims{}, invalid("act")
		}
		c.Act = &actClaim{Sub: a.str("sub"), PrincipalType: a.str("principal_type")}
		r.bad = r.bad || a.bad
	}
	if r.bad {
		return claims{}, invalid("claims")
	}
	return c, nil
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
	c, err := parseClaims(payload)
	if err != nil {
		return auth.Principal{}, err
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
	exp, ok := unixTime(*c.Exp)
	if !ok {
		return auth.Principal{}, invalid("exp out of range")
	}
	now := v.now()
	if !exp.After(now.Add(-v.cfg.Leeway)) {
		return auth.Principal{}, fmt.Errorf("auth/jwt: exp: %w", auth.ErrExpired)
	}
	if c.Nbf != nil {
		nbf, ok := unixTime(*c.Nbf)
		if !ok {
			return auth.Principal{}, invalid("nbf out of range")
		}
		if nbf.After(now.Add(v.cfg.Leeway)) {
			return auth.Principal{}, invalid("nbf in the future")
		}
	}
	if c.Iat != nil {
		iat, ok := unixTime(*c.Iat)
		if !ok {
			return auth.Principal{}, invalid("iat out of range")
		}
		if iat.After(now.Add(v.cfg.Leeway)) {
			return auth.Principal{}, invalid("iat in the future")
		}
	}
	var authTime time.Time
	if c.AuthTime != nil {
		at, ok := unixTime(*c.AuthTime)
		if !ok {
			return auth.Principal{}, invalid("auth_time out of range")
		}
		if at.After(now.Add(v.cfg.Leeway)) {
			return auth.Principal{}, invalid("auth_time in the future")
		}
		authTime = at
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
		TokenType: auth.TokenTypeAccess, AuthMethods: c.Amr, AuthTime: authTime,
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

// Range of a time claim: the years 1 to 9999.
const (
	minUnix = -62135596800
	maxUnix = 253402300799
)

// unixTime converts a time claim. It reports false for a value that is not
// finite or not in the years 1 to 9999.
func unixTime(f float64) (time.Time, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < minUnix || f > maxUnix {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
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

// parseScopes splits the scope claim. Each key must be identity:admin or match
// the permission key pattern. An empty claim gives no scopes.
func parseScopes(scope string) ([]string, bool) {
	if scope == "" {
		return nil, true
	}
	if len(scope) > maxScopeLen {
		return nil, false
	}
	keys := strings.Split(scope, " ")
	for _, k := range keys {
		if len(k) > maxPermissionKeyLen || (k != adminScope && !permissionKey.MatchString(k)) {
			return nil, false
		}
	}
	return keys, true
}
