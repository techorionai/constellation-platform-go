package mock

import (
	"context"
	"errors"
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

var _ auth.TokenExchanger = (*TokenExchanger)(nil)

// TokenExchanger is a mock of [auth.TokenExchanger].
type TokenExchanger struct {
	// ExchangeFunc replaces Exchange. If it is nil, Exchange returns the error
	// [auth.ErrTokenUnavailable], so a mock that is not set up fails closed.
	ExchangeFunc func(ctx context.Context, req auth.ExchangeRequest) (string, error)

	mu    sync.Mutex
	calls int
}

// Exchange calls ExchangeFunc, or returns auth.ErrTokenUnavailable.
func (t *TokenExchanger) Exchange(ctx context.Context, req auth.ExchangeRequest) (string, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.ExchangeFunc != nil {
		return t.ExchangeFunc(ctx, req)
	}
	return "", fmt.Errorf("mock.TokenExchanger: ExchangeFunc is nil: %w", auth.ErrTokenUnavailable)
}

// ExchangeCalls returns the number of Exchange calls.
func (t *TokenExchanger) ExchangeCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

var _ auth.ClientAssertion = (*ClientAssertion)(nil)

// ClientAssertion is a mock of [auth.ClientAssertion].
type ClientAssertion struct {
	// ID is the value of ClientID.
	ID string
	// AssertionFunc replaces Assertion. If it is nil, Assertion returns an
	// error, so a mock that is not set up fails closed.
	AssertionFunc func(ctx context.Context) (string, error)

	mu    sync.Mutex
	calls int
}

// ClientID returns ID.
func (c *ClientAssertion) ClientID() string { return c.ID }

// Assertion calls AssertionFunc, or returns an error.
func (c *ClientAssertion) Assertion(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.AssertionFunc != nil {
		return c.AssertionFunc(ctx)
	}
	return "", errors.New("mock.ClientAssertion: AssertionFunc is nil")
}

// AssertionCalls returns the number of Assertion calls.
func (c *ClientAssertion) AssertionCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
