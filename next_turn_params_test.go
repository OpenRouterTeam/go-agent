package agent

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/next-turn-tool-choice-loop.test.ts (#114)
// and the composition cases of next-turn-params.test.ts.

func allowedChoice(names ...string) components.OpenAIResponsesToolChoiceUnion {
	tools := make([]map[string]any, 0, len(names))
	for _, n := range names {
		tools = append(tools, map[string]any{"type": "function", "name": n})
	}
	return components.CreateOpenAIResponsesToolChoiceUnionToolChoiceAllowed(components.ToolChoiceAllowed{
		Mode:  components.CreateModeModeAuto(components.ModeAutoAuto),
		Tools: tools,
		Type:  components.ToolChoiceAllowedTypeAllowedTools,
	})
}

type patternInput struct {
	Pattern string `json:"pattern"`
}

func searchToolCallResponse() *operations.CreateResponsesResponse {
	call := components.OutputFunctionCallItem{CallID: "call_search", Name: "tool_search", Arguments: `{"pattern":"weather"}`}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	return &created
}

func makeSearchTool(widened []string, computeChoice bool) *TypedTool[patternInput] {
	cfg := ToolConfig[patternInput]{Name: "tool_search", Execute: func(context.Context, patternInput, ToolExecuteContext) (any, error) {
		return map[string]any{"found": widened}, nil
	}}
	if computeChoice {
		cfg.NextTurnParams = NextTurnParamsFunctions{"toolChoice": func(_ context.Context, ntc NextTurnParamsContext) (any, error) {
			if args, _ := ntc.ToolCall.Arguments.(map[string]any); args["pattern"] != "weather" {
				t := allowedChoice("tool_search")
				return t, nil
			}
			return allowedChoice(widened...), nil
		}}
	}
	return MustNewTool(cfg)
}

func runSearchLoop(t *testing.T, computeChoice bool) *fakeSender {
	t.Helper()
	getWeather := MustNewTool(ToolConfig[struct{}]{Name: "get_weather", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{searchToolCallResponse(), plainTextResponse("done")}}
	narrow := allowedChoice("tool_search")
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{makeSearchTool([]string{"tool_search", "get_weather"}, computeChoice), getWeather}, Request: components.ResponsesRequest{ToolChoice: &narrow}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 {
		t.Fatalf("calls = %d", sender.calls)
	}
	return sender
}

func TestNextTurnParamsToolChoiceReachesFollowUp(t *testing.T) {
	sender := runSearchLoop(t, true)
	first := toolChoiceJSON(t, sender.requests[0].ToolChoice)
	if want := toolChoiceJSON(t, choicePtr(allowedChoice("tool_search"))); first != want {
		t.Fatalf("turn 1 choice = %s, want %s", first, want)
	}
	second := toolChoiceJSON(t, sender.requests[1].ToolChoice)
	if want := toolChoiceJSON(t, choicePtr(allowedChoice("tool_search", "get_weather"))); second != want {
		t.Fatalf("turn 2 choice = %s, want widened %s", second, want)
	}
	// The tools array is byte-identical across turns (prompt-cache prefix).
	if len(sender.requests[0].Tools) != 2 || len(sender.requests[1].Tools) != 2 {
		t.Fatalf("tools changed: %d -> %d", len(sender.requests[0].Tools), len(sender.requests[1].Tools))
	}
	for i := range sender.requests[0].Tools {
		if sender.requests[0].Tools[i].ResponsesRequestToolFunction.Name != sender.requests[1].Tools[i].ResponsesRequestToolFunction.Name {
			t.Fatal("tools array reordered between turns")
		}
	}
}

func TestNoNextTurnToolChoiceLeavesChoiceUntouched(t *testing.T) {
	sender := runSearchLoop(t, false)
	a, b := toolChoiceJSON(t, sender.requests[0].ToolChoice), toolChoiceJSON(t, sender.requests[1].ToolChoice)
	if a != b {
		t.Fatalf("choice changed without a tool computing one: %s -> %s", a, b)
	}
}

func TestNextTurnParamsComposeAcrossToolsAndCalls(t *testing.T) {
	var seenByB []string
	a := MustNewTool(ToolConfig[sampleInput]{Name: "a", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "a", nil },
		NextTurnParams: NextTurnParamsFunctions{
			"model": func(_ context.Context, ntc NextTurnParamsContext) (any, error) {
				return "model-from-a", nil
			},
			"bogusKey": func(context.Context, NextTurnParamsContext) (any, error) { return "ignored", nil },
		}})
	callsOfB := 0
	b := MustNewTool(ToolConfig[sampleInput]{Name: "b", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "b", nil },
		NextTurnParams: NextTurnParamsFunctions{"temperature": func(_ context.Context, ntc NextTurnParamsContext) (any, error) {
			callsOfB++
			if ntc.Request.Model != nil {
				seenByB = append(seenByB, *ntc.Request.Model)
			}
			return 0.25, nil
		}}})
	unused := MustNewTool(ToolConfig[sampleInput]{Name: "unused", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "", nil },
		NextTurnParams: NextTurnParamsFunctions{"model": func(context.Context, NextTurnParamsContext) (any, error) { return "never", nil }}})
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "b1", Name: "b", Arguments: `{"query":"1"}`}),
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "a1", Name: "a", Arguments: `{"query":"2"}`}),
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "b2", Name: "b", Arguments: `{"query":"3"}`}),
	}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created, plainTextResponse("done")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "base-model", Input: "hi", Tools: []Tool{a, b, unused}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	follow := sender.requests[1]
	if follow.Model == nil || *follow.Model != "model-from-a" {
		t.Fatalf("follow-up model = %v, want model-from-a", follow.Model)
	}
	if temp, ok := follow.Temperature.GetOrZero(); !ok || temp != 0.25 {
		t.Fatalf("follow-up temperature = %v (set=%v)", temp, ok)
	}
	// Tools order: a's changes apply before b's, so b sees a's model; b runs
	// once per call.
	if callsOfB != 2 || len(seenByB) != 2 || seenByB[0] != "model-from-a" {
		t.Fatalf("composition broken: callsOfB=%d seenByB=%v", callsOfB, seenByB)
	}
}

// A tool-computed forced choice with a different semantic value re-arms
// immediately (it is the new caller policy), and is itself relaxed once it
// produced a tool call (upstream #100 + #114 interaction).
func TestNextTurnForcedChoiceReArmsThenRelaxes(t *testing.T) {
	fnChoice := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceFunction(components.OpenAIResponsesToolChoiceFunction{Name: "get_weather", Type: components.OpenAIResponsesToolChoiceTypeFunctionFunction})
	tool := weatherTool(func(c *ToolConfig[locationInput]) {
		c.NextTurnParams = NextTurnParamsFunctions{"toolChoice": func(context.Context, NextTurnParamsContext) (any, error) {
			return fnChoice, nil
		}}
	})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{weatherCallResponse("r1", "c1"), weatherCallResponse("r2", "c2"), plainTextResponse("done")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{`"required"`, `{"name":"get_weather","type":"function"}`, `"auto"`}
	for i, w := range want {
		if got := toolChoiceJSON(t, sender.requests[i].ToolChoice); got != w {
			t.Fatalf("request %d choice = %s, want %s", i, got, w)
		}
	}
}

// Upstream "throws a descriptive error when tool call arguments are not an
// object".
func TestNextTurnParamsRejectsNonObjectArguments(t *testing.T) {
	funcs := NextTurnParamsFunctions{"model": func(context.Context, NextTurnParamsContext) (any, error) { return "x", nil }}
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "a", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "", nil }, NextTurnParams: funcs})
	_, _, err := applyToolNextTurnParams(context.Background(), []Tool{tool}, nil, []ParsedToolCall{{CallID: "c", Name: "a", Arguments: []any{1}}}, components.ResponsesRequest{})
	if err == nil || err.Error() != "tool call arguments for a must be an object, got array" {
		t.Fatalf("err = %v", err)
	}
}

// Invalid keys are skipped (with a warning) and never applied.
func TestNextTurnParamsSkipsInvalidKeys(t *testing.T) {
	computed, err := ExecuteNextTurnParamsFunctions(context.Background(), NextTurnParamsFunctions{
		"bogus": func(context.Context, NextTurnParamsContext) (any, error) { return 1, nil },
		"topK":  func(context.Context, NextTurnParamsContext) (any, error) { return 7, nil },
	}, NextTurnParamsContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := computed["bogus"]; ok || computed["topK"] != 7 {
		t.Fatalf("computed = %#v", computed)
	}
	req := ApplyNextTurnParamsToRequest(components.ResponsesRequest{}, computed)
	if req.TopK == nil || *req.TopK != 7 {
		t.Fatalf("topK not applied: %v", req.TopK)
	}
}
