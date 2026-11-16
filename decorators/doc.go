// Package decorators holds the tracing and logging decorators for executors.
//
// A decorator implements the same interface as the executor that it wraps.
// [ApplyTracing] makes a span and records the metric executor.duration.
// [ApplyLogging] writes one log record for each call. The NoResult forms do the
// same for [executor.Executor].
//
// A decorator never logs the params or the result, because they can contain
// secrets and personal data. The error text goes to the log record and to the
// span, so the text of an error that a command returns must not contain secrets
// or personal data.
//
// The recommended order, innermost first, is: the command, then logging, then
// tracing. Then tracing is the outermost decorator, and the log records have
// the trace_id of the executor span.
//
//	var h executor.ExecutorWithReturn[Params, Result] = command
//	h = decorators.ApplyLogging(h, "CreateOrder")
//	h = decorators.ApplyTracing(h, "CreateOrder")
package decorators
