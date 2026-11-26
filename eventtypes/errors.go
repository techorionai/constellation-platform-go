package eventtypes

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxRetryAfter is the longest wait time that [Publisher.Run] uses from the server.
const maxRetryAfter = 5 * time.Minute

// Errors of a [Registry].
var (
	// ErrDenied means that the events service does not accept the credentials
	// or the scopes of the client, or that the client has no role events:producer.
	// An operator can fix it.
	ErrDenied = errors.New("eventtypes: registration denied")
	// ErrUnavailable means that the events service is down or that the request
	// failed. The caller can retry.
	ErrUnavailable = errors.New("eventtypes: registry unavailable")
	// ErrRejected means that the registry refused the request with a fixed 4xx
	// status that a retry cannot fix, for example 400, 404, or 409.
	ErrRejected = errors.New("eventtypes: registration rejected")
)

// Problem is one problem that the events service found in a registration.
type Problem struct {
	// Code is the stable code, for example "invalid_data_schema".
	Code string
	// Field is the JSON Pointer to the incorrect value. It can be empty.
	Field string
	// Message is the message for a person. It can be empty.
	Message string
}

// InvalidError means that the events service rejected the registration. A retry
// gives the same result.
type InvalidError struct {
	// Code is the code of the problem, for example "registration_app_mismatch"
	// or "content_too_large".
	Code string
	// Problems are the field errors.
	Problems []Problem
}

// Error returns the text of the error.
func (e *InvalidError) Error() string {
	var b strings.Builder
	b.WriteString("eventtypes: invalid registration")
	if e.Code != "" {
		b.WriteString(": " + e.Code)
	}
	for _, p := range e.Problems {
		b.WriteString(" [" + p.Code)
		if p.Field != "" {
			b.WriteString(" " + p.Field)
		}
		b.WriteString("]")
	}
	return b.String()
}

// Codes returns the code of the error and the codes of its problems.
func (e *InvalidError) Codes() []string {
	var codes []string
	if e.Code != "" {
		codes = append(codes, e.Code)
	}
	for _, p := range e.Problems {
		codes = append(codes, p.Code)
	}
	return codes
}

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
