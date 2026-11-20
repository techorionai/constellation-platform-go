package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// ExampleMiddleware shows the middleware order: requestid, then auth.Middleware,
// then auth.RequireScopes for each route.
func ExampleMiddleware() {
	verifier := &mock.Verifier{VerifyFunc: func(_ context.Context, token string) (auth.Principal, error) {
		if token != "good" {
			return auth.Principal{}, auth.ErrInvalidToken
		}
		return auth.Principal{
			ID: "u1", Type: auth.PrincipalUser, ClientID: "web",
			Scopes: []string{"demo:ticket:read"},
		}, nil
	}}
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFromContext(r.Context())
		_, _ = fmt.Fprintf(w, "hello %s", p.ID)
	})
	mux := http.NewServeMux()
	mux.Handle("GET /tickets", auth.RequireScopes("demo:ticket:read")(read))
	mux.Handle("DELETE /tickets", auth.RequireScopes("demo:ticket:admin")(read))
	handler := requestid.Middleware()(auth.Middleware(verifier)(mux))

	call := func(method, token string) {
		req := httptest.NewRequestWithContext(context.Background(), method, "/tickets", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Println(rec.Code)
	}
	call(http.MethodGet, "good")
	call(http.MethodGet, "")
	call(http.MethodGet, "bad")
	call(http.MethodDelete, "good")
	// Output:
	// 200
	// 401
	// 401
	// 403
}

// ExamplePrincipal_HasScopes shows the scope checks.
func ExamplePrincipal_HasScopes() {
	p := auth.Principal{Scopes: []string{"demo:ticket:read"}}
	fmt.Println(p.HasScopes("demo:ticket:read"))
	fmt.Println(p.MissingScopes("demo:ticket:read", "demo:ticket:write"))
	// Output:
	// true
	// [demo:ticket:write]
}
