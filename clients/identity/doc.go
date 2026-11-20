// Package identity is the client of the identity service API.
//
// The file identity.gen.go comes from the OpenAPI file in api/openapi. Do not
// change it by hand. The tag of the contracts release is in
// api/openapi/CONTRACTS_VERSION. To update the client, copy the OpenAPI file of a
// new release, write the tag in CONTRACTS_VERSION, and run go generate ./... in
// the module. CI fails if the generated file is not current.
//
// [New] makes a [ClientWithResponses] that adds a bearer token for the audience
// "identity" to each request. The caller gives the token source and the scopes.
// The package imports the port auth.TokenSource, not the adapter
// auth/clientcred.
//
//	src, _ := clientcred.New(cfg.Client)
//	c, _ := identity.New(cfg.Identity, src, []string{"identity:manifest:publish"})
//	registry := identity.NewManifestRegistry(c)
//
// [ManifestRegistry] is the adapter of manifest.Registry.
//
// Config is read with envPrefix:"IDENTITY__":
//
//	IDENTITY__URL      base URL of the identity service, required
//	IDENTITY__TIMEOUT  time limit of one request, default 10s
package identity
