package pluginruntime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// validateSchema intentionally implements the stable object subset accepted by
// axiom.plugin/v1. Full JSON Schema is not exposed as a promise by this API.
func validateSchema(raw, schemaRaw json.RawMessage) error {
	var schema struct {
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return errors.New("invalid schema")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("invalid JSON")
	}
	if schema.Type != "object" {
		return fmt.Errorf("unsupported root schema type %q", schema.Type)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("expected object")
	}
	for _, name := range schema.Required {
		if _, exists := object[name]; !exists {
			return fmt.Errorf("missing required property %q", name)
		}
	}
	for name, item := range object {
		propertySchema, declared := schema.Properties[name]
		if !declared {
			if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
				return fmt.Errorf("unknown property %q", name)
			}
			continue
		}
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(propertySchema, &property) != nil || !matchesType(item, property.Type) {
			return fmt.Errorf("property %q must be %s", name, property.Type)
		}
	}
	return nil
}

func matchesType(value any, expected string) bool {
	switch expected {
	case "":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}
