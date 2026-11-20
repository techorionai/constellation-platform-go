package requestid_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/techorionai/constellation-platform-go/requestid"
)

// U23
func TestMiddleware(t *testing.T) {
	uuid7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	cases := []struct {
		name, header string
		keep         bool
	}{
		{"valid", "req-1.A_b", true},
		{"bad", "a b", false},
		{"none", "", false},
		{"too long", string(make([]byte, 0)) + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := requestid.Middleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = requestid.FromContext(r.Context())
			}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(requestid.Header, tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header().Get(requestid.Header)
			if got != seen {
				t.Fatalf("header %q differs from context %q", got, seen)
			}
			if tc.keep && got != tc.header {
				t.Fatalf("got %q, want %q", got, tc.header)
			}
			if !tc.keep && !uuid7.MatchString(got) {
				t.Fatalf("got %q, want a UUID v7", got)
			}
		})
	}
}
