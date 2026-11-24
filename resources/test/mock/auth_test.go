package mock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// Issue 15 U27: a mock with nil functions fails closed.
func TestU27FailClosed(t *testing.T) {
	ex := &mock.TokenExchanger{}
	if tok, err := ex.Exchange(context.Background(), auth.ExchangeRequest{}); err == nil || tok != "" ||
		!errors.Is(err, auth.ErrTokenUnavailable) || ex.ExchangeCalls() != 1 {
		t.Errorf("TokenExchanger: %q %v", tok, err)
	}
	ca := &mock.ClientAssertion{ID: "c1"}
	if a, err := ca.Assertion(context.Background()); err == nil || a != "" || ca.AssertionCalls() != 1 {
		t.Errorf("ClientAssertion: %q %v", a, err)
	}
	if ca.ClientID() != "c1" {
		t.Error("ClientID")
	}
	// With a function set, the mocks call it.
	ex.ExchangeFunc = func(context.Context, auth.ExchangeRequest) (string, error) { return "tok", nil }
	if tok, err := ex.Exchange(context.Background(), auth.ExchangeRequest{}); err != nil || tok != "tok" {
		t.Errorf("ExchangeFunc: %q %v", tok, err)
	}
	ca.AssertionFunc = func(context.Context) (string, error) { return "a", nil }
	if a, err := ca.Assertion(context.Background()); err != nil || a != "a" {
		t.Errorf("AssertionFunc: %q %v", a, err)
	}
}
