package eventtypes_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/techorionai/constellation-platform-go/eventtypes"
)

func schemaOfSize(n int) json.RawMessage {
	// {"description":"xxx"} padded to exactly n bytes.
	prefix, suffix := `{"description":"`, `"}`
	pad := n - len(prefix) - len(suffix)
	return json.RawMessage(prefix + strings.Repeat("x", pad) + suffix)
}

func fileOf(t *testing.T, r eventtypes.Registration) fstest.MapFS {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"event-types.json": {Data: b}}
}

func goodType(name string) eventtypes.EventType {
	return eventtypes.EventType{Type: name, Description: "An event.", DataSchema: json.RawMessage(`{"type":"object"}`)}
}

func goodReg() eventtypes.Registration {
	return eventtypes.Registration{Source: "/helpdesk", App: "helpdesk", EventTypes: []eventtypes.EventType{goodType("helpdesk.ticket.created")}}
}

// Issue 22 U1
func TestU1LoadValid(t *testing.T) {
	r := eventtypes.Registration{Source: "/helpdesk", App: "helpdesk"}
	for i := range 200 {
		et := goodType(fmt.Sprintf("helpdesk.ticket.t%d", i))
		et.Description = strings.Repeat("é", 1024)
		r.EventTypes = append(r.EventTypes, et)
	}
	r.EventTypes[0].DataSchema = schemaOfSize(64 << 10)
	got, err := eventtypes.Load(fileOf(t, r), "event-types.json")
	if err != nil {
		t.Fatal(err)
	}
	if got.App != "helpdesk" || len(got.Types()) != 200 || got.Types()[199] != "helpdesk.ticket.t199" {
		t.Errorf("registration %q, %d types", got.App, len(got.Types()))
	}
}

// Issue 22 U2
func TestU2LoadBadRules(t *testing.T) {
	mod := func(f func(*eventtypes.Registration)) eventtypes.Registration {
		r := goodReg()
		f(&r)
		return r
	}
	many := func(n int) []eventtypes.EventType {
		var out []eventtypes.EventType
		for i := range n {
			out = append(out, goodType(fmt.Sprintf("helpdesk.ticket.t%d", i)))
		}
		return out
	}
	schema := func(s string) func(*eventtypes.Registration) {
		return func(r *eventtypes.Registration) { r.EventTypes[0].DataSchema = json.RawMessage(s) }
	}
	dir := t.TempDir()
	cases := []struct {
		name, want string
		reg        eventtypes.Registration
	}{
		{"app pattern", "/app: app_key", mod(func(r *eventtypes.Registration) { r.App = "Help-Desk"; r.Source = "/Help-Desk" })},
		{"app 33 chars", "/app: app_key", mod(func(r *eventtypes.Registration) {
			r.App = strings.Repeat("a", 33)
			r.Source = "/" + r.App
		})},
		{"source", "/source: source", mod(func(r *eventtypes.Registration) { r.Source = "/other" })},
		{"type of another app", "/eventTypes/0/type: type_app_prefix", mod(func(r *eventtypes.Registration) { r.EventTypes[0].Type = "help.ticket.created" })},
		{"type pattern", "/eventTypes/0/type: type_pattern", mod(func(r *eventtypes.Registration) { r.EventTypes[0].Type = "helpdesk.Ticket" })},
		{"duplicate", "/eventTypes/1/type: type_duplicate", mod(func(r *eventtypes.Registration) { r.EventTypes = append(r.EventTypes, r.EventTypes[0]) })},
		{"0 types", "/eventTypes: type_count", mod(func(r *eventtypes.Registration) { r.EventTypes = nil })},
		{"201 types", "/eventTypes: type_count", mod(func(r *eventtypes.Registration) { r.EventTypes = many(201) })},
		{"empty description", "/eventTypes/0/description: description_length", mod(func(r *eventtypes.Registration) { r.EventTypes[0].Description = "" })},
		{"long description", "description_length", mod(func(r *eventtypes.Registration) { r.EventTypes[0].Description = strings.Repeat("a", 1025) })},
		{"schema not object", "data_schema_object", mod(schema(`[1]`))},
		{"schema 64 KiB + 1", "data_schema_size", mod(func(r *eventtypes.Registration) { r.EventTypes[0].DataSchema = schemaOfSize(64<<10 + 1) })},
		{"schema does not compile", "/eventTypes/0/dataSchema: data_schema_compile", mod(schema(`{"type": 5}`))},
		{"https ref", "data_schema_ref", mod(schema(`{"$ref":"https://example.com/s.json"}`))},
		{"file ref", "data_schema_ref", mod(schema(fmt.Sprintf(`{"$ref":"%s"}`, writeSchemaFile(t, dir))))},
		{"relative ref", "data_schema_ref", mod(schema(`{"$ref":"other.json"}`))},
		{"draft 7", "data_schema_draft", mod(schema(`{"$schema":"http://json-schema.org/draft-07/schema#"}`))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.reg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if _, err := eventtypes.Load(fileOf(t, c.reg), "event-types.json"); err == nil {
				t.Error("Load accepted the file")
			}
		})
	}

	// Three bad rules give three problems.
	r := goodReg()
	r.Source = "/other"
	r.EventTypes[0].Description = ""
	r.EventTypes[0].DataSchema = json.RawMessage(`{"type":5}`)
	err := r.Validate()
	if err == nil {
		t.Fatal("no error")
	}
	for _, w := range []string{"/source", "/eventTypes/0/description", "/eventTypes/0/dataSchema"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error has no %q: %v", w, err)
		}
	}

	// File rules.
	files := map[string]string{
		"unknown field": `{"source":"/helpdesk","app":"helpdesk","eventTypes":[],"x":1}`,
		"trailing data": mustJSON(t, goodReg()) + `{}`,
		"too large":     `{"source":"` + strings.Repeat("a", eventtypes.MaxRegistrationBytes) + `"}`,
	}
	for name, content := range files {
		if _, err := eventtypes.Load(fstest.MapFS{"f.json": {Data: []byte(content)}}, "f.json"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := eventtypes.Load(fstest.MapFS{}, "missing.json"); err == nil {
		t.Error("missing file: no error")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeSchemaFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "s.json")
	if err := os.WriteFile(p, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash("/" + strings.TrimPrefix(filepath.ToSlash(p), "/"))}).String()
}

// The encoded registration has a maximum of 4 MiB.
func TestRegistrationSizeLimit(t *testing.T) {
	r := goodReg()
	r.EventTypes = nil
	for i := range 100 {
		et := goodType(fmt.Sprintf("helpdesk.ticket.t%d", i))
		et.DataSchema = schemaOfSize(64 << 10)
		r.EventTypes = append(r.EventTypes, et)
	}
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "registration_size") {
		t.Fatalf("error = %v", err)
	}
}
