package agent

// Ports tests/unit/manual-tool-wire-schema.test.ts and the strict cases of
// tests/unit/server-tool.test.ts (convertToolsToAPIFormat), plus MarkMcp's
// WireInputSchema substitution.

import (
	"context"
	"strings"
	"testing"
)

type mtwChoice struct {
	Choice string `json:"choice"`
}

func mtwWireSchema() map[string]any {
	return map[string]any{
		"type":           "object",
		"~rootMetadata":  "remove me",
		"additionalInfo": map[string]any{"~nested": true, "keep": 1},
		"properties": map[string]any{
			"choice": map[string]any{
				"~propertyMetadata": "remove me",
				"anyOf":             []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}},
				"oneOf":             []any{map[string]any{"const": "first"}, map[string]any{"const": "second", "~x": 1}},
			},
		},
		"required": []any{"choice"},
	}
}

const mtwWireSchemaSanitized = `{"additionalInfo":{"keep":1},"properties":{"choice":{"anyOf":[{"type":"string"},{"type":"number"}],"oneOf":[{"const":"first"},{"const":"second"}]}},"required":["choice"],"type":"object"}`

func mtwBool(v bool) *bool { return &v }

// upstream: "serializes anyOf and oneOf while sanitizing tilde-prefixed keys"
func TestManualToolWireInputSchemaSerializedVerbatimSanitized(t *testing.T) {
	tool := MustNewTool(ToolConfig[mtwChoice]{
		Name: "choose_value", Description: "Choose a value",
		InputSchema:     phObjectSchema("choice"),
		WireInputSchema: mtwWireSchema(),
		Strict:          mtwBool(true),
	})
	if !IsManualTool(tool) {
		t.Fatal("fixture must be a manual tool")
	}
	wire := phJSONMap(t, tool.ToAPITool())
	if wire["type"] != "function" || wire["name"] != "choose_value" || wire["description"] != "Choose a value" || wire["strict"] != true {
		t.Fatalf("wire tool = %v", wire)
	}
	if got := phCanonicalJSON(t, phJSON(t, wire["parameters"])); got != mtwWireSchemaSanitized {
		t.Fatalf("parameters = %s\nwant       %s", got, mtwWireSchemaSanitized)
	}
	// The derived schema still drives local metadata, not the wire one.
	if got := phCanonicalJSON(t, phJSON(t, tool.InputSchema())); got != phCanonicalJSON(t, phJSON(t, phObjectSchema("choice"))) {
		t.Fatalf("InputSchema must stay the derived schema, got %s", got)
	}
}

// upstream: "does not mutate the caller-owned schema and emits a copy"
func TestManualToolWireInputSchemaNotMutatedAndCopied(t *testing.T) {
	schema := mtwWireSchema()
	before := phJSON(t, schema)
	tool := MustNewTool(ToolConfig[mtwChoice]{Name: "copy_schema", WireInputSchema: schema})
	first := tool.ToAPITool()
	if phJSON(t, schema) != before {
		t.Fatal("the caller-owned schema must not be mutated")
	}
	// Mutating the emitted parameters must not leak into the next emission.
	first.ResponsesRequestToolFunction.Parameters["injected"] = true
	if props, ok := first.ResponsesRequestToolFunction.Parameters["properties"].(map[string]any); ok {
		props["choice"] = "clobbered"
	}
	second := phJSONMap(t, tool.ToAPITool())
	if got := phCanonicalJSON(t, phJSON(t, second["parameters"])); got != mtwWireSchemaSanitized {
		t.Fatalf("each emission must be an independent copy, got %s", got)
	}
	if phJSON(t, schema) != before {
		t.Fatal("mutating an emission must not reach the caller-owned schema")
	}
}

// upstream: "falls back to the Zod-derived schema without wireInputSchema";
// a WireInputSchema whose root is not an object is ignored the same way.
func TestManualToolFallsBackToDerivedSchema(t *testing.T) {
	derived := phCanonicalJSON(t, phJSON(t, phObjectSchema("value")))
	for name, wireSchema := range map[string]map[string]any{
		"no wire schema":    nil,
		"array root":        {"type": "array", "items": map[string]any{"type": "string"}},
		"untyped root":      {"properties": map[string]any{"x": map[string]any{}}},
		"union-typed root":  {"type": []any{"object", "null"}},
		"empty wire schema": {},
	} {
		tool := MustNewTool(ToolConfig[struct {
			Value string `json:"value"`
		}]{Name: "fallback_schema", InputSchema: phObjectSchema("value"), WireInputSchema: wireSchema})
		wire := phJSONMap(t, tool.ToAPITool())
		if got := phCanonicalJSON(t, phJSON(t, wire["parameters"])); got != derived {
			t.Fatalf("%s: parameters = %s, want derived %s", name, got, derived)
		}
	}
}

// WireInputSchema is only valid on manual tools.
func TestWireInputSchemaRejectedOnExecutableTools(t *testing.T) {
	wire := map[string]any{"type": "object"}
	cases := map[string]ToolConfig[mtwChoice]{
		"execute": {Name: "e", WireInputSchema: wire, Execute: func(context.Context, mtwChoice, ToolExecuteContext) (any, error) { return nil, nil }},
		"generate": {Name: "g", WireInputSchema: wire, Generate: func(context.Context, mtwChoice, ToolExecuteContext, func(any) error) (any, error) {
			return nil, nil
		}},
		"onToolCalled": {Name: "h", WireInputSchema: wire, OnToolCalled: func(context.Context, mtwChoice, ToolExecuteContext) (any, bool, error) { return nil, false, nil }},
	}
	for name, cfg := range cases {
		if _, err := NewTool(cfg); err == nil || !strings.Contains(err.Error(), "WireInputSchema is only supported on manual tools") {
			t.Fatalf("%s: want a manual-only error, got %v", name, err)
		}
	}
	if _, err := NewTool(ToolConfig[mtwChoice]{Name: "m", WireInputSchema: wire, Manual: true}); err != nil {
		t.Fatalf("an explicit manual tool accepts WireInputSchema: %v", err)
	}
}

// server-tool.test.ts: "serializes strict: true from the tool definition",
// "serializes strict: false as false instead of null", "defaults strict to
// null when not declared" (Go omits the field: provider default).
func TestStrictPassthrough(t *testing.T) {
	mk := func(strict *bool) Tool {
		return MustNewTool(ToolConfig[mtwChoice]{
			Name: "echo", InputSchema: phObjectSchema("choice"), Strict: strict,
			Execute: func(_ context.Context, in mtwChoice, _ ToolExecuteContext) (any, error) { return in.Choice, nil },
		})
	}
	if v, ok := phJSONMap(t, mk(mtwBool(true)).ToAPITool())["strict"]; !ok || v != true {
		t.Fatalf("strict true -> %v (%v)", v, ok)
	}
	if v, ok := phJSONMap(t, mk(mtwBool(false)).ToAPITool())["strict"]; !ok || v != false {
		t.Fatalf("strict false must serialize as false, got %v (present=%v)", v, ok)
	}
	if v, ok := phJSONMap(t, mk(nil).ToAPITool())["strict"]; ok {
		t.Fatalf("an undeclared strict must be omitted (provider default), got %v", v)
	}
	// Strict reaches the dispatched request.
	sender := phSender(phTextResp("r", "ok"))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{mk(mtwBool(false))}}); err != nil {
		t.Fatal(err)
	}
	if fn := sender.requests[0].Tools[0].ResponsesRequestToolFunction; fn == nil || phJSONMap(t, sender.requests[0].Tools[0])["strict"] != false {
		t.Fatalf("dispatched tool = %s", phJSON(t, sender.requests[0].Tools[0]))
	}
}

// MarkMcp(t, McpMarkOptions{WireInputSchema}) substitutes the MCP server's
// schema on the wire while the derived schema keeps validating calls.
func TestMarkMcpWireInputSchemaSubstitutesParameters(t *testing.T) {
	ctx := context.Background()
	var rec phCounter
	base := phOKTool("remote_lookup", &rec, func(c *ToolConfig[phTargetInput]) { c.Strict = mtwBool(true) })
	marked := MarkMcp(base, McpMarkOptions{WireInputSchema: mtwWireSchema()})
	if !IsMcpTool(marked) || IsMcpTool(base) {
		t.Fatal("only the marked tool is MCP-branded")
	}
	wire := phJSONMap(t, marked.ToAPITool())
	if got := phCanonicalJSON(t, phJSON(t, wire["parameters"])); got != mtwWireSchemaSanitized {
		t.Fatalf("MCP wire parameters = %s", got)
	}
	if wire["name"] != "remote_lookup" || wire["strict"] != true {
		t.Fatalf("other function fields must be kept: %v", wire)
	}
	if got := phCanonicalJSON(t, phJSON(t, phJSONMap(t, base.ToAPITool())["parameters"])); got != phCanonicalJSON(t, phJSON(t, phObjectSchema("target"))) {
		t.Fatalf("marking must not change the unmarked tool's wire schema: %s", got)
	}
	// Execution still validates against the tool's own schema.
	res, err := ExecuteTool(ctx, marked, ParsedToolCall{ID: "c", CallID: "c", Name: "remote_lookup", Arguments: map[string]any{}}, ToolExecuteContext{})
	if err != nil || res.Error == nil || rec.count() != 0 {
		t.Fatalf("invalid args must fail the tool's own validation: res=%+v err=%v ran=%d", res, err, rec.count())
	}
	res, err = ExecuteTool(ctx, marked, ParsedToolCall{ID: "c", CallID: "c", Name: "remote_lookup", Arguments: map[string]any{"target": "x"}}, ToolExecuteContext{})
	if err != nil || res.Error != nil || res.Source != ToolSourceMCP || rec.count() != 1 {
		t.Fatalf("valid call: res=%+v err=%v ran=%d", res, err, rec.count())
	}
	// No/invalid wire schema: wire shape unchanged.
	for _, opts := range [][]McpMarkOptions{nil, {{WireInputSchema: map[string]any{"type": "array"}}}} {
		m := MarkMcp(base, opts...)
		if phJSON(t, m.ToAPITool()) != phJSON(t, base.ToAPITool()) {
			t.Fatalf("MarkMcp(%v) must keep the wrapped wire shape", opts)
		}
	}
	// The substituted schema reaches the dispatched request.
	sender := phSender(phTextResp("r", "ok"))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{marked}}); err != nil {
		t.Fatal(err)
	}
	if got := phCanonicalJSON(t, phJSON(t, phJSONMap(t, sender.requests[0].Tools[0])["parameters"])); got != mtwWireSchemaSanitized {
		t.Fatalf("dispatched parameters = %s", got)
	}
}

// server-tool.test.ts: "passes server tool configs through verbatim" and
// "mixes client + server tools in one array"
func TestServerAndClientToolsMixedOnTheWire(t *testing.T) {
	var rec phCounter
	client := phOKTool("echo", &rec, nil)
	server := NewServerTool(ServerToolConfig{Name: "web_search", Config: webSearchServerTool()})
	sender := phSender(phTextResp("r", "ok"))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{client, server}}); err != nil {
		t.Fatal(err)
	}
	tools := sender.requests[0].Tools
	if len(tools) != 2 {
		t.Fatalf("tools = %s", phJSON(t, tools))
	}
	if m := phJSONMap(t, tools[0]); m["type"] != "function" || m["name"] != "echo" {
		t.Fatalf("client tool = %v", m)
	}
	if phJSON(t, tools[1]) != phJSON(t, webSearchServerTool()) {
		t.Fatalf("server tool must pass through verbatim: %s", phJSON(t, tools[1]))
	}
}
