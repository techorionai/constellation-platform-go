// Package valkey is the Valkey checker of the package health.
//
// valkey-go sends commands over shared connections, so a busy client does not
// block the PING.
package valkey
