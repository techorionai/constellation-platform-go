package auth

import (
	"net/http"
)

// BearerTransport returns a round tripper that adds the header "Authorization:
// Bearer <token>" to each request. It gets the token from src for the audience
// and the scopes. If base is nil, it uses [http.DefaultTransport].
//
// The round tripper clones the request, so the request of the caller does not
// change. If the token source returns an error, the round tripper does not send
// the request and returns the error.
func BearerTransport(base http.RoundTripper, src TokenSource, audience string, scopes ...string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{base: base, src: src, audience: audience, scopes: append([]string(nil), scopes...)}
}

type bearerTransport struct {
	base     http.RoundTripper
	src      TokenSource
	audience string
	scopes   []string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.src.Token(req.Context(), t.audience, t.scopes...)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(clone)
}
