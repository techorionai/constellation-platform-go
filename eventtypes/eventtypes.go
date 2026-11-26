package eventtypes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/techorionai/constellation-platform-go/auth"
)

// Limits of the events service.
const (
	// MaxRegistrationBytes is the largest encoded registration.
	MaxRegistrationBytes = 4 << 20
	// MaxDataSchemaBytes is the largest dataSchema of one event type.
	MaxDataSchemaBytes = 64 << 10

	maxTypeLen        = 128
	maxTypes          = 200
	maxDescriptionLen = 1024
	draft2020         = "https://json-schema.org/draft/2020-12/schema"
)

var (
	typePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2,}$`)
)

// Registry sends a registration to the events service.
type Registry interface {
	// Register sends the registration. It returns the result, or an
	// *[InvalidError], [ErrDenied], [ErrRejected], or [ErrUnavailable]. An error
	// of another kind counts as [ErrUnavailable].
	Register(ctx context.Context, r Registration) (Result, error)
}

// Result is the answer of the events service to a registration. It has no
// schemas.
type Result struct {
	// App is the key of the app.
	App string
	// Version is the version of the registration. It increases with each change.
	Version int
	// RegisteredAt is the time of the last change.
	RegisteredAt time.Time
	// EventTypeCount is the count of the types that the service has not removed.
	EventTypeCount int
}

// EventType is one event type that a service publishes.
type EventType struct {
	// Type is the name, for example "helpdesk.ticket.created".
	Type string `json:"type"`
	// Description is the text for a person. It has 1 to 1024 characters.
	Description string `json:"description"`
	// DataSchema is a JSON Schema (draft 2020-12) of the data of the event.
	DataSchema json.RawMessage `json:"dataSchema"`
	// Deprecated is true if the event type is deprecated.
	Deprecated bool `json:"deprecated,omitempty"`
}

// Registration holds the event types that a service registers.
type Registration struct {
	// Source is the source of the events of the service. It is "/<app>".
	Source string `json:"source"`
	// App is the key of the app.
	App string `json:"app"`
	// EventTypes are the event types of the app.
	EventTypes []EventType `json:"eventTypes"`
}

// Types returns the names of the event types. Give them to outbox.Config.Types.
func (r Registration) Types() []string {
	out := make([]string, 0, len(r.EventTypes))
	for _, t := range r.EventTypes {
		out = append(out, t.Type)
	}
	return out
}

// Validate checks the registration with the rules of the events service. It
// returns all problems in one error. Each problem names the rule and the JSON
// Pointer of the value.
func (r Registration) Validate() error {
	var errs []error
	add := func(ptr, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", ptr, fmt.Sprintf(format, args...)))
	}
	if !auth.ValidAppKey(r.App) {
		add("/app", "app_key: %q is not a valid app key", r.App)
	}
	if r.Source != "/"+r.App {
		add("/source", "source: must be %q", "/"+r.App)
	}
	if n := len(r.EventTypes); n < 1 || n > maxTypes {
		add("/eventTypes", "type_count: %d types, need 1 to %d", n, maxTypes)
	}
	seen := make(map[string]struct{}, len(r.EventTypes))
	for i, t := range r.EventTypes {
		base := fmt.Sprintf("/eventTypes/%d", i)
		if !typePattern.MatchString(t.Type) || len(t.Type) > maxTypeLen {
			add(base+"/type", "type_pattern: %q is not a valid event type", t.Type)
		} else if !strings.HasPrefix(t.Type, r.App+".") {
			add(base+"/type", "type_app_prefix: %q does not start with %q", t.Type, r.App+".")
		}
		if _, dup := seen[t.Type]; dup {
			add(base+"/type", "type_duplicate: %q occurs more than one time", t.Type)
		}
		seen[t.Type] = struct{}{}
		if n := utf8.RuneCountInString(t.Description); n < 1 || n > maxDescriptionLen {
			add(base+"/description", "description_length: %d characters, need 1 to %d", n, maxDescriptionLen)
		}
		if err := checkSchema(t.Type, t.DataSchema); err != nil {
			add(base+"/dataSchema", "%v", err)
		}
	}
	if len(errs) == 0 {
		if b, err := json.Marshal(r); err != nil {
			errs = append(errs, fmt.Errorf("/: encode: %w", err))
		} else if len(b) > MaxRegistrationBytes {
			errs = append(errs, fmt.Errorf("/: registration_size: %d bytes, the maximum is %d", len(b), MaxRegistrationBytes))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("eventtypes: invalid registration: %w", err)
	}
	return nil
}

// noLoader refuses each URL, so a $ref cannot read a file or call a server.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("a reference to %q is not allowed", url)
}

// checkSchema checks one dataSchema.
func checkSchema(typ string, raw json.RawMessage) error {
	if len(raw) > MaxDataSchemaBytes {
		return fmt.Errorf("data_schema_size: %d bytes, the maximum is %d", len(raw), MaxDataSchemaBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("data_schema_object: not valid JSON: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return errors.New("data_schema_object: must be a JSON object")
	}
	if s, has := obj["$schema"]; has && s != draft2020 {
		return fmt.Errorf("data_schema_draft: $schema must be %s", draft2020)
	}
	if err := checkLocalRefs(doc); err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	uri := "urn:constellation:event-type:" + typ
	if err := c.AddResource(uri, doc); err != nil {
		return fmt.Errorf("data_schema_compile: %w", err)
	}
	if _, err := c.Compile(uri); err != nil {
		return fmt.Errorf("data_schema_compile: %w", err)
	}
	return nil
}

// Load reads a registration from a JSON file in fsys, for example an embedded
// file, and checks it with [Registration.Validate]. It refuses unknown fields,
// data after the object, and a file of more than [MaxRegistrationBytes].
func Load(fsys fs.FS, path string) (Registration, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return Registration{}, fmt.Errorf("eventtypes: cannot read %s: %w", path, err)
	}
	if len(data) > MaxRegistrationBytes {
		return Registration{}, fmt.Errorf("eventtypes: %s: registration_size: %d bytes, the maximum is %d", path, len(data), MaxRegistrationBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Registration
	if err := dec.Decode(&r); err != nil {
		return Registration{}, fmt.Errorf("eventtypes: cannot parse %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Registration{}, fmt.Errorf("eventtypes: %s has data after the registration", path)
	}
	if err := r.Validate(); err != nil {
		return Registration{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Keywords whose value is one schema, a list of schemas, or a map of schemas.
// checkLocalRefs goes only into these keywords. It does not go into const, enum,
// default, examples, or an unknown keyword, because their values are data.
var (
	schemaKeywords     = []string{"items", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"}
	schemaListKeywords = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
	schemaMapKeywords  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
)

// checkLocalRefs reports a $ref or $dynamicRef of a schema that does not start
// with "#". A reference must point into the schema itself. It examines the
// schema keywords only.
func checkLocalRefs(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range []string{"$ref", "$dynamicRef"} {
		if s, ok := m[k].(string); ok && !strings.HasPrefix(s, "#") {
			return fmt.Errorf("data_schema_ref: %s %q must be a local reference that starts with #", k, s)
		}
	}
	for _, k := range schemaKeywords {
		if err := checkLocalRefs(m[k]); err != nil {
			return err
		}
	}
	for _, k := range schemaListKeywords {
		if list, ok := m[k].([]any); ok {
			for _, item := range list {
				if err := checkLocalRefs(item); err != nil {
					return err
				}
			}
		}
	}
	for _, k := range schemaMapKeywords {
		if sub, ok := m[k].(map[string]any); ok {
			for _, item := range sub {
				if err := checkLocalRefs(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
