package agent

// Coverage for the empty-final-response retry (model_result.go retryCurrentRequest,
// reached from run()).
//
// This path had zero test coverage despite running on the terminal turn of every
// tool-using conversation, and its opt-out flag StrictFinalResponse had no test
// references at all. It is also a documented divergence from upstream: upstream
// treats an empty `output` array as an invalid response and errors, while here
// the SDK's separate OutputText convenience field makes an empty Output
// unreliable as an "invalid response" signal, so the port retries with
// tool_choice=none instead. Both halves of that behavior are asserted here.

import (
	"context"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// emptyFinalResponse has neither Output items nor OutputText, which is what
// isEmptyFinalResponse treats as "the model returned nothing".
func emptyFinalResponse(id string) components.OpenResponsesResult {
	return components.OpenResponsesResult{ID: id}
}

// TestEmptyFinalResponseTriggersRetryWithToolChoiceNone asserts that when a tool
// round is followed by an empty final response, the loop retries once and pins
// tool_choice=none so the model answers instead of calling another tool.
func TestEmptyFinalResponseTriggersRetryWithToolChoiceNone(t *testing.T) {
	ctx := context.Background()
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "tool output", nil
	}})

	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	toolTurn := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(call),
	}}
	empty := emptyFinalResponse("resp_2")
	recovered := components.OpenResponsesResult{ID: "resp_3", OutputText: openrouter.String("recovered answer")}

	createdTool := operations.CreateCreateResponsesResponseOpenResponsesResult(toolTurn)
	createdEmpty := operations.CreateCreateResponsesResponseOpenResponsesResult(empty)
	createdRecovered := operations.CreateCreateResponsesResponseOpenResponsesResult(recovered)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		&createdTool, &createdEmpty, &createdRecovered,
	}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if text != "recovered answer" {
		t.Fatalf("retry result did not become the final text: %q", text)
	}
	if sender.calls != 3 {
		t.Fatalf("expected 3 requests (tool round, empty final, retry), got %d", sender.calls)
	}

	// The retry is the third request, and must pin tool_choice=none while tools
	// are still present -- that is what stops it looping back into a tool call.
	retryReq := sender.requests[2]
	if len(retryReq.Tools) == 0 {
		t.Fatal("retry request unexpectedly dropped the tools")
	}
	if retryReq.ToolChoice == nil {
		t.Fatal("retry request must set tool_choice=none, got nil")
	}
	if retryReq.ToolChoice.OpenAIResponsesToolChoiceNone == nil {
		t.Fatalf("retry tool_choice should be none, got %+v", retryReq.ToolChoice)
	}
}

// TestStrictFinalResponseSkipsRetry asserts the opt-out: with
// StrictFinalResponse the empty response is accepted as final and no extra
// request is sent.
func TestStrictFinalResponseSkipsRetry(t *testing.T) {
	ctx := context.Background()
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "tool output", nil
	}})

	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	toolTurn := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(call),
	}}
	empty := emptyFinalResponse("resp_2")

	createdTool := operations.CreateCreateResponsesResponseOpenResponsesResult(toolTurn)
	createdEmpty := operations.CreateCreateResponsesResponseOpenResponsesResult(empty)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdTool, &createdEmpty}}

	result, err := CallModel(ctx, sender, CallModelInput{
		Model:               "openai/test",
		Input:               "hi",
		Tools:               []Tool{tool},
		StrictFinalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if text != "" {
		t.Fatalf("StrictFinalResponse should surface the empty response as-is, got %q", text)
	}
	if sender.calls != 2 {
		t.Fatalf("StrictFinalResponse must not retry: expected 2 requests, got %d", sender.calls)
	}
}

// TestEmptyFirstResponseWithoutToolRoundIsNotRetried pins the other guard on the
// retry condition: it requires a completed tool round. An empty response on a
// plain no-tools call is returned as-is rather than costing a second request.
func TestEmptyFirstResponseWithoutToolRoundIsNotRetried(t *testing.T) {
	ctx := context.Background()
	empty := emptyFinalResponse("resp_1")
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(empty)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("no tool round means no retry: expected 1 request, got %d", sender.calls)
	}
}
