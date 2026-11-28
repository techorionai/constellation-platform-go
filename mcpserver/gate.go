package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/techorionai/constellation-platform-go/auth"
)

// gate reads the JSON-RPC body before the SDK. For a tools/call of a known tool
// with a missing scope it writes the 403 response. The tool wrapper checks the
// scopes again.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
		if err != nil {
			writeProblem(ctx, w, http.StatusBadRequest, "bad_request", "Bad request")
			return
		}
		if int64(len(body)) > s.cfg.MaxBodyBytes {
			writeProblem(ctx, w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large")
			return
		}
		if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
			writeProblem(ctx, w, http.StatusBadRequest, "batch_not_supported", "JSON-RPC batches are not supported")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var msg struct {
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil || msg.Method != "tools/call" {
			next.ServeHTTP(w, r)
			return
		}
		scopes, known := s.scopesOf(msg.Params.Name)
		if !known {
			next.ServeHTTP(w, r)
			return
		}
		p, ok := auth.PrincipalFromContext(ctx)
		if !ok {
			slog.ErrorContext(ctx, "mcpserver: the request has no principal, the chain has no auth.Middleware")
			writeProblem(ctx, w, http.StatusInternalServerError, "auth_not_configured", "Authentication not configured")
			return
		}
		if missing := p.MissingScopes(scopes...); len(missing) > 0 {
			if s.cfg.OnRefused != nil {
				s.cfg.OnRefused(ctx, Refusal{Tool: msg.Params.Name, Missing: missing, Arguments: msg.Params.Arguments})
			}
			sctx, span := s.tel.start(ctx, msg.Params.Name)
			s.tel.count(sctx, msg.Params.Name, outcomeDenied)
			span.End()
			auth.WriteInsufficientScope(ctx, w, scopes, missing, s.metadataURL)
			return
		}
		next.ServeHTTP(w, r)
	})
}
