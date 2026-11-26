package identity_test

import (
	"os"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/clients/identity"
)

// Issue 18 U20: the contract is v0.5.0 and the generated client has the MFA operations.
func TestU20ContractV050(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi/CONTRACTS_VERSION")
	if err != nil || strings.TrimSpace(string(raw)) != "v0.5.0" {
		t.Fatalf("CONTRACTS_VERSION = %q, %v", raw, err)
	}
	// The code compiles only if the generated names exist.
	_ = (*identity.ClientWithResponses).MeMfaGetWithResponse
	_ = (*identity.ClientWithResponses).MeTotpRemoveWithResponse
	_ = (*identity.ClientWithResponses).UserMfaGetWithResponse
	_ = (*identity.ClientWithResponses).UserMfaResetWithResponse
	var st identity.MfaStatus
	_ = st
	var rm identity.MeTotpRemoveResponse
	_ = rm.ApplicationproblemJSON401
	_ = rm.Headers401
}
