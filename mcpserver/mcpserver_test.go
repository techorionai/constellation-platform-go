package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/mcpserver"
)

func newDefault(t *testing.T) *env {
	t.Helper()
	return newEnv(t, principals(testPrincipals), nil)
}

// resp is a response with the body read and closed.
type resp struct {
	StatusCode int
	Header     http.Header
	Body       *bytes.Reader
}

func rawPost(t *testing.T, url, token string, body string, hdr map[string]string) *resp {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &resp{StatusCode: res.StatusCode, Header: res.Header, Body: bytes.NewReader(data)}
}

func callBody(tool, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

// U1: AddTool refuses a bad spec and does not panic.
func TestU1AddToolErrors(t *testing.T) {
	e := newDefault(t)
	h := func(context.Context, auth.Principal, getIn) (item, error) { return item{}, nil }
	bad := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
		"x": {Ref: "#/$defs/missing"},
	}}
	cases := map[string]mcpserver.ToolSpec{
		"scope of another app": {Name: "t1", Scopes: []string{"crm:x:read"}},
		"no scope":             {Name: "t2"},
		"duplicate name":       {Name: "get_item", Scopes: []string{scopeRead}},
		"schema not resolved":  {Name: "t3", Scopes: []string{scopeRead}, InputSchema: bad},
		"no name":              {Scopes: []string{scopeRead}},
		"input not object":     {Name: "t4", Scopes: []string{scopeRead}, InputSchema: &jsonschema.Schema{Type: "string"}},
	}
	for name, spec := range cases {
		if err := mcpserver.AddTool(e.srv, spec, h); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := mcpserver.New(mcpserver.Config{App: "Bad App"}); err == nil {
		t.Error("New with a bad config: no error")
	}
}

// U2 and U15: tools/list has the scopes in _meta, the schemas, the annotations,
// and the enum of a given schema.
func TestU2U15ToolsList(t *testing.T) {
	e := newDefault(t)
	s := connect(t, e, "read")
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("tools: %d", len(res.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	get, upd := byName["get_item"], byName["update_item"]
	if get == nil || upd == nil {
		t.Fatalf("tools: %v", byName)
	}
	wantMeta := func(tool *mcp.Tool, want ...string) {
		t.Helper()
		got, _ := tool.Meta[mcpserver.ScopesMetaKey].([]any)
		var s []string
		for _, v := range got {
			s = append(s, v.(string))
		}
		if strings.Join(s, ",") != strings.Join(want, ",") {
			t.Errorf("%s meta scopes %v, want %v", tool.Name, s, want)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s has no schema", tool.Name)
		}
	}
	wantMeta(get, scopeRead)
	wantMeta(upd, scopeRead, scopeWrite)
	if !get.Annotations.ReadOnlyHint || !get.Annotations.IdempotentHint || get.Annotations.DestructiveHint == nil || *get.Annotations.DestructiveHint {
		t.Errorf("get annotations %+v", get.Annotations)
	}
	if upd.Annotations.ReadOnlyHint || upd.Annotations.IdempotentHint {
		t.Errorf("update annotations %+v", upd.Annotations)
	}
	in, _ := json.Marshal(upd.InputSchema)
	if !strings.Contains(string(in), `"enum":["open","closed"]`) {
		t.Errorf("update input schema has no enum: %s", in)
	}
	// A valid value calls the handler.
	s2 := connect(t, e, "all")
	if res := call(t, s2, "update_item", map[string]any{"id": "a", "status": "closed"}); res.IsError {
		t.Errorf("valid call failed: %s", textOf(t, res))
	}
}

// U3: ToolError and other errors.
func TestU3ToolErrors(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := newDefault(t)
	s := connect(t, e, "all")
	res := call(t, s, "update_item", map[string]any{"id": "dup", "status": "open"})
	if !res.IsError || res.StructuredContent != nil {
		t.Fatalf("isError %v structured %v", res.IsError, res.StructuredContent)
	}
	var te mcpserver.ToolError
	if err := json.Unmarshal([]byte(textOf(t, res)), &te); err != nil {
		t.Fatal(err)
	}
	if te.Code != "item_name_taken" || te.Message == "" || len(te.Fields) != 2 || te.Fields[0].Field != "/id" {
		t.Errorf("tool error %+v", te)
	}
	res = call(t, s, "update_item", map[string]any{"id": "boom", "status": "open"})
	if !res.IsError || res.StructuredContent != nil {
		t.Fatalf("isError %v structured %v", res.IsError, res.StructuredContent)
	}
	if got := textOf(t, res); got != `{"code":"internal","message":"internal error"}` {
		t.Errorf("internal error text %s", got)
	}
	if strings.Contains(textOf(t, res), "hunter2") {
		t.Error("the result has the error text")
	}
	if !strings.Contains(logs.String(), "hunter2") {
		t.Errorf("the log has no full error: %s", logs.String())
	}
}

// U5: gate limits.
func TestU5Gate(t *testing.T) {
	e := newEnv(t, principals(testPrincipals), func(c *mcpserver.Config) { c.MaxBodyBytes = 1 << 20 })
	url := e.ts.URL + "/mcp"
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", 1<<20) + `"}}`
	if res := rawPost(t, url, "all", big, nil); res.StatusCode != 413 {
		t.Errorf("big body: %d", res.StatusCode)
	}
	if res := rawPost(t, url, "all", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, nil); res.StatusCode != 400 {
		t.Errorf("batch: %d", res.StatusCode)
	}
	res := rawPost(t, url, "read", callBody("no_such_tool", `{}`), nil)
	if res.StatusCode == 403 || res.StatusCode == 413 {
		t.Errorf("unknown tool: %d", res.StatusCode)
	}
	if len(e.rec.refused) != 0 {
		t.Errorf("hook ran for an unknown tool")
	}
}

// U6: the gate refuses a call with a missing scope.
func TestU6GateMissingScope(t *testing.T) {
	e := newDefault(t)
	res := rawPost(t, e.ts.URL+"/mcp", "read", callBody("update_item", `{"id":"abc","status":"open"}`), nil)
	if res.StatusCode != 403 {
		t.Fatalf("status %d", res.StatusCode)
	}
	www := res.Header.Get("WWW-Authenticate")
	for _, want := range []string{`error="insufficient_scope"`, scopeRead + " " + scopeWrite, `resource_metadata="` + e.ts.URL + `/.well-known/oauth-protected-resource/mcp"`} {
		if !strings.Contains(www, want) {
			t.Errorf("WWW-Authenticate %q lacks %q", www, want)
		}
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type %q", ct)
	}
	var body struct {
		Code          string
		MissingScopes []string
		Status        int
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "insufficient_scope" || body.Status != 403 || len(body.MissingScopes) != 1 || body.MissingScopes[0] != scopeWrite {
		t.Errorf("body %+v", body)
	}
	if len(e.rec.refused) != 1 {
		t.Fatalf("OnRefused ran %d times", len(e.rec.refused))
	}
	r := e.rec.refused[0]
	if r.Tool != "update_item" || len(r.Missing) != 1 || !strings.Contains(string(r.Arguments), `"abc"`) {
		t.Errorf("refusal %+v", r)
	}
	if *e.handlerCalls != 0 {
		t.Error("handler ran")
	}
}

// U7: origin check.
func TestU7Origin(t *testing.T) {
	e := newEnv(t, principals(testPrincipals), func(c *mcpserver.Config) { c.AllowedOrigins = []string{"https://ok.example"} })
	url := e.ts.URL + "/mcp"
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if res := rawPost(t, url, "all", list, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != 403 {
		t.Errorf("evil origin: %d", res.StatusCode)
	}
	if res := rawPost(t, url, "all", list, map[string]string{"Origin": "https://ok.example"}); res.StatusCode != 200 {
		t.Errorf("allowed origin: %d", res.StatusCode)
	}
	if res := rawPost(t, url, "all", list, nil); res.StatusCode != 200 {
		t.Errorf("no origin: %d", res.StatusCode)
	}
}

// U8: no log record and no span has the value of an argument.
func TestU8NoArgumentsInTelemetry(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	e := newEnv(t, principals(testPrincipals), func(c *mcpserver.Config) { c.TracerProvider = tp })
	s := connect(t, e, "all")
	call(t, s, "update_item", map[string]any{"id": "secret-value", "status": "open"})
	call(t, s, "update_item", map[string]any{"id": "secret-value", "status": "bad"})
	call(t, s, "get_item", map[string]any{"id": "secret-value"})
	spans := sr.Ended()
	found := false
	for _, sp := range spans {
		if sp.Name() == "mcp.tool" {
			found = true
		}
		if strings.Contains(sp.Name()+attrsText(sp), "secret-value") {
			t.Errorf("span %s has the value", sp.Name())
		}
	}
	if !found {
		t.Error("no mcp.tool span")
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Errorf("a log record has the value: %s", logs.String())
	}
}

func attrsText(sp sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, a := range sp.Attributes() {
		b.WriteString(string(a.Key) + "=" + a.Value.String() + " ")
	}
	return b.String()
}

// U9: link template checks.
func TestU9LinkTemplateChecks(t *testing.T) {
	e := newDefault(t)
	ok := func(mod func(*mcpserver.LinkTemplateSet)) mcpserver.LinkTemplateSet {
		set := mcpserver.LinkTemplateSet{App: app, BaseURL: "http://localhost:8080", Templates: []mcpserver.LinkTemplate{
			{ResourceType: "item", Template: "{base_url}/items/{id}"},
		}}
		if mod != nil {
			mod(&set)
		}
		return set
	}
	for name, set := range map[string]mcpserver.LinkTemplateSet{
		"valid":                  ok(nil),
		"valid https with path":  ok(func(s *mcpserver.LinkTemplateSet) { s.BaseURL = "https://example.com/helpdesk" }),
		"valid with no variable": ok(func(s *mcpserver.LinkTemplateSet) { s.Templates[0].Template = "{base_url}/items" }),
	} {
		if err := set.Validate(app); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]mcpserver.LinkTemplateSet{}
	for _, tpl := range []string{"{base_url}/a/../b", "{base_url}/{+id}", "{base_url}/a?x=1", "{base_url}/a#x", "{base_url}/a@b", "/items/{id}", "{base_url}items"} {
		bad["template "+tpl] = ok(func(s *mcpserver.LinkTemplateSet) { s.Templates[0].Template = tpl })
	}
	for _, u := range []string{"http://example.com", "http://localhost:8080/", "https://example.com/", "https://user@example.com", "https://example.com/a/../b", "https://example.com/?q=1", "https://example.com/#f"} {
		bad["base URL "+u] = ok(func(s *mcpserver.LinkTemplateSet) { s.BaseURL = u })
	}
	bad["bad resource type"] = ok(func(s *mcpserver.LinkTemplateSet) { s.Templates[0].ResourceType = "Item" })
	bad["duplicate resource type"] = ok(func(s *mcpserver.LinkTemplateSet) { s.Templates = append(s.Templates, s.Templates[0]) })
	bad["empty list"] = ok(func(s *mcpserver.LinkTemplateSet) { s.Templates = nil })
	bad["another app"] = ok(func(s *mcpserver.LinkTemplateSet) { s.App = "crm" })
	for name, set := range bad {
		if err := e.srv.AddLinkTemplates(set); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// U10: read of the link templates resource.
func TestU10LinkTemplatesResource(t *testing.T) {
	e := newDefault(t)
	set := mcpserver.LinkTemplateSet{App: app, BaseURL: "https://demo.example.com", Templates: []mcpserver.LinkTemplate{
		{ResourceType: "item", Template: "{base_url}/items/{id}", Description: "An item."},
	}}
	if err := e.srv.AddLinkTemplates(set); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.AddLinkTemplates(set); err == nil {
		t.Error("second add: no error")
	}
	s := connect(t, e, "read")
	res, err := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "constellation://demo/link-templates"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("contents %+v", res.Contents)
	}
	var got mcpserver.LinkTemplateSet
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &got); err != nil {
		t.Fatal(err)
	}
	if got.App != app || got.BaseURL != set.BaseURL || len(got.Templates) != 1 || got.Templates[0] != set.Templates[0] {
		t.Errorf("set %+v", got)
	}
	if !strings.Contains(res.Contents[0].Text, `"baseUrl"`) || !strings.Contains(res.Contents[0].Text, `"resourceType"`) {
		t.Errorf("field names: %s", res.Contents[0].Text)
	}
}

// U11: metadata handler.
func TestU11Metadata(t *testing.T) {
	e := newDefault(t)
	res, err := http.Get(e.ts.URL + "/.well-known/oauth-protected-resource/mcp") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || doc.Resource != e.ts.URL+"/mcp" || len(doc.AuthorizationServers) != 1 ||
		strings.Join(doc.ScopesSupported, ",") != scopeRead+","+scopeWrite || len(doc.BearerMethods) != 1 || doc.BearerMethods[0] != "header" {
		t.Errorf("metadata %+v", doc)
	}
}

// U14: bad arguments.
func TestU14InvalidArguments(t *testing.T) {
	e := newDefault(t)
	s := connect(t, e, "all")
	cases := []struct {
		name  string
		args  map[string]any
		field string
	}{
		{"bad type", map[string]any{"id": 5, "status": "open"}, "/id"},
		{"bad enum", map[string]any{"id": "a", "status": "pending"}, "/status"},
		{"missing required", map[string]any{"id": "a"}, "/status"},
	}
	for _, c := range cases {
		res := call(t, s, "update_item", c.args)
		if !res.IsError {
			t.Fatalf("%s: no error", c.name)
		}
		var te mcpserver.ToolError
		if err := json.Unmarshal([]byte(textOf(t, res)), &te); err != nil {
			t.Fatal(err)
		}
		if te.Code != "invalid_arguments" || len(te.Fields) != 1 || te.Fields[0].Field != c.field {
			t.Errorf("%s: %+v", c.name, te)
		}
	}
	if *e.handlerCalls != 0 {
		t.Error("handler ran")
	}
	if len(e.rec.invalid) != len(cases) {
		t.Fatalf("OnInvalid ran %d times", len(e.rec.invalid))
	}
	if r := e.rec.invalid[0]; r.Tool != "update_item" || len(r.Missing) != 0 || !strings.Contains(string(r.Arguments), `"id"`) {
		t.Errorf("refusal %+v", r)
	}
	// Two bad values give two field errors.
	res := call(t, s, "update_item", map[string]any{"id": 5, "status": "pending"})
	var te mcpserver.ToolError
	if err := json.Unmarshal([]byte(textOf(t, res)), &te); err != nil {
		t.Fatal(err)
	}
	if len(te.Fields) != 2 {
		t.Errorf("fields %+v", te.Fields)
	}
}

// U16: no-store headers on all responses.
func TestU16NoStore(t *testing.T) {
	e := newEnv(t, principals(testPrincipals), func(c *mcpserver.Config) { c.AllowedOrigins = []string{"https://ok.example"} })
	url := e.ts.URL + "/mcp"
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", 1<<20) + `"}}`
	cases := map[string]*resp{
		"success": rawPost(t, url, "all", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil),
		"400":     rawPost(t, url, "all", `[]`, nil),
		"401":     rawPost(t, url, "", `{}`, nil),
		"403":     rawPost(t, url, "read", callBody("update_item", `{}`), nil),
		"413":     rawPost(t, url, "all", big, nil),
		"origin":  rawPost(t, url, "all", `{}`, map[string]string{"Origin": "https://evil.example"}),
	}
	wantStatus := map[string]int{"success": 200, "400": 400, "401": 401, "403": 403, "413": 413, "origin": 403}
	for name, res := range cases {
		if res.StatusCode != wantStatus[name] {
			t.Errorf("%s: status %d", name, res.StatusCode)
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" {
			t.Errorf("%s: headers %v", name, res.Header)
		}
	}
}

// U17: the counter of tool calls.
func TestU17Counter(t *testing.T) {
	e := newDefault(t)
	all, read := connect(t, e, "all"), connect(t, e, "read")
	call(t, all, "get_item", map[string]any{"id": "a"})
	call(t, all, "update_item", map[string]any{"id": "dup", "status": "open"})
	if res, err := read.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_item", Arguments: map[string]any{"id": "a", "status": "open"}}); err == nil {
		t.Errorf("denied call: no error, %+v", res)
	}
	call(t, all, "update_item", map[string]any{"id": "a", "status": "bad"})

	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	unit := ""
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.mcp.tool.calls" {
				continue
			}
			unit = m.Unit
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				tool, _ := dp.Attributes.Value("tool")
				out, _ := dp.Attributes.Value("outcome")
				got[tool.AsString()+"/"+out.AsString()] += dp.Value
			}
		}
	}
	want := map[string]int64{"get_item/success": 1, "update_item/error": 1, "update_item/denied": 1, "update_item/invalid": 1}
	if unit != "{call}" {
		t.Errorf("unit %q", unit)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

// The default meter provider is used if the config has none.
func TestDefaultProviders(t *testing.T) {
	_ = otel.GetMeterProvider()
	s, err := mcpserver.New(mcpserver.Config{
		App: app, Name: "demo", Verifier: principals(nil), ResourceURL: "https://demo.example/mcp",
		AuthorizationServers: []string{"https://identity.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Handler() == nil || s.MetadataHandler() == nil {
		t.Error("nil handler")
	}
}
