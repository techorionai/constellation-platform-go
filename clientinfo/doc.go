// Package clientinfo gives the client IP address and the user agent of an HTTP
// request. The audit writer puts them into each audit entry.
//
// [Middleware] sets an [Info] in the context of the request. Read it with
// [FromContext].
//
// The IP address is the host of RemoteAddr. The default has no trusted proxy,
// so a client cannot forge the address with a header. When RemoteAddr is in
// [Config.TrustedProxies], the middleware reads X-Forwarded-For from right to
// left and takes the first address that is not a trusted proxy. A bad item
// makes the middleware ignore the header, and the address is the host of
// RemoteAddr. Before each comparison, the middleware removes the IPv6 zone and
// unmaps an IPv4-mapped IPv6 address. Set only the addresses of the own
// proxies. A wrong value lets a client forge its address.
//
// The user agent is made valid UTF-8 and cut to [MaxUserAgentLen] characters.
// An empty user agent is not set.
//
// An IP address is personal data. This package logs nothing.
package clientinfo
