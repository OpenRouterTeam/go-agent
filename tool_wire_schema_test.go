package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Ported from upstream tests/unit/manual-tool-wire-schema.test.ts (#118) and
// the strict pass-through cases from #99.

type choiceInput struct {
	Choice string `json:"choice"`
}

func TestManualToolWireInputSchemaSerializesAnyOfOneOfAndSanitizes(t *testing.T) {
	strict := true
	wire := map[string]any{
		"type":          "object",
		"~rootMetadata": "remove me",
		"properties": map[string]any{
			"choice": map[string]any{
				"~propertyMetadata": "remove me",
				"anyOf":             []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}},
				"oneOf":             []any{map[string]any{"const": "first"}, map[string]any{"const": "second"}},
			},
		},
		"required": []any{"choice"},
	}
	tool := MustNewTool(ToolConfig[choiceInput]{Name: "choose_value", Description: "Choose a value", Manual: true, WireInputSchema: wire, Strict: &strict})
	fn := tool.ToAPITool().ResponsesRequestToolFunction
	if fn == nil || fn.Name != "choose_value" {
		t.Fatalf("expected a function tool, got %#v", tool.ToAPITool())
	}
	if desc, _ := fn.Description.GetOrZero(); desc != "Choose a value" {
		t.Fatalf("description = %q", desc)
	}
	if s, ok := fn.Strict.GetOrZero(); !ok || !s {
		t.Fatalf("strict must pass through as true, got %v (set=%v)", s, ok)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{
				"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}},
				"oneOf": []any{map[string]any{"const": "first"}, map[string]any{"const": "second"}},
			},
		},
		"required": []any{"choice"},
	}
	if !reflect.DeepEqual(fn.Parameters, want) {
		t.Fatalf("parameters = %#v\nwant %#v", fn.Parameters, want)
	}
}

func TestManualToolWireInputSchemaDoesNotMutateCallerSchema(t *testing.T) {
	wire := map[string]any{
		"type":          "object",
		"~rootMetadata": true,
		"properties":    map[string]any{"value": map[string]any{"type": "string", "~nestedMetadata": true}},
	}
	before, _ := json.Marshal(wire)
	tool := MustNewTool(ToolConfig[struct {
		Value string `json:"value"`
	}]{Name: "copy_schema", Manual: true, WireInputSchema: wire})
	params := tool.ToAPITool().ResponsesRequestToolFunction.Parameters
	after, _ := json.Marshal(wire)
	if string(before) != string(after) {
		t.Fatalf("caller-owned schema was mutated: %s -> %s", before, after)
	}
	params["type"] = "mutated"
	if wire["type"] != "object" {
		t.Fatal("emitted parameters must be a copy, not the caller's map")
	}
}

func TestManualToolWithoutWireInputSchemaFallsBackToGeneratedSchema(t *testing.T) {
	tool := MustNewTool(ToolConfig[struct {
		Value string `json:"value"`
	}]{Name: "fallback_schema", Manual: true})
	params := tool.ToAPITool().ResponsesRequestToolFunction.Parameters
	props, _ := params["properties"].(map[string]any)
	value, _ := props["value"].(map[string]any)
	if params["type"] != "object" || value["type"] != "string" {
		t.Fatalf("expected generated object schema with value:string, got %#v", params)
	}
}

func TestWireInputSchemaRejectedOnExecutableTool(t *testing.T) {
	_, err := NewTool(ToolConfig[choiceInput]{Name: "exec", WireInputSchema: map[string]any{"type": "object"}, Execute: func(context.Context, choiceInput, ToolExecuteContext) (any, error) {
		return nil, nil
	}})
	if !errors.Is(err, ErrWireInputSchemaNotManual) {
		t.Fatalf("err = %v, want ErrWireInputSchemaNotManual", err)
	}
}

// #99: strict is passed through rather than hardcoded. Unset serializes as
// explicit null (upstream `strict ?? null`); false stays false.
func TestFunctionToolStrictPassThrough(t *testing.T) {
	exec := func(context.Context, choiceInput, ToolExecuteContext) (any, error) { return nil, nil }
	unset := MustNewTool(ToolConfig[choiceInput]{Name: "a", Execute: exec}).ToAPITool()
	b, err := json.Marshal(unset)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if v, present := wire["strict"]; !present || v != nil {
		t.Fatalf("unset strict must serialize as null, got %v (present=%v) in %s", v, present, b)
	}
	f := false
	off := MustNewTool(ToolConfig[choiceInput]{Name: "b", Execute: exec, Strict: &f}).ToAPITool()
	if s, ok := off.ResponsesRequestToolFunction.Strict.GetOrZero(); !ok || s {
		t.Fatalf("strict=false must pass through, got %v set=%v", s, ok)
	}
}
