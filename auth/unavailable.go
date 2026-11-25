package auth

import (
	"errors"
	"fmt"
	"time"
)

// UnavailableError is an unavailable result with a wait time from the server.
// errors.Is works with the wrapped error, for example [ErrTokenUnavailable].
type UnavailableError struct {
	// Err is [ErrTokenUnavailable] or [ErrIntrospectionUnavailable].
	Err error
	// RetryAfter is the wait time. It is 0 if the server gave no value.
	RetryAfter time.Duration
}

// Error returns the message of the wrapped error and the wait time.
func (e *UnavailableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v (retry after %s)", e.Err, e.RetryAfter.Round(time.Millisecond))
	}
	return e.Err.Error()
}

// Unwrap returns the wrapped error.
func (e *UnavailableError) Unwrap() error { return e.Err }

// RetryAfter returns the wait time of err, if it has one. The second result is
// false if err has no [UnavailableError] or the wait time is 0.
func RetryAfter(err error) (time.Duration, bool) {
	var ue *UnavailableError
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		return ue.RetryAfter, true
	}
	return 0, false
}
