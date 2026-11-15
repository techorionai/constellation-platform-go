package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/config"
)

type dbConfig struct {
	URL config.Secret `env:"URL,required" envFile:"true"`
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// U1
func TestLoadSimple(t *testing.T) {
	t.Setenv("SVC_NAME", "a")
	var cfg struct {
		Name string `env:"NAME"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "a" {
		t.Fatalf("got %q", cfg.Name)
	}
}

// U2
func TestLoadNested(t *testing.T) {
	t.Setenv("SVC_DATABASE__URL", "x")
	var cfg struct {
		Database dbConfig `envPrefix:"DATABASE__"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL.Reveal() != "x" {
		t.Fatalf("got %q", cfg.Database.URL.Reveal())
	}
}

// U3
func TestLoadFile(t *testing.T) {
	for _, content := range []string{"x\n", "x\r\n"} {
		t.Setenv("SVC_DATABASE__URL_FILE", writeFile(t, content))
		var cfg struct {
			Database dbConfig `envPrefix:"DATABASE__"`
		}
		if err := config.Load("SVC_", &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Database.URL.Reveal() != "x" {
			t.Fatalf("got %q", cfg.Database.URL.Reveal())
		}
	}
}

// U4
func TestLoadFileAndValue(t *testing.T) {
	t.Setenv("SVC_DATABASE__URL", "x")
	t.Setenv("SVC_DATABASE__URL_FILE", writeFile(t, "y"))
	var cfg struct {
		Database dbConfig `envPrefix:"DATABASE__"`
	}
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_DATABASE__URL") {
		t.Fatalf("got %v", err)
	}
}

// U5
func TestLoadFileMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing")
	t.Setenv("SVC_DATABASE__URL_FILE", p)
	var cfg struct {
		Database dbConfig `envPrefix:"DATABASE__"`
	}
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_DATABASE__URL_FILE") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("got %v", err)
	}
}

// U6
func TestLoadCollectsErrors(t *testing.T) {
	var cfg struct {
		A string `env:"A,required"`
		B string `env:"B,required"`
	}
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_A") || !strings.Contains(err.Error(), "SVC_B") {
		t.Fatalf("got %v", err)
	}
}

// U7
func TestLoadParseErrorHidesValue(t *testing.T) {
	t.Setenv("SVC_PORT", "abc")
	var cfg struct {
		Port int `env:"PORT"`
	}
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_PORT") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("got %v", err)
	}
}

// U8
func TestLoadDefault(t *testing.T) {
	var cfg struct {
		N int `env:"N" envDefault:"10"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.N != 10 {
		t.Fatalf("got %d", cfg.N)
	}
}

// U9
func TestLoadBadPrefix(t *testing.T) {
	for _, p := range []string{"svc", "SVC", ""} {
		var cfg struct{}
		if err := config.Load(p, &cfg); err == nil {
			t.Errorf("prefix %q: want error", p)
		}
	}
}

// U10
func TestSecretRedacted(t *testing.T) {
	s := config.Secret("p")
	for _, got := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s), //nolint:gocritic
	} {
		if got != "[redacted]" {
			t.Errorf("got %q", got)
		}
	}
	b, err := json.Marshal(s)
	if err != nil || string(b) != `"[redacted]"` {
		t.Errorf("json: %s %v", b, err)
	}
	b, err = json.Marshal(struct{ P config.Secret }{s})
	if err != nil || strings.Contains(string(b), "p\"") {
		t.Errorf("json struct: %s %v", b, err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "k", s)
	if !strings.Contains(buf.String(), "[redacted]") || strings.Contains(buf.String(), "k=p") {
		t.Errorf("slog: %s", buf.String())
	}
	if s.Reveal() != "p" {
		t.Error("reveal")
	}
}

// U11
func TestLoadIgnoresOtherPrefix(t *testing.T) {
	t.Setenv("OTHER_NAME", "z")
	var cfg struct {
		Name string `env:"NAME"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "" {
		t.Fatalf("got %q", cfg.Name)
	}
}

// U31
func TestLoadFileOnlyWithTag(t *testing.T) {
	t.Setenv("SVC_LOG_FILE", "/tmp/a.log")
	t.Setenv("SVC_TOKEN_FILE", writeFile(t, "x"))
	var cfg struct {
		Path  string `env:"LOG_FILE"`
		Token string `env:"TOKEN"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Path != "/tmp/a.log" || cfg.Token != "" {
		t.Fatalf("got %+v", cfg)
	}
}
