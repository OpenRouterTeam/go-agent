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

func truncatedToolCallResponse(output []components.OutputItems) components.OpenResponsesResult {
	if output == nil {
		output = []components.OutputItems{
			components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "call_1", Name: "run_shell", Arguments: `{"commands":`}),
		}
	}
	reason := components.ReasonMaxOutputTokens
	return components.OpenResponsesResult{
		ID:                "resp_truncated",
		Object:            components.OpenResponsesResultObjectResponse,
		Model:             "test-model",
		Status:            components.OpenAIResponsesResponseStatusIncomplete,
		IncompleteDetails: &components.IncompleteDetails{Reason: &reason},
		Output:            output,
		ToolChoice:        components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto),
	}
}

func truncatedBatchResponse() components.OpenResponsesResult {
	var out []components.OutputItems
	for i, city := range []string{"Paris", "London", "Tokyo"} {
		args, _ := json.Marshal(map[string]string{"city": city})
		out = append(out, components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "call_weather_" + string(rune('0'+i)), Name: "get_weather", Arguments: string(args)}))
	}
	out = append(out, components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "call_shell", Name: "run_shell", Arguments: `{"commands":`}))
	return truncatedToolCallResponse(out)
}

type shellInput struct {
	Commands []string `json:"commands"`
}
type cityInput struct {
	City string `json:"city"`
}

func TestTruncatedResponseYieldsNoToolCalls(t *testing.T) {
	resp := truncatedToolCallResponse(nil)
	if ResponseHasToolCalls(resp) {
		t.Fatal("ResponseHasToolCalls must be false for a max_output_tokens-truncated response")
	}
	if calls := ExtractToolCallsFromResponse(resp); len(calls) != 0 {
		t.Fatalf("ExtractToolCallsFromResponse = %#v, want none", calls)
	}
	// A completed (non-truncated) response with the same call still counts.
	ok := truncatedBatchResponse()
	ok.Status = components.OpenAIResponsesResponseStatusCompleted
	ok.IncompleteDetails = nil
	if !ResponseHasToolCalls(ok) || len(ExtractToolCallsFromResponse(ok)) != 3 {
		t.Fatal("a completed response's tool calls must still be extracted")
	}
}

func TestTruncatedTurnFinalizesWithoutExecutingOrReRequesting(t *testing.T) {
	var mu sync.Mutex
	var executed []any
	shell := MustNewTool(ToolConfig[shellInput]{Name: "run_shell", Execute: func(_ context.Context, in shellInput, _ ToolExecuteContext) (any, error) {
		mu.Lock()
		executed = append(executed, in)
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	}})
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(truncatedToolCallResponse(nil))
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "Run echo hello.", StopWhen: []StopCondition{StepCountIs(3)}, Tools: []Tool{shell}})
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
		t.Fatalf("response = %s/%s, want resp_truncated/incomplete", resp.ID, resp.Status)
	}
}

func TestTruncatedBatchExecutesNoneOfTheCompletedCalls(t *testing.T) {
	var mu sync.Mutex
	executed := 0
	weather := MustNewTool(ToolConfig[cityInput]{Name: "get_weather", Execute: func(context.Context, cityInput, ToolExecuteContext) (any, error) {
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
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "Weather, then shell.", StopWhen: []StopCondition{StepCountIs(3)}, Tools: []Tool{weather, shell}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed != 0 {
		t.Fatalf("executed %d calls from a truncated batch, want 0", executed)
	}
	if sender.calls != 1 {
		t.Fatalf("requests = %d, want 1", sender.calls)
	}
	var ids []string
	for _, item := range resp.Output {
		if item.OutputFunctionCallItem != nil {
			ids = append(ids, item.OutputFunctionCallItem.CallID)
		}
	}
	want := []string{"call_weather_0", "call_weather_1", "call_weather_2", "call_shell"}
	if len(ids) != len(want) {
		t.Fatalf("caller must see the whole turn, got %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("output[%d] = %s, want %s", i, ids[i], want[i])
		}
	}
}

// Streaming variant: the terminal frame is response.incomplete, not
// response.completed. It must resolve to the truncated response rather than
// erroring, and still execute nothing.
func TestStreamedResponseIncompleteIsTerminalAndNotExecuted(t *testing.T) {
	executed := 0
	var mu sync.Mutex
	shell := MustNewTool(ToolConfig[shellInput]{Name: "run_shell", Execute: func(context.Context, shellInput, ToolExecuteContext) (any, error) {
		mu.Lock()
		executed++
		mu.Unlock()
		return "ok", nil
	}})
	incomplete := components.CreateStreamEventsResponseIncomplete(components.StreamEventsResponseIncomplete{
		Response: truncatedToolCallResponse(nil), SequenceNumber: 2,
		Type: components.StreamEventsResponseIncompleteTypeResponseIncomplete,
	})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		streamingResponse(t, []components.StreamEvents{textDelta("thinking", 1), incomplete}),
	}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "go", Tools: []Tool{shell}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatalf("response.incomplete must be a terminal event, got error %v", err)
	}
	if resp.Status != components.OpenAIResponsesResponseStatusIncomplete || executed != 0 || sender.calls != 1 {
		t.Fatalf("status=%s executed=%d calls=%d", resp.Status, executed, sender.calls)
	}
}

// response.failed is terminal too: it must surface as an error through the
// consumers rather than a "stream ended" or a silent success.
func TestStreamedResponseFailedSurfacesError(t *testing.T) {
	failedResp := completedResponse("")
	failedResp.Status = components.OpenAIResponsesResponseStatusFailed
	failed := components.CreateStreamEventsResponseFailed(components.StreamEventsResponseFailed{
		Response: failedResp, SequenceNumber: 1,
		Type: components.StreamEventsResponseFailedTypeResponseFailed,
	})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{streamingResponse(t, []components.StreamEvents{failed})}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err == nil || !strings.Contains(err.Error(), "response failed") {
		t.Fatalf("err = %v, want response failed", err)
	}
}
