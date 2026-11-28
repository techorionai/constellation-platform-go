//go:build integration

package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	authjwt "github.com/techorionai/constellation-platform-go/auth/jwt"
	"github.com/techorionai/constellation-platform-go/mcpserver"
	"github.com/techorionai/constellation-platform-go/resources/test/issuer"
)

// The tests use the JWT verifier and the test issuer with a live JWKS server.

type jwtFixture struct {
	iss *issuer.Issuer
	v   *authjwt.Verifier
}

func newJWT(t *testing.T) *jwtFixture {
	t.Helper()
	iss := issuer.New(t)
	v, err := authjwt.New(authjwt.Config{Issuer: iss.Issuer(), Audience: app, JWKSURL: iss.URL(), AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return &jwtFixture{iss: iss, v: v}
}

func (f *jwtFixture) token(t *testing.T, c issuer.Claims) string {
	t.Helper()
	if c.Scope == "" {
		c.Scope = scopeRead + " " + scopeWrite
	}
	return f.iss.Mint(t, c)
}

// I1: a client lists the tools and calls a write tool.
func TestI1ListAndCall(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	s := connect(t, e, f.token(t, issuer.Claims{}))
	list, err := s.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) != 2 {
		t.Fatalf("tools %v err %v", list, err)
	}
	res := call(t, s, "update_item", map[string]any{"id": "i1", "status": "closed"})
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	var out item
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil || out.ID != "i1" || out.Status != "closed" {
		t.Errorf("structured content %s", b)
	}
}

// I2: no token, expired token, token for another audience.
func TestI2Unauthorized(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	now := time.Now()
	tokens := map[string]string{
		"none":     "",
		"expired":  f.token(t, issuer.Claims{IssuedAt: now.Add(-2 * time.Hour), Expires: now.Add(-time.Hour)}),
		"audience": f.token(t, issuer.Claims{Audience: "other"}),
	}
	for name, tok := range tokens {
		res := rawPost(t, e.ts.URL+"/mcp", tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
		if res.StatusCode != 401 {
			t.Errorf("%s: status %d", name, res.StatusCode)
		}
		if www := res.Header.Get("WWW-Authenticate"); !strings.Contains(www, `resource_metadata="`+e.ts.URL+`/.well-known/oauth-protected-resource/mcp"`) {
			t.Errorf("%s: WWW-Authenticate %q", name, www)
		}
	}
}

// I3: a token with only the read scope calls the write tool.
func TestI3InsufficientScope(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	tok := f.token(t, issuer.Claims{Scope: scopeRead})
	res := rawPost(t, e.ts.URL+"/mcp", tok, callBody("update_item", `{"id":"a","status":"open"}`), nil)
	if res.StatusCode != 403 || !strings.Contains(res.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("status %d header %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	var body struct {
		Code          string   `json:"code"`
		MissingScopes []string `json:"missingScopes"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "insufficient_scope" || len(body.MissingScopes) != 1 || body.MissingScopes[0] != scopeWrite {
		t.Errorf("body %+v", body)
	}
}

// I4: a token with act gives the handler a principal with Act set.
func TestI4Act(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	tok := f.token(t, issuer.Claims{Act: map[string]any{"sub": "agent-7", "principal_type": "agent"}})
	s := connect(t, e, tok)
	res := call(t, s, "get_item", map[string]any{"id": "a"})
	if res.IsError {
		t.Fatalf("error: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), `"actor":"agent-7"`) {
		t.Errorf("result %s", textOf(t, res))
	}
}

// I5: a client reads the link templates resource.
func TestI5LinkTemplates(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	set := mcpserver.LinkTemplateSet{App: app, BaseURL: "https://demo.example.com", Templates: []mcpserver.LinkTemplate{
		{ResourceType: "item", Template: "{base_url}/items/{id}"},
	}}
	if err := e.srv.AddLinkTemplates(set); err != nil {
		t.Fatal(err)
	}
	// A token with no tool scope can read the resource.
	s := connect(t, e, f.token(t, issuer.Claims{Scope: "demo:other:read"}))
	res, err := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "constellation://demo/link-templates"})
	if err != nil {
		t.Fatal(err)
	}
	var got mcpserver.LinkTemplateSet
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &got); err != nil || got.BaseURL != set.BaseURL {
		t.Errorf("set %+v err %v", got, err)
	}
	if r := rawPost(t, e.ts.URL+"/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"constellation://demo/link-templates"}}`, nil); r.StatusCode != 401 {
		t.Errorf("read with no token: %d", r.StatusCode)
	}
}

// I6: requests with three protocol versions.
func TestI6ProtocolVersions(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	tok := f.token(t, issuer.Claims{})
	for _, v := range []string{"2025-06-18", "2025-11-25", "2026-07-28"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		if v == "2026-07-28" {
			body = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
		}
		res := rawPost(t, e.ts.URL+"/mcp", tok, body, map[string]string{"Mcp-Protocol-Version": v, "Mcp-Method": "tools/list"})
		var out struct {
			Result struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"result"`
			Error *struct{ Message string } `json:"error"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if res.StatusCode != 200 || len(out.Result.Tools) != 2 {
			t.Errorf("%s: status %d tools %d error %v", v, res.StatusCode, len(out.Result.Tools), out.Error)
		}
	}
}

// I7: two instances behind a round robin proxy give no session error.
func TestI7TwoInstances(t *testing.T) {
	f := newJWT(t)
	a := newEnv(t, f.v, nil)
	b := newEnv(t, f.v, nil)
	targets := []*url.URL{mustURL(t, a.ts.URL), mustURL(t, b.ts.URL)}
	var n atomic.Int64
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		u := targets[n.Add(1)%2]
		pr.SetURL(u)
	}})
	t.Cleanup(proxy.Close)
	e := &env{ts: proxy}
	s := connect(t, e, f.token(t, issuer.Claims{}))
	for range 6 {
		if res := call(t, s, "get_item", map[string]any{"id": "a"}); res.IsError {
			t.Fatalf("error: %s", textOf(t, res))
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// I8: the hooks get the principal from the context and the arguments.
func TestI8Hooks(t *testing.T) {
	f := newJWT(t)
	e := newEnv(t, f.v, nil)
	readTok := f.token(t, issuer.Claims{Scope: scopeRead, Subject: "reader"})
	res := rawPost(t, e.ts.URL+"/mcp", readTok, callBody("update_item", `{"id":"target-9","status":"open"}`), nil)
	if res.StatusCode != 403 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if len(e.rec.refused) != 1 || e.rec.ctxPrin[0].ID != "reader" || !strings.Contains(string(e.rec.refused[0].Arguments), "target-9") {
		t.Fatalf("refused %+v principals %+v", e.rec.refused, e.rec.ctxPrin)
	}
	s := connect(t, e, f.token(t, issuer.Claims{Subject: "writer"}))
	call(t, s, "update_item", map[string]any{"id": "target-9", "status": "bad"})
	if len(e.rec.invalid) != 1 || e.rec.ctxPrin[1].ID != "writer" || !strings.Contains(string(e.rec.invalid[0].Arguments), "target-9") {
		t.Fatalf("invalid %+v principals %+v", e.rec.invalid, e.rec.ctxPrin)
	}
}

// I9: no keys available gives 503 with Retry-After and the no-store headers.
func TestI9KeysUnavailable(t *testing.T) {
	f := newJWT(t)
	tok := f.token(t, issuer.Claims{})
	f.iss.Close()
	// A new verifier has no cached keys.
	v, err := authjwt.New(authjwt.Config{Issuer: f.iss.Issuer(), Audience: app, JWKSURL: f.iss.URL(), AllowInsecureHTTP: true})
	if err != nil {
		t.Skipf("the verifier needs the keys at start: %v", err)
	}
	t.Cleanup(v.Close)
	e := newEnv(t, v, nil)
	res := rawPost(t, e.ts.URL+"/mcp", tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if res.StatusCode != 503 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" {
		t.Errorf("headers %v", res.Header)
	}
}
