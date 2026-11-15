// Package mock holds the mocks of ports.
//
// The rule for mocks: a mock replaces a port only in a unit test of code that
// uses the port. A server-side integration test does not use a mock. It uses a
// test container from resources/test/containers and a live dependency.
//
// Each mock has a function field for each method. If the field is nil, the mock
// uses a default that succeeds. Each mock counts its calls. A mock imports the
// package of its port, so a unit test that uses a mock is in the external test
// package (<package>_test).
package mock
