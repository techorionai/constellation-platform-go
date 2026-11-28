package eventtypes_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// U8 and U9
func TestU8U9ParityFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/parity/*.json")
	if err != nil || len(files) != 12 {
		t.Fatalf("fixtures: %d, %v", len(files), err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(f) //nolint:gosec // f comes from a fixed glob
			if err != nil {
				t.Fatal(err)
			}
			verr := regWithSchema(string(b)).Validate()
			switch {
			case strings.HasPrefix(name, "valid-"):
				if verr != nil {
					t.Fatal(verr)
				}
			case strings.HasPrefix(name, "invalid-"):
				if verr == nil || !strings.Contains(verr.Error(), "data_schema_ref") {
					t.Fatalf("want data_schema_ref, got %v", verr)
				}
			default:
				t.Fatalf("bad fixture name %s", name)
			}
		})
	}
}
