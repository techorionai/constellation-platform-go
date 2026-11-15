package valkey_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
)

type svcConfig struct {
	Valkey valkey.Config `envPrefix:"VALKEY__"`
}

// U20
func TestLoadPasswordFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SVC_VALKEY__ADDRESS", "h:6379")
	t.Setenv("SVC_VALKEY__KEY_PREFIX", "identity")
	t.Setenv("SVC_VALKEY__PASSWORD_FILE", p)
	var cfg svcConfig
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Valkey.Password.Reveal() != "secret" {
		t.Fatalf("got %q", cfg.Valkey.Password.Reveal())
	}
}

// U27
func TestKeyspace(t *testing.T) {
	ks, err := valkey.NewKeyspace("identity")
	if err != nil {
		t.Fatal(err)
	}
	if got := ks.Key("session", "42"); got != "identity:session:42" {
		t.Errorf("Key: %q", got)
	}
	if got := ks.Pattern("session"); got != "identity:session:*" {
		t.Errorf("Pattern: %q", got)
	}
	if got := ks.Pattern("a*b", "c?[d]\\"); got != `identity:a\*b:c\?\[d\]\\:*` {
		t.Errorf("Pattern escape: %q", got)
	}
	if ks.Prefix() != "identity" {
		t.Errorf("Prefix: %q", ks.Prefix())
	}
}

// U28
func TestNewKeyspaceInvalid(t *testing.T) {
	for _, p := range []string{"Identity", "1abc", strings.Repeat("a", 33), ""} {
		if _, err := valkey.NewKeyspace(p); !errors.Is(err, valkey.ErrInvalidKeyPrefix) {
			t.Errorf("%q: got %v", p, err)
		}
	}
}

// U29
func TestKeyEmptyPartPanics(t *testing.T) {
	ks, _ := valkey.NewKeyspace("identity")
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	ks.Key("session", "")
}

// U30
func TestLoadRequiresKeyPrefix(t *testing.T) {
	t.Setenv("SVC_VALKEY__ADDRESS", "h:6379")
	var cfg svcConfig
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_VALKEY__KEY_PREFIX") {
		t.Fatalf("got %v", err)
	}
}

// U37
func TestNewCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c, err := valkey.New(ctx, valkey.Config{Address: "127.0.0.1:1", KeyPrefix: "identity"})
	if !errors.Is(err, context.Canceled) || c != nil {
		t.Fatalf("got %v %v", c, err)
	}
}

// U38
func TestLoadDefaultsAndValues(t *testing.T) {
	t.Setenv("SVC_VALKEY__ADDRESS", "h:6379")
	t.Setenv("SVC_VALKEY__KEY_PREFIX", "identity")
	var cfg svcConfig
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	v := cfg.Valkey
	if v.Username != "" || v.TLS || v.DialTimeout != 5*time.Second {
		t.Fatalf("defaults: %+v", v)
	}
	t.Setenv("SVC_VALKEY__USERNAME", "u")
	t.Setenv("SVC_VALKEY__TLS", "true")
	t.Setenv("SVC_VALKEY__DIAL_TIMEOUT", "2s")
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	v = cfg.Valkey
	if v.Username != "u" || !v.TLS || v.DialTimeout != 2*time.Second {
		t.Fatalf("values: %+v", v)
	}
}
