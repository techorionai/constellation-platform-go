package cache

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors of the cache port.
var (
	// ErrMiss means that the key does not exist in the store.
	ErrMiss = errors.New("cache: miss")
	// ErrInvalidKey means that the key is empty or longer than [MaxKeyLen] bytes.
	ErrInvalidKey = errors.New("cache: invalid key")
	// ErrInvalidTTL means that the TTL is shorter than [MinTTL] or longer than
	// [MaxTTL].
	ErrInvalidTTL = errors.New("cache: invalid TTL")
	// ErrTooLarge means that the value is longer than [MaxValueSize] bytes.
	ErrTooLarge = errors.New("cache: value too large")
	// ErrDecode means that a stored value does not decode.
	ErrDecode = errors.New("cache: cannot decode the stored value")
	// ErrLoadPanic means that the load function of GetOrLoad panicked. The
	// error does not contain the panic value.
	ErrLoadPanic = errors.New("cache: the load function panicked")
)

// Limits that each [Store] enforces.
const (
	// MaxKeyLen is the maximum length of a key in bytes.
	MaxKeyLen = 256
	// MinTTL is the shortest TTL.
	MinTTL = time.Millisecond
	// MaxTTL is the longest TTL.
	MaxTTL = 30 * 24 * time.Hour
	// MaxValueSize is the maximum size of a value in bytes.
	MaxValueSize = 1 << 20
)

// Store is the cache port. It stores bytes under a key. The values of a Store
// expire. An entry without a TTL is not possible.
type Store interface {
	// Get returns ErrMiss if the key does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores the value. The TTL must be from [MinTTL] to [MaxTTL].
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes the keys. A key that does not exist is not an error.
	Delete(ctx context.Context, keys ...string) error
}

// CheckKey returns [ErrInvalidKey] if the key is empty or longer than
// [MaxKeyLen] bytes. Each [Store] calls it before it makes the stored key or
// sends a command. The error text does not contain the key.
func CheckKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the key is empty", ErrInvalidKey)
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("%w: the key is longer than %d bytes", ErrInvalidKey, MaxKeyLen)
	}
	return nil
}

// CheckTTL returns [ErrInvalidTTL] if the TTL is shorter than [MinTTL] or longer
// than [MaxTTL].
func CheckTTL(ttl time.Duration) error {
	if ttl < MinTTL || ttl > MaxTTL {
		return fmt.Errorf("%w: the TTL must be from %s to %s", ErrInvalidTTL, MinTTL, MaxTTL)
	}
	return nil
}

// CheckValue returns [ErrTooLarge] if the value is longer than [MaxValueSize]
// bytes.
func CheckValue(value []byte) error {
	if len(value) > MaxValueSize {
		return fmt.Errorf("%w: the value is longer than %d bytes", ErrTooLarge, MaxValueSize)
	}
	return nil
}
