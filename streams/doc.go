// Package streams has the ports and the logic of the event streams.
//
// The stream of an app is the Valkey stream events.stream:<app> ([StreamKey]).
// A [Consumer] reads a stream with a consumer group, calls a [Handler] for each
// message, and acknowledges the message when the handler returns nil. It claims
// messages that stay pending for too long. It drops a message after
// [ConsumerOptions.MaxDeliveries] deliveries, with a log record and a metric.
//
// Delivery is at least once and has no order. A handler must be idempotent by
// the source and the id of the event.
//
// The adapter streams/valkey implements the [Group] port, the publisher, the
// lag helper, and the trim purger. This package imports no Valkey driver. It
// never logs or traces the data of an event.
package streams
