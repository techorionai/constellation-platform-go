package mcpserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/mcpserver"
)

const (
	app        = "demo"
	scopeRead  = "demo:item:read"
	scopeWrite = "demo:item:write"
)

type getIn struct {
	ID string `json:"id" jsonschema:"item ID"`
}

type item struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Actor  string `json:"actor,omitempty"`
}

type updateIn struct {
	ID     string `json:"id" jsonschema:"item ID"`
	Status string `json:"status" jsonschema:"new status"`
}

// recorder keeps the data of the hooks.
type recorder struct {
	mu      sync.Mutex
	refused []mcpserver.Refusal
	invalid []mcpserver.Refusal
	ctxPrin []auth.Principal
}

func (r *recorder) onRefused(ctx context.Context, f mcpserver.Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = append(r.refused, f)
	p, _ := auth.PrincipalFromContext(ctx)
	r.ctxPrin = append(r.ctxPrin, p)
}

func (r *recorder) onInvalid(ctx context.Context, f mcpserver.Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalid = append(r.invalid, f)
	p, _ := auth.PrincipalFromContext(ctx)
	r.ctxPrin = append(r.ctxPrin, p)
}

type env struct {
	srv    *mcpserver.Server
	ts     *httptest.Server
	rec    *recorder
	reader *sdkmetric.ManualReader
	// handlerCalls counts the calls of the update handler.
	handlerCalls *int
}

// updateSchema is a given input schema with an enum and a maximum length.
func updateSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	s, err := jsonschema.For[updateIn](nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Properties["status"].Enum = []any{"open", "closed"}
	s.Properties["id"].MaxLength = jsonschema.Ptr(16)
	return s
}

// newEnv makes a server with a read tool and a write tool behind an HTTP test
// server. mod can change the config before the server starts.
func newEnv(t *testing.T, v auth.Verifier, mod func(*mcpserver.Config)) *env {
	t.Helper()
	rec := &recorder{}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	e := &env{rec: rec, reader: reader, handlerCalls: new(int)}
	cfg := mcpserver.Config{
		App: app, Name: "demo", Version: "1.0.0", Verifier: v,
		AuthorizationServers: []string{"https://identity.example"},
		OnRefused:            rec.onRefused, OnInvalid: rec.onInvalid,
		MeterProvider: mp,
	}
	// The resource URL needs the address of the test server. The handler reads
	// the URL after the server starts, so a first server with a placeholder is
	// not possible. Start the listener first.
	mux := http.NewServeMux()
	e.ts = httptest.NewServer(mux)
	t.Cleanup(e.ts.Close)
	cfg.ResourceURL = e.ts.URL + "/mcp"
	if mod != nil {
		mod(&cfg)
	}
	srv, err := mcpserver.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	registerTools(t, e)
	mux.Handle("/mcp", srv.Handler())
	mux.Handle("/.well-known/oauth-protected-resource/mcp", srv.MetadataHandler())
	return e
}

func registerTools(t *testing.T, e *env) {
	t.Helper()
	err := mcpserver.AddTool(e.srv, mcpserver.ToolSpec{
		Name: "get_item", Title: "Get item", Description: "Get one item.",
		Scopes: []string{scopeRead}, ReadOnly: true, Idempotent: true,
	}, func(_ context.Context, p auth.Principal, in getIn) (item, error) {
		actor := ""
		if p.Act != nil {
			actor = p.Act.ID
		}
		return item{ID: in.ID, Status: "open", Actor: actor}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = mcpserver.AddTool(e.srv, mcpserver.ToolSpec{
		Name: "update_item", Title: "Update item", Description: "Update one item.",
		Scopes: []string{scopeRead, scopeWrite}, InputSchema: updateSchema(t),
	}, func(_ context.Context, _ auth.Principal, in updateIn) (item, error) {
		*e.handlerCalls++
		switch in.ID {
		case "dup":
			return item{}, &mcpserver.ToolError{Code: "item_name_taken", Message: "the name is in use", Fields: []mcpserver.FieldError{
				{Field: "/id", Code: "taken", Message: "the ID is in use"},
				{Field: "/status", Code: "bad", Message: "the status is not allowed"},
			}}
		case "boom":
			return item{}, errors.New("database password is hunter2")
		}
		return item{ID: in.ID, Status: in.Status}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, e *env, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: e.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func call(t *testing.T, s *mcp.ClientSession, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content items: %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", res.Content[0])
	}
	return tc.Text
}

// verifierFor returns a verifier with fixed tokens.
func principals(m map[string]auth.Principal) auth.Verifier {
	return verifierFunc(func(_ context.Context, token string) (auth.Principal, error) {
		if p, ok := m[token]; ok {
			return p, nil
		}
		return auth.Principal{}, auth.ErrInvalidToken
	})
}

type verifierFunc func(context.Context, string) (auth.Principal, error)

func (f verifierFunc) Verify(ctx context.Context, token string) (auth.Principal, error) {
	return f(ctx, token)
}

var testPrincipals = map[string]auth.Principal{
	"all":  {ID: "u1", Type: auth.PrincipalUser, Scopes: []string{scopeRead, scopeWrite}, Audience: app},
	"read": {ID: "u2", Type: auth.PrincipalUser, Scopes: []string{scopeRead}, Audience: app},
	"agent": {ID: "agent1", Type: auth.PrincipalUser, Scopes: []string{scopeRead, scopeWrite}, Audience: app,
		Act: &auth.Actor{ID: "agent-7", Type: auth.PrincipalUser}},
}
