package agent

import (
	"context"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// model-result-replay-compaction.test.ts: CallModelInput.StreamReplay
// reaches ModelResult's fan-out streams. Under the default (full) replay a
// subscriber attached after the run replays every delta; under
// active-consumers, history every attached consumer already read is
// released once they leave, so a late subscriber sees none of it.
func TestCallModelStreamReplayModes(t *testing.T) {
	run := func(t *testing.T, replay StreamReplay) []string {
		t.Helper()
		resp := completedResponse("hello world")
		events := []components.StreamEvents{textDelta("hello ", 1), textDelta("world", 2), completedEvent(resp, 3)}
		sender := &fakeSender{responses: []*operations.CreateResponsesResponse{streamingResponse(t, events)}}
		result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi", StreamReplay: replay})
		if err != nil {
			t.Fatal(err)
		}
		// A first consumer attached for the whole run reads every delta.
		first, firstWait := result.TextStream(context.Background())
		var firstGot []string
		for d := range first {
			firstGot = append(firstGot, d)
		}
		if err := firstWait(); err != nil || strings.Join(firstGot, "") != "hello world" {
			t.Fatalf("attached consumer got %q err=%v", firstGot, err)
		}
		if _, err := result.Text(context.Background()); err != nil {
			t.Fatal(err)
		}
		ch, wait := result.TextStream(context.Background())
		var got []string
		for d := range ch {
			got = append(got, d)
		}
		if err := wait(); err != nil {
			t.Fatalf("late subscriber error: %v", err)
		}
		return got
	}
	if got := strings.Join(run(t, ""), ""); got != "hello world" {
		t.Fatalf("full replay: late subscriber should replay all deltas, got %q", got)
	}
	if got := run(t, StreamReplayActiveConsumers); len(got) != 0 {
		t.Fatalf("active-consumers: late subscriber should get no released history, got %q", got)
	}
}

// extractToolCallsFromResponse keeps a call whose arguments are not valid
// JSON (arguments stay the raw string) instead of dropping it, so the call
// is still paired with a function_call_output (a parse error) and the
// follow-up request is not sent with a dangling function_call.
func TestUnparseableToolArgumentsStillGetAnOutput(t *testing.T) {
	ran := 0
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		ran++
		return "ok", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_bad", Name: "search", Arguments: `{"query": "go"`, Type: components.OutputFunctionCallItemTypeFunctionCall}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("recovered")}
	c1 := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	c2 := operations.CreateCreateResponsesResponseOpenResponsesResult(second)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&c1, &c2}}

	calls := ExtractToolCallsFromResponse(first)
	if len(calls) != 1 || calls[0].Arguments != `{"query": "go"` {
		t.Fatalf("unparseable call must be kept with raw string arguments, got %#v", calls)
	}

	hooks := NewHooksManager()
	preToolUse := 0
	hooks.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		preToolUse++
		return VoidResult[PreToolUseResult](), nil
	}})
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "recovered" || ran != 0 || preToolUse != 0 {
		t.Fatalf("text=%q ran=%d preToolUse=%d: the body and hooks must not run on unparseable arguments", text, ran, preToolUse)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("expected a follow-up request, got %d", len(sender.requests))
	}
	var output *components.FunctionCallOutputItem
	for _, item := range sender.requests[1].Input.ArrayOfInputsUnion1 {
		if item.FunctionCallOutputItem != nil && item.FunctionCallOutputItem.CallID == "call_bad" {
			output = item.FunctionCallOutputItem
		}
	}
	if output == nil || output.Output.Str == nil || !strings.Contains(*output.Output.Str, "error") {
		t.Fatalf("follow-up must pair the call with an error output, got %#v", output)
	}
}
