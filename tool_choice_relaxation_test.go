package agent

// Ports tests/unit/tool-choice-relaxation.test.ts (DEV-785): a forced caller
// tool choice is one-shot per logical run.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type tcrLocation struct {
	Location string `json:"location"`
}

func tcrWeatherTool(cfg func(*ToolConfig[tcrLocation])) *TypedTool[tcrLocation] {
	c := ToolConfig[tcrLocation]{
		Name: "get_weather", Description: "Get the weather for a location.", InputSchema: phObjectSchema("location"),
		Execute: func(context.Context, tcrLocation, ToolExecuteContext) (any, error) {
			return map[string]any{"temperature": 22}, nil
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	return MustNewTool(c)
}

func tcrToolCall(id, callID string) components.OpenResponsesResult {
	return phResp(id, phFnCall(callID, "get_weather", `{"location":"Tokyo"}`))
}

func tcrRequest(choice *components.OpenAIResponsesToolChoiceUnion) components.ResponsesRequest {
	return components.ResponsesRequest{ToolChoice: choice}
}

func tcrRun(t *testing.T, sender *fakeSender, in CallModelInput) string {
	t.Helper()
	result, err := CallModel(context.Background(), sender, in)
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return text
}

const (
	tcrRequired = `"required"`
	tcrAuto     = `"auto"`
	tcrNone     = `"none"`
)

// upstream: "relaxes toolChoice:'required' to 'auto' after a successful tool round"
func TestForcedRequiredToolChoiceRelaxedAfterToolRound(t *testing.T) {
	sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "It is 22 degrees."))
	text := tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "What is the weather in Tokyo?", Tools: []Tool{tcrWeatherTool(nil)},
		Request: tcrRequest(phChoice(t, tcrRequired)), StopWhen: []StopCondition{StepCountIs(6)},
	})
	if text != "It is 22 degrees." || sender.calls != 2 {
		t.Fatalf("text=%q calls=%d", text, sender.calls)
	}
	if got := phWireChoice(t, sender, 0); got != tcrRequired {
		t.Fatalf("initial turn must carry the caller's forced choice, got %s", got)
	}
	if got := phWireChoice(t, sender, 1); got != tcrAuto {
		t.Fatalf("follow-up must relax to auto, got %s", got)
	}
	if names := phToolNames(t, sender, 1); !phEqualStrings(names, []string{"get_weather"}) {
		t.Fatalf("tools stay available on the relaxed follow-up, got %v", names)
	}
}

// upstream: "relaxes a forced specific-tool choice after a successful tool round"
func TestForcedFunctionToolChoiceRelaxedAfterToolRound(t *testing.T) {
	sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "Done."))
	tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)},
		Request: tcrRequest(phChoice(t, `{"type":"function","name":"get_weather"}`)),
	})
	if got := phWireChoice(t, sender, 0); got != `{"name":"get_weather","type":"function"}` {
		t.Fatalf("initial choice = %s", got)
	}
	if got := phWireChoice(t, sender, 1); got != tcrAuto {
		t.Fatalf("follow-up choice = %s, want auto", got)
	}
}

// upstream: "relaxes allowed_tools mode:'required' to mode:'auto' keeping the tool set"
func TestForcedAllowedToolsRelaxedToAutoModeKeepingTools(t *testing.T) {
	sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "Done."))
	tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)},
		Request: tcrRequest(phChoice(t, `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"get_weather"}]}`)),
	})
	if got := phWireChoice(t, sender, 0); got != `{"mode":"required","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}` {
		t.Fatalf("initial choice = %s", got)
	}
	if got := phWireChoice(t, sender, 1); got != `{"mode":"auto","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}` {
		t.Fatalf("follow-up must keep the tool set with mode auto, got %s", got)
	}
}

// upstream: "keeps toolChoice:'auto' unchanged on follow-up turns"; unforced
// choices (auto, none, allowed_tools in auto mode) pass through.
func TestUnforcedToolChoicePassesThrough(t *testing.T) {
	for _, raw := range []string{tcrAuto, tcrNone, `{"mode":"auto","tools":[{"name":"get_weather","type":"function"}],"type":"allowed_tools"}`} {
		sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "Done."))
		tcrRun(t, sender, CallModelInput{
			Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)},
			Request: tcrRequest(phChoice(t, raw)),
		})
		for i := 0; i < 2; i++ {
			if got := phWireChoice(t, sender, i); got != raw {
				t.Fatalf("%s: request %d choice = %s", raw, i, got)
			}
		}
	}
	// No configured choice stays absent.
	sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "Done."))
	tcrRun(t, sender, CallModelInput{Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)}})
	if a, b := phWireChoice(t, sender, 0), phWireChoice(t, sender, 1); a != "" || b != "" {
		t.Fatalf("an unset choice must stay unset, got %q / %q", a, b)
	}
}

// upstream: "still forces the final no-tools turn when the budget is genuinely exhausted"
func TestRelaxedChoiceStillEndsWithForcedFinalTurn(t *testing.T) {
	sender := phSender(tcrToolCall("resp_1", "call_1"), tcrToolCall("resp_2", "call_2"), phTextResp("resp_text", "Final summary."))
	text := tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)},
		Request:  tcrRequest(phChoice(t, tcrRequired)),
		StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true,
	})
	if text != "Final summary." || sender.calls != 3 {
		t.Fatalf("text=%q calls=%d", text, sender.calls)
	}
	for i, want := range []string{tcrRequired, tcrAuto, tcrNone} {
		if got := phWireChoice(t, sender, i); got != want {
			t.Fatalf("request %d choice = %s, want %s", i, got, want)
		}
	}
}

// upstream: "keeps an unchanged consumed choice relaxed across multiple follow-up turns"
func TestConsumedChoiceStaysRelaxedAcrossFollowUps(t *testing.T) {
	sender := phSender(tcrToolCall("resp_1", "call_1"), tcrToolCall("resp_2", "call_2"), phTextResp("resp_text", "Final summary."))
	tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{tcrWeatherTool(nil)},
		Request: tcrRequest(phChoice(t, tcrRequired)), StopWhen: []StopCondition{StepCountIs(6)},
	})
	for i, want := range []string{tcrRequired, tcrAuto, tcrAuto} {
		if got := phWireChoice(t, sender, i); got != want {
			t.Fatalf("request %d choice = %s, want %s", i, got, want)
		}
	}
}

// upstream: "persists satisfaction across approval resume and resets it after completion"
func TestConsumedChoicePersistsAcrossApprovalResume(t *testing.T) {
	tool := tcrWeatherTool(func(c *ToolConfig[tcrLocation]) { c.RequireApproval = true })
	mem := &phMemState{}
	in := CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, Request: tcrRequest(phChoice(t, tcrRequired))}

	first := phSender(tcrToolCall("resp_1", "call_1"))
	run1 := in
	run1.Input = "What is the weather in Tokyo?"
	if _, err := mustRun(t, first, run1); err != nil {
		t.Fatal(err)
	}
	if got := phWireChoice(t, first, 0); got != tcrRequired {
		t.Fatalf("initial choice = %s", got)
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingApproval || st.ConsumedForcedToolChoiceKey == "" {
		t.Fatalf("the pause must persist the consumed forced choice: status=%s key=%q", st.Status, st.ConsumedForcedToolChoiceKey)
	}

	resume := phSender(phTextResp("resp_text", "It is 22 degrees."))
	run2 := in
	run2.Input = []components.InputsUnion1{}
	run2.ApproveToolCalls = []string{"call_1"}
	if text := tcrRun(t, resume, run2); text != "It is 22 degrees." {
		t.Fatalf("resume text = %q", text)
	}
	if got := phWireChoice(t, resume, 0); got != tcrAuto {
		t.Fatalf("the resumed follow-up must stay relaxed, got %s", got)
	}
	st = mem.get()
	if st.Status != ConversationStatusComplete || st.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("completion must clear the consumed key: status=%s key=%q", st.Status, st.ConsumedForcedToolChoiceKey)
	}

	fresh := phSender(tcrToolCall("resp_2", "call_2"))
	run3 := in
	run3.Input = "Start a new weather request."
	if _, err := mustRun(t, fresh, run3); err != nil {
		t.Fatal(err)
	}
	if got := phWireChoice(t, fresh, 0); got != tcrRequired {
		t.Fatalf("a new logical run forces again, got %s", got)
	}
}

// The consumed key is part of the serialized state contract: a state that
// round-trips through SerializeConversationState keeps the relaxation, and an
// unconsumed state serializes without the field.
func TestConsumedChoiceSurvivesSerialization(t *testing.T) {
	ctx := context.Background()
	tool := tcrWeatherTool(func(c *ToolConfig[tcrLocation]) { c.RequireApproval = true })
	result, err := CallModel(ctx, phSender(tcrToolCall("resp_1", "call_1")), CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{tool}, Request: tcrRequest(phChoice(t, tcrRequired)),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := result.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := SerializeConversationState(st)
	if err != nil {
		t.Fatal(err)
	}
	if !tcrHasKey(t, blob, "ConsumedForcedToolChoiceKey") {
		t.Fatalf("a consumed key must be serialized: %s", blob)
	}
	restored, err := DeserializeConversationState(blob)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ConsumedForcedToolChoiceKey == "" || restored.ConsumedForcedToolChoiceKey != st.ConsumedForcedToolChoiceKey {
		t.Fatalf("consumed key lost in serialization: %q -> %q", st.ConsumedForcedToolChoiceKey, restored.ConsumedForcedToolChoiceKey)
	}
	resume := phSender(phTextResp("resp_text", "ok"))
	tcrRun(t, resume, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, State: &restored, ApproveToolCalls: []string{"call_1"},
		Request: tcrRequest(phChoice(t, tcrRequired)),
	})
	if got := phWireChoice(t, resume, 0); got != tcrAuto {
		t.Fatalf("resume from a deserialized state must stay relaxed, got %s", got)
	}

	plain, err := SerializeConversationState(CreateInitialState())
	if err != nil {
		t.Fatal(err)
	}
	if tcrHasKey(t, plain, "ConsumedForcedToolChoiceKey") {
		t.Fatalf("an unconsumed state must omit the additive field: %s", plain)
	}
}

func tcrHasKey(t *testing.T, raw, key string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	_, ok := m[key]
	return ok
}

// upstream: "relaxes the first model request after a manual client-tool resume"
func TestConsumedChoiceRelaxesFirstRequestAfterManualToolResume(t *testing.T) {
	manual := MustNewTool(ToolConfig[tcrLocation]{Name: "get_weather", InputSchema: phObjectSchema("location"), Manual: true})
	mem := &phMemState{}
	sender := phSender(tcrToolCall("resp_1", "call_1"), phTextResp("resp_text", "It is 22 degrees."))
	if _, err := mustRun(t, sender, CallModelInput{
		Model: "openai/test", Input: "weather?", Tools: []Tool{manual}, StateAccessor: mem,
		Request: tcrRequest(phChoice(t, tcrRequired)),
	}); err != nil {
		t.Fatal(err)
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingClientTools || st.ConsumedForcedToolChoiceKey == "" {
		t.Fatalf("status=%s key=%q", st.Status, st.ConsumedForcedToolChoiceKey)
	}
	text := tcrRun(t, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{manual}, StateAccessor: mem,
		Input:   []components.InputsUnion1{agdCallerOutput("call_1", `{"temperature":22}`)},
		Request: tcrRequest(phChoice(t, tcrRequired)),
	})
	if text != "It is 22 degrees." {
		t.Fatalf("text = %q", text)
	}
	if got := phWireChoice(t, sender, 1); got != tcrAuto {
		t.Fatalf("the first request after a client-tool resume must be relaxed, got %s", got)
	}
}
