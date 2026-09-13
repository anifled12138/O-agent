package capability

import (
	"encoding/json"
	"testing"
)

func TestValidateValueUsesCapabilityContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array","items":{"type":"integer"}}},"additionalProperties":false}`)
	if err := ValidateSchema(schema); err != nil {
		t.Fatal(err)
	}
	if err := ValidateValue(json.RawMessage(`{"items":[1,2,3]}`), schema); err != nil {
		t.Fatal(err)
	}
	if err := ValidateValue(json.RawMessage(`{"items":[1.5]}`), schema); err == nil {
		t.Fatal("fractional value passed integer validation")
	}
	if err := ValidateValue(json.RawMessage(`{"items":[],"extra":true}`), schema); err == nil {
		t.Fatal("unknown property passed closed schema")
	}
}

func TestContractRejectsPermissionsForScriptTiers(t *testing.T) {
	contract := Contract{
		APIVersion: APIVersion, ID: "fragment.test", Name: "Test", Summary: "Test fragment", Intent: "Transform input",
		Tier: TierFragment, Scope: ScopeTurn, Runtime: RuntimeJavaScript, Permissions: Permissions{Network: true},
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Fallback: "Return control to the agent",
	}
	contract.Normalize()
	if err := contract.Validate(); err == nil {
		t.Fatal("script fragment accepted host permissions")
	}
}
