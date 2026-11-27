package clientinfo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"

	"github.com/techorionai/constellation-platform-go/clientinfo"
)

func ExampleMiddleware() {
	cfg := clientinfo.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	h := clientinfo.Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		info, _ := clientinfo.FromContext(r.Context())
		fmt.Println(info.IP, info.UserAgent)
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:4000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("User-Agent", "example/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	// Output: 203.0.113.9 example/1.0
}
