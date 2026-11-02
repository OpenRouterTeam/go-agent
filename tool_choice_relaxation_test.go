package agent

import (
	"context"
	"encoding/json"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/tool-choice-relaxation.test.ts (DEV-785):
// a concrete forced tool choice is one-shot until its resolved semantic value
// changes.

type locationInput struct {
	Location string `json:"location" jsonschema:"required"`
}

func weatherToolCallResponse(id, callID string) components.OpenResponsesResult {
	return components.OpenResponsesResult{ID: id, Model: "test-model", Status: components.OpenAIResponsesResponseStatusCompleted, Output: []components.OutputItems{functionCallOutput("fc_"+callID, callID, "get_weather", `{"location":"Tokyo"}`)}}
}

func textOnlyResponse(text string) components.OpenResponsesResult {
	return components.OpenResponsesResult{ID: "resp_text", Model: "test-model", Status: components.OpenAIResponsesResponseStatusCompleted, OutputText: openrouter.String(text)}
}

func sequenceSender(resps ...components.OpenResponsesResult) *fakeSender {
	s := &fakeSender{}
	for _, r := range resps {
		created := operations.CreateCreateResponsesResponseOpenResponsesResult(r)
		s.responses = append(s.responses, &created)
	}
	return s
}

func weatherTool(t *testing.T, requireApproval bool) *TypedTool[locationInput] {
	t.Helper()
	return MustNewTool(ToolConfig[locationInput]{Name: "get_weather", Description: "Get the weather for a location.", RequireApproval: requireApproval, Execute: func(context.Context, locationInput, ToolExecuteContext) (any, error) {
		return map[string]any{"temperature": 22}, nil
	}})
}

func choiceRequired() *components.OpenAIResponsesToolChoiceUnion {
	c := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceRequired(components.OpenAIResponsesToolChoiceRequiredRequired)
	return &c
}

func choiceAuto() *components.OpenAIResponsesToolChoiceUnion {
	c := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto)
	return &c
}

func choiceFunction(name string) *components.OpenAIResponsesToolChoiceUnion {
	c := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceFunction(components.OpenAIResponsesToolChoiceFunction{Name: name, Type: components.OpenAIResponsesToolChoiceTypeFunctionFunction})
	return &c
}

func choiceAllowed(mode string, names ...string) *components.OpenAIResponsesToolChoiceUnion {
	var tools []map[string]any
	for _, n := range names {
		tools = append(tools, map[string]any{"type": "function", "name": n})
	}
	m := components.CreateModeModeAuto(components.ModeAutoAuto)
	if mode == "required" {
		m = components.CreateModeModeRequired(components.ModeRequiredRequired)
	}
	c := components.CreateOpenAIResponsesToolChoiceUnionToolChoiceAllowed(components.ToolChoiceAllowed{Mode: m, Tools: tools, Type: components.ToolChoiceAllowedTypeAllowedTools})
	return &c
}

// choiceJSON renders a request's tool_choice as wire JSON ("" when unset).
func choiceJSON(t *testing.T, c *components.OpenAIResponsesToolChoiceUnion) string {
	t.Helper()
	if c == nil {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertChoices(t *testing.T, sender *fakeSender, want ...string) {
	t.Helper()
	if len(sender.requests) != len(want) {
		t.Fatalf("requests = %d, want %d", len(sender.requests), len(want))
	}
	for i, w := range want {
		if got := choiceJSON(t, sender.requests[i].ToolChoice); got != w {
			t.Fatalf("request %d tool_choice = %s, want %s", i, got, w)
		}
	}
}

func TestForcedToolChoiceRelaxesAfterToolRound(t *testing.T) {
	cases := []struct {
		name   string
		choice *components.OpenAIResponsesToolChoiceUnion
		first  string
		second string
	}{
		{"required", choiceRequired(), `"required"`, `"auto"`},
		{"specific function", choiceFunction("get_weather"), `{"name":"get_weather","type":"function"}`, `"auto"`},
		{"allowed_tools required keeps the set", choiceAllowed("required", "get_weather"), `{"mode":"required","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}`, `{"mode":"auto","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}`},
		{"auto unchanged", choiceAuto(), `"auto"`, `"auto"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := sequenceSender(weatherToolCallResponse("resp_1", "call_1"), textOnlyResponse("It is 22 degrees."))
			result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool(t, false)}, Request: components.ResponsesRequest{ToolChoice: tc.choice}, StopWhen: []StopCondition{StepCountIs(5)}})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if text != "It is 22 degrees." {
				t.Fatalf("text = %q", text)
			}
			assertChoices(t, sender, tc.first, tc.second)
			// Tools stay available on the relaxed follow-up.
			if len(sender.requests[1].Tools) == 0 {
				t.Fatal("follow-up must keep tools")
			}
		})
	}
}

func TestRelaxedChoiceStillForcesFinalNoToolsTurn(t *testing.T) {
	sender := sequenceSender(weatherToolCallResponse("resp_1", "call_1"), weatherToolCallResponse("resp_2", "call_2"), textOnlyResponse("Final summary."))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool(t, false)}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Final summary." {
		t.Fatalf("text = %q", text)
	}
	assertChoices(t, sender, `"required"`, `"auto"`, `"none"`)
}

func TestConsumedChoiceStaysRelaxedAcrossFollowUps(t *testing.T) {
	sender := sequenceSender(weatherToolCallResponse("resp_1", "call_1"), weatherToolCallResponse("resp_2", "call_2"), textOnlyResponse("Final summary."))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool(t, false)}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StopWhen: []StopCondition{StepCountIs(5)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, `"required"`, `"auto"`, `"auto"`)
}

func TestDynamicForcedChoiceReArmsOnDifferentValue(t *testing.T) {
	sender := sequenceSender(weatherToolCallResponse("resp_1", "call_1"), weatherToolCallResponse("resp_2", "call_2"), textOnlyResponse("Done."))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool(t, false)}, StopWhen: []StopCondition{StepCountIs(5)},
		ToolChoiceFunc: func(_ context.Context, tc TurnContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
			if tc.NumberOfTurns == 0 {
				return choiceRequired(), nil
			}
			return choiceFunction("get_weather"), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, `"required"`, `{"name":"get_weather","type":"function"}`, `"auto"`)
}

func TestDynamicForcedChoiceReArmsAfterUnforcedTurn(t *testing.T) {
	sender := sequenceSender(weatherToolCallResponse("resp_1", "call_1"), weatherToolCallResponse("resp_2", "call_2"), weatherToolCallResponse("resp_3", "call_3"), textOnlyResponse("Done."))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{weatherTool(t, false)}, StopWhen: []StopCondition{StepCountIs(5)},
		ToolChoiceFunc: func(_ context.Context, tc TurnContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
			if tc.NumberOfTurns == 1 {
				return choiceAuto(), nil
			}
			return choiceRequired(), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, sender, `"required"`, `"auto"`, `"required"`, `"auto"`)
}

func TestForcedChoiceSatisfactionPersistsAcrossApprovalResume(t *testing.T) {
	tool := weatherTool(t, true)
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	first := sequenceSender(weatherToolCallResponse("resp_1", "call_1"))
	paused, err := CallModel(context.Background(), first, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paused.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, first, `"required"`)
	if accessor.state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s", accessor.state.Status)
	}
	if want, _ := canonicalJSON("required"); accessor.state.ConsumedForcedToolChoiceKey != want {
		t.Fatalf("consumed key = %q, want %q", accessor.state.ConsumedForcedToolChoiceKey, want)
	}
	// The consumed key survives the versioned serialization round trip.
	blob, err := SerializeConversationState(accessor.state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(blob)
	if err != nil || restored.ConsumedForcedToolChoiceKey != `"required"` {
		t.Fatalf("round trip lost the consumed key: %q, %v", restored.ConsumedForcedToolChoiceKey, err)
	}

	second := sequenceSender(textOnlyResponse("It is 22 degrees."))
	resumed, err := CallModel(context.Background(), second, CallModelInput{Model: "test-model", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor, ApproveToolCalls: []string{"call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := resumed.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "It is 22 degrees." {
		t.Fatalf("text = %q", text)
	}
	assertChoices(t, second, `"auto"`)
	if accessor.state.Status != ConversationStatusComplete || accessor.state.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("completion must clear the consumed key: status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}

	third := sequenceSender(weatherToolCallResponse("resp_2", "call_2"))
	fresh, err := CallModel(context.Background(), third, CallModelInput{Model: "test-model", Input: "Start a new weather request.", Tools: []Tool{tool}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, third, `"required"`)
}

func TestForcedChoiceRelaxedAfterManualClientToolResume(t *testing.T) {
	manual := MustNewTool(ToolConfig[locationInput]{Name: "get_weather", Manual: true})
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	first := sequenceSender(weatherToolCallResponse("resp_1", "call_1"))
	paused, err := CallModel(context.Background(), first, CallModelInput{Model: "test-model", Input: "What is the weather in Tokyo?", Tools: []Tool{manual}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paused.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingClientTools || accessor.state.ConsumedForcedToolChoiceKey != `"required"` {
		t.Fatalf("status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}
	second := sequenceSender(textOnlyResponse("It is 22 degrees."))
	output := components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: "call_1", Output: components.CreateFunctionCallOutputItemOutputUnion2Str(`{"temperature":22}`), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput})
	resumed, err := CallModel(context.Background(), second, CallModelInput{Model: "test-model", Input: []components.InputsUnion1{output}, Tools: []Tool{manual}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := resumed.Text(context.Background()); err != nil || text != "It is 22 degrees." {
		t.Fatalf("text=%q err=%v", text, err)
	}
	assertChoices(t, second, `"auto"`)
}

func TestForcedChoiceRelaxedAfterHITLResume(t *testing.T) {
	hitl := MustNewTool(ToolConfig[locationInput]{Name: "get_weather", OnToolCalled: func(context.Context, locationInput, ToolExecuteContext) (any, bool, error) {
		return nil, false, nil // pause for a human
	}})
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	paused, err := CallModel(context.Background(), sequenceSender(weatherToolCallResponse("resp_1", "call_1")), CallModelInput{Model: "test-model", Input: "weather?", Tools: []Tool{hitl}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paused.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingHITL || accessor.state.ConsumedForcedToolChoiceKey != `"required"` {
		t.Fatalf("status=%s key=%q", accessor.state.Status, accessor.state.ConsumedForcedToolChoiceKey)
	}
	second := sequenceSender(textOnlyResponse("It is 22 degrees."))
	output := components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: "call_1", Output: components.CreateFunctionCallOutputItemOutputUnion2Str(`{"temperature":22}`), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput})
	resumed, err := CallModel(context.Background(), second, CallModelInput{Model: "test-model", Input: []components.InputsUnion1{output}, Tools: []Tool{hitl}, Request: components.ResponsesRequest{ToolChoice: choiceRequired()}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertChoices(t, second, `"auto"`)
}

func TestRelaxForcedToolChoiceShapes(t *testing.T) {
	if relaxForcedToolChoice(nil) != nil {
		t.Fatal("nil passes through")
	}
	none := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceNone(components.OpenAIResponsesToolChoiceNoneNone)
	if got := choiceJSON(t, relaxForcedToolChoice(&none)); got != `"none"` {
		t.Fatalf("none relaxed to %s", got)
	}
	if got := choiceJSON(t, relaxForcedToolChoice(choiceAllowed("auto", "a"))); got != `{"mode":"auto","tools":[{"name":"a","type":"function"}],"type":"allowed_tools"}` {
		t.Fatalf("allowed auto relaxed to %s", got)
	}
	if isForcedToolChoice(choiceAllowed("auto", "a")) || !isForcedToolChoice(choiceAllowed("required", "a")) || isForcedToolChoice(&none) || !isForcedToolChoice(choiceFunction("x")) {
		t.Fatal("isForcedToolChoice misclassified a choice")
	}
}

// The first request after an approval resume is a continuation (turn 1), and
// a pausing round does not resolve the next turn's choice.
func TestToolChoiceFuncTurnNumbersAcrossApprovalResume(t *testing.T) {
	tool := weatherTool(t, true)
	var turns []int
	fn := func(_ context.Context, tc TurnContext) (*components.OpenAIResponsesToolChoiceUnion, error) {
		turns = append(turns, tc.NumberOfTurns)
		return choiceAuto(), nil
	}
	paused, err := CallModel(context.Background(), sequenceSender(weatherToolCallResponse("resp_1", "call_1")), CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}, ToolChoiceFunc: fn})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := paused.State(context.Background())
	if len(turns) != 1 || turns[0] != 0 {
		t.Fatalf("paused run resolved turns %v, want [0]", turns)
	}
	turns = nil
	resumed, err := CallModel(context.Background(), sequenceSender(textOnlyResponse("done")), CallModelInput{Model: "test-model", Tools: []Tool{tool}, State: &state, ApproveToolCalls: []string{"call_1"}, ToolChoiceFunc: fn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0] != 1 {
		t.Fatalf("resume resolved turns %v, want [1]", turns)
	}
}
