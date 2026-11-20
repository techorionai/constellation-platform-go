// Package clientcred is the adapter of [auth.TokenSource] for the OAuth client
// credentials grant with private_key_jwt client authentication (RFC 6749 and RFC
// 7523).
//
// A [Source] signs a client assertion with the private key of the client and
// sends it to the token endpoint of the identity service. It caches the token for
// each audience and scope set until 80 percent of the token lifetime passes.
// Calls for the same audience and scopes share one request.
//
// Config is read with envPrefix:"CLIENT__":
//
//	CLIENT__ID            client ID, required
//	CLIENT__KEY_ID        kid of the signing key, required
//	CLIENT__PRIVATE_KEY   PEM EC P-256 private key, required
//	                      (or CLIENT__PRIVATE_KEY_FILE)
//	CLIENT__ISSUER        issuer URL of the identity service, required
//	CLIENT__TOKEN_URL     URL of the token endpoint, default <issuer>/oauth/token
//	CLIENT__HTTP_TIMEOUT  time limit of one token request, default 10s
//
// The assertion audience is always Issuer. TokenURL is the URL that the service
// calls. It can differ from Issuer, for example the internal URL in Compose.
//
// The code never logs or records the private key, the assertion, or a token.
// A token request has the span auth.clientcred.token and counts in the metric
// constellation.auth.token.request.
//
//	src, err := clientcred.New(cfg.Client)
//	token, err := src.Token(ctx, "identity", "identity:manifest:publish")
package clientcred
