package mock

import (
	"context"
	"fmt"
	"sync"

	"github.com/techorionai/constellation-platform-go/auth"
)

var _ auth.Verifier = (*Verifier)(nil)

// Verifier is a mock of [auth.Verifier].
type Verifier struct {
	// VerifyFunc replaces Verify. If it is nil, Verify rejects each token with
	// [auth.ErrInvalidToken], so a mock that is not set up fails closed.
	VerifyFunc func(ctx context.Context, token string) (auth.Principal, error)

	mu    sync.Mutex
	calls int
}

// Verify calls VerifyFunc, or returns auth.ErrInvalidToken.
func (v *Verifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	if v.VerifyFunc != nil {
		return v.VerifyFunc(ctx, token)
	}
	return auth.Principal{}, fmt.Errorf("mock.Verifier: VerifyFunc is nil: %w", auth.ErrInvalidToken)
}

// VerifyCalls returns the number of Verify calls.
func (v *Verifier) VerifyCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}
