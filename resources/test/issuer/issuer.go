package issuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Default values of the claims.
const (
	DefaultSubject       = "user-1"
	DefaultClientID      = "client-1"
	DefaultPrincipalType = "user"
	DefaultAudience      = "demo"
	DefaultScope         = "demo:ticket:read"
)

type key struct {
	kid       string
	priv      *ecdsa.PrivateKey
	published bool
}

// Issuer is a test token issuer with a JWKS endpoint.
type Issuer struct {
	srv   *httptest.Server
	clock func() time.Time

	mu      sync.Mutex
	keys    []*key
	status  int
	body    []byte
	fetches int
	nextKey int
}

// Option changes the behavior of [New].
type Option func(*Issuer)

// WithClock sets the clock that gives the default iat and exp of a token. The
// default is time.Now.
func WithClock(now func() time.Time) Option { return func(i *Issuer) { i.clock = now } }

// New makes an issuer with one key and starts its JWKS server. The server stops
// at the end of the test.
func New(t testing.TB, opts ...Option) *Issuer {
	t.Helper()
	i := &Issuer{clock: time.Now}
	for _, opt := range opts {
		opt(i)
	}
	i.addKey(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", i.serveJWKS)
	i.srv = httptest.NewServer(mux)
	t.Cleanup(i.srv.Close)
	return i
}

func (i *Issuer) addKey(t testing.TB) *key {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("issuer: generate key: %v", err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.nextKey++
	k := &key{kid: fmt.Sprintf("test-key-%d", i.nextKey), priv: priv, published: true}
	i.keys = append(i.keys, k)
	return k
}

// URL returns the URL of the JWKS endpoint. Use it as JWKSURL.
func (i *Issuer) URL() string { return i.srv.URL + "/.well-known/jwks.json" }

// Issuer returns the issuer value of the tokens. Use it as Issuer.
func (i *Issuer) Issuer() string { return i.srv.URL }

// Rotate adds a new key and keeps the old keys. The new key signs the next
// tokens. It returns the key ID.
func (i *Issuer) Rotate(t testing.TB) string {
	t.Helper()
	return i.addKey(t).kid
}

// RemoveKey removes a key from the JWKS. The issuer keeps the private key, so a
// test can still sign a token with it. This models a key that the issuer
// removed.
func (i *Issuer) RemoveKey(t testing.TB, kid string) {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, k := range i.keys {
		if k.kid == kid {
			k.published = false
			return
		}
	}
	t.Fatalf("issuer: unknown key %q", kid)
}

// KeyIDs returns the IDs of all keys, the oldest first.
func (i *Issuer) KeyIDs() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	ids := make([]string, len(i.keys))
	for n, k := range i.keys {
		ids[n] = k.kid
	}
	return ids
}

// SetStatus makes the JWKS endpoint return the status code. Status 0 restores
// the normal response.
func (i *Issuer) SetStatus(t testing.TB, code int) {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.status = code
}

// SetBody makes the JWKS endpoint return a raw body with status 200. A nil body
// restores the normal response.
func (i *Issuer) SetBody(t testing.TB, body []byte) {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.body = body
}

// Fetches returns the number of requests that the JWKS endpoint got.
func (i *Issuer) Fetches() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetches
}

// Close stops the JWKS server. A later fetch fails with a network error.
func (i *Issuer) Close() { i.srv.Close() }

// JWK returns the public JWK of a key, as a map that a test can change.
func (i *Issuer) JWK(kid string) map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, k := range i.keys {
		if k.kid == kid {
			return jwk(k)
		}
	}
	return nil
}

func jwk(k *key) map[string]any {
	raw, err := k.priv.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "kid": k.kid,
		"x": b64(raw[1:33]), "y": b64(raw[33:]),
	}
}

func (i *Issuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.fetches++
	status, body := i.status, i.body
	var keys []map[string]any
	for _, k := range i.keys {
		if k.published {
			keys = append(keys, jwk(k))
		}
	}
	i.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if body != nil {
		_, _ = w.Write(body)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// Claims describes a token. A zero field gives the default. Use Extra to set a
// claim to an empty value, and Remove to remove a claim.
type Claims struct {
	Issuer        string
	Subject       string
	ClientID      string
	PrincipalType string
	// Scope is the scope claim. The default is DefaultScope.
	Scope string
	// Audience is a string or a []string. The default is DefaultAudience.
	Audience any
	// IssuedAt, NotBefore, and Expires are the time claims. The defaults are
	// now, no nbf, and now plus 5 minutes.
	IssuedAt  time.Time
	NotBefore time.Time
	Expires   time.Time
	// Act is the act claim. The default is none.
	Act map[string]any
	// Extra sets or replaces claims by name. It wins over the fields above.
	Extra map[string]any
	// Remove removes claims by name. It wins over all other fields.
	Remove []string

	// Alg is the alg header. The default is ES256. The values none, HS256, and
	// RS256 make a forged token: none has no signature, HS256 uses the public
	// key bytes as the secret, and RS256 uses a new RSA key.
	Alg string
	// Typ is the typ header. The default is at+jwt.
	Typ string
	// Kid is the kid header. If the issuer has a key with this ID, the token
	// uses it to sign. The default is the newest key.
	Kid string
	// SigningKey signs the token in place of an issuer key. It makes a token
	// with a key that is not in the JWKS.
	SigningKey *ecdsa.PrivateKey
}

// GenerateKey makes an ES256 key that is not in any JWKS. Use it in
// [Claims].SigningKey.
func GenerateKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("issuer: generate key: %v", err)
	}
	return priv
}

// Mint signs a token.
func (i *Issuer) Mint(t testing.TB, c Claims) string {
	t.Helper()
	now := i.clock()
	payload := map[string]any{
		"iss":            first(c.Issuer, i.Issuer()),
		"sub":            first(c.Subject, DefaultSubject),
		"client_id":      first(c.ClientID, DefaultClientID),
		"principal_type": first(c.PrincipalType, DefaultPrincipalType),
		"scope":          first(c.Scope, DefaultScope),
		"aud":            DefaultAudience,
		"iat":            now.Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"jti":            fmt.Sprintf("jti-%d", now.UnixNano()),
	}
	if c.Audience != nil {
		payload["aud"] = c.Audience
	}
	if !c.IssuedAt.IsZero() {
		payload["iat"] = c.IssuedAt.Unix()
	}
	if !c.Expires.IsZero() {
		payload["exp"] = c.Expires.Unix()
	}
	if !c.NotBefore.IsZero() {
		payload["nbf"] = c.NotBefore.Unix()
	}
	if c.Act != nil {
		payload["act"] = c.Act
	}
	for k, v := range c.Extra {
		payload[k] = v
	}
	for _, k := range c.Remove {
		delete(payload, k)
	}

	signer, kid := i.signer(c)
	header := map[string]any{"alg": first(c.Alg, "ES256"), "typ": first(c.Typ, "at+jwt"), "kid": kid}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("issuer: marshal header: %v", err)
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("issuer: marshal payload: %v", err)
	}
	signingInput := b64(hb) + "." + b64(pb)
	sig, err := sign(c.Alg, signer, signingInput)
	if err != nil {
		t.Fatalf("issuer: sign: %v", err)
	}
	return signingInput + "." + b64(sig)
}

func (i *Issuer) signer(c Claims) (*ecdsa.PrivateKey, string) {
	if c.SigningKey != nil {
		return c.SigningKey, first(c.Kid, "foreign-key")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if c.Kid != "" {
		for _, k := range i.keys {
			if k.kid == c.Kid {
				return k.priv, k.kid
			}
		}
	}
	k := i.keys[len(i.keys)-1]
	return k.priv, first(c.Kid, k.kid)
}

func sign(alg string, k *ecdsa.PrivateKey, input string) ([]byte, error) {
	digest := sha256.Sum256([]byte(input))
	switch alg {
	case "", "ES256":
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			return nil, err
		}
		return append(pad32(r.Bytes()), pad32(s.Bytes())...), nil
	case "none":
		return nil, nil
	case "HS256":
		pub, err := k.PublicKey.Bytes()
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, pub)
		mac.Write([]byte(input))
		return mac.Sum(nil), nil
	case "RS256":
		rk, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		return rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, digest[:])
	}
	return nil, fmt.Errorf("unsupported alg %q", alg)
}

func first(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}
