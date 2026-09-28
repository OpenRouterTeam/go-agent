package agent

import (
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Ported from upstream chat-compat.test.ts "assistant tool call conversion
// (#11)" and "return type is assignable to callModel input (#41)".

func TestFromChatMessagesNullContentToolCallEmitsOnlyFunctionCall(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{
		{Role: "user", Content: "What is the weather in Paris?"},
		{Role: "assistant", Content: nil, ToolCalls: []ParsedToolCall{{ID: "call_123", Name: "get_weather", RawArgs: `{"location":"Paris"}`}}},
		{Role: "tool", ToolCallID: "call_123", Content: "Sunny, 22C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (user, function_call, function_call_output), got %d: %#v", len(items), items)
	}
	if r, ok := roleOfInput(items[0]); !ok || r != "user" {
		t.Fatalf("item 0 should be the user message, got %#v", items[0])
	}
	fc := items[1].FunctionCallItem
	if fc == nil {
		t.Fatalf("item 1 should be a function_call (empty assistant message omitted), got %#v", items[1])
	}
	if fc.CallID != "call_123" || fc.ID != "call_123" || fc.Name != "get_weather" || fc.Arguments != `{"location":"Paris"}` {
		t.Fatalf("function_call fields = %#v", fc)
	}
	if fc.Status == nil || *fc.Status != components.ToolCallStatusCompleted {
		t.Fatalf("function_call status must be completed, got %v", fc.Status)
	}
	out := items[2].FunctionCallOutputItem
	if out == nil || out.CallID != "call_123" || out.Output.Str == nil || *out.Output.Str != "Sunny, 22C" {
		t.Fatalf("item 2 should be the function_call_output, got %#v", items[2])
	}
}

func TestFromChatMessagesTextAndToolCallsEmitsBoth(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{
		{Role: "assistant", Content: "Let me check the weather for you.", ToolCalls: []ParsedToolCall{{ID: "call_456", Name: "get_weather", RawArgs: `{"location":"London"}`}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want message + function_call, got %#v", items)
	}
	if r, ok := roleOfInput(items[0]); !ok || r != "assistant" {
		t.Fatalf("item 0 should be the assistant message, got %#v", items[0])
	}
	if items[1].FunctionCallItem == nil || items[1].FunctionCallItem.CallID != "call_456" {
		t.Fatalf("item 1 should be the function_call, got %#v", items[1])
	}
}

func TestFromChatMessagesParallelToolCallsInOrder(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ParsedToolCall{
			{ID: "call_a", Name: "get_weather", RawArgs: `{"location":"Paris"}`},
			{ID: "call_b", Name: "get_time", RawArgs: `{"tz":"UTC"}`},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].FunctionCallItem == nil || items[1].FunctionCallItem == nil {
		t.Fatalf("want exactly two function_call items, got %#v", items)
	}
	if items[0].FunctionCallItem.CallID != "call_a" || items[1].FunctionCallItem.CallID != "call_b" {
		t.Fatalf("function_call order lost: %q, %q", items[0].FunctionCallItem.CallID, items[1].FunctionCallItem.CallID)
	}
}

func TestFromChatMessagesDoesNotReStringifyArguments(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ParsedToolCall{{ID: "call_raw", Name: "noop", RawArgs: `{"a":1}`}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0].FunctionCallItem.Arguments; got != `{"a":1}` {
		t.Fatalf("arguments = %q, want the already-serialized string forwarded verbatim", got)
	}
}

func TestFromChatMessagesEmptyToolCallsKeepsMessage(t *testing.T) {
	items, err := FromChatMessages([]ChatMessage{
		{Role: "assistant", Content: "No tools needed.", ToolCalls: []ParsedToolCall{}},
		{Role: "assistant", Content: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want both assistant messages preserved (no tool calls replace them), got %#v", items)
	}
	for i, it := range items {
		if r, ok := roleOfInput(it); !ok || r != "assistant" {
			t.Fatalf("item %d should be an assistant message, got %#v", i, it)
		}
	}
}
