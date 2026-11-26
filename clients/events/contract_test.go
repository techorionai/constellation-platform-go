package events_test

import (
	"os"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/clients/events"
)

// Issue 22 U14: the contract is v0.8.0 and the generated client has the events operations.
func TestU14ContractV080(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi/CONTRACTS_VERSION")
	if err != nil || strings.TrimSpace(string(raw)) != "v0.8.0" {
		t.Fatalf("CONTRACTS_VERSION = %q, %v", raw, err)
	}
	// The code compiles only if the generated names exist.
	_ = (*events.ClientWithResponses).EventTypeRegistrationsListWithResponse
	_ = (*events.ClientWithResponses).EventTypeRegistrationsRegisterWithResponse
	_ = (*events.ClientWithResponses).EventTypeRegistrationsGetWithResponse
	_ = (*events.ClientWithResponses).EventTypesListWithResponse
	_ = (*events.ClientWithResponses).EventTypesGetWithResponse
	var res events.EventTypeRegistrationResult
	_ = res
	for _, f := range []string{"Constellation.Identity.openapi.yaml", "Constellation.Events.openapi.yaml"} {
		if _, err := os.Stat("../../api/openapi/" + f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
