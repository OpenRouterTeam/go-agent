package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/max-output-tokens-truncation.test.ts.

type shellInput struct {
	Commands []string `json:"commands" jsonschema:"required"`
}

type weatherInput struct {
	City string `json:"city" jsonschema:"required"`
}

func functionCallOutput(id, callID, name, args string) components.OutputItems {
	return components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{ID: &id, CallID: callID, Name: name, Arguments: args, Type: components.OutputFunctionCallItemTypeFunctionCall})
}

// truncatedToolCallResponse is a turn the provider stopped at
// max_output_tokens two tokens into the tool call.
func truncatedToolCallResponse(output ...components.OutputItems) components.OpenResponsesResult {
	if len(output) == 0 {
		output = []components.OutputItems{functionCallOutput("fc_1", "call_1", "run_shell", `{"commands":`)}
	}
	reason := components.ReasonMaxOutputTokens
	return components.OpenResponsesResult{
		ID:                "resp_truncated",
		Model:             "test-model",
		Object:            components.OpenResponsesResultObjectResponse,
		Status:            components.OpenAIResponsesResponseStatusIncomplete,
		IncompleteDetails: &components.IncompleteDetails{Reason: &reason},
		Output:            output,
		ToolChoice:        components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto),
	}
}

// truncatedBatchResponse: three weather calls completed before the budget ran
// out inside the fourth call. The batch is one plan.
func truncatedBatchResponse() components.OpenResponsesResult {
	var out []components.OutputItems
	for i, city := range []string{"Paris", "London", "Tokyo"} {
		args, _ := json.Marshal(map[string]any{"city": city})
		idx := string(rune('0' + i))
		out = append(out, functionCallOutput("fc_weather_"+idx, "call_weather_"+idx, "get_weather", string(args)))
	}
	out = append(out, functionCallOutput("fc_shell", "call_shell", "run_shell", `{"commands":`))
	return truncatedToolCallResponse(out...)
}

func TestTruncatedResponseExtractsNoToolCalls(t *testing.T) {
	resp := truncatedToolCallResponse()
	if ResponseHasToolCalls(resp) {
		t.Fatal("ResponseHasToolCalls must be false for a max_output_tokens truncation")
	}
	if calls := ExtractToolCallsFromResponse(resp); len(calls) != 0 {
		t.Fatalf("ExtractToolCallsFromResponse = %#v, want none", calls)
	}
	// Not even the calls that completed before the cut-off.
	batch := truncatedBatchResponse()
	if ResponseHasToolCalls(batch) || len(ExtractToolCallsFromResponse(batch)) != 0 {
		t.Fatal("a truncated batch must yield no executable calls")
	}
	// A completed batch is unaffected by the guard.
	ok := truncatedBatchResponse()
	ok.Status = components.OpenAIResponsesResponseStatusCompleted
	ok.IncompleteDetails = nil
	if !ResponseHasToolCalls(ok) || len(ExtractToolCallsFromResponse(ok)) != 3 {
		t.Fatalf("a completed response must still yield its calls")
	}
	// An incomplete response for another reason is not the truncation guard.
	filtered := truncatedBatchResponse()
	reason := components.ReasonContentFilter
	filtered.IncompleteDetails = &components.IncompleteDetails{Reason: &reason}
	if !ResponseHasToolCalls(filtered) {
		t.Fatal("only max_output_tokens truncation suppresses tool calls")
	}
}

func TestTruncatedTurnFinalizesWithoutExecutingOrRerequesting(t *testing.T) {
	var mu sync.Mutex
	var executed []any
	shell := MustNewTool(ToolConfig[shellInput]{Name: "run_shell", Execute: func(_ context.Context, in shellInput, _ ToolExecuteContext) (any, error) {
		mu.Lock()
		executed = append(executed, in)
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	}})
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(truncatedToolCallResponse())
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "Run echo hello.", Tools: []Tool{shell}, StopWhen: []StopCondition{StepCountIs(3)}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(executed) != 0 {
		t.Fatalf("truncated call executed: %#v", executed)
	}
	if sender.calls != 1 {
		t.Fatalf("requests = %d, want 1 (no re-request on the exhausted budget)", sender.calls)
	}
	if resp.ID != "resp_truncated" || resp.Status != components.OpenAIResponsesResponseStatusIncomplete {
		t.Fatalf("response = %s/%s", resp.ID, resp.Status)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusComplete || len(state.PendingToolCalls) != 0 {
		t.Fatalf("truncated turn must finalize: status=%s pending=%d", state.Status, len(state.PendingToolCalls))
	}
}

func TestTruncatedBatchDoesNotExecuteCallsCompletedBeforeCutoff(t *testing.T) {
	var mu sync.Mutex
	executed := 0
	weather := MustNewTool(ToolConfig[weatherInput]{Name: "get_weather", Execute: func(context.Context, weatherInput, ToolExecuteContext) (any, error) {
		mu.Lock()
		executed++
		mu.Unlock()
		return map[string]any{"temperature": 22}, nil
	}})
	shell := MustNewTool(ToolConfig[shellInput]{Name: "run_shell", Execute: func(context.Context, shellInput, ToolExecuteContext) (any, error) {
		mu.Lock()
		executed++
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	}})
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(truncatedBatchResponse())
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "Weather in three cities, then run echo hello.", Tools: []Tool{weather, shell}, StopWhen: []StopCondition{StepCountIs(3)}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed != 0 || sender.calls != 1 {
		t.Fatalf("executed=%d requests=%d, want 0/1", executed, sender.calls)
	}
	// The caller gets the whole turn, cut-off item included, to resume from.
	var ids []string
	for _, item := range resp.Output {
		ids = append(ids, item.OutputFunctionCallItem.CallID)
	}
	if strings.Join(ids, ",") != "call_weather_0,call_weather_1,call_weather_2,call_shell" {
		t.Fatalf("output ids = %v", ids)
	}
}

// A streamed turn terminates with response.incomplete, not response.completed:
// the incomplete response must be returned (not "stream ended without
// response.completed"), its calls not executed, and the tool-call stream must
// still report what the model emitted.
func TestStreamingIncompleteTerminalEventFinalizes(t *testing.T) {
	executed := false
	shell := MustNewTool(ToolConfig[shellInput]{Name: "run_shell", Execute: func(context.Context, shellInput, ToolExecuteContext) (any, error) {
		executed = true
		return "ok", nil
	}})
	incomplete := components.CreateStreamEventsResponseIncomplete(components.StreamEventsResponseIncomplete{Response: truncatedToolCallResponse(), SequenceNumber: 1})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{streamingResponse(t, []components.StreamEvents{incomplete})}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "go", Tools: []Tool{shell}})
	if err != nil {
		t.Fatal(err)
	}
	stream, wait := result.ToolCallsStream(context.Background())
	var streamed []ParsedToolCall
	for c := range stream {
		streamed = append(streamed, c)
	}
	if err := wait(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatalf("incomplete terminal event must not error: %v", err)
	}
	if resp.Status != components.OpenAIResponsesResponseStatusIncomplete || executed || sender.calls != 1 {
		t.Fatalf("status=%s executed=%v calls=%d", resp.Status, executed, sender.calls)
	}
	if len(streamed) != 1 || streamed[0].CallID != "call_1" {
		t.Fatalf("tool-call stream must still report the emitted call: %#v", streamed)
	}
	calls, _ := result.ToolCalls(context.Background())
	if len(calls) != 0 {
		t.Fatalf("ToolCalls must not report the truncated call as executable: %#v", calls)
	}
}

func TestStreamingFailedTerminalEventSurfacesError(t *testing.T) {
	failedResp := completedResponse("")
	failedResp.Status = components.OpenAIResponsesResponseStatusFailed
	failed := components.CreateStreamEventsResponseFailed(components.StreamEventsResponseFailed{Response: failedResp, SequenceNumber: 1})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{streamingResponse(t, []components.StreamEvents{failed})}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err == nil || !strings.Contains(err.Error(), "response failed") {
		t.Fatalf("expected a response failed error, got %v", err)
	}
}
