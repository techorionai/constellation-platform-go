//go:build integration

package valkey_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/cache"
	cachevk "github.com/techorionai/constellation-platform-go/cache/valkey"
	clientvk "github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

func adminClient(t *testing.T, v *containers.ValkeyInstance, prefix string) *clientvk.Client {
	t.Helper()
	c, err := clientvk.New(t.Context(), clientvk.Config{Address: v.Address(), Password: v.AdminPassword(), KeyPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func rawDo(ctx context.Context, c valkeygo.Client, args ...string) valkeygo.ValkeyResult {
	return c.Do(ctx, c.B().Arbitrary(args[0]).Args(args[1:]...).Build())
}

type item struct {
	Name string `json:"name"`
}

func TestIntegrationStore(t *testing.T) {
	v := containers.Valkey(t)
	ctx := t.Context()

	// I1
	t.Run("I1 round trip and key prefix", func(t *testing.T) {
		c := adminClient(t, v, "identity")
		s := cachevk.NewStore(c)
		if err := s.Set(ctx, "user:1", []byte("value"), time.Minute); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "user:1")
		if err != nil || string(got) != "value" {
			t.Fatalf("got %q, %v", got, err)
		}
		raw, err := rawDo(ctx, c.Raw, "GET", "identity:cache:user:1").ToString()
		if err != nil || raw != "value" {
			t.Fatalf("raw key: %q, %v", raw, err)
		}
		if err := s.Delete(ctx, "user:1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "user:1"); !errors.Is(err, cache.ErrMiss) {
			t.Fatalf("err = %v", err)
		}
	})

	// I2
	t.Run("I2 TTL", func(t *testing.T) {
		c := adminClient(t, v, "identity")
		s := cachevk.NewStore(c)
		if err := s.Set(ctx, "ttl", []byte("v"), 2*time.Second); err != nil {
			t.Fatal(err)
		}
		pttl, err := rawDo(ctx, c.Raw, "PTTL", "identity:cache:ttl").AsInt64()
		if err != nil || pttl <= 0 || pttl > 2000 {
			t.Fatalf("PTTL = %d, %v", pttl, err)
		}
	})

	// I3
	t.Run("I3 delete with a missing key", func(t *testing.T) {
		c := adminClient(t, v, "identity")
		s := cachevk.NewStore(c)
		_ = s.Set(ctx, "a", []byte("1"), time.Minute)
		_ = s.Set(ctx, "b", []byte("2"), time.Minute)
		if err := s.Delete(ctx, "a", "missing", "b"); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"a", "b"} {
			if _, err := s.Get(ctx, k); !errors.Is(err, cache.ErrMiss) {
				t.Fatalf("key %s: err = %v", k, err)
			}
		}
	})

	// I4
	t.Run("I4 ACL user", func(t *testing.T) {
		user, password := v.NewACLUser(t, "identity")
		c, err := clientvk.New(ctx, clientvk.Config{Address: v.Address(), Username: user, Password: password, KeyPrefix: "identity"})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s := cachevk.NewStore(c)
		if err := s.Set(ctx, "acl", []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Get(ctx, "acl"); err != nil || string(got) != "v" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	// I5
	t.Run("I5 GetOrLoad", func(t *testing.T) {
		c := adminClient(t, v, "identity")
		items := cache.New[item](cachevk.NewStore(c), "items")
		loads := 0
		load := func(context.Context) (item, error) { loads++; return item{"Ada"}, nil }
		for range 2 {
			got, err := items.GetOrLoad(ctx, "i5", time.Minute, load)
			if err != nil || got.Name != "Ada" {
				t.Fatalf("got %+v, %v", got, err)
			}
		}
		if loads != 1 {
			t.Fatalf("loads = %d", loads)
		}
	})

	// I19
	t.Run("I19 miss", func(t *testing.T) {
		s := cachevk.NewStore(adminClient(t, v, "identity"))
		if _, err := s.Get(ctx, "does-not-exist"); !errors.Is(err, cache.ErrMiss) {
			t.Fatalf("err = %v", err)
		}
	})

	// I20
	t.Run("I20 permission error is not a miss", func(t *testing.T) {
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(old) })

		user, password := v.NewACLUser(t, "helpdesk")
		c, err := clientvk.New(ctx, clientvk.Config{Address: v.Address(), Username: user, Password: password, KeyPrefix: "identity"})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s := cachevk.NewStore(c)
		_, err = s.Get(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "NOPERM") || errors.Is(err, cache.ErrMiss) {
			t.Fatalf("err = %v", err)
		}
		items := cache.New[item](s, "items")
		loads := 0
		got, err := items.GetOrLoad(ctx, "x", time.Minute, func(context.Context) (item, error) { loads++; return item{"Ada"}, nil })
		if err != nil || got.Name != "Ada" || loads != 1 {
			t.Fatalf("got %+v, %v, loads %d", got, err, loads)
		}
		if strings.Count(buf.String(), `"level":"WARN"`) < 1 {
			t.Fatalf("log: %s", buf.String())
		}
	})

	// I21
	t.Run("I21 TTL rounding", func(t *testing.T) {
		c := adminClient(t, v, "identity")
		s := cachevk.NewStore(c)
		if err := s.Set(ctx, "round", []byte("v"), 10*time.Second+500*time.Microsecond); err != nil {
			t.Fatal(err)
		}
		pttl, err := rawDo(ctx, c.Raw, "PTTL", "identity:cache:round").AsInt64()
		if err != nil || pttl <= 9000 || pttl > 10001 {
			t.Fatalf("PTTL = %d, %v", pttl, err)
		}
	})

	// I23
	t.Run("I23 invalid keys", func(t *testing.T) {
		s := cachevk.NewStore(adminClient(t, v, "identity"))
		for _, key := range []string{"", strings.Repeat("a", 257)} {
			if err := s.Set(ctx, key, []byte("v"), time.Minute); !errors.Is(err, cache.ErrInvalidKey) {
				t.Fatalf("Set(%d bytes): err = %v", len(key), err)
			}
			if _, err := s.Get(ctx, key); !errors.Is(err, cache.ErrInvalidKey) {
				t.Fatalf("Get(%d bytes): err = %v", len(key), err)
			}
			if err := s.Delete(ctx, key); !errors.Is(err, cache.ErrInvalidKey) {
				t.Fatalf("Delete(%d bytes): err = %v", len(key), err)
			}
		}
	})
}

// delCalls reads the number of DEL calls from INFO commandstats.
func delCalls(t *testing.T, c valkeygo.Client) int {
	t.Helper()
	info, err := rawDo(t.Context(), c, "INFO", "commandstats").ToString()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		if rest, ok := strings.CutPrefix(line, "cmdstat_del:calls="); ok {
			n, _, _ := strings.Cut(rest, ",")
			v, err := strconv.Atoi(n)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// I22: this test has its own container and does not run in parallel.
func TestIntegrationDeleteCommandCount(t *testing.T) {
	v := containers.Valkey(t)
	c := adminClient(t, v, "identity")
	s := cachevk.NewStore(c)
	admin := v.Admin(t)

	before := delCalls(t, admin)
	if err := s.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := delCalls(t, admin); after != before {
		t.Fatalf("DEL count changed from %d to %d for no keys", before, after)
	}
	if err := s.Delete(t.Context(), "a", "b", "c"); err != nil {
		t.Fatal(err)
	}
	if after := delCalls(t, admin); after != before+3 {
		t.Fatalf("DEL count = %d, want %d", after, before+3)
	}
}
