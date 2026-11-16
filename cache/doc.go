// Package cache holds the cache port and the typed cache of the platform.
//
// [Store] is the port. It stores bytes under a key, and each entry has a TTL.
// The adapter for Valkey is in the package cache/valkey. [Cache] is a typed
// cache on a [Store]. It adds a namespace to each key, encodes the values (JSON
// by default), records spans and metrics, and has [Cache.GetOrLoad]. This
// package does not import a Valkey driver.
//
// The cache is not the source of truth. [Cache.GetOrLoad] calls the load
// function when the cache has a miss, has a value that does not decode, or
// fails. Calls for the same key in one process share one load call.
//
//	profiles := cache.New[Profile](store, "profile")
//	p, err := profiles.GetOrLoad(ctx, "user:42", time.Minute,
//		func(ctx context.Context) (Profile, error) { return repo.Find(ctx, 42) })
//
// A span, a log record, and a metric attribute never contain a key or a value.
// Do not put secrets or decrypted secret values in the cache. The default JSON
// codec writes a config.Secret field as "[redacted]".
//
// [Instrument] records the store metrics for code that uses a [Store] without
// [Cache].
package cache
