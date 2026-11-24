package auth

import (
	"context"
	"fmt"
	"strings"
)

// APITokenPrefix is the prefix of a personal API token.
const APITokenPrefix = "cpat_"

type byFormat struct {
	jwt, apiToken Verifier
}

// ByFormat returns a [Verifier] that selects a verifier by the format of the
// token. A token with the prefix "cpat_" goes to apiToken. All other tokens go
// to jwt. The function checks strings only. If apiToken is nil, an API token
// gives [ErrInvalidToken]. If jwt is nil, any other token gives
// [ErrInvalidToken].
func ByFormat(jwt, apiToken Verifier) Verifier {
	return byFormat{jwt: jwt, apiToken: apiToken}
}

// Verify sends the token to the verifier for its format.
func (b byFormat) Verify(ctx context.Context, token string) (Principal, error) {
	v := b.jwt
	if strings.HasPrefix(token, APITokenPrefix) {
		v = b.apiToken
	}
	if v == nil {
		return Principal{}, fmt.Errorf("auth: no verifier for this token format: %w", ErrInvalidToken)
	}
	return v.Verify(ctx, token)
}
