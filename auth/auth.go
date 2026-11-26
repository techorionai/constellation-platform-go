package auth

import (
	"context"
	"errors"
	"slices"
	"time"
)

// PrincipalType is the kind of a principal.
type PrincipalType string

// Values of PrincipalType.
const (
	PrincipalUser     PrincipalType = "user"
	PrincipalAgent    PrincipalType = "agent"
	PrincipalService  PrincipalType = "service"
	PrincipalWorkflow PrincipalType = "workflow"
)

// Valid reports whether t is a known principal type.
func (t PrincipalType) Valid() bool {
	switch t {
	case PrincipalUser, PrincipalAgent, PrincipalService, PrincipalWorkflow:
		return true
	}
	return false
}

// Actor is the principal that acts for another principal. It comes from the act
// claim of the token.
type Actor struct {
	ID   string
	Type PrincipalType
}

// TokenType is the kind of credential that a [Principal] came from.
type TokenType string

// Values of TokenType. The zero value is treated as [TokenTypeAccess].
const (
	// TokenTypeAccess is a JWT access token.
	TokenTypeAccess TokenType = "access_token"
	// TokenTypeAPI is a personal API token that the identity service checked.
	TokenTypeAPI TokenType = "api_token"
)

// OrAccess returns the token type, or [TokenTypeAccess] for the zero value.
func (t TokenType) OrAccess() TokenType {
	if t == "" {
		return TokenTypeAccess
	}
	return t
}

// Principal is the verified identity of the caller of a request.
type Principal struct {
	// ID is the subject of the token.
	ID string
	// Type is the kind of the principal.
	Type PrincipalType
	// ClientID is the OAuth client that asked for the token.
	ClientID string
	// Scopes are the permission keys of the token.
	Scopes []string
	// Audience is the audience that the verifier checked.
	Audience string
	// Act is the acting principal, or nil.
	Act *Actor
	// TokenType is the kind of token that the verifier accepted. The zero value
	// means [TokenTypeAccess]. A service can refuse API tokens on some routes.
	TokenType TokenType
	// AuthMethods is the amr claim of the token (RFC 8176). It is empty if the
	// token has none. The adapter keeps values that it does not know.
	AuthMethods []string
	// AuthTime is the auth_time claim of the token. It is the zero time if the
	// token has none.
	AuthTime time.Time
}

// HasMFA returns true if AuthMethods contains "mfa".
//
// HasMFA does not check Act and does not check the age of AuthTime. A token
// with Act does not satisfy MFA. A sensitive action must use [CheckMFA] or
// [RequireMFA], not HasMFA.
func (p Principal) HasMFA() bool {
	return slices.Contains(p.AuthMethods, "mfa")
}

// HasScopes reports whether the principal has all the scopes. It returns true
// when scopes is empty.
func (p Principal) HasScopes(scopes ...string) bool {
	return len(p.MissingScopes(scopes...)) == 0
}

// MissingScopes returns the scopes that the principal does not have, in the
// order of the request.
func (p Principal) MissingScopes(scopes ...string) []string {
	have := make(map[string]struct{}, len(p.Scopes))
	for _, s := range p.Scopes {
		have[s] = struct{}{}
	}
	var missing []string
	for _, s := range scopes {
		if _, ok := have[s]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}

type principalKey struct{}

// ContextWithPrincipal returns a context that carries the principal.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the principal of the context. The second result
// is false when the context has no principal.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Verifier is the port that turns a token into a principal. An implementation
// returns an error that wraps one of the sentinel errors, so that errors.Is
// works.
type Verifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// Sentinel errors of Verify and of the middleware.
var (
	// ErrNoToken means the request has no bearer token.
	ErrNoToken = errors.New("auth: no token")
	// ErrInvalidToken means the token is malformed or fails a check.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrExpired means the token expired.
	ErrExpired = errors.New("auth: token expired")
	// ErrWrongAudience means the audience of the token is not the audience of
	// the service.
	ErrWrongAudience = errors.New("auth: wrong audience")
	// ErrWrongIssuer means the issuer of the token is not the trusted issuer.
	ErrWrongIssuer = errors.New("auth: wrong issuer")
	// ErrMissingScope means the principal lacks a required scope.
	ErrMissingScope = errors.New("auth: missing scope")
	// ErrReauthenticationRequired means the token does not show a fresh MFA
	// login.
	ErrReauthenticationRequired = errors.New("auth: reauthentication required")
	// ErrKeysUnavailable means the verifier has no signing keys.
	ErrKeysUnavailable = errors.New("auth: signing keys unavailable")
	// ErrIntrospectionUnavailable means the verifier cannot say if an API token
	// is valid, for example because the identity service is down, the client
	// is rejected, or the verifier is overloaded.
	ErrIntrospectionUnavailable = errors.New("auth: introspection unavailable")
)

// Outcome returns the value of the outcome attribute of the metric
// constellation.auth.verify for an error. A nil error gives "ok".
func Outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNoToken):
		return "no_token"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrWrongIssuer):
		return "wrong_issuer"
	case errors.Is(err, ErrWrongAudience):
		return "wrong_audience"
	case errors.Is(err, ErrKeysUnavailable):
		return "keys_unavailable"
	case errors.Is(err, ErrIntrospectionUnavailable):
		return "unavailable"
	default:
		return "invalid"
	}
}

// ClientAssertion is the port that gives a private_key_jwt client assertion
// (RFC 7523). The adapter auth/clientcred implements it.
type ClientAssertion interface {
	// ClientID returns the client ID of the service.
	ClientID() string
	// Assertion returns a private_key_jwt client assertion with the issuer as
	// audience. Each call returns a new assertion with a new jti.
	Assertion(ctx context.Context) (string, error)
}
