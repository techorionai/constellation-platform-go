package eventtypes_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/eventtypes"
)

func regWithSchema(s string) eventtypes.Registration {
	r := goodReg()
	r.EventTypes[0].DataSchema = json.RawMessage(s)
	return r
}

// nested returns a schema with n nested "properties" steps.
func nested(n int) string {
	return strings.Repeat(`{"properties":{"a":`, n) + `{}` + strings.Repeat(`}}`, n)
}

func wantErr(t *testing.T, s, code string) {
	t.Helper()
	err := regWithSchema(s).Validate()
	if err == nil || !strings.Contains(err.Error(), "/eventTypes/0/dataSchema: "+code) {
		t.Fatalf("%.80s: want %s, got %v", s, code, err)
	}
	if _, lerr := eventtypes.Load(fileOf(t, regWithSchema(s)), "event-types.json"); lerr == nil {
		t.Error("Load accepted the file")
	}
}

func wantValid(t *testing.T, s string) {
	t.Helper()
	if err := regWithSchema(s).Validate(); err != nil {
		t.Fatalf("%.80s: %v", s, err)
	}
}

// U1
func TestU1SchemaID(t *testing.T) {
	wantErr(t, `{"$id":"urn:x"}`, "data_schema_id")
	wantErr(t, `{"$defs":{"a":{"$id":"urn:x"}}}`, "data_schema_id")
}

// U1b
func TestU1bPropertyIDAndConstID(t *testing.T) {
	wantValid(t, `{"properties":{"$id":{"type":"string"}}}`)
	wantValid(t, `{"const":{"$id":"urn:x"}}`)
}

// U2
func TestU2SchemaDepth(t *testing.T) {
	wantValid(t, nested(31))
	wantErr(t, nested(32), "data_schema_depth")
}

// U3
func TestU3RefTarget(t *testing.T) {
	wantValid(t, `{"$defs":{"a":{"type":"string"}},"properties":{"x":{"$ref":"#/$defs/a"}}}`)
	wantErr(t, `{"properties":{"x":{"$ref":"#/$defs/a"}}}`, "data_schema_ref")
}

// U4
func TestU4RefAnchor(t *testing.T) {
	wantValid(t, `{"$defs":{"a":{"$anchor":"x"}},"properties":{"p":{"$ref":"#x"}}}`)
	wantValid(t, `{"$defs":{"a":{"$dynamicAnchor":"x"}},"properties":{"p":{"$ref":"#x"}}}`)
	wantErr(t, `{"properties":{"p":{"$ref":"#x"}}}`, "data_schema_ref")
}

// U5
func TestU5Pattern(t *testing.T) {
	wantValid(t, `{"pattern":"^[a-z]+$"}`)
	wantErr(t, `{"pattern":"^(?!a).*$"}`, "data_schema_compile")
}

// U6
func TestU6PatternProperties(t *testing.T) {
	wantErr(t, `{"patternProperties":{"(?=x)":{"type":"string"}}}`, "data_schema_compile")
}

// U7
func TestU7LoadIndex(t *testing.T) {
	r := eventtypes.Registration{Source: "/helpdesk", App: "helpdesk", EventTypes: []eventtypes.EventType{
		goodType("helpdesk.ticket.a"), goodType("helpdesk.ticket.b"), goodType("helpdesk.ticket.c"),
	}}
	r.EventTypes[2].DataSchema = json.RawMessage(`{"$id":"urn:x"}`)
	_, err := eventtypes.Load(fileOf(t, r), "event-types.json")
	if err == nil || !strings.Contains(err.Error(), "/eventTypes/2/dataSchema: data_schema_id") {
		t.Fatalf("got %v", err)
	}
}

// parityFixtures maps each fixture file to the error code that it must give.
// An empty code means that the schema is valid.
var parityFixtures = map[string]string{
	"invalid-anchor-in-unknown-keyword.json":      "data_schema_ref",
	"invalid-deep-in-unknown-keyword.json":        "data_schema_ref",
	"invalid-dynamic-ref-to-unknown-keyword.json": "data_schema_ref",
	"invalid-escaped-pointer.json":                "data_schema_ref",
	"invalid-id-in-unknown-keyword.json":          "data_schema_ref",
	"invalid-nested-ref-to-unexamined.json":       "data_schema_ref",
	"invalid-pointer-into-const.json":             "data_schema_ref",
	"valid-anchor.json":                           "",
	"valid-defs-escape.json":                      "",
	"valid-helpdesk-ticket-data.json":             "",
	"valid-property-id.json":                      "",
	"valid-root-ref.json":                         "",
}

// U8 and U9
func TestU8U9ParityFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/parity/*.json")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, filepath.Base(f))
	}
	want := slices.Sorted(maps.Keys(parityFixtures))
	if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Fatalf("fixture files: got %v, want %v", got, want)
	}
	for _, name := range want {
		code := parityFixtures[name]
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata/parity", name)) //nolint:gosec // name comes from a fixed list
			if err != nil {
				t.Fatal(err)
			}
			verr := regWithSchema(string(b)).Validate()
			if code == "" {
				if verr != nil {
					t.Fatal(verr)
				}
				return
			}
			if verr == nil || !strings.Contains(verr.Error(), code) {
				t.Fatalf("want %s, got %v", code, verr)
			}
		})
	}
}

// R3: the first error does not depend on the map order.
func TestSchemaWalkOrder(t *testing.T) {
	const doc = `{"properties":{"b":{"$id":"urn:b"},"a":{"$ref":"https://x/y"},"c":{"$id":"urn:c"}}}`
	for range 20 {
		wantErr(t, doc, "data_schema_ref")
	}
}

// R2: the error shows the value of the reference.
func TestSchemaRefValueInError(t *testing.T) {
	verr := regWithSchema(`{"properties":{"x":{"$ref":"#/$defs/nope"}}}`).Validate()
	if verr == nil || !strings.Contains(verr.Error(), `"#/$defs/nope"`) {
		t.Fatalf("got %v", verr)
	}
}
