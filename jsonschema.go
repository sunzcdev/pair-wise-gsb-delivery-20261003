package toml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const jsonSchema202012 = "https://json-schema.org/draft/2020-12/schema"

type jsonSchema struct {
	Schema     string                 `json:"$schema,omitempty"`
	Type       string                 `json:"type,omitempty"`
	Properties map[string]*jsonSchema `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
	Items      *jsonSchema            `json:"items,omitempty"`
	AnyOf      []*jsonSchema          `json:"anyOf,omitempty"`
	TOMLType   string                 `json:"x-toml-type,omitempty"`
}

// InferJSONSchema derives a small JSON Schema 2020-12 subset from complete TOML
// examples. The result describes the structure represented by the examples; it
// deliberately does not infer business constraints such as bounds, patterns,
// defaults, or completeness.
func InferJSONSchema(samples ...[]byte) ([]byte, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("infer JSON Schema: at least one TOML sample is required")
	}

	var root *jsonSchema
	for i, sample := range samples {
		var value map[string]interface{}
		if err := Unmarshal(sample, &value); err != nil {
			return nil, fmt.Errorf("infer JSON Schema from TOML sample %d: %w", i+1, err)
		}

		candidate, err := inferJSONSchemaValue(value)
		if err != nil {
			return nil, fmt.Errorf("infer JSON Schema from TOML sample %d: %w", i+1, err)
		}
		root = mergeJSONSchema(root, candidate)
	}

	root.Schema = jsonSchema202012
	return json.MarshalIndent(root, "", "  ")
}

func inferJSONSchemaValue(value interface{}) (*jsonSchema, error) {
	switch value := value.(type) {
	case map[string]interface{}:
		schema := &jsonSchema{
			Type:       "object",
			Properties: make(map[string]*jsonSchema, len(value)),
			Required:   make([]string, 0, len(value)),
		}
		for key, child := range value {
			childSchema, err := inferJSONSchemaValue(child)
			if err != nil {
				return nil, err
			}
			schema.Properties[key] = childSchema
			schema.Required = append(schema.Required, key)
		}
		sort.Strings(schema.Required)
		return schema, nil
	case []interface{}:
		schema := &jsonSchema{Type: "array"}
		var itemSchema *jsonSchema
		for _, item := range value {
			candidate, err := inferJSONSchemaValue(item)
			if err != nil {
				return nil, err
			}
			itemSchema = mergeJSONSchema(itemSchema, candidate)
		}
		schema.Items = itemSchema
		return schema, nil
	case string:
		return scalarJSONSchema("string", ""), nil
	case int64:
		return scalarJSONSchema("integer", ""), nil
	case float64:
		return scalarJSONSchema("number", ""), nil
	case bool:
		return scalarJSONSchema("boolean", ""), nil
	case LocalDate:
		return scalarJSONSchema("string", "local-date"), nil
	case LocalTime:
		return scalarJSONSchema("string", "local-time"), nil
	case LocalDateTime:
		return scalarJSONSchema("string", "local-date-time"), nil
	case time.Time:
		return scalarJSONSchema("string", "offset-date-time"), nil
	default:
		return nil, fmt.Errorf("unsupported TOML value type %T", value)
	}
}

func scalarJSONSchema(schemaType string, tomlType string) *jsonSchema {
	return &jsonSchema{Type: schemaType, TOMLType: tomlType}
}

func mergeJSONSchema(left *jsonSchema, right *jsonSchema) *jsonSchema {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	case len(left.AnyOf) > 0 || len(right.AnyOf) > 0:
		leftOptions := jsonSchemaAlternatives(left)
		rightOptions := jsonSchemaAlternatives(right)
		options := make([]*jsonSchema, 0, len(leftOptions)+len(rightOptions))
		options = append(options, leftOptions...)
		options = append(options, rightOptions...)
		return unionJSONSchema(options...)
	case sameJSONSchemaKind(left, right):
		return mergeSameKindJSONSchema(left, right)
	default:
		return unionJSONSchema(left, right)
	}
}

func mergeSameKindJSONSchema(left *jsonSchema, right *jsonSchema) *jsonSchema {
	switch left.Type {
	case "object":
		merged := &jsonSchema{
			Type:       "object",
			Properties: make(map[string]*jsonSchema, len(left.Properties)+len(right.Properties)),
			Required:   intersectRequired(left.Required, right.Required),
		}
		for key, schema := range left.Properties {
			merged.Properties[key] = schema
		}
		for key, schema := range right.Properties {
			if existing, ok := merged.Properties[key]; ok {
				merged.Properties[key] = mergeJSONSchema(existing, schema)
			} else {
				merged.Properties[key] = schema
			}
		}
		return merged
	case "array":
		merged := &jsonSchema{Type: "array"}
		switch {
		case left.Items == nil:
			merged.Items = right.Items
		case right.Items == nil:
			merged.Items = left.Items
		default:
			merged.Items = mergeJSONSchema(left.Items, right.Items)
		}
		return merged
	default:
		return left
	}
}

func sameJSONSchemaKind(left *jsonSchema, right *jsonSchema) bool {
	if left.Type != right.Type {
		return false
	}
	return left.TOMLType == right.TOMLType
}

func jsonSchemaAlternatives(schema *jsonSchema) []*jsonSchema {
	if len(schema.AnyOf) > 0 {
		return schema.AnyOf
	}
	return []*jsonSchema{schema}
}

func unionJSONSchema(options ...*jsonSchema) *jsonSchema {
	flattened := make([]*jsonSchema, 0, len(options))
	for _, option := range options {
		flattened = append(flattened, jsonSchemaAlternatives(option)...)
	}

	merged := make([]*jsonSchema, 0, len(flattened))
	for _, option := range flattened {
		matched := false
		for i, existing := range merged {
			if sameJSONSchemaKind(existing, option) {
				merged[i] = mergeSameKindJSONSchema(existing, option)
				matched = true
				break
			}
		}
		if !matched {
			merged = append(merged, option)
		}
	}

	sortJSONSchemaAlternatives(merged)
	if len(merged) == 1 {
		return merged[0]
	}
	return &jsonSchema{AnyOf: merged}
}

func sortJSONSchemaAlternatives(options []*jsonSchema) {
	sort.Slice(options, func(i int, j int) bool {
		left, _ := json.Marshal(options[i])
		right, _ := json.Marshal(options[j])
		return bytes.Compare(left, right) < 0
	})
}

func intersectRequired(left []string, right []string) []string {
	rightKeys := make(map[string]struct{}, len(right))
	for _, key := range right {
		rightKeys[key] = struct{}{}
	}

	required := make([]string, 0, len(left))
	for _, key := range left {
		if _, ok := rightKeys[key]; ok {
			required = append(required, key)
		}
	}
	sort.Strings(required)
	return required
}
