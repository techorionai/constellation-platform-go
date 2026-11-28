package mcpserver

import (
	"encoding/json"
	"net/http"
)

// MetadataHandler returns the handler of the protected resource metadata of
// RFC 9728. Mount it at /.well-known/oauth-protected-resource<path of the MCP
// endpoint>. It needs no token.
func (s *Server) MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeProblem(r.Context(), w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		doc := map[string]any{
			"resource":                 s.cfg.ResourceURL,
			"authorization_servers":    s.cfg.AuthorizationServers,
			"scopes_supported":         s.allScopes(),
			"bearer_methods_supported": []string{"header"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(doc)
	})
}
