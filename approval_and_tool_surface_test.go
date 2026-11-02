package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Ports of upstream tests/unit/manual-tool-wire-schema.test.ts,
// call-model-active-tools.test.ts, approval-gate-regressions.test.ts (the
// parts this port implements), turn-context threading (#91), and strict
// passthrough (#99).

type choiceInput struct {
	Choice string `json:"choice" jsonschema:"required"`
}

func apiFunction(t *testing.T, tool Tool) components.ResponsesRequestToolFunction {
	t.Helper()
	u := tool.ToAPITool()
	if u.ResponsesRequestToolFunction == nil {
		t.Fatalf("not a function tool: %#v", u)
	}
	return *u.ResponsesRequestToolFunction
}

func TestManualToolWireInputSchemaIsSanitizedCopy(t *testing.T) {
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
	original := deepCopyJSONValue(wire)
	strict := true
	manual := MustNewTool(ToolConfig[choiceInput]{Name: "choose_value", Description: "Choose a value", Manual: true, WireInputSchema: wire, Strict: &strict})
	fn := apiFunction(t, manual)
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{"choice": map[string]any{
			"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}},
			"oneOf": []any{map[string]any{"const": "first"}, map[string]any{"const": "second"}},
		}},
		"required": []any{"choice"},
	}
	if !reflect.DeepEqual(fn.Parameters, want) {
		t.Fatalf("parameters = %#v", fn.Parameters)
	}
	if s, ok := fn.Strict.GetOrZero(); !ok || !s {
		t.Fatal("strict: true must be forwarded")
	}
	if !reflect.DeepEqual(wire, original) {
		t.Fatal("caller-owned schema was mutated")
	}
	fn.Parameters["type"] = "mutated"
	if wire["type"] != "object" {
		t.Fatal("emitted parameters must be a copy")
	}
}

func TestWireInputSchemaIgnoredForExecutableTools(t *testing.T) {
	wire := map[string]any{"type": "object", "properties": map[string]any{"other": map[string]any{"type": "number"}}}
	manual := MustNewTool(ToolConfig[choiceInput]{Name: "fallback_schema", Manual: true})
	exec := MustNewTool(ToolConfig[choiceInput]{Name: "exec", WireInputSchema: wire, Execute: func(context.Context, choiceInput, ToolExecuteContext) (any, error) { return "ok", nil }})
	for _, tool := range []Tool{manual, exec} {
		if got := apiFunction(t, tool).Parameters; !reflect.DeepEqual(got, tool.InputSchema()) {
			t.Fatalf("%s must use the struct-derived schema, got %#v", tool.ToolName(), got)
		}
	}
}

func TestStrictPassthroughDefaultsToNull(t *testing.T) {
	plain := MustNewTool(ToolConfig[choiceInput]{Name: "plain", Execute: func(context.Context, choiceInput, ToolExecuteContext) (any, error) { return nil, nil }})
	b, err := json.Marshal(plain.ToAPITool())
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	_ = json.Unmarshal(b, &wire)
	if v, present := wire["strict"]; !present || v != nil {
		t.Fatalf("unset Strict must serialize as strict:null, got %s", b)
	}
	off := false
	b, _ = json.Marshal(MustNewTool(ToolConfig[choiceInput]{Name: "off", Strict: &off, Execute: plain.config.Execute}).ToAPITool())
	_ = json.Unmarshal(b, &wire)
	if wire["strict"] != false {
		t.Fatalf("strict:false must be forwarded, got %s", b)
	}
}

func toolNames(req components.ResponsesRequest) []string {
	var names []string
	for _, tool := range req.Tools {
		if tool.ResponsesRequestToolFunction != nil {
			names = append(names, tool.ResponsesRequestToolFunction.Name)
		} else {
			names = append(names, "<server>")
		}
	}
	return names
}

func TestActiveToolsFiltersSentAndExecutableTools(t *testing.T) {
	mk := func(name string) Tool {
		return MustNewTool(ToolConfig[choiceInput]{Name: name, Execute: func(context.Context, choiceInput, ToolExecuteContext) (any, error) { return "ok", nil }})
	}
	server := NewServerTool(ServerToolConfig{Name: "web", Config: webSearchServerTool()})
	tools := []Tool{mk("a"), mk("b"), mk("c"), server}
	cases := []struct {
		name   string
		active []string
		tools  []Tool
		want   []string
	}{
		{"only active", []string{"a", "c"}, tools, []string{"a", "c", "<server>"}},
		{"unknown names ignored", []string{"a", "nope"}, tools, []string{"a", "<server>"}},
		{"omitted sends all", nil, tools, []string{"a", "b", "c", "<server>"}},
		{"filtered to empty omits tools", []string{}, []Tool{mk("a")}, nil},
		{"explicitly empty omits tools", nil, []Tool{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := sequenceSender(textOnlyResponse("ok"))
			result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: tc.tools, ActiveTools: tc.active})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := result.Text(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := toolNames(sender.requests[0]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
			b, _ := json.Marshal(sender.requests[0])
			var wire map[string]any
			_ = json.Unmarshal(b, &wire)
			if _, present := wire["tools"]; tc.want == nil && present {
				t.Fatalf("tools key must be omitted, got %s", b)
			}
		})
	}

	// A filtered-out tool is not executable either: a call to it is treated
	// as an unknown tool rather than run.
	ran := false
	hidden := MustNewTool(ToolConfig[locationInput]{Name: "get_weather", Execute: func(context.Context, locationInput, ToolExecuteContext) (any, error) {
		ran = true
		return "ok", nil
	}})
	result, err := CallModel(context.Background(), sequenceSender(weatherToolCallResponse("r1", "call_1"), textOnlyResponse("done")), CallModelInput{Model: "m", Input: "hi", Tools: []Tool{hidden, mk("a")}, ActiveTools: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a tool filtered out by ActiveTools must not execute")
	}
}

type targetInput struct {
	Target string `json:"target" jsonschema:"required"`
}

func TestApprovalPredicateFailsClosedOnSchemaInvalidArguments(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	// An explicit InputSchema, so a missing required field is rejected by the
	// same validation Execute applies.
	schema := map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}, "required": []any{"target"}}
	gated := MustNewTool(ToolConfig[targetInput]{Name: "strict_action", InputSchema: schema, Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return false, nil
	}, Execute: func(context.Context, targetInput, ToolExecuteContext) (any, error) { return "ok", nil }})
	ctx := context.Background()
	for _, call := range []ParsedToolCall{
		{CallID: "3", Name: "strict_action", Arguments: map[string]any{}},
		{CallID: "4", Name: "strict_action", RawArgs: `{"target":5}`},
		{CallID: "5", Name: "strict_action", RawArgs: `"just a string"`},
	} {
		req, err := ToolRequiresApproval(ctx, gated, call, TurnContext{}, nil)
		if err != nil || !req {
			t.Fatalf("call %s must fail closed: req=%v err=%v", call.CallID, req, err)
		}
	}
	if calls != 0 {
		t.Fatalf("predicate must not see invalid arguments, called %d times", calls)
	}
	req, err := ToolRequiresApproval(ctx, gated, ParsedToolCall{CallID: "6", Name: "strict_action", RawArgs: `{"target":"prod"}`}, TurnContext{}, nil)
	if err != nil || req || calls != 1 {
		t.Fatalf("valid arguments reach the predicate: req=%v calls=%d err=%v", req, calls, err)
	}
	// Call-level checks stay unconditional even for invalid arguments.
	callLevel := func(context.Context, ParsedToolCall, TurnContext) (bool, error) { return false, nil }
	if req, _ := ToolRequiresApproval(ctx, gated, ParsedToolCall{CallID: "7", Name: "strict_action", Arguments: map[string]any{}}, TurnContext{}, callLevel); req {
		t.Fatal("a false call-level check must be honored for invalid arguments")
	}
}

// Upstream #54: the AllowFinalResponse path must run the same approval gate
// as the in-loop path — the gated call pauses, it is never executed, and no
// final text-coercion request goes out.
func TestAllowFinalResponsePathEnforcesApprovalGate(t *testing.T) {
	var mu sync.Mutex
	safeRuns, dangerRuns := 0, 0
	safe := MustNewTool(ToolConfig[targetInput]{Name: "safe", Execute: func(context.Context, targetInput, ToolExecuteContext) (any, error) {
		mu.Lock()
		safeRuns++
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	}})
	danger := MustNewTool(ToolConfig[targetInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, targetInput, ToolExecuteContext) (any, error) {
		mu.Lock()
		dangerRuns++
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	}})
	turn0 := components.OpenResponsesResult{ID: "resp_turn_0", Output: []components.OutputItems{functionCallOutput("fc_safe", "call_safe", "safe", `{"target":"staging"}`)}}
	turn1 := components.OpenResponsesResult{ID: "resp_turn_1", Output: []components.OutputItems{functionCallOutput("fc_danger", "call_danger", "danger", `{"target":"prod"}`)}}
	sender := sequenceSender(turn0, turn1, textOnlyResponse("Done."))
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "do the thing", Tools: []Tool{safe, danger}, StateAccessor: accessor, StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if safeRuns != 1 || dangerRuns != 0 {
		t.Fatalf("safe=%d danger=%d, want 1/0", safeRuns, dangerRuns)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval || len(accessor.state.PendingToolCalls) != 1 || accessor.state.PendingToolCalls[0].CallID != "call_danger" {
		t.Fatalf("state = %s pending=%#v", accessor.state.Status, accessor.state.PendingToolCalls)
	}
	if req, _ := result.RequiresApproval(context.Background()); !req {
		t.Fatal("RequiresApproval must report the pause")
	}
	if sender.calls != 2 {
		t.Fatalf("requests = %d, want 2 (no final request while paused)", sender.calls)
	}
}

// Upstream #91: the executed call is threaded into the tool's
// TurnContext.ToolCall on the streaming loop path.
func TestExecutedCallThreadedIntoTurnContext(t *testing.T) {
	var mu sync.Mutex
	var seen *components.OutputFunctionCallItem
	tool := MustNewTool(ToolConfig[locationInput]{Name: "get_weather", Execute: func(_ context.Context, _ locationInput, ec ToolExecuteContext) (any, error) {
		mu.Lock()
		seen = ec.Turn.ToolCall
		mu.Unlock()
		return "sunny", nil
	}})
	result, err := CallModel(context.Background(), sequenceSender(weatherToolCallResponse("r1", "call_1"), textOnlyResponse("done")), CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.CallID != "call_1" || seen.Name != "get_weather" || seen.Arguments != `{"location":"Tokyo"}` || seen.ID == nil || *seen.ID != "call_1" {
		t.Fatalf("TurnContext.ToolCall = %#v", seen)
	}
	// A caller-provided TurnContext.ToolCall still wins.
	provided := &components.OutputFunctionCallItem{CallID: "orig", Name: "x"}
	ec := BuildToolExecuteContext(ParsedToolCall{CallID: "c", Name: "get_weather"}, TurnContext{ToolCall: provided}, nil, nil)
	if ec.Turn.ToolCall != provided {
		t.Fatal("caller-provided turn.ToolCall must win")
	}
}

// Struct-derived schemas are emitted with a root `$ref` into `$defs`; the
// validation Execute applies — and therefore the fail-closed approval check —
// must follow it, or a call missing a required field would sail through.
func TestStructDerivedSchemaValidationFollowsRefs(t *testing.T) {
	ran := false
	predicateCalls := 0
	gated := MustNewTool(ToolConfig[targetInput]{Name: "strict_action", Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
		predicateCalls++
		return false, nil
	}, Execute: func(context.Context, targetInput, ToolExecuteContext) (any, error) {
		ran = true
		return "ok", nil
	}})
	if _, hasRef := gated.InputSchema()["$ref"]; !hasRef {
		t.Fatal("precondition: reflected schema is expected to use $ref")
	}
	missing := ParsedToolCall{CallID: "c1", Name: "strict_action", RawArgs: `{}`}
	if req, err := ToolRequiresApproval(context.Background(), gated, missing, TurnContext{}, nil); err != nil || !req || predicateCalls != 0 {
		t.Fatalf("missing required field must fail closed: req=%v calls=%d err=%v", req, predicateCalls, err)
	}
	res, err := ExecuteTool(context.Background(), gated, missing, ToolExecuteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || ran {
		t.Fatalf("Execute must reject a call missing a required field: err=%v ran=%v", res.Error, ran)
	}
	valid := ParsedToolCall{CallID: "c2", Name: "strict_action", Arguments: map[string]any{"target": "prod", "extra": 1}}
	if req, _ := ToolRequiresApproval(context.Background(), gated, valid, TurnContext{}, nil); req || predicateCalls != 1 {
		t.Fatalf("valid arguments must reach the predicate: req=%v calls=%d", req, predicateCalls)
	}
}
