package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/eventtypes"
	"github.com/techorionai/constellation-platform-go/internal/retryafter"
)

var _ eventtypes.Registry = (*EventTypeRegistry)(nil)

// EventTypeRegistry is the adapter of [eventtypes.Registry]. It registers the
// event types with the events API.
type EventTypeRegistry struct {
	client *ClientWithResponses
}

// NewEventTypeRegistry makes a registry from a client. The client needs the
// scope events:event_type:register, and its service client needs the role
// events:producer.
func NewEventTypeRegistry(c *ClientWithResponses) *EventTypeRegistry {
	return &EventTypeRegistry{client: c}
}

// Register sends the registration. It maps the result to the errors of the
// [eventtypes] package:
//
//	200                                    eventtypes.Result, nil
//	413                                    *eventtypes.InvalidError (code content_too_large)
//	422                                    *eventtypes.InvalidError (code and field errors of the problem)
//	401, 403                               eventtypes.ErrDenied
//	408, 429, 5xx, network error,
//	token unavailable, response too large  eventtypes.ErrUnavailable
//	429, 503 with Retry-After,
//	token unavailable with a wait time     *eventtypes.UnavailableError (see eventtypes.RetryAfter)
//	other 4xx                              eventtypes.ErrRejected
//	invalid_client, invalid_scope,
//	unauthorized_client from the token
//	endpoint                               eventtypes.ErrDenied
//	other                                  eventtypes.ErrUnavailable
func (r *EventTypeRegistry) Register(ctx context.Context, reg eventtypes.Registration) (eventtypes.Result, error) {
	body := EventTypeRegistration{
		App:        reg.App,
		Source:     reg.Source,
		EventTypes: make([]EventTypeDefinition, 0, len(reg.EventTypes)),
	}
	for _, t := range reg.EventTypes {
		var schema map[string]any
		if err := json.Unmarshal(t.DataSchema, &schema); err != nil {
			// Validate already refuses this. Do not send a bad body.
			return eventtypes.Result{}, &eventtypes.InvalidError{
				Code:     "invalid_data_schema",
				Problems: []eventtypes.Problem{{Code: "invalid_data_schema", Field: "/eventTypes/dataSchema"}},
			}
		}
		def := EventTypeDefinition{Type: t.Type, Description: t.Description, DataSchema: schema}
		if t.Deprecated {
			d := true
			def.Deprecated = &d
		}
		body.EventTypes = append(body.EventTypes, def)
	}
	resp, err := r.client.EventTypeRegistrationsRegisterWithResponse(ctx, reg.App, body)
	if err != nil {
		return eventtypes.Result{}, mapRequestError(err)
	}
	status := resp.StatusCode()
	if ra, ok := retryAfter(resp.HTTPResponse); ok {
		return eventtypes.Result{}, &eventtypes.UnavailableError{Cause: fmt.Errorf("status %d", status), RetryAfter: ra}
	}
	switch status {
	case http.StatusOK:
		if resp.JSON200 == nil {
			return eventtypes.Result{}, fmt.Errorf("%w: status 200 with no result", eventtypes.ErrUnavailable)
		}
		j := resp.JSON200
		return eventtypes.Result{
			App: j.App, Version: int(j.Version), RegisteredAt: j.RegisteredAt, EventTypeCount: int(j.EventTypeCount),
		}, nil
	case http.StatusRequestEntityTooLarge:
		return eventtypes.Result{}, &eventtypes.InvalidError{Code: "content_too_large"}
	case http.StatusUnprocessableEntity:
		return eventtypes.Result{}, invalidError(resp.ApplicationproblemJSON422)
	case http.StatusUnauthorized, http.StatusForbidden:
		return eventtypes.Result{}, fmt.Errorf("%w: status %d", eventtypes.ErrDenied, status)
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return eventtypes.Result{}, fmt.Errorf("%w: status %d", eventtypes.ErrUnavailable, status)
	default:
		if status >= 400 && status < 500 {
			return eventtypes.Result{}, fmt.Errorf("%w: status %d", eventtypes.ErrRejected, status)
		}
		return eventtypes.Result{}, fmt.Errorf("%w: status %d", eventtypes.ErrUnavailable, status)
	}
}

// retryAfter returns the wait time of a 429, or of a 503 with a valid Retry-After.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	d, ok := retryafter.FromStatus(resp.StatusCode, resp.Header, time.Now())
	return d, ok && d > 0
}

func invalidError(p *UnprocessableContent) error {
	e := &eventtypes.InvalidError{}
	if p != nil {
		e.Code = p.Code
		if p.Errors != nil {
			for _, fe := range *p.Errors {
				e.Problems = append(e.Problems, eventtypes.Problem{Code: fe.Code, Field: fe.Field, Message: fe.Message})
			}
		}
	}
	return e
}

func mapRequestError(err error) error {
	var oe *auth.OAuthError
	if errors.As(err, &oe) {
		switch oe.Code {
		case "invalid_client", "invalid_scope", "unauthorized_client":
			return fmt.Errorf("%w: token endpoint returned %s", eventtypes.ErrDenied, oe.Code)
		}
		return fmt.Errorf("%w: token endpoint returned %s", eventtypes.ErrUnavailable, oe.Code)
	}
	// This covers auth.ErrTokenUnavailable, auth.ErrBadTokenResponse, a network
	// error, a time out, and ErrResponseTooLarge. The wait time of the token
	// endpoint goes on.
	ra, _ := auth.RetryAfter(err)
	return &eventtypes.UnavailableError{Cause: err, RetryAfter: ra}
}
