package mock

import (
	"context"
	"sync"
	"time"

	"github.com/techorionai/constellation-platform-go/cache"
)

var _ cache.Store = (*CacheStore)(nil)

// CacheStore is a mock of [cache.Store]. It keeps the values in a map. The
// default methods apply [cache.CheckKey], [cache.CheckTTL], and
// [cache.CheckValue], as each real store does. The values never expire.
type CacheStore struct {
	// GetFunc replaces Get. If it is nil, Get reads the map.
	GetFunc func(ctx context.Context, key string) ([]byte, error)
	// SetFunc replaces Set. If it is nil, Set writes the map.
	SetFunc func(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// DeleteFunc replaces Delete. If it is nil, Delete removes the keys from the
	// map.
	DeleteFunc func(ctx context.Context, keys ...string) error

	mu       sync.Mutex
	values   map[string][]byte
	ttls     map[string]time.Duration
	getCalls int
	setCalls int
	delCalls int
}

// Get calls GetFunc, or reads the map. It returns [cache.ErrMiss] if the key is
// not in the map.
func (s *CacheStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.getCalls++
	s.mu.Unlock()
	if s.GetFunc != nil {
		return s.GetFunc(ctx, key)
	}
	if err := cache.CheckKey(key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return nil, cache.ErrMiss
	}
	return append([]byte(nil), v...), nil
}

// Set calls SetFunc, or writes the map.
func (s *CacheStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.setCalls++
	s.mu.Unlock()
	if s.SetFunc != nil {
		return s.SetFunc(ctx, key, value, ttl)
	}
	if err := cache.CheckKey(key); err != nil {
		return err
	}
	if err := cache.CheckTTL(ttl); err != nil {
		return err
	}
	if err := cache.CheckValue(value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.values[key] = append([]byte(nil), value...)
	s.ttls[key] = ttl
	return nil
}

// Delete calls DeleteFunc, or removes the keys from the map.
func (s *CacheStore) Delete(ctx context.Context, keys ...string) error {
	s.mu.Lock()
	s.delCalls++
	s.mu.Unlock()
	if s.DeleteFunc != nil {
		return s.DeleteFunc(ctx, keys...)
	}
	for _, k := range keys {
		if err := cache.CheckKey(k); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.values, k)
		delete(s.ttls, k)
	}
	return nil
}

func (s *CacheStore) init() {
	if s.values == nil {
		s.values = map[string][]byte{}
		s.ttls = map[string]time.Duration{}
	}
}

// Put writes a value to the map with no check and no call count. Use it to
// prepare a test, for example a value that does not decode.
func (s *CacheStore) Put(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.values[key] = append([]byte(nil), value...)
}

// Value returns the value in the map for the key.
func (s *CacheStore) Value(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	return append([]byte(nil), v...), ok
}

// TTL returns the TTL of the last Set call for the key.
func (s *CacheStore) TTL(key string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttls[key]
}

// GetCalls returns the number of Get calls.
func (s *CacheStore) GetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls
}

// SetCalls returns the number of Set calls.
func (s *CacheStore) SetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setCalls
}

// DeleteCalls returns the number of Delete calls.
func (s *CacheStore) DeleteCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delCalls
}
