package agent

import (
	"context"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream hitl-tool.test.ts "threads the executed tool call into
// the hook context" (#91): under the streaming CallModel loop, execute and
// OnToolCalled must see Turn.ToolCall populated from the executed call.

func TestExecuteContextCarriesExecutedToolCallInLoop(t *testing.T) {
	var seen *components.OutputFunctionCallItem
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(_ context.Context, _ sampleInput, ec ToolExecuteContext) (any, error) {
		seen = ec.Turn.ToolCall
		return "ok", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "r2", OutputText: openrouter.String("done")}
	result, err := CallModel(context.Background(), twoTurnSender(first, second), CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("execute saw Turn.ToolCall == nil under the CallModel loop")
	}
	if seen.CallID != "call_1" || seen.Name != "search" || seen.Arguments != `{"query":"go"}` {
		t.Fatalf("threaded tool call = %+v", seen)
	}
}

func TestHITLOnToolCalledSeesToolCallID(t *testing.T) {
	var seenID string
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "review", OnToolCalled: func(_ context.Context, _ sampleInput, ec ToolExecuteContext) (any, bool, error) {
		if ec.Turn.ToolCall != nil {
			seenID = ec.Turn.ToolCall.CallID
		}
		return nil, false, nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_hitl", Name: "review", Arguments: `{"query":"go"}`}
	resp := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	result, err := CallModel(context.Background(), &fakeSender{responses: []*operations.CreateResponsesResponse{&resp}}, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingHITL {
		t.Fatalf("status = %s", state.Status)
	}
	if seenID != "call_hitl" {
		t.Fatalf("OnToolCalled saw call id %q, want call_hitl", seenID)
	}
}

func TestCallerProvidedTurnToolCallWins(t *testing.T) {
	var seen *components.OutputFunctionCallItem
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(_ context.Context, _ sampleInput, ec ToolExecuteContext) (any, error) {
		seen = ec.Turn.ToolCall
		return "ok", nil
	}})
	provided := &components.OutputFunctionCallItem{CallID: "provided", Name: "search", Arguments: "{}"}
	if _, err := ExecuteTool(context.Background(), tool, ParsedToolCall{CallID: "exec", Name: "search", RawArgs: `{"query":"x"}`}, ToolExecuteContext{Turn: TurnContext{ToolCall: provided}}); err != nil {
		t.Fatal(err)
	}
	if seen != provided {
		t.Fatalf("caller-provided Turn.ToolCall must win, got %+v", seen)
	}
}

func TestExecuteContextCarriesConversationID(t *testing.T) {
	var seen string
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(_ context.Context, _ sampleInput, ec ToolExecuteContext) (any, error) {
		seen = ec.ConversationID
		return "ok", nil
	}})
	state := CreateInitialState()
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "r2", OutputText: openrouter.String("done")}
	result, err := CallModel(context.Background(), twoTurnSender(first, second), CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}, State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen == "" || seen != state.ID {
		t.Fatalf("ConversationID = %q, want %q", seen, state.ID)
	}
}
