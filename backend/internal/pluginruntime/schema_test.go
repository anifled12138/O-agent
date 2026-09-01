package pluginruntime

import (
	"encoding/json"
	"testing"
)

func TestValidateSchemaContract(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["count"],"properties":{"count":{"type":"integer"},"items":{"type":"array"}},"additionalProperties":false}`)
	if err := validateSchema(json.RawMessage(`{"count":2,"items":[]}`), schema); err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	for _, raw := range []string{`{"items":[]}`, `{"count":"two"}`, `{"count":2,"extra":true}`} {
		if validateSchema(json.RawMessage(raw), schema) == nil {
			t.Fatalf("invalid output accepted: %s", raw)
		}
	}
}
