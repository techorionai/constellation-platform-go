package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/health"
)

var _ health.Checker = (*Checker)(nil)

// Checker is a mock of [health.Checker].
type Checker struct {
	// CheckName is the result of Name.
	CheckName string
	// CheckFunc replaces Check. If it is nil, Check returns nil.
	CheckFunc func(ctx context.Context) error

	mu    sync.Mutex
	calls int
}

// Name returns CheckName.
func (c *Checker) Name() string { return c.CheckName }

// Check calls CheckFunc, or returns nil.
func (c *Checker) Check(ctx context.Context) error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.CheckFunc != nil {
		return c.CheckFunc(ctx)
	}
	return nil
}

// CheckCalls returns the number of Check calls.
func (c *Checker) CheckCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
