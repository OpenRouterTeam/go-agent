package agent

import (
	"context"
	"errors"
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

func TestConversationStateSerializationRoundTrip(t *testing.T) {
	state := CreateInitialState()
	if state.Version != ConversationStateVersion {
		t.Fatalf("expected fresh state to carry ConversationStateVersion, got %d", state.Version)
	}

	json, err := SerializeConversationState(state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(json)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != ConversationStateVersion || restored.ID != state.ID || restored.Status != state.Status {
		t.Fatalf("round trip mismatch: got %+v, want %+v", restored, state)
	}
}

func TestConversationStateSerializationLegacyVersionless(t *testing.T) {
	legacy := `{"ID":"conv_legacy","Messages":[],"Status":"complete","CreatedAt":1600000000000,"UpdatedAt":1600000000100}`
	restored, err := DeserializeConversationState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != ConversationStateVersion {
		t.Fatalf("expected version-less legacy blob to normalize to version %d, got %d", ConversationStateVersion, restored.Version)
	}
	if restored.ID != "conv_legacy" || restored.Status != ConversationStatusComplete {
		t.Fatalf("unexpected restored state: %+v", restored)
	}
}

func TestConversationStateSerializationUnsupportedVersion(t *testing.T) {
	future := `{"Version":2,"ID":"conv_future","Messages":[],"Status":"in_progress","CreatedAt":1,"UpdatedAt":1}`
	_, err := DeserializeConversationState(future)
	var verErr *UnsupportedStateVersionError
	if !errors.As(err, &verErr) {
		t.Fatalf("expected *UnsupportedStateVersionError, got %v", err)
	}
	if verErr.Found != 2 || len(verErr.Supported) != 1 || verErr.Supported[0] != ConversationStateVersion {
		t.Fatalf("unexpected error details: %+v", verErr)
	}

	// The version guard must run before structural validation: a reshaped
	// future blob should still fail with UnsupportedStateVersionError, not a
	// misleading InvalidStateError about missing v1 fields.
	reshaped := `{"Version":2,"conversationId":"conv_future","history":[]}`
	_, err = DeserializeConversationState(reshaped)
	if !errors.As(err, &verErr) {
		t.Fatalf("expected *UnsupportedStateVersionError for reshaped future blob, got %v", err)
	}
}

func TestConversationStateSerializationInvalidShapes(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"missing id", `{"Messages":[],"Status":"in_progress"}`},
		{"messages not array", `{"ID":"x","Messages":"nope","Status":"in_progress"}`},
		{"missing status", `{"ID":"x","Messages":[]}`},
		{"missing createdAt", `{"ID":"x","Messages":[],"Status":"in_progress","UpdatedAt":1}`},
		{"missing updatedAt", `{"ID":"x","Messages":[],"Status":"in_progress","CreatedAt":1}`},
		{"not json", `not-json{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeserializeConversationState(tc.json)
			var invErr *InvalidStateError
			if !errors.As(err, &invErr) {
				t.Fatalf("expected *InvalidStateError, got %v", err)
			}
		})
	}
}

func TestConversationStateSerializationInjectsVersion(t *testing.T) {
	versionless := ConversationState{ID: "conv_no_ver", Messages: []components.InputsUnion1{}, Status: ConversationStatusInProgress, CreatedAt: 42, UpdatedAt: 43}
	if versionless.Version != 0 {
		t.Fatalf("expected zero-value Version on a hand-built state")
	}
	out, err := SerializeConversationState(versionless)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(out)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != ConversationStateVersion {
		t.Fatalf("expected serialize to inject version %d, got %d", ConversationStateVersion, restored.Version)
	}
}
