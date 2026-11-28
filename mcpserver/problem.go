package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/requestid"
)

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestId,omitempty"`
	TraceID   string `json:"traceId,omitempty"`
}

func writeProblem(ctx context.Context, w http.ResponseWriter, status int, code, title string) {
	p := problem{Type: "about:blank", Title: title, Status: status, Code: code, RequestID: requestid.FromContext(ctx)}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		p.TraceID = sc.TraceID().String()
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.WarnContext(ctx, "mcpserver: write problem failed", slog.Any("error", err))
	}
}
