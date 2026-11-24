// Package lru is a small generic LRU cache with a size limit and a per-entry
// expiry time. It is safe for concurrent use.
package lru

import (
	"container/list"
	"sync"
	"time"
)

type item[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time
}

// Cache is an LRU cache. When it is full, a new entry removes the entry that
// was used least recently. An expired entry is never returned.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	max   int
	items map[K]*list.Element
	order *list.List // front is the most recently used entry
}

// New returns a cache that holds at most max entries. A max of less than 1 is
// treated as 1.
func New[K comparable, V any](max int) *Cache[K, V] {
	if max < 1 {
		max = 1
	}
	return &Cache[K, V]{max: max, items: make(map[K]*list.Element), order: list.New()}
}

// Get returns the value for the key. The entry is a miss when it is not in the
// cache or when now is not before its expiry time. An expired entry is removed.
func (c *Cache[K, V]) Get(key K, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	it := el.Value.(*item[K, V])
	if !now.Before(it.expiresAt) {
		c.removeElement(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return it.value, true
}

// Set stores the value until expiresAt. It replaces an existing entry. If the
// cache is full, it removes the least recently used entry.
func (c *Cache[K, V]) Set(key K, value V, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*item[K, V])
		it.value, it.expiresAt = value, expiresAt
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&item[K, V]{key: key, value: value, expiresAt: expiresAt})
	for c.order.Len() > c.max {
		c.removeElement(c.order.Back())
	}
}

// Delete removes the key.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Len returns the number of entries, including entries that are expired but not
// yet removed.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *Cache[K, V]) removeElement(el *list.Element) {
	it := c.order.Remove(el).(*item[K, V])
	delete(c.items, it.key)
}
