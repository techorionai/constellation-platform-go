package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/techorionai/constellation-platform-go/auth"
)

// maxJWKSBytes is the largest JWKS body that the verifier accepts.
const maxJWKSBytes = 64 << 10

// Values of the outcome attribute of constellation.auth.jwks.fetch.
const (
	fetchOK           = "ok"
	fetchHTTPError    = "http_error"
	fetchBadBody      = "bad_body"
	fetchTooLarge     = "too_large"
	fetchNoKeys       = "no_keys"
	fetchNetworkError = "network_error"
)

// keySet is the cache of the public keys, by key ID. It is never changed after
// it is made. A refresh replaces it.
type keySet map[string]*ecdsa.PublicKey

func (v *Verifier) currentKeys() keySet {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.keys
}

// refresh fetches the JWKS. If force is false, it does nothing when the last
// attempt is more recent than MinRefetchInterval, and it reports blocked.
// Concurrent calls share one fetch. If detach is true, the fetch does not end
// when ctx ends, because other callers share the result. In both cases refresh
// returns the error of ctx when ctx ends before the fetch ends.
func (v *Verifier) refresh(ctx context.Context, force, detach bool) (blocked bool, err error) {
	ch := v.group.DoChan("jwks", func() (any, error) {
		now := v.now()
		v.mu.Lock()
		if !force && !v.lastAttempt.IsZero() && now.Sub(v.lastAttempt) < v.cfg.MinRefetchInterval {
			v.mu.Unlock()
			return true, nil
		}
		v.lastAttempt = now
		v.mu.Unlock()

		fctx := ctx
		if detach {
			fctx = context.WithoutCancel(ctx)
		}
		fctx, cancel := context.WithTimeout(fctx, v.cfg.HTTPTimeout)
		defer cancel()
		keys, outcome, err := v.fetch(fctx)
		v.fetches.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
		if err != nil {
			if detach || ctx.Err() == nil {
				slog.WarnContext(context.WithoutCancel(ctx), "auth/jwt: JWKS fetch failed",
					slog.String("outcome", outcome), slog.Any("error", err))
			}
			return false, nil
		}
		v.mu.Lock()
		v.keys = keys
		v.mu.Unlock()
		return false, nil
	})
	select {
	case res := <-ch:
		b, _ := res.Val.(bool)
		return b, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (v *Verifier) fetch(ctx context.Context) (keySet, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, fetchNetworkError, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fetchNetworkError, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fetchHTTPError, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fetchNetworkError, err
	}
	if len(body) > maxJWKSBytes {
		return nil, fetchTooLarge, errors.New("body is larger than 64 KiB")
	}
	keys, err := parseJWKS(body)
	if err != nil {
		if errors.Is(err, errNoKeys) {
			return nil, fetchNoKeys, err
		}
		return nil, fetchBadBody, err
	}
	return keys, fetchOK, nil
}

var errNoKeys = errors.New("key set has no accepted key")

// parseJWKS reads a key set. It accepts a key only if kty is EC, crv is P-256,
// use is sig or absent, alg is ES256 or absent, kid is set, and the key has no
// private part. It ignores all other keys.
func parseJWKS(body []byte) (keySet, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	keys := keySet{}
	for _, raw := range doc.Keys {
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON(raw); err != nil {
			continue
		}
		pub, ok := jwk.Key.(*ecdsa.PublicKey)
		if !ok || !jwk.IsPublic() || jwk.KeyID == "" {
			continue
		}
		if pub.Curve != elliptic.P256() {
			continue
		}
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		if jwk.Algorithm != "" && jwk.Algorithm != string(jose.ES256) {
			continue
		}
		keys[jwk.KeyID] = pub
	}
	if len(keys) == 0 {
		return nil, errNoKeys
	}
	return keys, nil
}

// lookup returns the key for a key ID. An unknown ID starts a refetch if the
// rate limit permits it. If the rate limit blocks the refetch, the result is
// [auth.ErrKeysUnavailable], so the client can retry.
func (v *Verifier) lookup(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	keys := v.currentKeys()
	if len(keys) == 0 {
		if _, err := v.refresh(ctx, false, true); err != nil {
			return nil, fmt.Errorf("auth/jwt: wait for keys: %w: %w", auth.ErrKeysUnavailable, err)
		}
		keys = v.currentKeys()
		if len(keys) == 0 {
			return nil, errKeysUnavailable()
		}
	}
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	blocked, err := v.refresh(ctx, false, true)
	if err != nil {
		return nil, fmt.Errorf("auth/jwt: wait for keys: %w: %w", auth.ErrKeysUnavailable, err)
	}
	if k, ok := v.currentKeys()[kid]; ok {
		return k, nil
	}
	if blocked {
		return nil, fmt.Errorf("auth/jwt: unknown kid and the refetch rate limit is active: %w", auth.ErrKeysUnavailable)
	}
	return nil, nil
}

// now returns the time of the verifier clock.
func (v *Verifier) now() time.Time { return v.clock() }
