// Package limithttp limits the size of the response body of an HTTP client.
package limithttp

import (
	"io"
	"net/http"
)

// Transport is an [http.RoundTripper] that limits the size of each response
// body. A body of more than Max bytes gives the error Err.
type Transport struct {
	Base http.RoundTripper
	Max  int64
	Err  error
}

// RoundTrip sends the request and wraps the response body.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.Max {
		_ = resp.Body.Close()
		return nil, t.Err
	}
	resp.Body = &body{rc: resp.Body, left: t.Max, err: t.Err}
	return resp, nil
}

type body struct {
	rc   io.ReadCloser
	left int64
	err  error
}

func (b *body) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, b.err
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return 0, b.err
	}
	return n, err
}

func (b *body) Close() error { return b.rc.Close() }
