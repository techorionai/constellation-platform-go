package mock

import (
	"context"
	"sync"
	"time"

	"github.com/techorionai/constellation-platform-go/streams"
)

var _ streams.Group = (*StreamGroup)(nil)

// StreamGroup is a mock of [streams.Group]. A nil function gives the zero
// result: Read and Claim wait for the end of the context (Read) or return
// nothing (Claim), and the other methods return nil. It records the acks.
type StreamGroup struct {
	// StreamApp and GroupName are the results of Stream and Name. The defaults
	// are "identity" and "helpdesk".
	StreamApp string
	GroupName string

	EnsureFunc  func(ctx context.Context) error
	ReadFunc    func(ctx context.Context, count int, block time.Duration) ([]streams.Message, error)
	ClaimFunc   func(ctx context.Context, idle time.Duration, count int) ([]streams.Message, []string, error)
	AckFunc     func(ctx context.Context, ids ...string) error
	CleanupFunc func(ctx context.Context, idle time.Duration) (int, error)
	LeaveFunc   func(ctx context.Context) error

	mu       sync.Mutex
	acks     []string
	cleanups int
	leaves   int
}

// Stream returns StreamApp or "identity".
func (g *StreamGroup) Stream() string {
	if g.StreamApp == "" {
		return "identity"
	}
	return g.StreamApp
}

// Name returns GroupName or "helpdesk".
func (g *StreamGroup) Name() string {
	if g.GroupName == "" {
		return "helpdesk"
	}
	return g.GroupName
}

// Ensure calls EnsureFunc.
func (g *StreamGroup) Ensure(ctx context.Context) error {
	if g.EnsureFunc != nil {
		return g.EnsureFunc(ctx)
	}
	return nil
}

// Read calls ReadFunc. Without it, Read waits for the end of ctx or for block.
func (g *StreamGroup) Read(ctx context.Context, count int, block time.Duration) ([]streams.Message, error) {
	if g.ReadFunc != nil {
		return g.ReadFunc(ctx, count, block)
	}
	t := time.NewTimer(block)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, nil
	}
}

// Claim calls ClaimFunc.
func (g *StreamGroup) Claim(ctx context.Context, idle time.Duration, count int) ([]streams.Message, []string, error) {
	if g.ClaimFunc != nil {
		return g.ClaimFunc(ctx, idle, count)
	}
	return nil, nil, nil
}

// Ack records the IDs and calls AckFunc.
func (g *StreamGroup) Ack(ctx context.Context, ids ...string) error {
	g.mu.Lock()
	g.acks = append(g.acks, ids...)
	g.mu.Unlock()
	if g.AckFunc != nil {
		return g.AckFunc(ctx, ids...)
	}
	return nil
}

// Cleanup counts the call and calls CleanupFunc.
func (g *StreamGroup) Cleanup(ctx context.Context, idle time.Duration) (int, error) {
	g.mu.Lock()
	g.cleanups++
	g.mu.Unlock()
	if g.CleanupFunc != nil {
		return g.CleanupFunc(ctx, idle)
	}
	return 0, nil
}

// Leave counts the call and calls LeaveFunc.
func (g *StreamGroup) Leave(ctx context.Context) error {
	g.mu.Lock()
	g.leaves++
	g.mu.Unlock()
	if g.LeaveFunc != nil {
		return g.LeaveFunc(ctx)
	}
	return nil
}

// Acked returns the IDs of all Ack calls.
func (g *StreamGroup) Acked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.acks...)
}

// CleanupCalls returns the number of Cleanup calls.
func (g *StreamGroup) CleanupCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cleanups
}

// LeaveCalls returns the number of Leave calls.
func (g *StreamGroup) LeaveCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.leaves
}
