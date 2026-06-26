package agent

import (
	"encoding/json"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type StreamableOutputItem = components.OutputItems

type UnsupportedContent struct {
	OriginalType string `json:"original_type"`
	Data         any    `json:"data"`
	Reason       string `json:"reason"`
}

func ExtractTextFromResponse(resp components.OpenResponsesResult) string {
	if resp.OutputText != nil {
		return *resp.OutputText
	}
	var b strings.Builder
	for _, item := range resp.Output {
		if item.OutputMessageItem != nil {
			for _, c := range item.OutputMessageItem.Content {
				if c.ResponseOutputText != nil {
					b.WriteString(c.ResponseOutputText.Text)
				}
			}
		}
	}
	return b.String()
}

func ExtractToolCallsFromResponse(resp components.OpenResponsesResult) []ParsedToolCall {
	var calls []ParsedToolCall
	for _, item := range resp.Output {
		if item.OutputFunctionCallItem == nil {
			continue
		}
		fc := item.OutputFunctionCallItem
		raw, args, err := ParseToolArguments(fc.Arguments)
		if err != nil {
			continue
		}
		id := fc.CallID
		if id == "" && fc.ID != nil {
			id = *fc.ID
		}
		calls = append(calls, ParsedToolCall{ID: id, CallID: fc.CallID, Name: fc.Name, Arguments: args, RawArgs: string(raw)})
	}
	return calls
}

func ExtractTextDeltas(events []components.StreamEvents) []string {
	var out []string
	for _, ev := range events {
		if ev.TextDeltaEvent != nil {
			out = append(out, ev.TextDeltaEvent.Delta)
		}
	}
	return out
}
func ExtractReasoningDeltas(events []components.StreamEvents) []string {
	var out []string
	for _, ev := range events {
		if ev.ReasoningDeltaEvent != nil {
			out = append(out, ev.ReasoningDeltaEvent.Delta)
		}
		if ev.ReasoningSummaryTextDeltaEvent != nil {
			out = append(out, ev.ReasoningSummaryTextDeltaEvent.Delta)
		}
	}
	return out
}
func ExtractToolDeltas(events []components.StreamEvents) []string {
	var out []string
	for _, ev := range events {
		if ev.FunctionCallArgsDeltaEvent != nil {
			out = append(out, ev.FunctionCallArgsDeltaEvent.Delta)
		}
	}
	return out
}

// HasUnsupportedContent reports whether a Claude message carries any
// unsupported content (mirrors upstream hasUnsupportedContent(message)).
func HasUnsupportedContent(msg ClaudeMessage) bool {
	return len(msg.UnsupportedContent) > 0
}

// ExtractUnsupportedContent returns the unsupported-content carriers on a Claude
// message filtered by original type (mirrors upstream
// extractUnsupportedContent(message, originalType)). An empty originalType
// returns all carriers.
func ExtractUnsupportedContent(msg ClaudeMessage, originalType string) []UnsupportedContent {
	var out []UnsupportedContent
	for _, uc := range msg.UnsupportedContent {
		if originalType == "" || uc.OriginalType == originalType {
			out = append(out, uc)
		}
	}
	return out
}

// GetUnsupportedContentSummary returns a count of unsupported-content carriers
// by original type (mirrors upstream getUnsupportedContentSummary(message)).
func GetUnsupportedContentSummary(msg ClaudeMessage) map[string]int {
	summary := map[string]int{}
	for _, uc := range msg.UnsupportedContent {
		summary[uc.OriginalType]++
	}
	return summary
}

// ScanUnsupportedContent is a best-effort inspection helper that recursively
// finds unsupported-content carriers anywhere in an arbitrary value (e.g. an
// input-item slice produced by FromClaudeMessages/FromChatMessages or a raw
// response). It is intentionally non-core: it returns a best-effort result and
// never errors, and is not part of the upstream public surface.
func ScanUnsupportedContent(v any) []UnsupportedContent {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var found []UnsupportedContent
	scanUnsupportedJSON(b, &found)
	return found
}

func scanUnsupportedJSON(b []byte, found *[]UnsupportedContent) {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return
	}
	scanUnsupportedValue(v, found)
}
func scanUnsupportedValue(v any, found *[]UnsupportedContent) {
	switch x := v.(type) {
	case map[string]any:
		if ot, ok := x["original_type"].(string); ok {
			uc := UnsupportedContent{OriginalType: ot, Data: x["data"]}
			if r, ok := x["reason"].(string); ok {
				uc.Reason = r
			}
			*found = append(*found, uc)
		}
		for _, child := range x {
			scanUnsupportedValue(child, found)
		}
	case []any:
		for _, child := range x {
			scanUnsupportedValue(child, found)
		}
	}
}
