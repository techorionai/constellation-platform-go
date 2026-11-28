package mcpserver

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// checkArguments validates the arguments against the resolved input schema. On
// success it returns the arguments with the schema defaults applied. On failure
// it returns one field error for each bad value.
func checkArguments(rs *jsonschema.Resolved, args json.RawMessage) (json.RawMessage, []FieldError) {
	v := map[string]any{}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, []FieldError{{Field: "", Code: "type", Message: "the arguments must be a JSON object"}}
		}
	}
	var inst any = v
	if err := rs.ApplyDefaults(&inst); err != nil {
		return nil, []FieldError{{Field: "", Code: "invalid", Message: "the arguments are not valid"}}
	}
	if err := rs.Validate(inst); err != nil {
		fields := fieldErrors(rs.Schema(), inst, "")
		if len(fields) == 0 {
			fields = []FieldError{{Field: "", Code: errorCode(err), Message: errorMessage(err)}}
		}
		return nil, fields
	}
	out, err := json.Marshal(inst)
	if err != nil {
		return nil, []FieldError{{Field: "", Code: "invalid", Message: "the arguments are not valid"}}
	}
	return out, nil
}

// fieldErrors finds the bad values one level at a time. For an object schema it
// reports each missing required property and each bad property value. It
// returns nothing for a value that it cannot split further.
func fieldErrors(s *jsonschema.Schema, v any, ptr string) []FieldError {
	obj, isObj := v.(map[string]any)
	if !isObj || s == nil || (len(s.Properties) == 0 && len(s.Required) == 0) {
		return nil
	}
	var out []FieldError
	for _, name := range s.Required {
		if _, ok := obj[name]; !ok {
			out = append(out, FieldError{Field: ptr + "/" + escapePointer(name), Code: "required", Message: "the property is required"})
		}
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := ptr + "/" + escapePointer(name)
		sub, known := s.Properties[name]
		if !known {
			if s.AdditionalProperties != nil && s.AdditionalProperties.Not != nil {
				out = append(out, FieldError{Field: child, Code: "additional_property", Message: "the property is not allowed"})
			}
			continue
		}
		rs, err := sub.Resolve(nil)
		if err != nil {
			continue
		}
		verr := rs.Validate(obj[name])
		if verr == nil {
			continue
		}
		if nested := fieldErrors(sub, obj[name], child); len(nested) > 0 {
			out = append(out, nested...)
			continue
		}
		out = append(out, FieldError{Field: child, Code: errorCode(verr), Message: errorMessage(verr)})
	}
	return out
}

// errorCode returns the keyword of a validation error, for example "type" or
// "enum".
func errorCode(err error) string {
	kw, _, _ := strings.Cut(err.Error(), ":")
	if kw == "" || strings.ContainsAny(kw, " \n\"") {
		return "invalid"
	}
	return kw
}

// errorMessage returns a message with no value of the arguments. A validation
// error can have the value, and the value can be personal data.
func errorMessage(err error) string {
	return "the value does not match the schema keyword " + errorCode(err)
}

func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}
