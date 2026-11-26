package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/eventtypes"
)

var _ eventtypes.Registry = (*EventTypeRegistry)(nil)

// EventTypeRegistry is a mock of [eventtypes.Registry].
type EventTypeRegistry struct {
	// RegisterFunc replaces Register. If it is nil, Register returns a result with
	// the app and the count of the registration, and version 1.
	RegisterFunc func(ctx context.Context, r eventtypes.Registration) (eventtypes.Result, error)

	mu    sync.Mutex
	calls int
}

// Register calls RegisterFunc, or returns a default result.
func (r *EventTypeRegistry) Register(ctx context.Context, reg eventtypes.Registration) (eventtypes.Result, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.RegisterFunc != nil {
		return r.RegisterFunc(ctx, reg)
	}
	return eventtypes.Result{App: reg.App, Version: 1, EventTypeCount: len(reg.EventTypes)}, nil
}

// RegisterCalls returns the number of Register calls.
func (r *EventTypeRegistry) RegisterCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
