package agent

// Ports tests/unit/max-output-tokens-truncation.test.ts, plus the streamed
// terminal-event cases (response.incomplete accepted as the response,
// response.failed surfaced as an error).

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type motCommands struct {
	Commands []string `json:"commands"`
}

type motCity struct {
	City string `json:"city"`
}

// motTruncated marks resp as cut off at max_output_tokens.
func motTruncated(resp components.OpenResponsesResult) components.OpenResponsesResult {
	reason := components.ReasonMaxOutputTokens
	resp.Status = components.OpenAIResponsesResponseStatusIncomplete
	resp.IncompleteDetails = &components.IncompleteDetails{Reason: &reason}
	return resp
}

// motTruncatedToolCallResponse: the budget ran out inside run_shell's arguments.
func motTruncatedToolCallResponse() components.OpenResponsesResult {
	return motTruncated(phResp("resp_truncated", phFnCall("call_shell", "run_shell", `{"commands":`)))
}

// motTruncatedBatchResponse: three complete weather calls, then a cut-off one.
func motTruncatedBatchResponse() components.OpenResponsesResult {
	resp := phResp("resp_truncated_batch")
	for i, city := range []string{"Paris", "London", "Tokyo"} {
		resp.Output = append(resp.Output, phFnCall("call_weather_"+string(rune('0'+i)), "get_weather", `{"city":"`+city+`"}`))
	}
	resp.Output = append(resp.Output, phFnCall("call_shell", "run_shell", `{"commands":`))
	return motTruncated(resp)
}

func motTools(executed *phCounter) []Tool {
	shell := MustNewTool(ToolConfig[motCommands]{
		Name:        "run_shell",
		InputSchema: map[string]any{"type": "object", "required": []any{"commands"}, "properties": map[string]any{"commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
		Execute: func(_ context.Context, in motCommands, _ ToolExecuteContext) (any, error) {
			executed.add(in)
			return map[string]any{"ok": true}, nil
		},
	})
	weather := MustNewTool(ToolConfig[motCity]{
		Name: "get_weather", InputSchema: phObjectSchema("city"),
		Execute: func(_ context.Context, in motCity, _ ToolExecuteContext) (any, error) {
			executed.add(in)
			return map[string]any{"temperature": 22}, nil
		},
	})
	return []Tool{weather, shell}
}

// upstream: "extracts no tool calls from a response truncated at max_output_tokens"
func TestTruncatedResponseHasNoToolCalls(t *testing.T) {
	for _, resp := range []components.OpenResponsesResult{motTruncatedToolCallResponse(), motTruncatedBatchResponse()} {
		if ResponseHasToolCalls(resp) {
			t.Fatalf("%s: a max_output_tokens truncation has no executable tool calls", resp.ID)
		}
		if calls := ExtractToolCallsFromResponse(resp); len(calls) != 0 {
			t.Fatalf("%s: ExtractToolCallsFromResponse = %+v, want none", resp.ID, calls)
		}
	}

	// Edge cases: only the max_output_tokens reason suppresses calls.
	complete := phResp("ok", phFnCall("c", "get_weather", `{"city":"Paris"}`))
	if !ResponseHasToolCalls(complete) || len(ExtractToolCallsFromResponse(complete)) != 1 {
		t.Fatal("a completed response keeps its tool calls")
	}
	contentFilter := components.ReasonContentFilter
	filtered := complete
	filtered.Status = components.OpenAIResponsesResponseStatusIncomplete
	filtered.IncompleteDetails = &components.IncompleteDetails{Reason: &contentFilter}
	if !ResponseHasToolCalls(filtered) {
		t.Fatal("an incomplete response for another reason keeps its tool calls")
	}
	noDetails := complete
	noDetails.Status = components.OpenAIResponsesResponseStatusIncomplete
	if !ResponseHasToolCalls(noDetails) {
		t.Fatal("an incomplete status without details keeps its tool calls")
	}
	if ResponseHasToolCalls(phTextResp("t", "hi")) {
		t.Fatal("a text-only response has no tool calls")
	}
}

// upstream: "finalizes on the truncated turn without executing the partial
// call or requesting again"
func TestTruncatedTurnFinalizesWithoutExecuting(t *testing.T) {
	ctx := context.Background()
	var executed phCounter
	sender := phSender(motTruncatedToolCallResponse())
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "Run echo hello.", Tools: motTools(&executed),
		StopWhen: []StopCondition{StepCountIs(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if executed.count() != 0 || sender.calls != 1 {
		t.Fatalf("executed=%v calls=%d; a truncated turn runs nothing and is not re-requested", executed.values(), sender.calls)
	}
	if resp.ID != "resp_truncated" || resp.Status != components.OpenAIResponsesResponseStatusIncomplete {
		t.Fatalf("Response must be the truncated turn: id=%s status=%s", resp.ID, resp.Status)
	}
	st, err := result.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != ConversationStatusComplete || len(st.PendingToolCalls) != 0 {
		t.Fatalf("the run completes (no pause, no pending): status=%s pending=%+v", st.Status, st.PendingToolCalls)
	}
	if calls, err := result.ToolCalls(ctx); err != nil || len(calls) != 0 {
		t.Fatalf("ToolCalls = %+v, %v", calls, err)
	}
}

// upstream: "does not execute the calls that completed before the cut-off either"
func TestTruncatedBatchExecutesNoCalls(t *testing.T) {
	ctx := context.Background()
	var executed phCounter
	sender := phSender(motTruncatedBatchResponse())
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "Weather in three cities, then run echo hello.", Tools: motTools(&executed),
		StopWhen: []StopCondition{StepCountIs(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if executed.count() != 0 || sender.calls != 1 {
		t.Fatalf("executed=%v calls=%d", executed.values(), sender.calls)
	}
	var ids []string
	for _, item := range resp.Output {
		if item.OutputFunctionCallItem != nil {
			ids = append(ids, item.OutputFunctionCallItem.CallID)
		}
	}
	if !phEqualStrings(ids, []string{"call_weather_0", "call_weather_1", "call_weather_2", "call_shell"}) {
		t.Fatalf("the caller gets the whole turn to resume from, got %v", ids)
	}
}

// A truncated turn carrying an approval-gated call does not pause: nothing
// in the batch is executable, so there is nothing to approve.
func TestTruncatedTurnDoesNotPauseForApproval(t *testing.T) {
	ctx := context.Background()
	var runs phCounter
	gated := phOKTool("gated", &runs, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	sender := phSender(motTruncated(phResp("r", phFnCall("call_g", "gated", `{"target":"x"}`))))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{gated}})
	if err != nil {
		t.Fatal(err)
	}
	requires, err := result.RequiresApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requires || runs.count() != 0 || sender.calls != 1 {
		t.Fatalf("requires=%v runs=%d calls=%d", requires, runs.count(), sender.calls)
	}
}

// A streamed `response.incomplete` terminal event is a materialized response,
// not a "stream ended without response.completed" error.
func TestStreamedResponseIncompleteIsAccepted(t *testing.T) {
	ctx := context.Background()
	var executed phCounter
	truncated := completedResponse("")
	truncated.OutputText = nil
	truncated.ID = "resp_stream_truncated"
	truncated.Output = []components.OutputItems{phFnCall("call_shell", "run_shell", `{"commands":`)}
	truncated = motTruncated(truncated)

	sender := &fakeSender{responses: streamingResponsesOf(t, []components.StreamEvents{
		components.CreateStreamEventsResponseIncomplete(components.StreamEventsResponseIncomplete{Response: truncated, SequenceNumber: 1}),
	})}
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "Run echo hello.", Tools: motTools(&executed)})
	if err != nil {
		t.Fatal(err)
	}
	full, waitFull := result.FullResponsesStream(ctx)
	var types []string
	for ev := range full {
		types = append(types, ev.Type)
	}
	if err := waitFull(); err != nil {
		t.Fatalf("an incomplete terminal event is not a stream error: %v", err)
	}
	resp, err := result.Response(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_stream_truncated" || resp.Status != components.OpenAIResponsesResponseStatusIncomplete {
		t.Fatalf("Response = %s/%s", resp.ID, resp.Status)
	}
	if executed.count() != 0 || sender.calls != 1 {
		t.Fatalf("executed=%d calls=%d", executed.count(), sender.calls)
	}
	if !phEqualStrings(types, []string{"turn.start", "response.event", "response.completed", "turn.end"}) {
		t.Fatalf("full stream event order = %v", types)
	}
}

// A streamed `response.failed` terminal event surfaces as an error through
// every consumer instead of a silent empty response.
func TestStreamedResponseFailedSurfacesError(t *testing.T) {
	ctx := context.Background()
	failed := completedResponse("")
	failed.Status = components.OpenAIResponsesResponseStatusFailed
	failed.Error = &components.ResponsesErrorField{Code: components.CodeServerError, Message: "upstream exploded"}
	sender := &fakeSender{responses: streamingResponsesOf(t, []components.StreamEvents{
		textDelta("partial", 1),
		components.CreateStreamEventsResponseFailed(components.StreamEventsResponseFailed{Response: failed, SequenceNumber: 2}),
	})}
	mem := &phMemState{}
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", StateAccessor: mem})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("Response must surface the failure, got %v", err)
	}
	ch, wait := result.TextStream(ctx)
	for range ch {
	}
	if err := wait(); err == nil {
		t.Fatal("TextStream must surface the failure")
	}
	if _, err := phDrainToolStream(ctx, result); err == nil {
		t.Fatal("ToolStream must surface the failure")
	}
	if st := mem.get(); st != nil && len(phUserTexts(st.Messages)) != 0 {
		t.Fatalf("a failed request must not persist the user turn: %+v", st.Messages)
	}
}

// Upstream buildToolCallStream (getToolCallsStream) yields each call as its
// output item arrives, so it still reports the model's emitted calls on a
// truncated turn — and the Go doc comment on ResponseHasToolCalls promises
// the same for ToolCallsStream.
func TestToolCallsStreamReportsCallsOfTruncatedTurn(t *testing.T) {
	ctx := context.Background()
	var executed phCounter
	result, err := CallModel(ctx, phSender(motTruncatedBatchResponse()), CallModelInput{Model: "openai/test", Input: "go", Tools: motTools(&executed)})
	if err != nil {
		t.Fatal(err)
	}
	ch, wait := result.ToolCallsStream(ctx)
	var names []string
	for c := range ch {
		names = append(names, c.CallID)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("ToolCallsStream must report the emitted calls of a truncated turn")
	}
}
