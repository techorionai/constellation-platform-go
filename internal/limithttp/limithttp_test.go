package limithttp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/internal/limithttp"
)

var errBig = errors.New("too big")

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func do(t *testing.T, n int, length int64) error {
	t.Helper()
	tr := limithttp.Transport{Max: 10, Err: errBig, Base: rt(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", n)))}, nil
	})}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return err
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return err
}

func TestLimit(t *testing.T) {
	if err := do(t, 10, 10); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := do(t, 11, -1); !errors.Is(err, errBig) {
		t.Errorf("unknown length, over the limit: %v", err)
	}
	if err := do(t, 11, 11); !errors.Is(err, errBig) {
		t.Errorf("known length, over the limit: %v", err)
	}
}
