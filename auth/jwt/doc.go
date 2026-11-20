// Package jwt is the adapter of the auth.Verifier port for RFC 9068 access
// tokens. It verifies the tokens locally with the JWKS of the identity service.
//
// [New] makes a [Verifier] from a [Config]. The verifier accepts only ES256
// tokens with the header typ at+jwt. It checks the signature, the issuer, the
// audience, the time claims, and the claims of AccessTokenClaims of
// constellation-contracts. It maps the claims to an auth.Principal.
//
// The verifier fetches the JWKS on the first Verify. A service can start when
// the identity service is down. Until the first fetch works, Verify returns
// auth.ErrKeysUnavailable. A refresh replaces the full key set, so a key that
// the issuer removed stops working after the next refresh. An unknown kid starts
// one refetch at most once in MinRefetchInterval. [Verifier.Start] adds a
// background refresh. [Verifier.Close] stops it.
//
//	v, err := jwt.New(cfg.Auth)
//	if err != nil { ... }
//	v.Start(ctx)
//	defer v.Close()
//	handler := auth.Middleware(v)(mux)
//
// The package counts each JWKS fetch in the metric
// constellation.auth.jwks.fetch, with the attribute outcome.
package jwt
