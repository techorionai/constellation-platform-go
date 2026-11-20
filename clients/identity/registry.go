package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/manifest"
)

var _ manifest.Registry = (*ManifestRegistry)(nil)

// ManifestRegistry is the adapter of [manifest.Registry]. It publishes a
// manifest with the identity API.
type ManifestRegistry struct {
	client *ClientWithResponses
}

// NewManifestRegistry makes a registry from a client. The client needs the
// scope identity:manifest:publish.
func NewManifestRegistry(c *ClientWithResponses) *ManifestRegistry {
	return &ManifestRegistry{client: c}
}

// Publish sends the manifest. It maps the result to the errors of the
// [manifest] package:
//
//	200                                    nil
//	422                                    *manifest.InvalidError
//	401, 403                               manifest.ErrDenied
//	5xx, network error, token unavailable  manifest.ErrUnavailable
//	invalid_client, invalid_scope,
//	unauthorized_client from the token
//	endpoint                               manifest.ErrDenied
//	other                                  manifest.ErrUnavailable
func (r *ManifestRegistry) Publish(ctx context.Context, m manifest.PermissionManifest) error {
	body := PermissionManifest{
		App:          m.App,
		Permissions:  make([]PermissionDefinition, 0, len(m.Permissions)),
		DefaultRoles: make([]RoleDefinition, 0, len(m.DefaultRoles)),
	}
	for _, p := range m.Permissions {
		body.Permissions = append(body.Permissions, PermissionDefinition{Key: p.Key, Description: p.Description})
	}
	for _, d := range m.DefaultRoles {
		body.DefaultRoles = append(body.DefaultRoles, RoleDefinition{
			Key: d.Key, Name: d.Name, Description: d.Description,
			Permissions: append([]string{}, d.Permissions...),
		})
	}
	resp, err := r.client.PermissionManifestsPublishWithResponse(ctx, m.App, body)
	if err != nil {
		return mapRequestError(err)
	}
	status := resp.StatusCode()
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnprocessableEntity:
		return invalidError(resp.ApplicationproblemJSON422)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d", manifest.ErrDenied, status)
	default:
		return fmt.Errorf("%w: status %d", manifest.ErrUnavailable, status)
	}
}

func invalidError(p *UnprocessableContent) error {
	e := &manifest.InvalidError{}
	if p != nil {
		e.Code = p.Code
		if p.Errors != nil {
			for _, fe := range *p.Errors {
				e.Problems = append(e.Problems, manifest.Problem{Code: fe.Code, Field: fe.Field, Message: fe.Message})
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
			return fmt.Errorf("%w: token endpoint returned %s", manifest.ErrDenied, oe.Code)
		}
		return fmt.Errorf("%w: token endpoint returned %s", manifest.ErrUnavailable, oe.Code)
	}
	// This covers auth.ErrTokenUnavailable, auth.ErrBadTokenResponse, a network
	// error, and a time out.
	return fmt.Errorf("%w: %w", manifest.ErrUnavailable, err)
}
