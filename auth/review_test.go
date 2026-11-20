package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// R10: a request with more than one Authorization header gets 401, in any order.
func TestTwoAuthorizationHeaders(t *testing.T) {
	good := func(_ context.Context, token string) (auth.Principal, error) {
		if token == "good" {
			return testPrincipal, nil
		}
		return auth.Principal{}, auth.ErrInvalidToken
	}
	for _, order := range [][]string{{"Bearer good", "Bearer junk"}, {"Bearer junk", "Bearer good"}, {"Bearer good", "Bearer good"}} {
		e := newEnv(t, &mock.Verifier{VerifyFunc: good})
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		for _, h := range order {
			req.Header.Add("Authorization", h)
		}
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != 401 || *e.ran {
			t.Errorf("%v: status %d ran %v", order, rec.Code, *e.ran)
		}
	}
	e := newEnv(t, &mock.Verifier{VerifyFunc: good})
	if rec := e.do("Bearer good"); rec.Code != 204 {
		t.Errorf("one header: status %d", rec.Code)
	}
}

// R11: a mock verifier with no VerifyFunc rejects the token.
func TestMockVerifierWithoutFuncFailsClosed(t *testing.T) {
	e := newEnv(t, &mock.Verifier{})
	rec := e.do("Bearer x")
	if rec.Code != 401 || *e.ran {
		t.Fatalf("status %d ran %v", rec.Code, *e.ran)
	}
}
