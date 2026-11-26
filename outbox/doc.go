// Package outbox is the transactional outbox and the outbox relay.
//
// A service writes an event with [Writer.Write] in the same database
// transaction as the business change. The writer builds a CloudEvent, checks
// it against the limits of the contract (lib/events.tsp), and inserts it with
// the [Inserter] port. The adapter outbox/postgres inserts the row into the
// table platform_outbox.
//
// The [Relay] reads the unpublished rows with the [Store] port and adds each
// event to the Valkey stream of the service with the [Publisher] port. The
// adapter streams/valkey has the publisher. Delivery is at least once. The
// order of events is not guaranteed.
//
// The actor of an event comes from the code of the service, never from request
// data. A request handler uses [ActorFromPrincipal]. An event with no request
// principal uses [ServiceActor] with the client ID of the service.
//
// This package imports no database driver. It never logs or traces the data of
// an event.
package outbox
