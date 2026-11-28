package mcpserver

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/techorionai/constellation-platform-go/mcpserver"

// Outcomes of a tool call.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
	outcomeDenied  = "denied"
	outcomeInvalid = "invalid"
)

type telemetry struct {
	tracer  trace.Tracer
	counter metric.Int64Counter
}

func newTelemetry(mp metric.MeterProvider, tp trace.TracerProvider) telemetry {
	counter, _ := mp.Meter(scopeName).Int64Counter("constellation.mcp.tool.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Number of MCP tool calls, by tool and outcome."))
	return telemetry{tracer: tp.Tracer(scopeName), counter: counter}
}

func (t telemetry) start(ctx context.Context, tool string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "mcp.tool", trace.WithAttributes(attribute.String("mcp.tool.name", tool)))
}

func (t telemetry) count(ctx context.Context, tool, outcome string) {
	t.counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tool", tool), attribute.String("outcome", outcome)))
}
