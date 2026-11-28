package mcpserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/techorionai/constellation-platform-go/auth"
)

type fixedVerifier struct{ p auth.Principal }

func (f fixedVerifier) Verify(context.Context, string) (auth.Principal, error) { return f.p, nil }

// U4: the tool wrapper checks the scopes again. The test chain has no gate.
func TestU4WrapperChecksScopes(t *testing.T) {
	var refused []Refusal
	s, err := New(Config{
		App: "demo", Name: "demo", Verifier: fixedVerifier{auth.Principal{ID: "u", Scopes: []string{"demo:a:read"}}},
		ResourceURL: "https://demo.example/mcp", AuthorizationServers: []string{"https://id.example"},
		OnRefused: func(_ context.Context, r Refusal) { refused = append(refused, r) },
	})
	if err != nil {
		t.Fatal(err)
	}
	type in struct {
		ID string `json:"id"`
	}
	ran := false
	if err := AddTool(s, ToolSpec{Name: "w", Scopes: []string{"demo:a:read", "demo:a:write"}},
		func(context.Context, auth.Principal, in) (struct{}, error) { ran = true; return struct{}{}, nil }); err != nil {
		t.Fatal(err)
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	h := auth.Middleware(s.cfg.Verifier)(sdk)
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"w","arguments":{"id":"x1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer t")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(res.Body)
	if res.StatusCode != 200 || !strings.Contains(buf.String(), `"isError":true`) {
		t.Fatalf("status %d body %s", res.StatusCode, buf.String())
	}
	if ran {
		t.Error("handler ran")
	}
	if len(refused) != 1 || refused[0].Tool != "w" || len(refused[0].Missing) != 1 || refused[0].Missing[0] != "demo:a:write" ||
		!strings.Contains(string(refused[0].Arguments), "x1") {
		t.Errorf("refusals %+v", refused)
	}
}
