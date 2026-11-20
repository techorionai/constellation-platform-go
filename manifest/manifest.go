package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// Registry publishes a permission manifest to the identity service.
type Registry interface {
	// Publish sends the manifest. It returns nil, an *[InvalidError], [ErrDenied],
	// or [ErrUnavailable]. An error of another kind counts as [ErrUnavailable].
	Publish(ctx context.Context, m PermissionManifest) error
}

// Errors of a [Registry].
var (
	// ErrDenied means that the identity service does not accept the credentials
	// or the scopes of the client. An operator can fix the client config.
	ErrDenied = errors.New("manifest: publish denied")
	// ErrUnavailable means that the identity service is down or that the request
	// failed. The caller can retry.
	ErrUnavailable = errors.New("manifest: registry unavailable")
)

// Problem is one problem that the identity service found in a manifest.
type Problem struct {
	// Code is the stable code, for example "permission_app_mismatch".
	Code string
	// Field is the JSON Pointer to the incorrect value. It can be empty.
	Field string
	// Message is the message for a person. It can be empty.
	Message string
}

// InvalidError means that the identity service rejected the manifest. A retry
// gives the same result.
type InvalidError struct {
	// Code is the code of the problem, for example "manifest_app_mismatch".
	Code string
	// Problems are the field errors.
	Problems []Problem
}

// Error returns the text of the error.
func (e *InvalidError) Error() string {
	var b strings.Builder
	b.WriteString("manifest: invalid manifest")
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

// PermissionDefinition is one permission that a service defines.
type PermissionDefinition struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// RoleDefinition is a default role of a service, with its permissions.
type RoleDefinition struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// PermissionManifest holds the permissions and the default roles that a service
// publishes.
type PermissionManifest struct {
	// App is the key of the application that owns the permissions.
	App string `json:"app"`
	// Permissions are the permissions that the application defines.
	Permissions []PermissionDefinition `json:"permissions"`
	// DefaultRoles are the default roles of the application.
	DefaultRoles []RoleDefinition `json:"defaultRoles"`
}

var (
	appKeyPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)
)

const (
	maxAppKeyLen        = 32
	maxRoleKeyLen       = 64
	maxPermissionKeyLen = 128
	adminPermission     = "identity:admin"
)

// Validate checks the manifest with the rules of the identity service: the
// app key, the app prefix of each permission key, the role permissions that the
// manifest defines, and no duplicate key. It returns all problems in one error.
func (m PermissionManifest) Validate() error {
	var errs []error
	if !appKeyPattern.MatchString(m.App) || len(m.App) > maxAppKeyLen {
		errs = append(errs, fmt.Errorf("app %q is not a valid app key", m.App))
	}
	defined := make(map[string]struct{}, len(m.Permissions))
	for i, p := range m.Permissions {
		validShape := permissionPattern.MatchString(p.Key) || (m.App == "identity" && p.Key == adminPermission)
		switch {
		case !validShape || len(p.Key) > maxPermissionKeyLen:
			errs = append(errs, fmt.Errorf("permissions[%d]: key %q is not a valid permission key", i, p.Key))
		case !strings.HasPrefix(p.Key, m.App+":"):
			errs = append(errs, fmt.Errorf("permissions[%d]: key %q does not start with the app prefix %q", i, p.Key, m.App+":"))
		}
		if _, dup := defined[p.Key]; dup {
			errs = append(errs, fmt.Errorf("permissions[%d]: duplicate key %q", i, p.Key))
		}
		defined[p.Key] = struct{}{}
	}
	roles := make(map[string]struct{}, len(m.DefaultRoles))
	for i, r := range m.DefaultRoles {
		if !appKeyPattern.MatchString(r.Key) || len(r.Key) > maxRoleKeyLen {
			errs = append(errs, fmt.Errorf("defaultRoles[%d]: key %q is not a valid role key", i, r.Key))
		}
		if _, dup := roles[r.Key]; dup {
			errs = append(errs, fmt.Errorf("defaultRoles[%d]: duplicate key %q", i, r.Key))
		}
		roles[r.Key] = struct{}{}
		seen := make(map[string]struct{}, len(r.Permissions))
		for _, key := range r.Permissions {
			if _, ok := defined[key]; !ok {
				errs = append(errs, fmt.Errorf("defaultRoles[%d]: permission %q is not in the manifest", i, key))
			}
			if _, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("defaultRoles[%d]: duplicate permission %q", i, key))
			}
			seen[key] = struct{}{}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("manifest: invalid manifest: %w", err)
	}
	return nil
}

// Load reads a manifest from a JSON file in fsys, for example an embedded file,
// and validates it. It rejects unknown fields.
func Load(fsys fs.FS, path string) (PermissionManifest, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return PermissionManifest{}, fmt.Errorf("manifest: cannot read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m PermissionManifest
	if err := dec.Decode(&m); err != nil {
		return PermissionManifest{}, fmt.Errorf("manifest: cannot parse %s: %w", path, err)
	}
	if dec.More() {
		return PermissionManifest{}, fmt.Errorf("manifest: %s has data after the manifest", path)
	}
	if err := m.Validate(); err != nil {
		return PermissionManifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
