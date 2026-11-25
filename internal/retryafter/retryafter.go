// Package retryafter reads the Retry-After header of an HTTP response.
package retryafter

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Min is the shortest wait time that Parse returns.
	Min = time.Second
	// Max is the longest wait time that Parse returns.
	Max = time.Hour
)

// Parse reads a Retry-After header value. It accepts delta seconds and an HTTP date.
// It returns false if the value is missing or not valid. A zero or negative value,
// and a date in the past, give 1 s. A value of more than 1 h gives 1 h.
func Parse(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	var d time.Duration
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		switch {
		case n > int64(Max/time.Second):
			d = Max
		case n < 0:
			d = 0
		default:
			d = time.Duration(n) * time.Second
		}
	} else if t, err := http.ParseTime(value); err == nil {
		d = t.Sub(now)
	} else if isDigits(value) {
		// A number that is too large for int64.
		d = Max
	} else {
		return 0, false
	}
	return Clamp(d), true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Clamp limits d to the range from Min to Max.
func Clamp(d time.Duration) time.Duration {
	if d < Min {
		return Min
	}
	if d > Max {
		return Max
	}
	return d
}

// FromStatus returns the wait time of a response with this status and these
// headers. The second result is true if
// the status is 429, or the status is 503 and the header has a valid value. For
// a 429 with no valid header, the duration is 0 and the result is true.
func FromStatus(status int, h http.Header, now time.Time) (time.Duration, bool) {
	switch status {
	case http.StatusTooManyRequests:
		d, _ := Parse(h.Get("Retry-After"), now)
		return d, true
	case http.StatusServiceUnavailable:
		return Parse(h.Get("Retry-After"), now)
	}
	return 0, false
}
