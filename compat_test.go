package agent

import (
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func TestFormatCompatibilityCarriesMetadataAndUnsupportedContent(t *testing.T) {
	resp := components.OpenResponsesResult{ID: "resp_1", Model: "model", OutputText: openrouter.String("hi")}
	msg, err := ToClaudeMessage(resp)
	if err != nil {
		t.Fatalf("ToClaudeMessage: %v", err)
	}
	if msg.ID != "resp_1" || msg.Model != "model" {
		t.Fatalf("Claude metadata not preserved")
	}
	unsupported := map[string]any{"unsupported_content": map[string]any{"original_type": "image", "data": map[string]any{"url": "x"}, "reason": "not supported"}}
	if len(ScanUnsupportedContent(unsupported)) == 0 {
		t.Fatalf("unsupported carrier not detected")
	}
	if got := ScanUnsupportedContent(unsupported)[0].OriginalType; got != "image" {
		t.Fatalf("original_type = %q", got)
	}
}

func TestClaudeAndChatConversionsPreserveStructuredCarriers(t *testing.T) {
	claude := []ClaudeMessage{{ID: "msg_1", Type: "message", Role: "user", Model: "claude", StopReason: "end_turn", Content: []ClaudeContentBlock{{Type: "text", Text: "hello"}, {Type: "image", Source: &ClaudeImageSource{Type: "url", URL: "https://example.com/a.png"}}, {Type: "tool_use", ID: "call_1", Name: "search", Input: map[string]any{"query": "go"}}}}}
	items, err := FromClaudeMessages(claude)
	if err != nil {
		t.Fatalf("FromClaudeMessages: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected message plus tool_use, got %d", len(items))
	}
	var sawClaudeMessage, sawToolUse bool
	for _, item := range items {
		if item.EasyInputMessage != nil {
			sawClaudeMessage = true
		}
		if item.FunctionCallItem != nil {
			sawToolUse = true
		}
	}
	if !sawClaudeMessage || !sawToolUse {
		t.Fatalf("Claude conversion did not preserve structured message/tool_use: %#v", items)
	}
	if len(ScanUnsupportedContent(items)) == 0 {
		t.Fatalf("Claude metadata should be carried as structured unsupported content")
	}

	chatItems, err := FromChatMessages([]ChatMessage{{Role: "user", Content: []map[string]any{{"type": "input_image", "image_url": "https://example.com/a.png"}}}, {Role: "tool", ToolCallID: "call_1", Content: map[string]any{"ok": true}}})
	if err != nil {
		t.Fatalf("FromChatMessages: %v", err)
	}
	if chatItems[0].EasyInputMessage == nil {
		t.Fatalf("chat structured content should stay structured")
	}
	chatContent, ok := chatItems[0].EasyInputMessage.Content.GetOrZero()
	if !ok || chatContent.Any == nil {
		t.Fatalf("chat structured content should stay structured")
	}
	if chatItems[1].FunctionCallOutputItem == nil {
		t.Fatalf("chat tool message should become function_call_output")
	}
}

// roleOfInput returns the role string carried by an EasyInputMessage input item.
func roleOfInput(item components.InputsUnion1) (string, bool) {
	if item.EasyInputMessage == nil {
		return "", false
	}
	r := item.EasyInputMessage.Role
	switch {
	case r.EasyInputMessageRoleSystem != nil:
		return "system", true
	case r.EasyInputMessageRoleDeveloper != nil:
		return "developer", true
	case r.EasyInputMessageRoleAssistant != nil:
		return "assistant", true
	case r.EasyInputMessageRoleUser != nil:
		return "user", true
	}
	return "", false
}

// TestClaudeRoundTripEveryRoleAndCarrier asserts a full lossless Claude round
// trip (FromClaudeMessages -> ... -> FromClaudeMessages) preserves every role
// (system/developer/user/assistant/tool) and every carrier (text, image,
// tool_use, tool_result, metadata, unsupported-content). Run-2 punch-list 16c.
func TestClaudeRoundTripEveryRoleAndCarrier(t *testing.T) {
	roles := []string{"system", "developer", "user", "assistant"}
	for _, role := range roles {
		msgs := []ClaudeMessage{{
			ID: "m_" + role, Type: "message", Role: role, Model: "claude-x", StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "text", Text: "hi " + role},
				{Type: "image", Source: &ClaudeImageSource{Type: "url", URL: "https://example.com/i.png"}},
			},
		}}
		items, err := FromClaudeMessages(msgs)
		if err != nil {
			t.Fatalf("role %s: FromClaudeMessages: %v", role, err)
		}
		gotRole, ok := roleOfInput(items[0])
		if !ok || gotRole != role {
			t.Fatalf("role %s not preserved, got %q (ok=%v)", role, gotRole, ok)
		}
		carriers := ScanUnsupportedContent(items)
		if len(carriers) == 0 {
			t.Fatalf("role %s: metadata carrier missing", role)
		}
		var sawMeta bool
		for _, c := range carriers {
			if c.OriginalType == "claude_message_metadata" {
				sawMeta = true
			}
		}
		if !sawMeta {
			t.Fatalf("role %s: claude_message_metadata carrier not round-tripped", role)
		}
	}

	// tool_use + tool_result carriers
	toolMsgs := []ClaudeMessage{
		{Role: "assistant", Content: []ClaudeContentBlock{{Type: "tool_use", ID: "call_42", Name: "lookup", Input: map[string]any{"q": "x"}}}},
		{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "call_42", Content: map[string]any{"answer": 7}}}},
	}
	items, err := FromClaudeMessages(toolMsgs)
	if err != nil {
		t.Fatalf("tool round trip: %v", err)
	}
	var sawCall, sawOutput bool
	for _, it := range items {
		if it.FunctionCallItem != nil && it.FunctionCallItem.CallID == "call_42" {
			sawCall = true
		}
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.CallID == "call_42" {
			sawOutput = true
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("tool_use/tool_result not preserved: call=%v output=%v", sawCall, sawOutput)
	}
}

// TestChatRoundTripEveryRole asserts FromChatMessages handles every chat role
// and that tool messages become function_call_output items. Run-2 16c (Chat).
func TestChatRoundTripEveryRole(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "be terse"},
		{Role: "developer", Content: "use tools"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "calling", ToolCalls: []ParsedToolCall{{CallID: "c1", Name: "t", Arguments: map[string]any{"a": 1}}}},
		{Role: "tool", ToolCallID: "c1", Content: map[string]any{"ok": true}},
	}
	items, err := FromChatMessages(msgs)
	if err != nil {
		t.Fatalf("FromChatMessages: %v", err)
	}
	wantRoles := map[string]bool{"system": false, "developer": false, "user": false, "assistant": false}
	var sawToolCall, sawToolOutput bool
	for _, it := range items {
		if r, ok := roleOfInput(it); ok {
			if _, want := wantRoles[r]; want {
				wantRoles[r] = true
			}
		}
		if it.FunctionCallItem != nil && it.FunctionCallItem.CallID == "c1" {
			sawToolCall = true
		}
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.CallID == "c1" {
			sawToolOutput = true
		}
	}
	for role, saw := range wantRoles {
		if !saw {
			t.Fatalf("chat role %q not converted", role)
		}
	}
	if !sawToolCall || !sawToolOutput {
		t.Fatalf("chat tool_call/tool_output not converted: call=%v output=%v", sawToolCall, sawToolOutput)
	}
}

// TestClaudeToFromRoundTripLossless covers Run-2 punch-list item 16c: a full
// ToClaudeMessage -> FromClaudeMessages round trip must preserve message
// metadata (id/model/stop_reason/usage), tool_use, reasoning, and unsupported
// content via the structured carrier.
func TestClaudeToFromRoundTripLossless(t *testing.T) {
	msgItem := components.OutputMessageItem{Content: []components.OutputMessageItemContent{{ResponseOutputText: &components.ResponseOutputText{Text: "answer"}}}}
	fc := components.OutputFunctionCallItem{CallID: "call_x", Name: "lookup", Arguments: `{"q":"go"}`}
	reasoning := components.OutputReasoningItem{ID: "r1", Summary: []components.ReasoningSummaryText{{Text: "thinking deeply"}}}
	resp := components.OpenResponsesResult{
		ID:     "resp_rt",
		Model:  "anthropic/claude",
		Status: components.OpenAIResponsesResponseStatusCompleted,
		Output: []components.OutputItems{
			components.CreateOutputItemsReasoning(reasoning),
			components.CreateOutputItemsMessage(msgItem),
			components.CreateOutputItemsFunctionCall(fc),
		},
	}

	claude, err := ToClaudeMessage(resp)
	if err != nil {
		t.Fatalf("ToClaudeMessage: %v", err)
	}
	if claude.ID != "resp_rt" || claude.Model != "anthropic/claude" {
		t.Fatalf("metadata not set: %+v", claude)
	}
	// A function_call present means stop_reason must be tool_use.
	if claude.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", claude.StopReason)
	}
	var sawText, sawThinking, sawToolUse bool
	for _, b := range claude.Content {
		switch b.Type {
		case "text":
			sawText = b.Text == "answer"
		case "thinking":
			sawThinking = b.Thinking == "thinking deeply"
		case "tool_use":
			sawToolUse = b.ID == "call_x" && b.Name == "lookup"
		}
	}
	if !sawText || !sawThinking || !sawToolUse {
		t.Fatalf("Claude content incomplete: text=%v thinking=%v tool_use=%v", sawText, sawThinking, sawToolUse)
	}

	// Round trip back to input items; metadata + thinking carrier + tool_use
	// must survive.
	items, err := FromClaudeMessages([]ClaudeMessage{claude})
	if err != nil {
		t.Fatalf("FromClaudeMessages: %v", err)
	}
	carriers := ScanUnsupportedContent(items)
	var sawMeta, sawThinkingCarrier bool
	for _, c := range carriers {
		switch c.OriginalType {
		case "claude_message_metadata":
			sawMeta = true
		case "thinking":
			sawThinkingCarrier = true
		}
	}
	if !sawMeta {
		t.Fatalf("metadata carrier lost on round trip: %#v", carriers)
	}
	if !sawThinkingCarrier {
		t.Fatalf("thinking carrier lost on round trip: %#v", carriers)
	}
	var sawCallRT bool
	for _, it := range items {
		if it.FunctionCallItem != nil && it.FunctionCallItem.CallID == "call_x" {
			sawCallRT = true
		}
	}
	if !sawCallRT {
		t.Fatalf("tool_use lost on round trip: %#v", items)
	}

	// The unsupported-content helpers operate on the Claude message itself.
	reasoningResp := components.OpenResponsesResult{ID: "r", Status: components.OpenAIResponsesResponseStatusCompleted, Output: []components.OutputItems{components.CreateOutputItemsWebSearchCall(components.OutputWebSearchCallItem{})}}
	cm, err := ToClaudeMessage(reasoningResp)
	if err != nil {
		t.Fatalf("ToClaudeMessage(server tool): %v", err)
	}
	if !HasUnsupportedContent(cm) {
		t.Fatalf("server-tool output should be carried as unsupported content")
	}
	if len(ExtractUnsupportedContent(cm, "openresponses_output_item")) == 0 {
		t.Fatalf("ExtractUnsupportedContent should find the server-tool carrier")
	}
	if GetUnsupportedContentSummary(cm)["openresponses_output_item"] == 0 {
		t.Fatalf("GetUnsupportedContentSummary should count the carrier")
	}
}
