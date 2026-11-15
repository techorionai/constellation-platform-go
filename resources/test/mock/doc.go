// Package mock holds the mocks of ports.
//
// This package is empty at this time. The rule for mocks: a mock replaces a
// port only in a unit test of code that uses the port. A server-side
// integration test does not use a mock. It uses a test container from
// resources/test/containers and a live dependency.
package mock
