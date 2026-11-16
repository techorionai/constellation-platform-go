// Package valkey is the Valkey adapter of the cache port in the package cache.
//
// [Store] stores each key as "<prefix>:cache:<key>", where prefix is the key
// prefix of the client of clients/valkey. It uses the commands GET, SET with
// PX, and DEL only. Delete sends one DEL for each key, so the adapter also works
// in cluster mode, where the keys can be in different slots.
//
//	store := cachevk.NewStore(kv)
//	profiles := cache.New[Profile](store, "profile")
package valkey
