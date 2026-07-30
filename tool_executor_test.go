package agent

// Coverage for ValidateAgainstSchema / validateValue, the guard that stops
// malformed model-produced tool arguments from reaching user tool code.
//
// It sat at 50%: only the object and string branches were exercised, leaving
// number, integer, boolean, array, nested properties, and array `items`
// recursion entirely unverified. A bug in any of those means bad input reaches
// a user's Execute function.
//
// Table-driven, since these are many small independent cases.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAgainstSchemaScalarAndArrayTypes(t *testing.T) {
	cases := []struct {
		name    string
		schema  map[string]any
		input   string
		wantErr string // substring; empty means the input must validate
	}{
		// number: JSON numbers decode to float64, so an integral literal is a
		// valid number too.
		{"number accepts float", map[string]any{"type": "number"}, `1.5`, ""},
		{"number accepts integral", map[string]any{"type": "number"}, `2`, ""},
		{"number rejects string", map[string]any{"type": "number"}, `"1.5"`, "expected number"},
		{"number rejects bool", map[string]any{"type": "number"}, `true`, "expected number"},
		{"number rejects null", map[string]any{"type": "number"}, `null`, "expected number"},

		// integer is number plus a whole-value check.
		{"integer accepts whole", map[string]any{"type": "integer"}, `7`, ""},
		{"integer accepts negative whole", map[string]any{"type": "integer"}, `-7`, ""},
		{"integer rejects fractional", map[string]any{"type": "integer"}, `7.5`, "expected integer"},
		{"integer rejects string", map[string]any{"type": "integer"}, `"7"`, "expected integer"},

		{"boolean accepts true", map[string]any{"type": "boolean"}, `true`, ""},
		{"boolean accepts false", map[string]any{"type": "boolean"}, `false`, ""},
		{"boolean rejects string", map[string]any{"type": "boolean"}, `"true"`, "expected boolean"},
		{"boolean rejects number", map[string]any{"type": "boolean"}, `1`, "expected boolean"},

		{"string accepts", map[string]any{"type": "string"}, `"hi"`, ""},
		{"string rejects number", map[string]any{"type": "string"}, `5`, "expected string"},

		// array, with and without an items schema.
		{"array accepts untyped items", map[string]any{"type": "array"}, `[1,"a",true]`, ""},
		{"array accepts empty", map[string]any{"type": "array"}, `[]`, ""},
		{"array rejects object", map[string]any{"type": "array"}, `{"a":1}`, "expected array"},
		{
			"array validates items schema",
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			`["a","b"]`, "",
		},
		{
			"array reports bad item with index in path",
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			`["a",2]`, "$[1]: expected string",
		},

		// unknown/absent type is not enforced -- the schema support is
		// deliberately partial, with exotic cases left at the JSON boundary.
		{"missing type accepts anything", map[string]any{}, `{"whatever":1}`, ""},
		{"unknown type accepts anything", map[string]any{"type": "any"}, `"x"`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgainstSchema(json.RawMessage(tc.input), tc.schema)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %s to validate, got %v", tc.input, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAgainstSchemaNestedRecursion covers the recursive descent through
// object properties and array items, including the error path a caller sees.
func TestValidateAgainstSchemaNestedRecursion(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"user"},
		"properties": map[string]any{
			"user": map[string]any{
				"type":     "object",
				"required": []any{"name"},
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
					"age":  map[string]any{"type": "integer"},
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
		},
	}

	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"fully valid", `{"user":{"name":"ada","age":36,"tags":["x","y"]}}`, ""},
		{"optional fields may be absent", `{"user":{"name":"ada"}}`, ""},
		{"missing top-level required", `{}`, "$.user: required"},
		{"missing nested required", `{"user":{"age":36}}`, "$.user.name: required"},
		{"wrong nested scalar type", `{"user":{"name":5}}`, "$.user.name: expected string"},
		{"wrong nested integer", `{"user":{"name":"ada","age":1.5}}`, "$.user.age: expected integer"},
		{"bad item inside nested array", `{"user":{"name":"ada","tags":["ok",7]}}`, "$.user.tags[1]: expected string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgainstSchema(json.RawMessage(tc.input), schema)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %s to validate, got %v", tc.input, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAgainstSchemaEdgeCases pins the two short-circuit paths: an empty
// schema validates anything, and malformed JSON is a decode error rather than a
// silent pass.
func TestValidateAgainstSchemaEdgeCases(t *testing.T) {
	if err := ValidateAgainstSchema(json.RawMessage(`{"anything":true}`), nil); err != nil {
		t.Fatalf("nil schema should validate anything, got %v", err)
	}
	if err := ValidateAgainstSchema(json.RawMessage(`{"a":1}`), map[string]any{}); err != nil {
		t.Fatalf("empty schema should validate anything, got %v", err)
	}
	if err := ValidateAgainstSchema(json.RawMessage(`{not json`), map[string]any{"type": "object"}); err == nil {
		t.Fatal("malformed JSON must return a decode error, not validate silently")
	}
}
