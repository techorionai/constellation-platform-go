package postgres

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// withPool returns the observe option with the pool name attribute and the
// extra attributes.
func withPool(database string, extra ...attribute.KeyValue) metric.ObserveOption {
	return metric.WithAttributes(append([]attribute.KeyValue{
		attribute.String("db.client.connection.pool.name", database),
	}, extra...)...)
}
