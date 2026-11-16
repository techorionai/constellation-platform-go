package valkey

import (
	"context"
	"errors"
	"fmt"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/cache"
	clientsvalkey "github.com/techorionai/constellation-platform-go/clients/valkey"
)

var _ cache.Store = (*Store)(nil)

// Store is the Valkey adapter of [cache.Store]. It stores each key as
// "<prefix>:cache:<key>", where prefix is the key prefix of the client.
type Store struct {
	client *clientsvalkey.Client
}

// NewStore returns a Store on the client.
func NewStore(client *clientsvalkey.Client) *Store {
	return &Store{client: client}
}

// valkeyKey checks the key, then makes the Valkey key. The check comes first,
// because Keyspace.Key panics on an empty part.
func (s *Store) valkeyKey(key string) (string, error) {
	if err := cache.CheckKey(key); err != nil {
		return "", err
	}
	return s.client.Keys.Key("cache", key), nil
}

// Get returns the value. It returns [cache.ErrMiss] if the key does not exist.
// It wraps each other error, for example NOPERM, and that error is not
// [cache.ErrMiss].
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	k, err := s.valkeyKey(key)
	if err != nil {
		return nil, err
	}
	raw := s.client.Raw
	v, err := raw.Do(ctx, raw.B().Get().Key(k).Build()).AsBytes()
	if err != nil {
		if valkeygo.IsValkeyNil(err) {
			return nil, cache.ErrMiss
		}
		return nil, fmt.Errorf("cache/valkey: GET failed: %w", err)
	}
	return v, nil
}

// ttlMillis rounds the TTL up to whole milliseconds, because PX takes
// milliseconds.
func ttlMillis(ttl time.Duration) int64 {
	ms := ttl / time.Millisecond
	if ttl%time.Millisecond != 0 {
		ms++
	}
	return int64(ms)
}

// Set stores the value with the TTL. It rounds the TTL up to whole
// milliseconds.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := cache.CheckKey(key); err != nil {
		return err
	}
	if err := cache.CheckTTL(ttl); err != nil {
		return err
	}
	if err := cache.CheckValue(value); err != nil {
		return err
	}
	k := s.client.Keys.Key("cache", key)
	raw := s.client.Raw
	cmd := raw.B().Set().Key(k).Value(valkeygo.BinaryString(value)).PxMilliseconds(ttlMillis(ttl)).Build()
	if err := raw.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("cache/valkey: SET failed: %w", err)
	}
	return nil
}

// Delete sends one DEL for each key in one DoMulti call, so that it also works
// in cluster mode, where the keys can be in different slots. With no keys, it
// sends no command. If one or more DEL commands fail, it returns the errors
// joined.
func (s *Store) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	raw := s.client.Raw
	cmds := make(valkeygo.Commands, 0, len(keys))
	for _, key := range keys {
		k, err := s.valkeyKey(key)
		if err != nil {
			return err
		}
		cmds = append(cmds, raw.B().Del().Key(k).Build())
	}
	var errs []error
	for _, res := range raw.DoMulti(ctx, cmds...) {
		if err := res.Error(); err != nil {
			errs = append(errs, fmt.Errorf("cache/valkey: DEL failed: %w", err))
		}
	}
	return errors.Join(errs...)
}
