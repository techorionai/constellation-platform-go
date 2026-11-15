// Package valkey creates a Valkey client with tracing, connection metrics, and
// a required key prefix.
//
// Each service has its own key prefix and its own ACL user. [New] sends PING,
// and it writes a warn log record when the username is empty. [Keyspace] makes
// the keys of the service. The valkey-go commands are typed builders, so the
// package cannot add the prefix to each command. Code that uses Client.Raw must
// make each key with Client.Keys. The ACL rule of the user makes the server
// refuse a key without the prefix.
package valkey
