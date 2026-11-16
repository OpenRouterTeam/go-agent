package agent

// Ports packages/agent/src/lib/chat-compat.test.ts "assistant tool call
// conversion (#11)" and the null/empty content cases.

import (
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func ccItems(t *testing.T, msgs []ChatMessage) []map[string]any {
	t.Helper()
	items, err := FromChatMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, phJSONMap(t, it))
	}
	return out
}

func ccFunctionCall(callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "call_id": callID, "id": callID, "name": name, "arguments": args, "status": "completed"}
}

func ccEqual(t *testing.T, got []map[string]any, want []map[string]any) {
	t.Helper()
	if phCanonicalJSON(t, phJSON(t, got)) != phCanonicalJSON(t, phJSON(t, want)) {
		t.Fatalf("items =\n%s\nwant\n%s", phJSON(t, got), phJSON(t, want))
	}
}

// upstream: "emits a function_call item for an assistant message with null content and one toolCall"
func TestFromChatMessagesAssistantToolCallWithNullContent(t *testing.T) {
	got := ccItems(t, []ChatMessage{
		{Role: "user", Content: "What is the weather in Paris?"},
		{Role: "assistant", Content: nil, ToolCalls: []ParsedToolCall{{ID: "call_123", Name: "get_weather", RawArgs: `{"location":"Paris"}`}}},
		{Role: "tool", Content: "Sunny, 22C", ToolCallID: "call_123"},
	})
	ccEqual(t, got, []map[string]any{
		{"role": "user", "content": "What is the weather in Paris?"},
		ccFunctionCall("call_123", "get_weather", `{"location":"Paris"}`),
		{"type": "function_call_output", "call_id": "call_123", "output": "Sunny, 22C"},
	})
	// The converted history is valid CallModel input: a function_call is
	// answered by its output, in order.
	items, _ := FromChatMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ParsedToolCall{{ID: "call_123", Name: "get_weather", RawArgs: `{}`}}},
		{Role: "tool", Content: "ok", ToolCallID: "call_123"},
	})
	if kinds := phItemKinds(t, items); !phEqualStrings(kinds, []string{"function_call", "function_call_output"}) {
		t.Fatalf("kinds = %v", kinds)
	}
}

// upstream: "emits both a message item and a function_call item when assistant has text and toolCalls"
func TestFromChatMessagesAssistantTextAndToolCalls(t *testing.T) {
	got := ccItems(t, []ChatMessage{{Role: "assistant", Content: "Let me check the weather for you.", ToolCalls: []ParsedToolCall{{ID: "call_456", Name: "get_weather", RawArgs: `{"location":"London"}`}}}})
	ccEqual(t, got, []map[string]any{
		{"role": "assistant", "content": "Let me check the weather for you."},
		ccFunctionCall("call_456", "get_weather", `{"location":"London"}`),
	})
}

// upstream: "emits one function_call item per toolCall for parallel tool calls"
func TestFromChatMessagesParallelToolCalls(t *testing.T) {
	got := ccItems(t, []ChatMessage{{Role: "assistant", ToolCalls: []ParsedToolCall{
		{ID: "call_a", Name: "get_weather", RawArgs: `{"location":"Paris"}`},
		{ID: "call_b", Name: "get_time", RawArgs: `{"tz":"UTC"}`},
	}}})
	ccEqual(t, got, []map[string]any{
		ccFunctionCall("call_a", "get_weather", `{"location":"Paris"}`),
		ccFunctionCall("call_b", "get_time", `{"tz":"UTC"}`),
	})
}

// upstream: "does not re-stringify already-serialized tool call arguments";
// parsed Arguments are serialized once.
func TestFromChatMessagesToolCallArguments(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{{Role: "assistant", ToolCalls: []ParsedToolCall{
		{ID: "call_raw", Name: "noop", RawArgs: `{"a":1}`},
		{ID: "call_parsed", Name: "noop", Arguments: map[string]any{"b": 2}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, `{"b":2}`}
	for i, item := range items {
		if item.FunctionCallItem == nil {
			t.Fatalf("item %d is not a function_call: %s", i, phJSON(t, item))
		}
		if item.FunctionCallItem.Arguments != want[i] {
			t.Fatalf("item %d arguments = %q, want %q", i, item.FunctionCallItem.Arguments, want[i])
		}
	}
	if _, err := FromChatMessages([]ChatMessage{{Role: "assistant", ToolCalls: []ParsedToolCall{{ID: "c", Name: "n", Arguments: map[string]any{"bad": make(chan int)}}}}}); err == nil {
		t.Fatal("unencodable arguments must return an error, not a placeholder")
	}
}

// upstream: "emits nothing extra for an assistant message with an empty toolCalls array"
// plus "handles null content in assistant message" and empty-string content.
func TestFromChatMessagesContentEdgeCases(t *testing.T) {
	ccEqual(t, ccItems(t, []ChatMessage{{Role: "assistant", Content: "No tools needed.", ToolCalls: []ParsedToolCall{}}}),
		[]map[string]any{{"role": "assistant", "content": "No tools needed."}})
	ccEqual(t, ccItems(t, []ChatMessage{{Role: "assistant", Content: nil}}),
		[]map[string]any{{"role": "assistant", "content": ""}})
	ccEqual(t, ccItems(t, []ChatMessage{{Role: "user", Content: ""}}),
		[]map[string]any{{"role": "user", "content": ""}})
	// Empty-string assistant content is replaced by the tool calls too.
	ccEqual(t, ccItems(t, []ChatMessage{{Role: "assistant", Content: "", ToolCalls: []ParsedToolCall{{ID: "c1", Name: "f", RawArgs: `{}`}}}}),
		[]map[string]any{ccFunctionCall("c1", "f", `{}`)})
	if items, err := FromChatMessages(nil); err != nil || len(items) != 0 {
		t.Fatalf("empty input = %v, %v", items, err)
	}
}

// A ToChatMessage -> FromChatMessages round trip turns the response's
// function calls back into function_call items with their call ids.
func TestChatMessageToolCallRoundTrip(t *testing.T) {
	resp := phResp("resp_1", phMessage("m", "checking"), phFnCall("call_9", "lookup", `{"q":"go"}`))
	msg := ToChatMessage(resp)
	items, err := FromChatMessages([]ChatMessage{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].EasyInputMessage == nil || items[1].FunctionCallItem == nil {
		t.Fatalf("items = %s", phJSON(t, items))
	}
	fc := items[1].FunctionCallItem
	if fc.CallID != "call_9" || fc.Name != "lookup" || phCanonicalJSON(t, fc.Arguments) != `{"q":"go"}` || fc.Status == nil || *fc.Status != components.ToolCallStatusCompleted {
		t.Fatalf("function_call = %s", phJSON(t, fc))
	}
}
