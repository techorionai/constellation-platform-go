// Package audit is the producer side of the audit trail.
//
// A service writes an audit entry with [Writer.Write] in the same database
// transaction as the change. The writer builds a CloudEvent of the type
// "<app>.audit.recorded" and checks it against the limits of the contract (the
// audit entry model of constellation-contracts). It inserts the entry with the
// [Inserter] port. The adapter audit/postgres inserts the row into the table
// platform_audit_outbox. A second [outbox.Relay] adds the entry to the Valkey
// stream audit.stream:<app>. Only the audit service reads these streams.
//
// The actor of an entry is the verified principal in the context
// (auth.PrincipalFromContext). With no principal, the actor is the client ID of
// [Config.ClientID] with the actor type "service". [Entry] has no actor field,
// so a caller cannot replace the verified principal. To record the origin of an
// action with no principal, for example a command line action, put it in the
// details.
//
// The writer copies the request ID, the client IP address, and the user agent
// from the context (packages requestid and clientinfo). It leaves out a value
// that does not agree with the contract. It does not refuse the entry for such
// a value.
//
// The writer logs nothing and puts no entry data into spans. An entry must have
// no secret, no token, and no password.
package audit
