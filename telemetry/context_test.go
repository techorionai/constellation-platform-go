package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// U24
func TestContextWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	child := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(newHandler(slog.LevelInfo, []slog.Handler{child})).WithGroup("g")

	ctx := ContextWithAttrs(context.Background(), slog.String("request.id", "r1"))
	ctx = ContextWithAttrs(ctx, slog.String("principal.id", "p1"))
	logger.InfoContext(ctx, "with", slog.String("k", "v"))
	logger.InfoContext(context.Background(), "without")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"request.id":"r1"`) || !strings.Contains(lines[0], `"principal.id":"p1"`) {
		t.Errorf("record with context lacks attributes: %s", lines[0])
	}
	if strings.Contains(lines[0], `"g":{"request.id"`) {
		t.Errorf("attributes must be outside the group: %s", lines[0])
	}
	if strings.Contains(lines[1], "request.id") || strings.Contains(lines[1], "principal.id") {
		t.Errorf("record without context has attributes: %s", lines[1])
	}
}
