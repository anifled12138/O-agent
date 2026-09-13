package capability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

type schemaNode struct {
	Type                 any                   `json:"type"`
	Required             []string              `json:"required"`
	Properties           map[string]schemaNode `json:"properties"`
	Items                *schemaNode           `json:"items"`
	Enum                 []any                 `json:"enum"`
	AdditionalProperties *bool                 `json:"additionalProperties"`
}

func ValidateSchema(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 32<<10 || !json.Valid(raw) {
		return errors.New("schema must be valid JSON no larger than 32 KiB")
	}
	var root schemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if !acceptsType(root.Type, "object") {
		return errors.New("root schema must declare type object")
	}
	return validateSchemaNode(root, 0)
}

func validateSchemaNode(node schemaNode, depth int) error {
	if depth > 12 {
		return errors.New("schema nesting exceeds 12 levels")
	}
	for _, kind := range schemaTypes(node.Type) {
		switch kind {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("unsupported schema type %q", kind)
		}
	}
	for name, child := range node.Properties {
		if name == "" {
			return errors.New("schema property name cannot be empty")
		}
		if err := validateSchemaNode(child, depth+1); err != nil {
			return fmt.Errorf("property %s: %w", name, err)
		}
	}
	if node.Items != nil {
		if err := validateSchemaNode(*node.Items, depth+1); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	}
	return nil
}

func ValidateValue(raw json.RawMessage, schema json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("value is not valid JSON")
	}
	var node schemaNode
	if err := json.Unmarshal(schema, &node); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return validateValueAt(value, node, "$", 0)
}

func validateValueAt(value any, node schemaNode, path string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s exceeds validation depth", path)
	}
	if len(node.Enum) > 0 {
		matched := false
		actual, _ := json.Marshal(value)
		for _, candidate := range node.Enum {
			expected, _ := json.Marshal(candidate)
			if bytes.Equal(actual, expected) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not one of the allowed values", path)
		}
	}
	if !valueMatchesTypes(value, schemaTypes(node.Type)) {
		return fmt.Errorf("%s has the wrong JSON type", path)
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, required := range node.Required {
			if _, ok := typed[required]; !ok {
				return fmt.Errorf("%s.%s is required", path, required)
			}
		}
		for key, childValue := range typed {
			child, known := node.Properties[key]
			if !known {
				if node.AdditionalProperties != nil && !*node.AdditionalProperties {
					return fmt.Errorf("%s.%s is not allowed", path, key)
				}
				continue
			}
			if err := validateValueAt(childValue, child, path+"."+key, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if node.Items != nil {
			for index, item := range typed {
				if err := validateValueAt(item, *node.Items, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func schemaTypes(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			return []string{typed}
		}
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	}
	return nil
}

func acceptsType(value any, expected string) bool {
	for _, kind := range schemaTypes(value) {
		if kind == expected {
			return true
		}
	}
	return false
}

func valueMatchesTypes(value any, types []string) bool {
	if len(types) == 0 {
		return true
	}
	for _, kind := range types {
		switch kind {
		case "object":
			_, ok := value.(map[string]any)
			if ok {
				return true
			}
		case "array":
			_, ok := value.([]any)
			if ok {
				return true
			}
		case "string":
			_, ok := value.(string)
			if ok {
				return true
			}
		case "number":
			_, ok := value.(json.Number)
			if ok {
				return true
			}
		case "integer":
			if number, ok := value.(json.Number); ok {
				parsed, err := number.Float64()
				if err == nil && parsed == math.Trunc(parsed) {
					return true
				}
			}
		case "boolean":
			_, ok := value.(bool)
			if ok {
				return true
			}
		case "null":
			if value == nil {
				return true
			}
		}
	}
	return false
}
