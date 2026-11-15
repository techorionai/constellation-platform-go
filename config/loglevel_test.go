package config_test

import (
	"log/slog"
	"testing"

	"github.com/techorionai/constellation-platform-go/config"
)

// The startup sample in docs/usage.md loads a slog.Level from LOG_LEVEL.
func TestLoadSlogLevel(t *testing.T) {
	var cfg struct {
		LogLevel slog.Level `env:"LOG_LEVEL" envDefault:"INFO"`
	}
	if err := config.Load("SVC_", &cfg); err != nil || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("default: %v %v", cfg.LogLevel, err)
	}
	t.Setenv("SVC_LOG_LEVEL", "WARN")
	if err := config.Load("SVC_", &cfg); err != nil || cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("WARN: %v %v", cfg.LogLevel, err)
	}
}
