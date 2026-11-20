// Package issuer is a test token issuer. A service uses it in handler tests, to
// get real signed access tokens and a real JWKS endpoint.
//
// [New] makes an ES256 key pair and starts an httptest server that serves
// /.well-known/jwks.json. [Issuer.Mint] signs a token. The [Claims] type has
// defaults for all fields, and a test can change any field, also the header
// fields alg, typ, and kid. A test can also remove a claim. [Issuer.Rotate] adds
// a key and keeps the old key. [Issuer.RemoveKey] removes a key from the JWKS.
// [Issuer.SetStatus] and [Issuer.SetBody] make the endpoint return an error or
// a raw body.
//
//	iss := issuer.New(t)
//	verifier, _ := jwt.New(jwt.Config{Issuer: iss.Issuer(), Audience: "demo", JWKSURL: iss.URL(), AllowInsecureHTTP: true})
//	token := iss.Mint(t, issuer.Claims{Scope: "demo:ticket:read"})
//
// Only test code can import this package. The depguard rule
// no-test-helpers-in-production enforces this. The tokens are not secret, but
// the keys are valid for the test issuer only.
package issuer
