package clientinfo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/techorionai/constellation-platform-go/clientinfo"
)

// serve runs the middleware and returns the Info that the handler got.
func serve(t *testing.T, cfg clientinfo.Config, remote string, header http.Header) clientinfo.Info {
	t.Helper()
	var got clientinfo.Info
	var ok bool
	h := clientinfo.Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = clientinfo.FromContext(r.Context())
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.RemoteAddr = remote
	for k, v := range header {
		req.Header[k] = v
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("no Info in the context")
	}
	return got
}

func trusted(t *testing.T, prefixes ...string) clientinfo.Config {
	t.Helper()
	var cfg clientinfo.Config
	for _, p := range prefixes {
		cfg.TrustedProxies = append(cfg.TrustedProxies, netip.MustParsePrefix(p))
	}
	return cfg
}

// U14: with no trusted proxy, the header is ignored. A zone is removed.
func TestU14NoTrustedProxies(t *testing.T) {
	xff := http.Header{"X-Forwarded-For": {"1.2.3.4"}}
	if got := serve(t, clientinfo.Config{}, "10.0.0.5:1234", xff).IP; got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("IP = %s, want 10.0.0.5", got)
	}
	got := serve(t, clientinfo.Config{}, "[fe80::1%eth0]:1234", nil).IP
	if got.String() != "fe80::1" || got.Zone() != "" {
		t.Errorf("IP = %q, want fe80::1 with no zone", got)
	}
}

// U15: the trusted proxy rule.
func TestU15TrustedProxies(t *testing.T) {
	cfg := trusted(t, "10.0.0.0/8")
	xff := http.Header{"X-Forwarded-For": {"203.0.113.9, 198.51.100.7, 10.0.0.9"}}
	for _, remote := range []string{"10.0.0.5:1234", "[::ffff:10.0.0.5]:1234"} {
		if got := serve(t, cfg, remote, xff).IP; got != netip.MustParseAddr("198.51.100.7") {
			t.Errorf("remote %s: IP = %s, want 198.51.100.7", remote, got)
		}
	}
	bad := http.Header{"X-Forwarded-For": {"foo"}}
	if got := serve(t, cfg, "10.0.0.5:1234", bad).IP; got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("bad value: IP = %s, want 10.0.0.5", got)
	}
	// A client outside the trusted proxies cannot set the header.
	if got := serve(t, cfg, "192.0.2.1:1", xff).IP; got != netip.MustParseAddr("192.0.2.1") {
		t.Errorf("untrusted remote: IP = %s", got)
	}
	// A mapped IPv6 item and a zone in the header are normalized.
	mapped := http.Header{"X-Forwarded-For": {"198.51.100.7, ::ffff:10.0.0.9"}}
	if got := serve(t, cfg, "10.0.0.5:1", mapped).IP; got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("mapped item: IP = %s", got)
	}
	// Two header lines count as one list.
	two := http.Header{"X-Forwarded-For": {"198.51.100.7", "10.0.0.9"}}
	if got := serve(t, cfg, "10.0.0.5:1", two).IP; got != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("two lines: IP = %s", got)
	}
	// All items trusted: use RemoteAddr.
	all := http.Header{"X-Forwarded-For": {"10.0.0.9"}}
	if got := serve(t, cfg, "10.0.0.5:1", all).IP; got != netip.MustParseAddr("10.0.0.5") {
		t.Errorf("all trusted: IP = %s", got)
	}
}

// U16: the user agent and the configuration.
func TestU16UserAgentAndConfig(t *testing.T) {
	ua := strings.Repeat("a", 590) + "\xff\xfe" + strings.Repeat("b", 10)
	got := serve(t, clientinfo.Config{}, "10.0.0.5:1", http.Header{"User-Agent": {ua}}).UserAgent
	if utf8.RuneCountInString(got) != 512 || !utf8.ValidString(got) {
		t.Errorf("user agent has %d characters, valid = %v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if got := serve(t, clientinfo.Config{}, "10.0.0.5:1", nil).UserAgent; got != "" {
		t.Errorf("empty user agent = %q", got)
	}
	if _, err := clientinfo.ParseTrustedProxies("10.0.0.0/33"); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Errorf("err = %v, want an error that names TRUSTED_PROXIES", err)
	}
	cfg, err := clientinfo.ParseTrustedProxies("10.0.0.0/8, 192.168.1.5/32,")
	if err != nil || len(cfg.TrustedProxies) != 2 {
		t.Errorf("cfg = %+v, err = %v", cfg, err)
	}
	t.Setenv("TRUSTED_PROXIES", "bad")
	if _, err := clientinfo.ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Errorf("ConfigFromEnv err = %v", err)
	}
}

func TestFromContextEmpty(t *testing.T) {
	if _, ok := clientinfo.FromContext(t.Context()); ok {
		t.Error("FromContext of an empty context returned ok")
	}
}
