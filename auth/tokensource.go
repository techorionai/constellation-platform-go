package auth

import (
	"context"
	"errors"
	"fmt"
)

// TokenSource gets access tokens for calls to other services. The adapter
// auth/clientcred gets the tokens with the client credentials grant.
type TokenSource interface {
	// Token returns an access token for the audience and the scopes. It needs at
	// least one scope. Token does not retry. The caller decides.
	Token(ctx context.Context, audience string, scopes ...string) (string, error)
}

// Errors of a [TokenSource].
var (
	// ErrNoScopes is returned when the call has no scope. No request is sent.
	ErrNoScopes = errors.New("auth: token request has no scopes")
	// ErrBadTokenResponse is returned when the answer of the token endpoint is
	// not valid, for example when it has no access token or no lifetime.
	ErrBadTokenResponse = errors.New("auth: bad token response")
	// ErrTokenUnavailable is returned when the token endpoint is down, answers
	// with a 5xx status or with the OAuth error temporarily_unavailable, or when
	// a network error occurs. The caller can retry.
	ErrTokenUnavailable = errors.New("auth: token endpoint unavailable")
)

// OAuthError is an OAuth error (RFC 6749) from the token endpoint. Its text has
// the code only. It never contains a secret.
type OAuthError struct {
	// Code is the value of the error field, for example "invalid_client".
	Code string
}

// Error returns the text of the error.
func (e *OAuthError) Error() string {
	return fmt.Sprintf("auth: token endpoint returned the OAuth error %q", e.Code)
}

// ExchangeRequest is the input of [TokenExchanger.Exchange].
type ExchangeRequest struct {
	// SubjectToken is the access token of the user.
	SubjectToken string
	// AgentID is the ID of the agent that acts for the user. It is empty for a
	// token for the user only.
	AgentID string
	// Audience is the app that the new token is for.
	Audience string
	// Scopes are the permission keys that the new token needs. At least one.
	Scopes []string
}

// TokenExchanger gets a token for a user, and optionally an agent that acts for
// the user, with the token exchange grant (RFC 8693). The adapter
// auth/clientcred implements it.
//
// Exchange returns [ErrNoScopes] if Scopes is empty and [ErrInvalidToken] if
// SubjectToken is empty. It returns [ErrTokenUnavailable] when the identity
// service is not available, and *[OAuthError] for an OAuth error response.
type TokenExchanger interface {
	Exchange(ctx context.Context, req ExchangeRequest) (string, error)
}
