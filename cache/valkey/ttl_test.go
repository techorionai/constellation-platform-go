package valkey

import (
	"testing"
	"time"
)

// U37
func TestTTLMillis(t *testing.T) {
	for _, tc := range []struct {
		ttl  time.Duration
		want int64
	}{
		{1500 * time.Microsecond, 2},
		{time.Millisecond, 1},
		{time.Millisecond + time.Nanosecond, 2},
		{10*time.Second + 500*time.Microsecond, 10001},
	} {
		if got := ttlMillis(tc.ttl); got != tc.want {
			t.Errorf("ttlMillis(%v) = %d, want %d", tc.ttl, got, tc.want)
		}
	}
}
