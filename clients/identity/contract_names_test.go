package identity_test

import (
	"testing"

	"github.com/techorionai/constellation-platform-go/clients/identity"
)

// Issue 18 U20: the generated client has the MFA operations. Issue 22 drops the
// check of the version, because it blocks each new copy of the contract.
func TestU20ContractV050(t *testing.T) {
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
