package clientinfo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"unicode/utf8"
)

// EnvTrustedProxies is the variable that holds the trusted proxies.
const EnvTrustedProxies = "TRUSTED_PROXIES"

// MaxUserAgentLen is the maximum number of characters of the user agent.
const MaxUserAgentLen = 512

// Info is the information about the client of a request.
type Info struct {
	// IP is the client IP address. It has no zone and is not an IPv4-mapped
	// IPv6 address. It is the zero value when it is not known.
	IP netip.Addr
	// UserAgent is the user agent of the client. It is valid UTF-8 with a
	// maximum of [MaxUserAgentLen] characters. It is empty when it is not set.
	UserAgent string
}

type ctxKey struct{}

// ContextWith returns a context that carries the client information.
func ContextWith(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// FromContext returns the client information of the context. The second result
// is false when the context has none.
func FromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(ctxKey{}).(Info)
	return info, ok
}

// Config is the configuration of the middleware.
type Config struct {
	// TrustedProxies are the addresses of the own reverse proxies. The default
	// is empty: the middleware ignores X-Forwarded-For. Set only the addresses
	// of the proxies that you control.
	TrustedProxies []netip.Prefix
}

// ConfigFromEnv reads [EnvTrustedProxies], a comma list of CIDR prefixes. An
// invalid prefix gives an error that names the variable.
func ConfigFromEnv() (Config, error) {
	return ParseTrustedProxies(os.Getenv(EnvTrustedProxies))
}

// ParseTrustedProxies parses a comma list of CIDR prefixes. An empty text gives
// an empty list. An invalid prefix gives an error that names
// [EnvTrustedProxies].
func ParseTrustedProxies(list string) (Config, error) {
	var cfg Config
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return Config{}, fmt.Errorf("clientinfo: %s has an invalid prefix %q", EnvTrustedProxies, item)
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, p.Masked())
	}
	return cfg, nil
}

// Middleware returns the middleware that sets the [Info] of the request in the
// context. See the package documentation for the rules.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	proxies := append([]netip.Prefix(nil), cfg.TrustedProxies...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := Info{
				IP:        clientIP(r, proxies),
				UserAgent: cleanUserAgent(r.UserAgent()),
			}
			next.ServeHTTP(w, r.WithContext(ContextWith(r.Context(), info)))
		})
	}
}

func normalize(a netip.Addr) netip.Addr { return a.WithZone("").Unmap() }

func trusted(a netip.Addr, proxies []netip.Prefix) bool {
	for _, p := range proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request, proxies []netip.Prefix) netip.Addr {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	remote = normalize(remote)
	if len(proxies) == 0 || !trusted(remote, proxies) {
		return remote
	}
	var items []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for item := range strings.SplitSeq(v, ",") {
			items = append(items, strings.TrimSpace(item))
		}
	}
	for i := len(items) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(items[i])
		if err != nil {
			// A bad value: the header is not reliable.
			return remote
		}
		a = normalize(a)
		if !trusted(a, proxies) {
			return a
		}
	}
	return remote
}

func cleanUserAgent(ua string) string {
	ua = strings.ToValidUTF8(ua, "�")
	if utf8.RuneCountInString(ua) > MaxUserAgentLen {
		n := 0
		for i := range ua {
			if n == MaxUserAgentLen {
				ua = ua[:i]
				break
			}
			n++
		}
	}
	return ua
}
