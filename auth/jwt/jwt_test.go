package jwt_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	"github.com/techorionai/constellation-platform-go/auth"
	authjwt "github.com/techorionai/constellation-platform-go/auth/jwt"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/issuer"
)

const audience = "demo"

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	iss    *issuer.Issuer
	clock  *testClock
	v      *authjwt.Verifier
	reader *sdkmetric.ManualReader
}

func newFixture(t *testing.T, opts ...authjwt.Option) *fixture {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	iss := issuer.New(t, issuer.WithClock(clock.Now))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	all := append([]authjwt.Option{authjwt.WithClock(clock.Now), authjwt.WithMeterProvider(mp)}, opts...)
	v, err := authjwt.New(authjwt.Config{Issuer: iss.Issuer(), Audience: audience, JWKSURL: iss.URL(), AllowInsecureHTTP: true}, all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return &fixture{iss: iss, clock: clock, v: v, reader: reader}
}

func (f *fixture) verify(t *testing.T, c issuer.Claims) (auth.Principal, error) {
	t.Helper()
	return f.v.Verify(context.Background(), f.iss.Mint(t, c))
}

func (f *fixture) mustVerify(t *testing.T, c issuer.Claims) auth.Principal {
	t.Helper()
	p, err := f.verify(t, c)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return p
}

func (f *fixture) fetchOutcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.auth.jwks.fetch" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// U11
func TestVerifyValid(t *testing.T) {
	f := newFixture(t)
	p := f.mustVerify(t, issuer.Claims{Scope: "demo:ticket:read demo:ticket:write"})
	if p.ID != issuer.DefaultSubject || p.Type != auth.PrincipalUser || p.ClientID != issuer.DefaultClientID ||
		p.Audience != audience || p.Act != nil {
		t.Errorf("principal %+v", p)
	}
	if len(p.Scopes) != 2 || p.Scopes[0] != "demo:ticket:read" || p.Scopes[1] != "demo:ticket:write" {
		t.Errorf("scopes %v", p.Scopes)
	}
	if got := f.fetchOutcomes(t); got["ok"] != 1 {
		t.Errorf("fetch outcomes %v", got)
	}
}

// U12
func TestWrongAudience(t *testing.T) {
	f := newFixture(t)
	_, err := f.verify(t, issuer.Claims{Audience: "other-app"})
	wantErr(t, err, auth.ErrWrongAudience)
}

// U12a
func TestAudienceArray(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{Audience: []string{"other", audience}})
	_, err := f.verify(t, issuer.Claims{Audience: []string{"other", "third"}})
	wantErr(t, err, auth.ErrWrongAudience)
}

// U13
func TestWrongIssuer(t *testing.T) {
	f := newFixture(t)
	_, err := f.verify(t, issuer.Claims{Issuer: "https://evil.example"})
	wantErr(t, err, auth.ErrWrongIssuer)
}

func TestTimeClaims(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	t.Run("U14a exp inside leeway", func(t *testing.T) {
		f.mustVerify(t, issuer.Claims{Expires: now.Add(-10 * time.Second)})
	})
	t.Run("U14b exp outside leeway", func(t *testing.T) {
		_, err := f.verify(t, issuer.Claims{Expires: now.Add(-60 * time.Second)})
		wantErr(t, err, auth.ErrExpired)
	})
	t.Run("U14c nbf inside leeway", func(t *testing.T) {
		f.mustVerify(t, issuer.Claims{NotBefore: now.Add(10 * time.Second)})
	})
	t.Run("U14d nbf outside leeway", func(t *testing.T) {
		_, err := f.verify(t, issuer.Claims{NotBefore: now.Add(60 * time.Second)})
		wantErr(t, err, auth.ErrInvalidToken)
	})
	t.Run("U14e iat outside leeway", func(t *testing.T) {
		_, err := f.verify(t, issuer.Claims{IssuedAt: now.Add(60 * time.Second)})
		wantErr(t, err, auth.ErrInvalidToken)
	})
	t.Run("U14f no exp", func(t *testing.T) {
		_, err := f.verify(t, issuer.Claims{Remove: []string{"exp"}})
		wantErr(t, err, auth.ErrInvalidToken)
	})
}

// U15
func TestAlgConfusion(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{}) // load the keys
	for _, alg := range []string{"none", "HS256", "RS256"} {
		t.Run(alg, func(t *testing.T) {
			_, err := f.verify(t, issuer.Claims{Alg: alg})
			wantErr(t, err, auth.ErrInvalidToken)
		})
	}
}

// U16
func TestWrongTyp(t *testing.T) {
	f := newFixture(t)
	_, err := f.verify(t, issuer.Claims{Typ: "JWT"})
	wantErr(t, err, auth.ErrInvalidToken)
}

// U16a
func TestTypCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{Typ: "AT+JWT"})
	f.mustVerify(t, issuer.Claims{Typ: "application/at+jwt"})
}

// U16b
func TestChangedSignature(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	token := f.iss.Mint(t, issuer.Claims{})
	i := strings.LastIndex(token, ".") + 5
	b := []byte(token)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	_, err := f.v.Verify(context.Background(), string(b))
	wantErr(t, err, auth.ErrInvalidToken)
}

// U16c
func TestMissingClaims(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"sub", "client_id", "principal_type"} {
		t.Run(name, func(t *testing.T) {
			_, err := f.verify(t, issuer.Claims{Remove: []string{name}})
			wantErr(t, err, auth.ErrInvalidToken)
		})
	}
}

// U16d
func TestUnknownPrincipalType(t *testing.T) {
	f := newFixture(t)
	_, err := f.verify(t, issuer.Claims{PrincipalType: "robot"})
	wantErr(t, err, auth.ErrInvalidToken)
}

// U16e
func TestBadScope(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"openid demo:ticket:read", "Demo:ticket:read", "demo:ticket:read  demo:ticket:write"} {
		_, err := f.verify(t, issuer.Claims{Scope: s})
		wantErr(t, err, auth.ErrInvalidToken)
	}
}

// U16f
func TestActClaim(t *testing.T) {
	f := newFixture(t)
	p := f.mustVerify(t, issuer.Claims{Act: map[string]any{"sub": "u1", "principal_type": "user"}})
	if p.Act == nil || p.Act.ID != "u1" || p.Act.Type != auth.PrincipalUser {
		t.Errorf("act %+v", p.Act)
	}
	_, err := f.verify(t, issuer.Claims{Act: map[string]any{"principal_type": "user"}})
	wantErr(t, err, auth.ErrInvalidToken)
}

// U17
func TestUnknownKidRefetchOnce(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	f.clock.Advance(31 * time.Second)
	before := f.iss.Fetches()
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrInvalidToken)
	if got := f.iss.Fetches() - before; got != 1 {
		t.Errorf("refetches = %d, want 1", got)
	}
}

// U17a
func TestUnknownKidInsideMinRefetch(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	before := f.iss.Fetches()
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrKeysUnavailable)
	if got := f.iss.Fetches() - before; got != 0 {
		t.Errorf("fetches = %d, want 0", got)
	}
}

// U18
func TestKeyRotation(t *testing.T) {
	f := newFixture(t)
	oldKid := f.iss.KeyIDs()[0]
	f.mustVerify(t, issuer.Claims{})
	f.iss.Rotate(t)
	f.clock.Advance(31 * time.Second)
	before := f.iss.Fetches()
	f.mustVerify(t, issuer.Claims{}) // new key
	if got := f.iss.Fetches() - before; got != 1 {
		t.Errorf("refetches = %d, want 1", got)
	}
	f.mustVerify(t, issuer.Claims{Kid: oldKid}) // old key
}

// U19
func TestUnknownKidFlood(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	f.clock.Advance(31 * time.Second)
	key := issuer.GenerateKey(t)
	for i := 0; i < 50; i++ {
		_, err := f.verify(t, issuer.Claims{SigningKey: key, Kid: fmt.Sprintf("rand-%d", i)})
		if i == 0 {
			wantErr(t, err, auth.ErrInvalidToken)
		} else {
			wantErr(t, err, auth.ErrKeysUnavailable)
		}
	}
	if got := f.iss.Fetches(); got > 2 {
		t.Errorf("fetches = %d, want at most 2 (one first fetch, one refetch)", got)
	}
}

// U20
func TestServerDownAfterFetch(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	f.iss.Close()
	f.clock.Advance(31 * time.Second)
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{})
	if got := f.fetchOutcomes(t); got["network_error"] != 1 {
		t.Errorf("fetch outcomes %v", got)
	}
}

// U21
func TestServerDownBeforeFetch(t *testing.T) {
	f := newFixture(t) // New does not fetch and does not block
	f.iss.Close()
	_, err := f.verify(t, issuer.Claims{})
	wantErr(t, err, auth.ErrKeysUnavailable)
}

// U22
func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	f.iss.SetBody(t, bytes.Repeat([]byte(" "), 70<<10))
	f.clock.Advance(31 * time.Second)
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{})
	if got := f.fetchOutcomes(t); got["too_large"] != 1 {
		t.Errorf("fetch outcomes %v", got)
	}
}

// U22a
func TestBadStatusAndBadBody(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	unknown := issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"}

	f.iss.SetStatus(t, 500)
	f.clock.Advance(31 * time.Second)
	_, err := f.verify(t, unknown)
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{})

	f.iss.SetStatus(t, 0)
	f.iss.SetBody(t, []byte("{not json"))
	f.clock.Advance(31 * time.Second)
	_, err = f.verify(t, unknown)
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{})

	got := f.fetchOutcomes(t)
	if got["http_error"] != 1 || got["bad_body"] != 1 {
		t.Errorf("fetch outcomes %v", got)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func ecJWK(pub *ecdsa.PublicKey, kid string, size int) map[string]any {
	raw, err := pub.Bytes()
	if err != nil {
		panic(err)
	}
	crv := "P-256"
	if size == 48 {
		crv = "P-384"
	}
	return map[string]any{
		"kty": "EC", "crv": crv, "kid": kid,
		"x": b64(raw[1 : 1+size]), "y": b64(raw[1+size:]),
	}
}

// U22b
func TestIgnoredKeys(t *testing.T) {
	f := newFixture(t)
	validKid := f.iss.KeyIDs()[0]

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaJWK := map[string]any{
		"kty": "RSA", "kid": "rsa-key", "n": b64(rsaKey.N.Bytes()),
		"e": b64(big.NewInt(int64(rsaKey.E)).Bytes()),
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384JWK := ecJWK(&p384.PublicKey, "p384-key", 48)
	encKey := issuer.GenerateKey(t)
	encJWK := ecJWK(&encKey.PublicKey, "enc-key", 32)
	encJWK["use"] = "enc"
	privKey := issuer.GenerateKey(t)
	privJWK := ecJWK(&privKey.PublicKey, "priv-key", 32)
	dRaw, err := privKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	privJWK["d"] = b64(dRaw)

	body, err := json.Marshal(map[string]any{"keys": []any{
		rsaJWK, p384JWK, encJWK, privJWK, f.iss.JWK(validKid),
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.iss.SetBody(t, body)

	f.mustVerify(t, issuer.Claims{})
	for kid, k := range map[string]*ecdsa.PrivateKey{"enc-key": encKey, "priv-key": privKey} {
		f.clock.Advance(31 * time.Second)
		_, err := f.verify(t, issuer.Claims{SigningKey: k, Kid: kid})
		wantErr(t, err, auth.ErrInvalidToken)
	}
	for _, kid := range []string{"rsa-key", "p384-key"} {
		f.clock.Advance(31 * time.Second)
		_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: kid})
		wantErr(t, err, auth.ErrInvalidToken)
	}
}

// U22c
func TestRemovedKeyStopsWorking(t *testing.T) {
	f := newFixture(t)
	key1 := f.iss.KeyIDs()[0]
	f.iss.Rotate(t)
	f.mustVerify(t, issuer.Claims{Kid: key1})
	f.iss.RemoveKey(t, key1)
	f.clock.Advance(31 * time.Second)
	// An unknown kid starts the refresh that replaces the key set.
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrInvalidToken)
	f.clock.Advance(31 * time.Second)
	_, err = f.verify(t, issuer.Claims{Kid: key1})
	wantErr(t, err, auth.ErrInvalidToken)
	f.mustVerify(t, issuer.Claims{}) // the new key still works
}

// U25
func TestConfig(t *testing.T) {
	type holder struct {
		Auth authjwt.Config `envPrefix:"AUTH__"`
	}
	t.Setenv("SVC_AUTH__ISSUER", "x")
	_ = os.Unsetenv("SVC_AUTH__ISSUER")
	t.Setenv("SVC_AUTH__AUDIENCE", "x")
	_ = os.Unsetenv("SVC_AUTH__AUDIENCE")
	var h holder
	err := config.Load("SVC_", &h)
	if err == nil || !strings.Contains(err.Error(), "SVC_AUTH__ISSUER") || !strings.Contains(err.Error(), "SVC_AUTH__AUDIENCE") {
		t.Fatalf("config.Load error = %v", err)
	}
	for _, c := range []authjwt.Config{{Audience: "a"}, {Issuer: "https://i"}} {
		_, err := authjwt.New(c)
		if err == nil || !strings.Contains(err.Error(), "AUTH__") {
			t.Errorf("New error = %v", err)
		}
	}

	t.Setenv("SVC_AUTH__ISSUER", "https://id.example")
	t.Setenv("SVC_AUTH__AUDIENCE", "demo")
	h = holder{}
	if err := config.Load("SVC_", &h); err != nil {
		t.Fatal(err)
	}
	c := h.Auth
	if c.RefreshInterval != 10*time.Minute || c.MinRefetchInterval != 30*time.Second ||
		c.Leeway != 30*time.Second || c.HTTPTimeout != 5*time.Second || c.JWKSURL != "" {
		t.Errorf("defaults %+v", c)
	}

	// The default JWKS URL is <Issuer>/.well-known/jwks.json.
	iss := issuer.New(t)
	v, err := authjwt.New(authjwt.Config{Issuer: iss.Issuer(), Audience: audience, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.Verify(context.Background(), iss.Mint(t, issuer.Claims{})); err != nil {
		t.Fatalf("default JWKS URL: %v", err)
	}
}

// U27
func TestBackgroundRefresh(t *testing.T) {
	f := newFixture(t)
	f.v.SetTick(10 * time.Millisecond)
	f.mustVerify(t, issuer.Claims{})
	f.v.Start(context.Background())
	f.clock.Advance(11 * time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for f.iss.Fetches() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.iss.Fetches(); got != 2 {
		t.Errorf("fetches = %d, want 2", got)
	}
}

// U28
func TestCloseTwice(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	tr := &http.Transport{}
	f := newFixture(t, authjwt.WithHTTPClient(&http.Client{Transport: tr, Timeout: 5 * time.Second}))
	f.v.SetTick(10 * time.Millisecond)
	f.mustVerify(t, issuer.Claims{})
	f.v.Start(context.Background())
	f.v.Close()
	f.v.Close()
	f.mustVerify(t, issuer.Claims{})
	tr.CloseIdleConnections()
	f.iss.Close()
}

// Issue 13 U1 and U2
func TestIdentityAdminScope(t *testing.T) {
	f := newFixture(t)
	p := f.mustVerify(t, issuer.Claims{Scope: "identity:admin"})
	if len(p.Scopes) != 1 || p.Scopes[0] != "identity:admin" {
		t.Errorf("scopes %v", p.Scopes)
	}
	p = f.mustVerify(t, issuer.Claims{Scope: "identity:admin identity:client:write"})
	if len(p.Scopes) != 2 || p.Scopes[0] != "identity:admin" || p.Scopes[1] != "identity:client:write" {
		t.Errorf("scopes %v", p.Scopes)
	}
}

// Issue 13 U3 and U4
func TestBadTwoSegmentScope(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{
		"identity:user", "Identity:admin", "identity:Admin",
		"identity:admin ", "identity:admin\n", "identity:admin  identity:client:write",
		"demo:admin", "identity:", ":admin", "openid",
	} {
		_, err := f.verify(t, issuer.Claims{Scope: s})
		wantErr(t, err, auth.ErrInvalidToken)
	}
}

// A key with three segments stays valid.
func TestIdentityAdminReadStaysValid(t *testing.T) {
	f := newFixture(t)
	p := f.mustVerify(t, issuer.Claims{Scope: "identity:admin:read"})
	if len(p.Scopes) != 1 || p.Scopes[0] != "identity:admin:read" {
		t.Errorf("scopes %v", p.Scopes)
	}
}
