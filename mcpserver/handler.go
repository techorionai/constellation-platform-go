package mcpserver

import (
	"net/http"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/techorionai/constellation-platform-go/auth"
)

// Handler returns the MCP endpoint. The chain is the no-store headers, the
// origin check, the token check, the scope gate, and the SDK handler.
func (s *Server) Handler() http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	chain := auth.Middleware(s.cfg.Verifier, auth.WithResourceMetadata(s.metadataURL),
		auth.WithMeterProvider(s.cfg.MeterProvider))(s.gate(sdk))
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &noStoreWriter{ResponseWriter: rw}
		w.start()
		for _, o := range r.Header.Values("Origin") {
			if !slices.Contains(s.cfg.AllowedOrigins, o) {
				writeProblem(r.Context(), w, http.StatusForbidden, "origin_not_allowed", "Origin not allowed")
				return
			}
		}
		chain.ServeHTTP(w, r)
	})
}

// noStoreWriter sets the no-store headers when the response starts. The SDK
// handler sets its own Cache-Control header, so the headers must be set again
// at the start of the response.
type noStoreWriter struct {
	http.ResponseWriter
}

func (w *noStoreWriter) start() {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
}

func (w *noStoreWriter) WriteHeader(code int) {
	w.start()
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	w.start()
	return w.ResponseWriter.Write(b)
}

func (w *noStoreWriter) Flush() {
	w.start()
	_ = http.NewResponseController(w.ResponseWriter).Flush() // best effort
}

// Unwrap returns the wrapped writer, for [http.ResponseController].
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
