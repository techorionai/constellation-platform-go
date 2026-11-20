package mock

import (
	"context"
	"fmt"
	"sync"

	"github.com/techorionai/constellation-platform-go/auth"
)

var _ auth.TokenSource = (*TokenSource)(nil)

// TokenSource is a mock of [auth.TokenSource].
type TokenSource struct {
	// TokenFunc replaces Token. If it is nil, Token returns the error
	// [auth.ErrTokenUnavailable], so a mock that is not set up fails closed.
	TokenFunc func(ctx context.Context, audience string, scopes ...string) (string, error)

	mu    sync.Mutex
	calls int
}

// Token calls TokenFunc, or returns auth.ErrTokenUnavailable.
func (t *TokenSource) Token(ctx context.Context, audience string, scopes ...string) (string, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.TokenFunc != nil {
		return t.TokenFunc(ctx, audience, scopes...)
	}
	return "", fmt.Errorf("mock.TokenSource: TokenFunc is nil: %w", auth.ErrTokenUnavailable)
}

// TokenCalls returns the number of Token calls.
func (t *TokenSource) TokenCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}
