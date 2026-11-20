// Package auth holds the principal model, the token verification port, the scope
// checks, and the net/http middleware of a service.
//
// A [Verifier] turns a bearer token into a [Principal]. The adapter auth/jwt
// verifies access tokens locally with the JWKS of the identity service.
// [Middleware] reads the Authorization header, calls the verifier, and puts the
// principal in the request context. [RequireScopes] protects one route.
//
// Order of the middleware, outermost first: requestid.Middleware, then
// [Middleware], then [RequireScopes] for each route.
//
//	verifier, _ := authjwt.New(cfg.Auth)
//	mux := http.NewServeMux()
//	mux.Handle("GET /tickets", auth.RequireScopes("demo:ticket:read")(listTickets))
//	handler := requestid.Middleware()(auth.Middleware(verifier)(mux))
//
// Error map of the middleware:
//
//	ErrNoToken         401, WWW-Authenticate: Bearer
//	ErrInvalidToken    401, WWW-Authenticate: Bearer error="invalid_token"
//	ErrExpired         401, WWW-Authenticate: Bearer error="invalid_token"
//	ErrWrongIssuer     401, WWW-Authenticate: Bearer error="invalid_token"
//	ErrWrongAudience   401, WWW-Authenticate: Bearer error="invalid_token"
//	ErrKeysUnavailable 503, Retry-After: 5
//	other error        401, WWW-Authenticate: Bearer error="invalid_token"
//	missing scope      403, WWW-Authenticate: Bearer error="insufficient_scope"
//
// The body of each error is application/problem+json. It never contains the
// error text of the verifier, and the code never logs or records the token.
//
// The principal comes only from the verified token. Never read it from a header
// or from baggage. Each service checks the audience and the scopes of each
// request.
package auth
