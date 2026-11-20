// Package requestid gives a request ID to each HTTP request.
//
// [Middleware] reads the header X-Request-ID. It uses the value if it matches
// ^[A-Za-z0-9._-]{1,128}$. Otherwise it makes a new UUID v7. The ID goes in the
// request context, in the response header X-Request-ID, on the active span as
// http.request.id, and on the log records as request.id. The log records get
// the attribute through [telemetry.ContextWithAttrs], so a handler must write
// them with a context, for example with [log/slog.InfoContext].
//
// [FromContext] returns the ID.
//
// The request ID is not a security value. A client can send any ID that
// matches the pattern. Never use it for an access decision. The principal never
// comes from a header or from baggage.
//
//	handler := requestid.Middleware()(mux)
package requestid
