package agent

import (
	"encoding/json"
	"fmt"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// ClaudeUsage mirrors the Anthropic Claude message usage shape (distinct from
// the OpenRouter SDK usage type). Field names match upstream
// stream-transformers.ts convertToClaudeMessage.
type ClaudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ClaudeMessage is the Anthropic Claude assistant-message response shape
// produced by ToClaudeMessage. It carries Claude message metadata (id, model,
// stop_reason, stop_sequence, usage) and a top-level unsupported_content array
// for content Claude cannot represent, matching upstream's ClaudeMessage type.
type ClaudeMessage struct {
	ID                 string               `json:"id"`
	Type               string               `json:"type"`
	Role               string               `json:"role"`
	Model              string               `json:"model"`
	Content            []ClaudeContentBlock `json:"content"`
	StopReason         string               `json:"stop_reason"`
	StopSequence       *string              `json:"stop_sequence"`
	Usage              ClaudeUsage          `json:"usage"`
	UnsupportedContent []UnsupportedContent `json:"unsupported_content,omitempty"`
	// Metadata retains the legacy ad-hoc metadata bag for callers that relied
	// on it; the structured fields above are the faithful upstream surface.
	Metadata map[string]any `json:"metadata,omitempty"`
}

type ClaudeImageSource struct {
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

type ClaudeContentBlock struct {
	Type        string              `json:"type"`
	Text        string              `json:"text,omitempty"`
	ID          string              `json:"id,omitempty"`
	Name        string              `json:"name,omitempty"`
	Input       any                 `json:"input,omitempty"`
	ToolUseID   string              `json:"tool_use_id,omitempty"`
	Content     any                 `json:"content,omitempty"`
	Source      *ClaudeImageSource  `json:"source,omitempty"`
	IsError     bool                `json:"is_error,omitempty"`
	Thinking    string              `json:"thinking,omitempty"`
	Signature   string              `json:"signature,omitempty"`
	Unsupported *UnsupportedContent `json:"unsupported_content,omitempty"`
	Data        any                 `json:"data,omitempty"`
}

// ToClaudeMessage converts an OpenRouter Responses result into a Claude-style
// assistant message, preserving message metadata (id, model, status, usage) and
// round-tripping any content Claude cannot represent through the structured
// unsupported-content carrier. It returns an error rather than degrading a real
// failure (e.g. an un-encodable output item) into a fallback value, matching the
// contract's errors-are-values rule.
func ToClaudeMessage(resp components.OpenResponsesResult) (ClaudeMessage, error) {
	model := resp.Model
	if model == "" {
		model = "unknown"
	}
	msg := ClaudeMessage{ID: resp.ID, Type: "message", Role: "assistant", Model: model, Content: []ClaudeContentBlock{}, StopReason: mapClaudeStopReason(resp), StopSequence: nil}
	msg.Usage = claudeUsage(resp)
	var unsupported []UnsupportedContent

	for _, item := range resp.Output {
		switch {
		case item.OutputMessageItem != nil:
			for _, c := range item.OutputMessageItem.Content {
				if c.ResponseOutputText != nil {
					msg.Content = append(msg.Content, ClaudeContentBlock{Type: "text", Text: c.ResponseOutputText.Text})
					continue
				}
				// Non-text message parts (e.g. refusal) are not representable
				// in Claude assistant content; preserve them in the carrier.
				data, err := itemToAny(c)
				if err != nil {
					return ClaudeMessage{}, err
				}
				unsupported = append(unsupported, UnsupportedContent{OriginalType: "output_message_content", Data: data, Reason: "Claude does not have a native content type for this message part"})
			}
		case item.OutputFunctionCallItem != nil:
			var input any
			args := item.OutputFunctionCallItem.Arguments
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				// Preserve raw arguments if JSON parsing fails (matches
				// upstream's _raw_arguments fallback — model output, not an
				// internal failure).
				input = map[string]any{"_raw_arguments": args}
			}
			msg.Content = append(msg.Content, ClaudeContentBlock{Type: "tool_use", ID: item.OutputFunctionCallItem.CallID, Name: item.OutputFunctionCallItem.Name, Input: input})
		case item.OutputReasoningItem != nil:
			for _, s := range item.OutputReasoningItem.Summary {
				if s.Text != "" {
					msg.Content = append(msg.Content, ClaudeContentBlock{Type: "thinking", Thinking: s.Text, Signature: ""})
				}
			}
		default:
			// Unknown / server-tool output items are preserved in the carrier
			// so a round trip is lossless.
			data, err := itemToAny(item)
			if err != nil {
				return ClaudeMessage{}, err
			}
			unsupported = append(unsupported, UnsupportedContent{OriginalType: "openresponses_output_item", Data: data, Reason: "not representable in Claude messages"})
		}
	}
	msg.UnsupportedContent = unsupported
	return msg, nil
}

// mapClaudeStopReason mirrors upstream mapStopReason: tool_use when the response
// contains function calls, otherwise end_turn (or max_tokens for an incomplete
// max_output_tokens response).
func mapClaudeStopReason(resp components.OpenResponsesResult) string {
	for _, item := range resp.Output {
		if item.OutputFunctionCallItem != nil {
			return "tool_use"
		}
	}
	if resp.Status == components.OpenAIResponsesResponseStatusIncomplete {
		if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != nil && *resp.IncompleteDetails.Reason == components.ReasonMaxOutputTokens {
			return "max_tokens"
		}
		return "end_turn"
	}
	return "end_turn"
}

func claudeUsage(resp components.OpenResponsesResult) ClaudeUsage {
	usage, ok := resp.Usage.GetOrZero()
	if !ok {
		return ClaudeUsage{}
	}
	return ClaudeUsage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.InputTokensDetails.CachedTokens,
		CacheReadInputTokens:     0,
	}
}

// FromClaudeMessages converts Claude-style messages back into OpenRouter input
// items, restoring metadata and unsupported content carried by ToClaudeMessage
// so a ToClaudeMessage -> FromClaudeMessages round trip is lossless. It returns
// an error if a tool-use input cannot be encoded.
func FromClaudeMessages(messages []ClaudeMessage) ([]components.InputsUnion1, error) {
	var out []components.InputsUnion1
	for _, m := range messages {
		var content []any
		hasMetadata := m.ID != "" || m.Type != "" || m.Model != "" || m.StopReason != "" || m.StopSequence != nil || m.Usage != (ClaudeUsage{}) || len(m.Metadata) > 0
		if hasMetadata {
			stopSeq := ""
			if m.StopSequence != nil {
				stopSeq = *m.StopSequence
			}
			content = append(content, map[string]any{"type": "unsupported_content", "unsupported_content": UnsupportedContent{OriginalType: "claude_message_metadata", Data: map[string]any{"id": m.ID, "type": m.Type, "model": m.Model, "stop_reason": m.StopReason, "stop_sequence": stopSeq, "usage": m.Usage, "metadata": m.Metadata}, Reason: "metadata carrier for lossless Claude round trip"}})
		}
		// Carry the top-level unsupported_content array so it survives a round
		// trip (matches upstream ClaudeMessage.unsupported_content).
		for _, uc := range m.UnsupportedContent {
			content = append(content, map[string]any{"type": "unsupported_content", "unsupported_content": uc})
		}
		for _, block := range m.Content {
			switch block.Type {
			case "text":
				content = append(content, map[string]any{"type": "input_text", "text": block.Text})
			case "image":
				content = append(content, map[string]any{"type": "input_image", "image_url": claudeImageURL(block.Source), "detail": "auto"})
			case "tool_use":
				args, err := json.Marshal(block.Input)
				if err != nil {
					return nil, fmt.Errorf("marshal claude tool_use input for %q: %w", block.Name, err)
				}
				out = append(out, components.CreateInputsUnion1FunctionCallItem(components.FunctionCallItem{ID: block.ID, CallID: block.ID, Name: block.Name, Arguments: string(args), Type: components.FunctionCallItemTypeFunctionCall}))
			case "tool_result":
				result, err := contentToStringWithError(block.Content)
				if err != nil {
					return nil, fmt.Errorf("encode claude tool_result content for %q: %w", block.ToolUseID, err)
				}
				out = append(out, components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: block.ToolUseID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(result), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}))
			case "thinking":
				// Reasoning is carried as unsupported content so it survives a
				// round trip (Claude input has no native thinking block).
				content = append(content, map[string]any{"type": "unsupported_content", "unsupported_content": UnsupportedContent{OriginalType: "thinking", Data: map[string]any{"thinking": block.Thinking, "signature": block.Signature}, Reason: "Claude input has no native thinking block"}})
			case "unsupported_content":
				content = append(content, map[string]any{"type": "unsupported_content", "unsupported_content": block.Unsupported})
			}
		}
		if len(content) == 0 {
			continue
		}
		if len(content) == 1 {
			if one, ok := content[0].(map[string]any); ok {
				if text, ok := one["text"].(string); ok {
					out = append(out, easyMessage(m.Role, components.CreateEasyInputMessageContentUnion2Str(text)))
					continue
				}
			}
		}
		out = append(out, easyMessage(m.Role, components.CreateEasyInputMessageContentUnion2Any(content)))
	}
	return out, nil
}

func easyMessage(role string, content components.EasyInputMessageContentUnion2) components.InputsUnion1 {
	return components.CreateInputsUnion1EasyInputMessage(components.EasyInputMessage{Role: easyRole(role), Content: optionalnullable.From(openrouter.Pointer(content))})
}

func easyRole(role string) components.EasyInputMessageRoleUnion {
	switch role {
	case "assistant":
		return components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleAssistant(components.EasyInputMessageRoleAssistantAssistant)
	case "system":
		return components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleSystem(components.EasyInputMessageRoleSystemSystem)
	case "developer":
		return components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleDeveloper(components.EasyInputMessageRoleDeveloperDeveloper)
	default:
		return components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser)
	}
}

func claudeImageURL(source *ClaudeImageSource) string {
	if source == nil {
		return ""
	}
	if source.Type == "base64" {
		return "data:" + source.MediaType + ";base64," + source.Data
	}
	return source.URL
}

// itemToAny normalizes an SDK output item into a generic JSON value for the
// unsupported-content carrier, returning an error if it cannot be encoded.
func itemToAny(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal output item: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("normalize output item: %w", err)
	}
	return out, nil
}
