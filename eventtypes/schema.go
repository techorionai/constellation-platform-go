package eventtypes

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// maxSchemaDepth is the largest nesting depth of a dataSchema. The root has
// the depth 1. Each step into a member of the keyword lists adds 1. The count
// is equal to the count of the events service.
const maxSchemaDepth = 32

// Keywords whose value is one schema, a list of schemas, or a map of schemas.
// checkKeywords goes only into these keywords. It does not go into const, enum,
// default, examples, or an unknown keyword, because their values are data.
var (
	schemaKeywords     = []string{"items", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"}
	schemaListKeywords = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
	schemaMapKeywords  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
)

// schemaWalk holds the state of one check of a document. It is a copy of the
// rule walk of the events service.
type schemaWalk struct {
	// pointers is the set of JSON Pointers of the subschemas that the walk
	// examined. The root is the empty string.
	pointers map[string]struct{}
	// anchors is the set of the $anchor and $dynamicAnchor names of the
	// examined subschemas.
	anchors map[string]struct{}
	// refs is the list of the $ref and $dynamicRef values.
	refs []string
}

// checkKeywords reports a $ref or $dynamicRef that does not start with "#", a
// $id, a nesting depth of more than 32 levels, and a local reference whose
// target the walk did not examine. A target that the walk did not examine can
// be a part of an unknown keyword. The compiler compiles it as a schema, with
// no check of the $id and the depth. The root has the depth 1.
func checkKeywords(v any, depth int) error {
	w := &schemaWalk{pointers: map[string]struct{}{}, anchors: map[string]struct{}{}}
	if err := w.walk(v, depth, ""); err != nil {
		return err
	}
	for _, ref := range w.refs {
		if err := w.checkRef(ref); err != nil {
			return err
		}
	}
	return nil
}

// checkRef accepts a reference to an examined subschema or to the anchor of one.
func (w *schemaWalk) checkRef(ref string) error {
	frag := strings.TrimPrefix(ref, "#")
	if frag != "" && !strings.HasPrefix(frag, "/") {
		name, err := url.PathUnescape(frag)
		if _, ok := w.anchors[name]; err != nil || !ok {
			return errors.New("data_schema_ref: the anchor of the reference is not in an examined schema")
		}
		return nil
	}
	ptr, err := url.PathUnescape(frag)
	if err != nil {
		return errors.New("data_schema_ref: the pointer of the reference is not valid")
	}
	if _, ok := w.pointers[ptr]; !ok {
		return errors.New("data_schema_ref: the target of the reference is not a subschema that the service examines")
	}
	return nil
}

// escapePointer escapes one reference token of a JSON Pointer.
func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func (w *schemaWalk) walk(v any, depth int, ptr string) error {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if depth > maxSchemaDepth {
		return fmt.Errorf("data_schema_depth: more than %d levels", maxSchemaDepth)
	}
	w.pointers[ptr] = struct{}{}
	if _, has := m["$id"]; has {
		return errors.New("data_schema_id: $id must not be set")
	}
	for _, k := range []string{"$ref", "$dynamicRef"} {
		if s, ok := m[k].(string); ok {
			if !strings.HasPrefix(s, "#") {
				return fmt.Errorf("data_schema_ref: %s %q must be a local reference that starts with #", k, s)
			}
			w.refs = append(w.refs, s)
		}
	}
	for _, k := range []string{"$anchor", "$dynamicAnchor"} {
		if s, ok := m[k].(string); ok {
			w.anchors[s] = struct{}{}
		}
	}
	for _, k := range schemaKeywords {
		if err := w.walk(m[k], depth+1, ptr+"/"+k); err != nil {
			return err
		}
	}
	for _, k := range schemaListKeywords {
		if list, ok := m[k].([]any); ok {
			for i, item := range list {
				if err := w.walk(item, depth+1, fmt.Sprintf("%s/%s/%d", ptr, k, i)); err != nil {
					return err
				}
			}
		}
	}
	for _, k := range schemaMapKeywords {
		if sub, ok := m[k].(map[string]any); ok {
			for name, item := range sub {
				if err := w.walk(item, depth+1, ptr+"/"+k+"/"+escapePointer(name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
