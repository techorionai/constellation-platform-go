// Package eventtypes holds the event type registration of a service and the
// publisher that sends it to the events service.
//
// A service embeds a registration file (JSON) with its app, its source, and its
// event types. Call [Load] on the file in a unit test, so that a bad file fails
// in CI. At startup, [NewPublisher] and [Publisher.Run] register the file with
// retries, and [Publisher.Checker] reports the result to the readiness check.
// The same file gives the app and the types of the outbox writer: use
// [Registration.App] and [Registration.Types].
//
// The package has the port [Registry]. The package clients/events has the
// adapter. No log record and no span attribute of this package has a dataSchema
// or a description.
package eventtypes
