package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
)

// DefaultLoadTimeout is the time limit of the load function of
// [Cache.GetOrLoad] when [WithLoadTimeout] is not set.
const DefaultLoadTimeout = 10 * time.Second

var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func checkNamespace(namespace string) {
	if !namespacePattern.MatchString(namespace) {
		panic(fmt.Sprintf("cache: invalid namespace %q, it must match %s", namespace, namespacePattern))
	}
}

func isMiss(err error) bool { return errors.Is(err, ErrMiss) }

// Codec converts a value to bytes and back.
type Codec[T any] interface {
	Marshal(value T) ([]byte, error)
	Unmarshal(data []byte) (T, error)
}

type jsonCodec[T any] struct{}

func (jsonCodec[T]) Marshal(v T) ([]byte, error) { return json.Marshal(v) }

func (jsonCodec[T]) Unmarshal(data []byte) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

type options struct {
	codec       any
	loadTimeout time.Duration
}

// Option changes the settings of a [Cache].
type Option func(*options)

// WithCodec sets the codec. The default is JSON. The type T must be the type of
// the Cache, and [New] panics otherwise. The JSON codec writes a config.Secret
// field as "[redacted]". Do not put secrets in the cache.
func WithCodec[T any](c Codec[T]) Option {
	return func(o *options) { o.codec = c }
}

// WithLoadTimeout sets the time limit of the load function of
// [Cache.GetOrLoad]. The default is [DefaultLoadTimeout].
func WithLoadTimeout(d time.Duration) Option {
	return func(o *options) { o.loadTimeout = d }
}

// Cache is a typed cache on a [Store]. All keys have the prefix
// "<namespace>:". A span or a log record never contains a key or a value, and
// no metric attribute contains one.
type Cache[T any] struct {
	store       Store
	namespace   string
	codec       Codec[T]
	loadTimeout time.Duration
	m           *metrics
	group       singleflight.Group

	// onWait is a test hook. GetOrLoad calls it after the call joined the
	// group.
	onWait func()
}

// New returns a Cache on store. The namespace goes before each key as
// "<namespace>:<key>". It must match ^[a-z][a-z0-9_]{0,31}$, and New panics
// otherwise. A Cache records the cache metrics, so store must not be the result
// of [Instrument].
func New[T any](store Store, namespace string, opts ...Option) *Cache[T] {
	checkNamespace(namespace)
	o := options{loadTimeout: DefaultLoadTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	var codec Codec[T] = jsonCodec[T]{}
	if o.codec != nil {
		c, ok := o.codec.(Codec[T])
		if !ok {
			panic(fmt.Sprintf("cache: the codec %T is not a Codec for the type of this cache", o.codec))
		}
		codec = c
	}
	return &Cache[T]{
		store: store, namespace: namespace, codec: codec, loadTimeout: o.loadTimeout,
		m: newMetrics(namespace),
	}
}

func (c *Cache[T]) storeKey(key string) (string, error) {
	if key == "" {
		// The namespace would make the store key valid, so check the key itself.
		return "", CheckKey(key)
	}
	sk := c.namespace + ":" + key
	if err := CheckKey(sk); err != nil {
		return "", err
	}
	return sk, nil
}

func (c *Cache[T]) span(ctx context.Context, method string) (context.Context, trace.Span) {
	return otel.Tracer(scopeName).Start(ctx, "cache."+method,
		trace.WithAttributes(attribute.String("cache.namespace", c.namespace)))
}

func endSpan(span trace.Span, hit *bool, err error) {
	if hit != nil {
		span.SetAttributes(attribute.Bool("cache.hit", *hit))
	}
	if err != nil && !isMiss(err) {
		// The error text is not recorded, because it can contain a key.
		span.SetStatus(codes.Error, "cache error")
	}
	span.End()
}

// callLoad calls load and recovers a panic. The shared function of singleflight
// runs in its own goroutine, and singleflight panics again in a new goroutine
// that no caller can recover, so a panic in load must not leave this function.
// The error and the log record have the type of the panic value only, because
// the value can contain secret data.
func (c *Cache[T]) callLoad(ctx context.Context, load func(ctx context.Context) (T, error)) (v T, err error) {
	returned := false
	defer func() {
		if returned {
			return
		}
		r := recover()
		if r == nil {
			return // runtime.Goexit, which singleflight handles
		}
		slog.ErrorContext(ctx, "cache: the load function panicked",
			slog.String("cache.namespace", c.namespace), slog.String("panic_type", fmt.Sprintf("%T", r)))
		var zero T
		v, err = zero, ErrLoadPanic
	}()
	v, err = load(ctx)
	returned = true
	return v, err
}

// fetch reads and decodes one stored key. It records the metrics.
func (c *Cache[T]) fetch(ctx context.Context, sk string) (T, error) {
	var zero T
	start := time.Now()
	data, err := c.store.Get(ctx, sk)
	switch {
	case err == nil:
		c.m.record(ctx, "get", "hit", start, len(data))
	case isMiss(err):
		c.m.record(ctx, "get", "miss", start, -1)
		return zero, ErrMiss
	default:
		c.m.record(ctx, "get", "error", start, -1)
		return zero, err
	}
	v, derr := c.codec.Unmarshal(data)
	if derr != nil {
		c.m.decodeErrs.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(c.m.namespace))
		return zero, fmt.Errorf("%w: %w", ErrDecode, derr)
	}
	return v, nil
}

// put encodes and stores one value. It records the metrics.
func (c *Cache[T]) put(ctx context.Context, sk string, value T, ttl time.Duration) error {
	data, err := c.codec.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache: cannot encode the value: %w", err)
	}
	start := time.Now()
	if err := c.store.Set(ctx, sk, data, ttl); err != nil {
		c.m.record(ctx, "set", "error", start, -1)
		return err
	}
	c.m.record(ctx, "set", "ok", start, len(data))
	return nil
}

// Get returns the value for the key. It returns [ErrMiss] on a miss. It returns
// an error that wraps [ErrDecode] if the stored value does not decode.
func (c *Cache[T]) Get(ctx context.Context, key string) (v T, err error) {
	ctx, span := c.span(ctx, "Get")
	hit := false
	defer func() { endSpan(span, &hit, err) }()
	sk, err := c.storeKey(key)
	if err != nil {
		return v, err
	}
	v, err = c.fetch(ctx, sk)
	hit = err == nil
	return v, err
}

// Set stores the value. The TTL is required. See [CheckTTL].
func (c *Cache[T]) Set(ctx context.Context, key string, value T, ttl time.Duration) (err error) {
	ctx, span := c.span(ctx, "Set")
	hit := false
	defer func() { endSpan(span, &hit, err) }()
	sk, err := c.storeKey(key)
	if err != nil {
		return err
	}
	if err := CheckTTL(ttl); err != nil {
		return err
	}
	return c.put(ctx, sk, value, ttl)
}

// Delete removes the keys. With no keys, it does nothing.
func (c *Cache[T]) Delete(ctx context.Context, keys ...string) (err error) {
	ctx, span := c.span(ctx, "Delete")
	hit := false
	defer func() { endSpan(span, &hit, err) }()
	if len(keys) == 0 {
		return nil
	}
	sks := make([]string, len(keys))
	for i, k := range keys {
		sk, err := c.storeKey(k)
		if err != nil {
			return err
		}
		sks[i] = sk
	}
	start := time.Now()
	if err := c.store.Delete(ctx, sks...); err != nil {
		c.m.record(ctx, "delete", "error", start, -1)
		return err
	}
	c.m.record(ctx, "delete", "ok", start, -1)
	return nil
}

// GetOrLoad returns the cached value, or it calls load and stores the result.
//
// The key and the TTL are checked first. [ErrInvalidKey] and [ErrInvalidTTL]
// are returned without a call of the store or of load. A miss, a decode error,
// and a store error all lead to a call of load. The cache is not the source of
// truth, so a cache failure gives a warn log record and does not stop the
// request.
//
// Calls for the same key in one process at the same time share one call of
// load and one Set. load gets the context of the first caller without its
// cancel, with the time limit of [WithLoadTimeout]. If the context of a caller
// ends, that caller returns the context error and load continues for the other
// callers. If load returns an error, GetOrLoad returns it and stores nothing.
// If load panics, the panic does not go to the caller. GetOrLoad recovers it,
// writes an error log record with the type of the panic value, and returns
// [ErrLoadPanic] to all callers that wait for the same load.
//
// Do not call GetOrLoad in a transaction that changes the data that load reads.
func (c *Cache[T]) GetOrLoad(ctx context.Context, key string, ttl time.Duration,
	load func(ctx context.Context) (T, error)) (v T, err error) {
	ctx, span := c.span(ctx, "GetOrLoad")
	hit := false
	defer func() { endSpan(span, &hit, err) }()

	sk, err := c.storeKey(key)
	if err != nil {
		return v, err
	}
	if err := CheckTTL(ttl); err != nil {
		return v, err
	}

	cached, gerr := c.fetch(ctx, sk)
	switch {
	case gerr == nil:
		hit = true
		return cached, nil
	case isMiss(gerr):
	default:
		slog.WarnContext(ctx, "cache: cannot read the cache, so the value comes from load",
			slog.String("cache.namespace", c.namespace), slog.Any("error", gerr))
	}

	led := false
	ch := c.group.DoChan(sk, func() (any, error) {
		led = true
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.loadTimeout)
		defer cancel()
		start := time.Now()
		val, lerr := c.callLoad(lctx, load)
		outcome := "ok"
		if lerr != nil {
			outcome = "error"
		}
		c.m.loadDur.Record(lctx, time.Since(start).Seconds(),
			metric.WithAttributes(c.m.namespace, attribute.String("outcome", outcome)))
		if lerr != nil {
			return nil, lerr
		}
		if serr := c.put(lctx, sk, val, ttl); serr != nil {
			slog.WarnContext(lctx, "cache: cannot write the cache",
				slog.String("cache.namespace", c.namespace), slog.Any("error", serr))
		}
		return val, nil
	})
	if c.onWait != nil {
		c.onWait()
	}
	select {
	case r := <-ch:
		if !led {
			c.m.loadShared.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(c.m.namespace))
		}
		if r.Err != nil {
			return v, r.Err
		}
		// load can return a nil interface value, so the type check can fail.
		out, _ := r.Val.(T)
		return out, nil
	case <-ctx.Done():
		return v, ctx.Err()
	}
}
