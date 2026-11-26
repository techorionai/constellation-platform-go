// Package events is the client of the events service API.
//
// The file events.gen.go comes from the OpenAPI file in api/openapi. Do not
// change it by hand. The tag of the contracts release is in
// api/openapi/CONTRACTS_VERSION. To update the client, copy the OpenAPI file of a
// new release, write the tag in CONTRACTS_VERSION, and run go generate ./... in
// the module. CI fails if the generated file is not current.
//
// [New] makes a [ClientWithResponses] that adds a bearer token for the audience
// "events" to each request. The caller gives the token source and the scopes. A
// producer asks for the scope events:event_type:register. A catalog reader asks
// for events:catalog:read. The package imports the port auth.TokenSource, not
// the adapter auth/clientcred.
//
//	src, _ := clientcred.New(cfg.Client)
//	c, _ := events.New(cfg.Events, src, []string{"events:event_type:register"})
//	registry := events.NewEventTypeRegistry(c)
//
// [EventTypeRegistry] is the adapter of eventtypes.Registry. The client reads
// at most 1 MiB of a response. A catalog reader sets a larger limit with
// [WithMaxResponseBytes] or asks for a small pageSize.
//
// Config is read with envPrefix:"EVENTS__":
//
//	EVENTS__URL      base URL of the events service, required
//	EVENTS__TIMEOUT  time limit of one request, default 10s
//	EVENTS__ALLOW_INSECURE_HTTP
//	                 allows the scheme http for the URL, default false
package events
