package manifest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/manifest"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// runWithRetryAfter makes a publisher with the backoff 2 s. The registry gives
// the error once, then succeeds. It returns the waits of the publisher.
func runWithError(t *testing.T, first error) []time.Duration {
	t.Helper()
	var n int
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		n++
		if n == 1 {
			return first
		}
		return nil
	}}
	pub := manifest.NewPublisher(reg, testManifest, manifest.WithBackoff(2*time.Second, time.Minute))
	var waits []time.Duration
	manifest.SetSleep(pub, func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	})
	if err := pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return waits
}

// Issue 17 U11: the publisher waits for the Retry-After value.
func TestI17U11PublisherWaits(t *testing.T) {
	waits := runWithError(t, &manifest.UnavailableError{RetryAfter: 30 * time.Second})
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

// Issue 17 U12: the longest wait is 5 min.
func TestI17U12MaxWait(t *testing.T) {
	waits := runWithError(t, &manifest.UnavailableError{RetryAfter: time.Hour})
	if len(waits) != 1 || waits[0] != 5*time.Minute {
		t.Fatalf("waits = %v", waits)
	}
}

// A wait time that is shorter than the backoff does not shorten the backoff.
func TestI17ShortRetryAfter(t *testing.T) {
	waits := runWithError(t, &manifest.UnavailableError{RetryAfter: time.Millisecond})
	if len(waits) != 1 || waits[0] < time.Second || waits[0] > 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

// Issue 17 U27: manifest.RetryAfter.
func TestI17U27RetryAfter(t *testing.T) {
	if _, ok := manifest.RetryAfter(manifest.ErrUnavailable); ok {
		t.Error("a plain error has a wait time")
	}
	err := fmt.Errorf("wrap: %w", &manifest.UnavailableError{RetryAfter: 4 * time.Second})
	if d, ok := manifest.RetryAfter(err); !ok || d != 4*time.Second {
		t.Errorf("RetryAfter = %v, %v", d, ok)
	}
	for _, e := range []error{manifest.ErrUnavailable, err, &manifest.UnavailableError{Cause: errors.New("x")}} {
		if !errors.Is(e, manifest.ErrUnavailable) {
			t.Errorf("%v is not ErrUnavailable", e)
		}
	}
}
