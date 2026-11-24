// Package introspect is the adapter of [auth.Verifier] for personal API tokens.
// It checks a token with the introspection endpoint of the identity service (RFC
// 7662) and keeps the result in memory.
//
// The service client that calls introspection must be a service client with
// private_key_jwt. The adapter gets the client assertion from the port
// [auth.ClientAssertion]. The adapter auth/clientcred implements it.
//
// Config is read with envPrefix:"AUTH__INTROSPECT__":
//
//	AUTH__INTROSPECT__ISSUER               issuer URL of the identity service, required
//	AUTH__INTROSPECT__URL                  URL of the endpoint, default <issuer>/oauth/introspect
//	AUTH__INTROSPECT__AUDIENCE             app of the service, required
//	AUTH__INTROSPECT__CACHE_TTL            1s to 60s, default 30s
//	AUTH__INTROSPECT__INACTIVE_TTL         0 to 30s, default 5s (0 is no cache)
//	AUTH__INTROSPECT__CACHE_SIZE           100 to 1000000, default 10000
//	AUTH__INTROSPECT__INACTIVE_CACHE_SIZE  10 to 100000, default 1000
//	AUTH__INTROSPECT__MAX_CONCURRENT       1 to 256, default 16
//	AUTH__INTROSPECT__HTTP_TIMEOUT         1s to 30s, default 5s
//	AUTH__INTROSPECT__ALLOW_INSECURE_HTTP  allows the scheme http, default false
//
// A token must be "cpat_" and 43 characters of the base64url alphabet. Any other
// value gives [auth.ErrInvalidToken] with no call to the identity service. The
// cache key is the SHA-256 hash of the token. The adapter never logs or records
// a token, a hash, or a subject.
//
// A revoked token works for at most CacheTTL. At most MaxConcurrent calls to the
// identity service run at the same time. When all slots are in use, a token that
// is not in the cache gives [auth.ErrIntrospectionUnavailable]. Inactive results
// are in a separate cache, so they cannot remove active entries.
//
// The metric constellation.auth.introspect counts each result by outcome. The
// histogram constellation.auth.introspect.duration records the HTTP call.
//
//	v, err := introspect.New(cfg.Introspect, clientSource)
//	verifier := auth.ByFormat(jwtVerifier, v)
package introspect
