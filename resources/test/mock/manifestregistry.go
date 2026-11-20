package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/manifest"
)

var _ manifest.Registry = (*ManifestRegistry)(nil)

// ManifestRegistry is a mock of [manifest.Registry].
type ManifestRegistry struct {
	// PublishFunc replaces Publish. If it is nil, Publish returns nil.
	PublishFunc func(ctx context.Context, m manifest.PermissionManifest) error

	mu    sync.Mutex
	calls int
}

// Publish calls PublishFunc, or returns nil.
func (r *ManifestRegistry) Publish(ctx context.Context, m manifest.PermissionManifest) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.PublishFunc != nil {
		return r.PublishFunc(ctx, m)
	}
	return nil
}

// PublishCalls returns the number of Publish calls.
func (r *ManifestRegistry) PublishCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
