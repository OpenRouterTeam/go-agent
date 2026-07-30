package agent

// Coverage for the ModelResult read accessors, several of which are in the
// contract's required public API and had no deterministic test: ItemsStream,
// ToolCalls, ToolCallsStream, NewMessagesStream, PendingToolCalls,
// RequiresApproval, ContextUpdates and Cancel were all 0%.
//
// PendingToolCalls and RequiresApproval matter most: existing tests only ever
// reached a paused run through State(), so the public read path a caller
// actually uses to discover "what is waiting on me" was never exercised.

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// pausedResult builds a run that stops at awaiting_approval with one pending call.
func pausedResult(t *testing.T) (*ModelResult, *fakeSender) {
	t.Helper()
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		t.Fatal("approval-required tool must not execute before approval")
		return nil, nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "danger", Arguments: `{"query":"go"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(call),
	}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result, sender
}

func TestPausedRunExposesPendingCallsThroughPublicAccessors(t *testing.T) {
	ctx := context.Background()
	result, _ := pausedResult(t)

	needsApproval, err := result.RequiresApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !needsApproval {
		t.Fatal("RequiresApproval should report true while awaiting approval")
	}

	pending, err := result.PendingToolCalls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending call, got %d", len(pending))
	}
	if pending[0].CallID != "call_1" || pending[0].Name != "danger" {
		t.Fatalf("pending call not surfaced correctly: %+v", pending[0])
	}

	// The returned slice must be a copy: mutating it cannot corrupt the run's
	// own state, or a caller inspecting pending calls could break the resume.
	pending[0].CallID = "mutated"
	again, err := result.PendingToolCalls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].CallID != "call_1" {
		t.Fatalf("PendingToolCalls handed out an aliased slice: %q", again[0].CallID)
	}
}

func TestResultItemsStreamYieldsOutputItems(t *testing.T) {
	ctx := context.Background()
	result, _ := pausedResult(t)

	ch, wait := result.ItemsStream(ctx)
	var items []components.OutputItems
	for item := range ch {
		items = append(items, item)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(items))
	}
	if items[0].OutputFunctionCallItem == nil || items[0].OutputFunctionCallItem.CallID != "call_1" {
		t.Fatalf("output item not surfaced: %+v", items[0])
	}
}

func TestResultToolCallAccessorsAgree(t *testing.T) {
	ctx := context.Background()
	result, _ := pausedResult(t)

	calls, err := result.ToolCalls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "danger" {
		t.Fatalf("ToolCalls = %+v, want one call to danger", calls)
	}

	// The streaming variant must report the same calls as the snapshot.
	ch, wait := result.ToolCallsStream(ctx)
	var streamed []ParsedToolCall
	for c := range ch {
		streamed = append(streamed, c)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != len(calls) {
		t.Fatalf("ToolCallsStream saw %d calls, ToolCalls saw %d", len(streamed), len(calls))
	}
	if len(streamed) > 0 && streamed[0].CallID != calls[0].CallID {
		t.Fatalf("ToolCallsStream/ToolCalls disagree: %q vs %q", streamed[0].CallID, calls[0].CallID)
	}
}

func TestResultNewMessagesStreamAndContextUpdates(t *testing.T) {
	ctx := context.Background()
	result, _ := pausedResult(t)

	ch, wait := result.NewMessagesStream(ctx)
	var msgs []components.InputsUnion1
	for m := range ch {
		msgs = append(msgs, m)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	// A paused turn records the model's function_call as a new message so the
	// resume can replay it.
	if len(msgs) == 0 {
		t.Fatal("NewMessagesStream produced nothing for a turn with a tool call")
	}

	updates, waitCtx := result.ContextUpdates(ctx)
	for range updates {
	}
	if err := waitCtx(); err != nil {
		t.Fatal(err)
	}
}

// TestResultCancelStopsRunWithoutPanic covers Cancel, the public cancellation
// entry point, which had no test at all. Calling it after completion must be
// safe, and calling it twice must not panic.
func TestResultCancelStopsRunWithoutPanic(t *testing.T) {
	ctx := context.Background()
	resp := completedResponse("done")
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}

	result.Cancel()
	result.Cancel()

	// Reads after cancellation still return the completed result rather than
	// blocking or panicking.
	if _, err := result.Text(ctx); err != nil {
		t.Fatalf("Text after Cancel should still return the completed text: %v", err)
	}
}
