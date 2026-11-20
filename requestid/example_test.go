package requestid_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/techorionai/constellation-platform-go/requestid"
)

func ExampleMiddleware() {
	handler := requestid.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, requestid.FromContext(r.Context()))
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Println(rec.Body.String(), rec.Header().Get("X-Request-ID"))
	// Output: abc-123 abc-123
}
