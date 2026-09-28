package agent

import (
	"context"
	"encoding/json"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/tool-choice-relaxation.test.ts (#100,
// DEV-785): an unchanged forced tool choice is one-shot once it produced a
// tool call.

func weatherCallResponse(id, callID string) *operations.CreateResponsesResponse {
	call := components.OutputFunctionCallItem{CallID: callID, Name: "get_weather", Arguments: `{"location":"Tokyo"}`}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: id, Status: components.OpenAIResponsesResponseStatusCompleted, Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	return &created
}

func plainTextResponse(text string) *operations.CreateResponsesResponse {
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_text", Status: components.OpenAIResponsesResponseStatusCompleted, OutputText: openrouter.String(text)})
	return &created
}

type locationInput struct {
	Location string `json:"location"`
}

func weatherTool(opts ...func(*ToolConfig[locationInput])) *TypedTool[locationInput] {
	cfg := ToolConfig[locationInput]{Name: "get_weather", Execute: func(context.Context, locationInput, ToolExecuteContext) (any, error) {
		return map[string]any{"temperature": 22}, nil
	}}
	for _, o := range opts {
		o(&cfg)
	}
	return MustNewTool(cfg)
}

func toolChoiceJSON(t *testing.T, c *components.OpenAIResponsesToolChoiceUnion) string {
	t.Helper()
	if c == nil {
		return "<nil>"
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func choicePtr(c components.OpenAIResponsesToolChoiceUnion) *components.OpenAIResponsesToolChoiceUnion {
	return &c
}

var requiredChoice = components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceRequired(components.OpenAIResponsesToolChoiceRequiredRequired)

func runToolChoice(t *testing.T, choice components.OpenAIResponsesToolChoiceUnion, stop StopCondition, allowFinal any, responses ...*operations.CreateResponsesResponse) *fakeSender {
	t.Helper()
	sender := &fakeSender{responses: responses}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool()}, Request: components.ResponsesRequest{ToolChoice: choicePtr(choice)}, StopWhen: []StopCondition{stop}, AllowFinalResponse: allowFinal, MaxTurns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	return sender
}

func TestRequiredToolChoiceRelaxesToAutoAfterToolRound(t *testing.T) {
	sender := runToolChoice(t, requiredChoice, StepCountIs(5), nil, weatherCallResponse("resp_1", "call_1"), plainTextResponse("It is 22 degrees."))
	if sender.calls != 2 {
		t.Fatalf("calls = %d", sender.calls)
	}
	if got := toolChoiceJSON(t, sender.requests[0].ToolChoice); got != `"required"` {
		t.Fatalf("initial choice = %s, want required", got)
	}
	if got := toolChoiceJSON(t, sender.requests[1].ToolChoice); got != `"auto"` {
		t.Fatalf("follow-up choice = %s, want auto", got)
	}
	if len(sender.requests[1].Tools) == 0 {
		t.Fatal("tools must stay available on the relaxed follow-up")
	}
}

func TestSpecificToolChoiceRelaxesToAuto(t *testing.T) {
	fn := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceFunction(components.OpenAIResponsesToolChoiceFunction{Name: "get_weather", Type: components.OpenAIResponsesToolChoiceTypeFunctionFunction})
	sender := runToolChoice(t, fn, StepCountIs(5), nil, weatherCallResponse("resp_1", "call_1"), plainTextResponse("Done."))
	if got := toolChoiceJSON(t, sender.requests[0].ToolChoice); got != `{"name":"get_weather","type":"function"}` {
		t.Fatalf("initial choice = %s", got)
	}
	if got := toolChoiceJSON(t, sender.requests[1].ToolChoice); got != `"auto"` {
		t.Fatalf("follow-up choice = %s, want auto", got)
	}
}

func TestAllowedToolsRequiredRelaxesModeKeepingSet(t *testing.T) {
	allowed := components.CreateOpenAIResponsesToolChoiceUnionToolChoiceAllowed(components.ToolChoiceAllowed{
		Mode:  components.CreateModeModeRequired(components.ModeRequiredRequired),
		Tools: []map[string]any{{"type": "function", "name": "get_weather"}},
		Type:  components.ToolChoiceAllowedTypeAllowedTools,
	})
	sender := runToolChoice(t, allowed, StepCountIs(5), nil, weatherCallResponse("resp_1", "call_1"), plainTextResponse("Done."))
	got := sender.requests[1].ToolChoice
	if got == nil || got.ToolChoiceAllowed == nil || got.ToolChoiceAllowed.Mode.ModeAuto == nil {
		t.Fatalf("follow-up = %s, want allowed_tools mode auto", toolChoiceJSON(t, got))
	}
	if len(got.ToolChoiceAllowed.Tools) != 1 || got.ToolChoiceAllowed.Tools[0]["name"] != "get_weather" {
		t.Fatalf("allowed tool set not preserved: %s", toolChoiceJSON(t, got))
	}
	// The caller's own value is not mutated.
	if allowed.ToolChoiceAllowed.Mode.ModeRequired == nil {
		t.Fatal("caller-configured allowed_tools choice was mutated")
	}
}

func TestAutoToolChoiceUnchangedOnFollowUp(t *testing.T) {
	auto := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto)
	sender := runToolChoice(t, auto, StepCountIs(5), nil, weatherCallResponse("resp_1", "call_1"), plainTextResponse("Done."))
	if got := toolChoiceJSON(t, sender.requests[1].ToolChoice); got != `"auto"` {
		t.Fatalf("follow-up = %s", got)
	}
}

func TestRelaxedChoiceStillForcesFinalNoneTurn(t *testing.T) {
	sender := runToolChoice(t, requiredChoice, StepCountIs(2), true, weatherCallResponse("resp_1", "call_1"), weatherCallResponse("resp_2", "call_2"), plainTextResponse("Final summary."))
	if sender.calls != 3 {
		t.Fatalf("calls = %d, want 3", sender.calls)
	}
	if got := toolChoiceJSON(t, sender.requests[1].ToolChoice); got != `"auto"` {
		t.Fatalf("turn 2 = %s, want auto", got)
	}
	if got := toolChoiceJSON(t, sender.requests[2].ToolChoice); got != `"none"` {
		t.Fatalf("final turn = %s, want none", got)
	}
}

func TestConsumedChoiceStaysRelaxedAcrossFollowUps(t *testing.T) {
	sender := runToolChoice(t, requiredChoice, StepCountIs(5), nil, weatherCallResponse("resp_1", "call_1"), weatherCallResponse("resp_2", "call_2"), plainTextResponse("Final summary."))
	want := []string{`"required"`, `"auto"`, `"auto"`}
	for i, w := range want {
		if got := toolChoiceJSON(t, sender.requests[i].ToolChoice); got != w {
			t.Fatalf("request %d choice = %s, want %s", i, got, w)
		}
	}
}

func TestConsumedChoicePersistsAcrossApprovalResumeAndResetsAfterCompletion(t *testing.T) {
	tool := weatherTool(func(c *ToolConfig[locationInput]) { c.RequireApproval = true })
	accessor := &memoryStateAccessor{}
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{weatherCallResponse("resp_1", "call_1")}}
	first, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "weather?", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval || accessor.state.ConsumedForcedToolChoiceKey != `"required"` {
		t.Fatalf("paused state: status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}

	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{plainTextResponse("It is 22 degrees.")}}
	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{Model: "m", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor, ApproveToolCalls: []string{"call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := resumed.Text(context.Background()); err != nil || text != "It is 22 degrees." {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if got := toolChoiceJSON(t, resumeSender.requests[0].ToolChoice); got != `"auto"` {
		t.Fatalf("resume request choice = %s, want auto", got)
	}
	if accessor.state.Status != ConversationStatusComplete || accessor.state.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("completed state: status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}

	freshSender := &fakeSender{responses: []*operations.CreateResponsesResponse{weatherCallResponse("resp_2", "call_2")}}
	fresh, err := CallModel(context.Background(), freshSender, CallModelInput{Model: "m", Input: "new request", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := toolChoiceJSON(t, freshSender.requests[0].ToolChoice); got != `"required"` {
		t.Fatalf("fresh request choice = %s, want required", got)
	}
}

func TestManualClientToolResumeRelaxesFirstRequest(t *testing.T) {
	manual := MustNewTool(ToolConfig[locationInput]{Name: "get_weather", Manual: true})
	accessor := &memoryStateAccessor{}
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{weatherCallResponse("resp_1", "call_1")}}
	first, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "weather?", Tools: []Tool{manual}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingClientTools || accessor.state.ConsumedForcedToolChoiceKey != `"required"` {
		t.Fatalf("paused state: status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}
	output := components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: "call_1", Output: components.CreateFunctionCallOutputItemOutputUnion2Str(`{"temperature":22}`), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput})
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{plainTextResponse("It is 22 degrees.")}}
	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{Model: "m", Input: []components.InputsUnion1{output}, Tools: []Tool{manual}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := toolChoiceJSON(t, resumeSender.requests[0].ToolChoice); got != `"auto"` {
		t.Fatalf("first request after client-tool resume = %s, want auto", got)
	}
}

// The persisted key is canonical (sorted-key) JSON, mirroring upstream's
// canonicalizeKeyMaterial, independent of SDK struct field order.
func TestForcedToolChoiceKeyIsCanonical(t *testing.T) {
	allowed := components.CreateOpenAIResponsesToolChoiceUnionToolChoiceAllowed(components.ToolChoiceAllowed{
		Mode:  components.CreateModeModeRequired(components.ModeRequiredRequired),
		Tools: []map[string]any{{"type": "function", "name": "get_weather"}},
		Type:  components.ToolChoiceAllowedTypeAllowedTools,
	})
	if got, want := forcedToolChoiceKey(&allowed), `{"mode":"required","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}`; got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
	if got := forcedToolChoiceKey(&requiredChoice); got != `"required"` {
		t.Fatalf("required key = %s", got)
	}
	auto := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto)
	if got := forcedToolChoiceKey(&auto); got != "" {
		t.Fatalf("auto must not be a forced key, got %q", got)
	}
}

// A run that ended without a pause (complete) must not carry a consumed key
// into the next, fresh request (upstream "clears consumed choice state when
// an interrupted run becomes a fresh request").
func TestFreshRequestAfterCompletedRunForcesAgain(t *testing.T) {
	accessor := &memoryStateAccessor{}
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{weatherCallResponse("r1", "c1"), plainTextResponse("done")}}
	first, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "q1", Tools: []Tool{weatherTool()}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("completed run persisted consumed key %q", accessor.state.ConsumedForcedToolChoiceKey)
	}
	fresh := &fakeSender{responses: []*operations.CreateResponsesResponse{plainTextResponse("ok")}}
	second, err := CallModel(context.Background(), fresh, CallModelInput{Model: "m", Input: "q2", Tools: []Tool{weatherTool()}, Request: components.ResponsesRequest{ToolChoice: choicePtr(requiredChoice)}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := toolChoiceJSON(t, fresh.requests[0].ToolChoice); got != `"required"` {
		t.Fatalf("fresh request choice = %s, want required", got)
	}
}
