package identity_test

import (
	"os"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/clients/identity"
)

// Issue 17 U28 (and issue 15 U29): the generated client has the introspection operation and the
// open union IntrospectTokenTypeHint. TokenTypeHint has the values of v0.2.0.
func TestU28ContractV040(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi/CONTRACTS_VERSION")
	if err != nil || strings.TrimSpace(string(raw)) != "v0.4.0" {
		t.Fatalf("CONTRACTS_VERSION = %q, %v", raw, err)
	}
	// The code compiles only if the generated names exist.
	_ = (*identity.ClientWithResponses).OAuthIntrospectWithFormdataBodyWithResponse
	// v0.4.0: the publish operation has the 429 and 503 responses.
	var pub identity.PermissionManifestsPublishResponse
	_, _ = pub.ApplicationproblemJSON429, pub.ApplicationproblemJSON503
	_ = pub.Headers429
	var req identity.IntrospectRequest
	var hint identity.IntrospectTokenTypeHint
	if err := hint.FromIntrospectTokenTypeHint0(identity.IntrospectTokenTypeHint0ApiToken); err != nil {
		t.Fatal(err)
	}
	req.TokenTypeHint = &hint
	b, err := hint.MarshalJSON()
	if err != nil || string(b) != `"api_token"` {
		t.Fatalf("hint = %s, %v", b, err)
	}
	if identity.IntrospectTokenTypeHint0RefreshToken != "refresh_token" ||
		identity.IntrospectTokenTypeHint0AccessToken != "access_token" {
		t.Error("IntrospectTokenTypeHint values")
	}
	// TokenTypeHint keeps its two values, and RevokeRequest uses it.
	var rev identity.RevokeRequest
	var th identity.TokenTypeHint
	if err := th.FromTokenTypeHint0(identity.TokenTypeHint0RefreshToken); err != nil {
		t.Fatal(err)
	}
	rev.TokenTypeHint = &th
	if identity.TokenTypeHint0AccessToken != "access_token" || identity.TokenTypeHint0RefreshToken != "refresh_token" {
		t.Error("TokenTypeHint values")
	}
	if !strings.Contains(string(mustRead(t, "identity.gen.go")), "type TokenTypeHint0 string") {
		t.Error("TokenTypeHint0")
	}
	if strings.Contains(string(mustRead(t, "identity.gen.go")), "TokenTypeHint0ApiToken") &&
		!strings.Contains(string(mustRead(t, "identity.gen.go")), "IntrospectTokenTypeHint0ApiToken") {
		t.Error("api_token is in the wrong union")
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name) //nolint:gosec // fixed file names in the test
	if err != nil {
		t.Fatal(err)
	}
	return b
}
