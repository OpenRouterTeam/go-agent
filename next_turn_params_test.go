package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// Ported from upstream tests/unit/next-turn-params.test.ts,
// src/lib/next-turn-params.test.ts and
// tests/unit/next-turn-tool-choice-loop.test.ts.

type patternInput struct {
	Pattern string `json:"pattern" jsonschema:"required"`
}

func searchToolCallResponse() components.OpenResponsesResult {
	return components.OpenResponsesResult{ID: "resp_call", Model: "test-model", Status: components.OpenAIResponsesResponseStatusCompleted, Output: []components.OutputItems{functionCallOutput("fc_1", "call_1", "tool_search", `{"pattern":"weather"}`)}}
}

func TestBuildNextTurnParamsContextExtractsRequestFields(t *testing.T) {
	temp, topP, maxTokens, topK := 0.7, 0.9, int64(256), int64(40)
	req := components.ResponsesRequest{Model: openrouter.String("m"), Models: []string{"a", "b"}, Temperature: optionalnullable.From(&temp), TopP: optionalnullable.From(&topP), MaxOutputTokens: optionalnullable.From(&maxTokens), TopK: &topK, Instructions: optionalnullable.From(openrouter.String("be brief")), ToolChoice: choiceRequired()}
	ntc := BuildNextTurnParamsContext(ParsedToolCall{}, ToolExecutionResult{}, req)
	if ntc.Model != "m" || len(ntc.Models) != 2 || *ntc.Temperature != 0.7 || *ntc.TopP != 0.9 || *ntc.MaxOutputTokens != 256 || *ntc.TopK != 40 || *ntc.Instructions != "be brief" || choiceJSON(t, ntc.ToolChoice) != `"required"` {
		t.Fatalf("context = %+v", ntc)
	}
	empty := BuildNextTurnParamsContext(ParsedToolCall{}, ToolExecutionResult{}, components.ResponsesRequest{})
	if empty.Model != "" || empty.Temperature != nil || empty.ToolChoice != nil || len(empty.Input.ArrayOfInputsUnion1) != 0 {
		t.Fatalf("defaults = %+v", empty)
	}
}

func TestApplyNextTurnParamsReplacesToolChoiceAndKeepsTools(t *testing.T) {
	tools := []components.ResponsesRequestToolUnion{weatherTool(t, false).ToAPITool()}
	req := components.ResponsesRequest{Tools: tools, ToolChoice: choiceAllowed("auto", "tool_search")}
	got := ApplyNextTurnParamsToRequest(req, map[string]any{"toolChoice": choiceAllowed("auto", "tool_search", "get_weather")})
	if choiceJSON(t, got.ToolChoice) != choiceJSON(t, choiceAllowed("auto", "tool_search", "get_weather")) {
		t.Fatalf("tool choice = %s", choiceJSON(t, got.ToolChoice))
	}
	if &got.Tools[0] != &tools[0] {
		t.Fatal("tools array must be left untouched so the prompt-cache prefix survives")
	}
	untouched := ApplyNextTurnParamsToRequest(req, map[string]any{"temperature": 0.2})
	if choiceJSON(t, untouched.ToolChoice) != choiceJSON(t, req.ToolChoice) {
		t.Fatal("toolChoice must be left alone when no tool computed one")
	}
	if v, _ := untouched.Temperature.GetOrZero(); v != 0.2 {
		t.Fatalf("temperature = %v", v)
	}
	// nil unsets (upstream null → undefined).
	cleared := ApplyNextTurnParamsToRequest(untouched, map[string]any{"temperature": (*float64)(nil), "instructions": ""})
	if _, ok := cleared.Temperature.GetOrZero(); ok || cleared.Temperature.IsSet() {
		t.Fatal("nil temperature must unset the field")
	}
	if v, ok := cleared.Instructions.GetOrZero(); !ok || v != "" {
		t.Fatal("empty-string instructions must be kept")
	}
}

func TestExecuteNextTurnParamsFunctionsKeyOrderAndInvalidKeys(t *testing.T) {
	out, err := ExecuteNextTurnParamsFunctions(context.Background(), NextTurnParamsFunctions{
		"model":   func(_ context.Context, ntc NextTurnParamsContext) (any, error) { return "next-model", nil },
		"bogus":   func(context.Context, NextTurnParamsContext) (any, error) { return "x", nil },
		"models":  func(_ context.Context, ntc NextTurnParamsContext) (any, error) { return []string{ntc.Model}, nil },
		"unknown": nil,
	}, BuildNextTurnParamsContext(ParsedToolCall{}, ToolExecutionResult{}, components.ResponsesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["bogus"]; ok {
		t.Fatal("invalid keys must be skipped")
	}
	// models runs after model in the upstream key order, so it composes on it.
	if out["model"] != "next-model" || strings.Join(out["models"].([]string), ",") != "next-model" {
		t.Fatalf("out = %#v", out)
	}
}

func TestToolNextTurnParamsComposeAcrossToolsAndCalls(t *testing.T) {
	first := MustNewTool(ToolConfig[patternInput]{Name: "first", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{Instructions: func(_ context.Context, in patternInput, ntc NextTurnParamsContext) (*string, error) {
			prev := ""
			if ntc.Instructions != nil {
				prev = *ntc.Instructions
			}
			s := prev + "[" + in.Pattern + "]"
			return &s, nil
		}}})
	second := MustNewTool(ToolConfig[patternInput]{Name: "second", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{Instructions: func(_ context.Context, in patternInput, ntc NextTurnParamsContext) (*string, error) {
			s := *ntc.Instructions + "<" + in.Pattern + ">"
			return &s, nil
		}}})
	uncalled := MustNewTool(ToolConfig[patternInput]{Name: "uncalled", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{Model: func(context.Context, patternInput, NextTurnParamsContext) (string, error) {
			t.Fatal("a tool that was not called must not run")
			return "", nil
		}}})
	calls := []ParsedToolCall{
		{CallID: "c1", Name: "second", RawArgs: `{"pattern":"s"}`},
		{CallID: "c2", Name: "first", RawArgs: `{"pattern":"a"}`},
		{CallID: "c3", Name: "first", RawArgs: `{"pattern":"b"}`},
	}
	// Tools-slice order (first before second), once per call.
	tools := []Tool{first, second, uncalled, NewServerTool(ServerToolConfig{Name: "web", Config: webSearchServerTool()})}
	computed, err := executeToolNextTurnParams(context.Background(), calls, tools, components.ResponsesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := *(computed["instructions"].(*string)); got != "[a][b]<s>" {
		t.Fatalf("composed instructions = %q", got)
	}

	_, err = executeToolNextTurnParams(context.Background(), []ParsedToolCall{{CallID: "c", Name: "first", RawArgs: `[1,2]`}}, tools, components.ResponsesRequest{})
	if err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("non-object arguments must error descriptively, got %v", err)
	}
}

func makeToolSearch(widened ...string) *TypedTool[patternInput] {
	return MustNewTool(ToolConfig[patternInput]{Name: "tool_search", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) {
		return map[string]any{"found": widened}, nil
	}, NextTurnParams: &NextTurnParams[patternInput]{ToolChoice: func(context.Context, patternInput, NextTurnParamsContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
		return choiceAllowed("auto", widened...), nil
	}}})
}

type emptyInput struct{}

func TestNextTurnToolChoiceReachesDispatchedFollowUp(t *testing.T) {
	getWeather := MustNewTool(ToolConfig[emptyInput]{Name: "get_weather", Execute: func(context.Context, emptyInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	sender := sequenceSender(searchToolCallResponse(), textOnlyResponse("done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{makeToolSearch("tool_search", "get_weather"), getWeather}, Request: components.ResponsesRequest{ToolChoice: choiceAllowed("auto", "tool_search")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, choiceJSON(t, choiceAllowed("auto", "tool_search")), choiceJSON(t, choiceAllowed("auto", "tool_search", "get_weather")))
	// Tools are byte-identical across turns.
	a, _ := json.Marshal(sender.requests[0].Tools)
	b, _ := json.Marshal(sender.requests[1].Tools)
	if string(a) != string(b) {
		t.Fatalf("tools changed across turns:\n%s\n%s", a, b)
	}
}

func TestNextTurnToolChoiceUntouchedWhenNoToolComputesOne(t *testing.T) {
	plain := MustNewTool(ToolConfig[patternInput]{Name: "tool_search", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	sender := sequenceSender(searchToolCallResponse(), textOnlyResponse("done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{plain}, Request: components.ResponsesRequest{ToolChoice: choiceAllowed("auto", "tool_search")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := choiceJSON(t, choiceAllowed("auto", "tool_search"))
	assertChoices(t, sender, want, want)
}

// A tool-computed forced choice becomes the new caller policy: it reaches the
// next request, then relaxes once it has produced a tool call.
func TestNextTurnForcedToolChoiceReArmsThenRelaxes(t *testing.T) {
	forcer := MustNewTool(ToolConfig[patternInput]{Name: "tool_search", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{ToolChoice: func(context.Context, patternInput, NextTurnParamsContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
			return choiceFunction("get_weather"), nil
		}}})
	sender := sequenceSender(searchToolCallResponse(), weatherToolCallResponse("resp_2", "call_2"), textOnlyResponse("done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{forcer, weatherTool(t, false)}, StopWhen: []StopCondition{StepCountIs(5)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, "", `{"name":"get_weather","type":"function"}`, `"auto"`)
}

// Upstream processApprovalDecisions does not run nextTurnParams: a resolved
// approval (approved or rejected) does not change the resume request.
func TestNextTurnParamsNotAppliedOnApprovalResume(t *testing.T) {
	gated := MustNewTool(ToolConfig[patternInput]{Name: "tool_search", RequireApproval: true, Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{Model: func(context.Context, patternInput, NextTurnParamsContext) (string, error) { return "from-tool", nil }}})
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			paused, err := CallModel(context.Background(), sequenceSender(searchToolCallResponse()), CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{gated}})
			if err != nil {
				t.Fatal(err)
			}
			state, err := paused.State(context.Background())
			if err != nil || state.Status != ConversationStatusAwaitingApproval {
				t.Fatalf("status=%s err=%v", state.Status, err)
			}
			in := CallModelInput{Model: "test-model", Tools: []Tool{gated}, State: &state}
			if decision == "approve" {
				in.ApproveToolCalls = []string{"call_1"}
			} else {
				in.RejectToolCalls = []string{"call_1"}
			}
			sender := sequenceSender(textOnlyResponse("done"))
			resumed, err := CallModel(context.Background(), sender, in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.Text(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := *sender.requests[0].Model; got != "test-model" {
				t.Fatalf("resume request model = %q, want the caller's model", got)
			}
		})
	}
}

// Upstream applies nextTurnParams before the AllowFinalResponse
// text-coercion request too.
func TestNextTurnParamsAppliedToFinalResponseRequest(t *testing.T) {
	tool := MustNewTool(ToolConfig[patternInput]{Name: "tool_search", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: &NextTurnParams[patternInput]{Model: func(context.Context, patternInput, NextTurnParamsContext) (string, error) { return "final-model", nil }}})
	sender := sequenceSender(searchToolCallResponse(), textOnlyResponse("done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}, StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 2 || choiceJSON(t, sender.requests[1].ToolChoice) != `"none"` {
		t.Fatalf("expected a final none request, got %d requests", len(sender.requests))
	}
	if got := *sender.requests[1].Model; got != "final-model" {
		t.Fatalf("final request model = %q", got)
	}
}

// A dynamic ToolChoiceFunc is resolved before the round's tools run, so a
// tool-computed ToolChoice for the next turn overrides it (upstream
// resolveAsyncFunctionsForTurn precedes executeToolRound/applyNextTurnParams).
func TestNextTurnToolChoiceOverridesDynamicToolChoiceFunc(t *testing.T) {
	getWeather := MustNewTool(ToolConfig[emptyInput]{Name: "get_weather", Execute: func(context.Context, emptyInput, ToolExecuteContext) (any, error) { return "ok", nil }})
	sender := sequenceSender(searchToolCallResponse(), textOnlyResponse("done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{makeToolSearch("tool_search", "get_weather"), getWeather},
		ToolChoiceFunc: func(context.Context, TurnContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
			return choiceAllowed("auto", "tool_search"), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, choiceJSON(t, choiceAllowed("auto", "tool_search")), choiceJSON(t, choiceAllowed("auto", "tool_search", "get_weather")))
}
