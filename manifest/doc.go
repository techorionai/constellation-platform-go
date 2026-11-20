// Package manifest publishes the permission manifest of a service to the
// identity service.
//
// The [Registry] port publishes one [PermissionManifest]. The adapter
// clients/identity implements it. The [Publisher] calls the registry at startup
// with retries, and its [Publisher.Checker] reports the result to the readiness
// check. A service is not ready until identity knows its permissions, and it does
// not stop when identity is down.
//
// Place in the startup order:
//
//  1. Before the admin server: [Load] the manifest, make the token source, the
//     identity client with the scope identity:manifest:publish, the registry, and
//     the publisher.
//  2. Give [Publisher.Checker] to health.NewState with the other checkers.
//  3. Before MarkStarted: start [Publisher.Run] in a goroutine. Do not wait for it.
//
// [Load] reads the manifest from an embedded JSON file and checks the same rules
// as the identity service. A unit test of the service can call it to find a wrong
// manifest early.
//
// The package does not import net/http or an adapter.
package manifest
