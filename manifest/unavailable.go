package manifest

import (
	"errors"
	"fmt"
	"time"
)

// maxRetryAfter is the longest wait time that [Publisher.Run] uses from the server.
const maxRetryAfter = 5 * time.Minute

// UnavailableError is [ErrUnavailable] with a wait time from the server.
// errors.Is(err, ErrUnavailable) is true.
type UnavailableError struct {
	// Cause is the error that the registry got, or nil.
	Cause error
	// RetryAfter is the wait time. It is 0 if the server gave no value.
	RetryAfter time.Duration
}

// Error returns the message of [ErrUnavailable], the cause, and the wait time.
func (e *UnavailableError) Error() string {
	msg := ErrUnavailable.Error()
	if e.Cause != nil {
		msg = fmt.Sprintf("%s: %v", msg, e.Cause)
	}
	if e.RetryAfter > 0 {
		msg = fmt.Sprintf("%s (retry after %s)", msg, e.RetryAfter.Round(time.Millisecond))
	}
	return msg
}

// Unwrap returns [ErrUnavailable] and the cause.
func (e *UnavailableError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, e.Cause}
}

// RetryAfter returns the wait time of err, if it has one. The second result is
// false if err has no [UnavailableError] or the wait time is 0.
func RetryAfter(err error) (time.Duration, bool) {
	var ue *UnavailableError
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		return ue.RetryAfter, true
	}
	return 0, false
}
