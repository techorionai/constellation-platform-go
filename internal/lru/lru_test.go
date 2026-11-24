package lru

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// U36: size limit.
func TestU36SizeLimit(t *testing.T) {
	c := New[int, int](3)
	for i := 0; i < 10; i++ {
		c.Set(i, i, t0.Add(time.Hour))
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
}

// U36: expiry.
func TestU36Expiry(t *testing.T) {
	c := New[string, string](3)
	c.Set("a", "1", t0.Add(10*time.Second))
	if v, ok := c.Get("a", t0.Add(9*time.Second)); !ok || v != "1" {
		t.Fatalf("Get before expiry = %q, %v", v, ok)
	}
	if _, ok := c.Get("a", t0.Add(10*time.Second)); ok {
		t.Fatal("entry must be expired at its expiry time")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry must be removed, Len = %d", c.Len())
	}
}

// U36: order of removal.
func TestU36Order(t *testing.T) {
	c := New[string, int](2)
	exp := t0.Add(time.Hour)
	c.Set("a", 1, exp)
	c.Set("b", 2, exp)
	c.Get("a", t0) // a is now the most recent
	c.Set("c", 3, exp)
	if _, ok := c.Get("b", t0); ok {
		t.Fatal("b must be removed")
	}
	if _, ok := c.Get("a", t0); !ok {
		t.Fatal("a must stay")
	}
	c.Set("a", 9, exp) // replace
	if v, _ := c.Get("a", t0); v != 9 {
		t.Fatalf("a = %d, want 9", v)
	}
	c.Delete("a")
	if _, ok := c.Get("a", t0); ok {
		t.Fatal("a must be deleted")
	}
	if New[int, int](0).max != 1 {
		t.Fatal("max below 1 must be 1")
	}
}

// U36: 50 concurrent users. Run with -race.
func TestU36Concurrent(t *testing.T) {
	c := New[string, int](100)
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := strconv.Itoa((g*7 + i) % 200)
				c.Set(k, i, t0.Add(time.Hour))
				c.Get(k, t0)
				if i%50 == 0 {
					c.Delete(k)
				}
			}
		}()
	}
	wg.Wait()
	if c.Len() > 100 {
		t.Fatalf("Len = %d, want 100 or less", c.Len())
	}
}
