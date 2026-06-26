package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func GenerateConversationID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("conv_%d", time.Now().UnixNano())
	}
	return "conv_" + hex.EncodeToString(buf)
}

func CreateInitialState() ConversationState {
	now := time.Now().UnixMilli()
	return ConversationState{ID: GenerateConversationID(), Messages: []components.InputsUnion1{}, Status: ConversationStatusInProgress, CreatedAt: now, UpdatedAt: now}
}

func UpdateState(state ConversationState, mutate func(*ConversationState)) ConversationState {
	copyState := state
	copyState.Messages = append([]components.InputsUnion1{}, state.Messages...)
	copyState.PendingToolCalls = append([]ParsedToolCall{}, state.PendingToolCalls...)
	copyState.UnsentToolResults = append([]UnsentToolResult{}, state.UnsentToolResults...)
	mutate(&copyState)
	copyState.UpdatedAt = time.Now().UnixMilli()
	return copyState
}

func AppendToMessages(state ConversationState, items ...components.InputsUnion1) ConversationState {
	return UpdateState(state, func(s *ConversationState) { s.Messages = append(s.Messages, items...) })
}

func ToolRequiresApproval(ctx context.Context, t Tool, call ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (bool, error) {
	if IsServerTool(t) {
		return false, nil
	}
	if callLevel != nil {
		return callLevel(ctx, call, turn)
	}
	return t.RequiresApproval(ctx, call, turn)
}

func PartitionToolCalls(ctx context.Context, tools []Tool, calls []ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (approved []ParsedToolCall, pending []ParsedToolCall, err error) {
	for _, call := range calls {
		t := FindToolByName(tools, call.Name)
		if t == nil {
			approved = append(approved, call)
			continue
		}
		req, err := ToolRequiresApproval(ctx, t, call, turn, callLevel)
		if err != nil {
			return nil, nil, err
		}
		if req {
			pending = append(pending, call)
		} else {
			approved = append(approved, call)
		}
	}
	return approved, pending, nil
}

func CreateUnsentResult(call ParsedToolCall, output any) UnsentToolResult {
	return UnsentToolResult{CallID: call.CallID, Name: call.Name, Output: output}
}
func CreateRejectedResult(call ParsedToolCall, reason string) UnsentToolResult {
	return UnsentToolResult{CallID: call.CallID, Name: call.Name, Error: reason}
}

func UnsentResultsToAPIFormat(results []UnsentToolResult) []components.InputsUnion1 {
	items, err := UnsentResultsToAPIFormatWithError(results)
	if err != nil {
		return nil
	}
	return items
}

func UnsentResultsToAPIFormatWithError(results []UnsentToolResult) ([]components.InputsUnion1, error) {
	items := make([]components.InputsUnion1, 0, len(results))
	for _, r := range results {
		out := r.Output
		if r.Error != "" {
			out = map[string]any{"error": r.Error}
		}
		var s string
		if str, ok := out.(string); ok {
			s = str
		} else {
			b, err := json.Marshal(out)
			if err != nil {
				return nil, fmt.Errorf("marshal unsent tool output for %s: %w", r.CallID, err)
			}
			s = string(b)
		}
		item := components.FunctionCallOutputItem{CallID: r.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(s), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}
		items = append(items, components.CreateInputsUnion1FunctionCallOutputItem(item))
	}
	return items, nil
}
