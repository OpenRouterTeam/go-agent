package agent

import (
	"encoding/json"
	"fmt"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type ChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []ParsedToolCall `json:"tool_calls,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
}

// ToChatMessage converts an OpenRouter Responses result into an OpenAI
// Chat-style assistant message, carrying id/model/status metadata and any
// unsupported content so a round trip through FromChatMessages is lossless.
func ToChatMessage(resp components.OpenResponsesResult) ChatMessage {
	return ChatMessage{Role: "assistant", Content: ExtractTextFromResponse(resp), ToolCalls: ExtractToolCallsFromResponse(resp), Metadata: map[string]any{"id": resp.ID, "model": resp.Model, "status": string(resp.Status), "unsupported_content": ScanUnsupportedContent(resp)}}
}

// FromChatMessages converts OpenAI Chat-style messages into OpenRouter input
// items. It returns an error rather than degrading an encoding failure into a
// fallback value, matching the contract's errors-are-values rule.
func FromChatMessages(messages []ChatMessage) ([]components.InputsUnion1, error) {
	out := []components.InputsUnion1{}
	for _, m := range messages {
		if m.Role == "tool" {
			result, err := contentToStringWithError(m.Content)
			if err != nil {
				return nil, fmt.Errorf("encode chat tool content for %q: %w", m.ToolCallID, err)
			}
			out = append(out, components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: m.ToolCallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(result), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}))
			continue
		}
		if m.Role == "assistant" {
			// Upstream (#11/#41): an assistant message's content is
			// stringified, and the message item is omitted only when it has
			// no content AND tool calls replace it. A content-less assistant
			// message with no tool calls still round-trips as an empty message.
			content, err := contentToStringWithError(m.Content)
			if err != nil {
				return nil, fmt.Errorf("encode chat assistant content: %w", err)
			}
			if content != "" || len(m.ToolCalls) == 0 {
				out = append(out, easyMessage(m.Role, components.CreateEasyInputMessageContentUnion2Str(content)))
			}
		} else if text, ok := m.Content.(string); ok {
			out = append(out, easyMessage(m.Role, components.CreateEasyInputMessageContentUnion2Str(text)))
		} else {
			out = append(out, easyMessage(m.Role, components.CreateEasyInputMessageContentUnion2Any(m.Content)))
		}
		for _, call := range m.ToolCalls {
			// Chat tool calls carry a single id; it becomes both the item id
			// and the call_id, and `arguments` is forwarded as already
			// serialized (never re-stringified).
			callID := call.CallID
			if callID == "" {
				callID = call.ID
			}
			itemID := call.ID
			if itemID == "" {
				itemID = callID
			}
			status := components.ToolCallStatusCompleted
			args := call.RawArgs
			if args == "" && call.Arguments != nil {
				b, err := json.Marshal(call.Arguments)
				if err != nil {
					return nil, fmt.Errorf("marshal chat tool arguments for %q: %w", call.Name, err)
				}
				args = string(b)
			}
			out = append(out, components.CreateInputsUnion1FunctionCallItem(components.FunctionCallItem{ID: itemID, CallID: callID, Name: call.Name, Arguments: args, Status: &status, Type: components.FunctionCallItemTypeFunctionCall}))
		}
	}
	return out, nil
}

// contentToStringWithError encodes a tool/message content value to the string
// form the Responses API expects, returning an error on encode failure.
func contentToStringWithError(content any) (string, error) {
	if content == nil {
		return "", nil
	}
	if s, ok := content.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	return string(b), nil
}
