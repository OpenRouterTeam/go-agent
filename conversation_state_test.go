package agent

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// persistResponseToState is a test fixture that appends a response's output
// items to state and records its id. It fails the test on an encode error
// rather than swallowing it.
func persistResponseToState(t *testing.T, state ConversationState, resp components.OpenResponsesResult) ConversationState {
	t.Helper()
	items, err := responseInputItemsWithError(resp)
	if err != nil {
		t.Fatalf("persistResponseToState: %v", err)
	}
	return appendResponseItemsToState(state, resp, items)
}

func TestConversationStatePersistsFunctionCallBeforeOutput(t *testing.T) {
	state := CreateInitialState()
	fc := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(fc)}}
	state = persistResponseToState(t, state, resp)
	output := UnsentResultsToAPIFormat([]UnsentToolResult{{CallID: "call_1", Name: "search", Output: map[string]any{"ok": true}}})
	state = AppendToMessages(state, output...)

	if state.PreviousResponseID == nil || *state.PreviousResponseID != "resp_1" {
		t.Fatalf("previous response id not carried")
	}
	if len(state.Messages) != 2 {
		t.Fatalf("expected function_call then function_call_output, got %d", len(state.Messages))
	}
	if state.Messages[0].FunctionCallItem == nil && state.Messages[0].OutputFunctionCallItem == nil {
		t.Fatalf("first persisted item should be function_call")
	}
	if state.Messages[1].FunctionCallOutputItem == nil {
		t.Fatalf("second persisted item should be function_call_output")
	}
}

func TestPartitionToolCallsApproval(t *testing.T) {
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "delete", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "ok", nil }})
	approved, pending, err := PartitionToolCalls(context.Background(), []Tool{tool}, []ParsedToolCall{{CallID: "c1", Name: "delete", RawArgs: `{"query":"x"}`}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 0 || len(pending) != 1 {
		t.Fatalf("expected approval partition to hold pending call")
	}
}
