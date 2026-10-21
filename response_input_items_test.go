package agent

// Regression guard for carrying a response's output into the next turn's input.
// Upstream appends response.output to the conversation unchanged, so an assistant
// message keeps its content array, including url_citation annotations from the
// web-search server tool. Before go-sdk v0.5.10, InputsUnion1 panicked when it
// decoded array content, and the port worked around that by flattening the
// content to a string, which dropped the annotations from every follow-up
// request.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

const annotatedAssistantMessage = `{
	"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
	"content": [
		{"type": "output_text", "text": "Apple announced X [apple.com](https://www.apple.com/newsroom/x).",
		 "annotations": [{"type": "url_citation", "url": "https://www.apple.com/newsroom/x",
		                  "title": "Newsroom", "start_index": 18, "end_index": 63}]},
		{"type": "output_text", "text": " More.", "annotations": []}
	]
}`

func annotatedResponse(t *testing.T, extra ...string) components.OpenResponsesResult {
	t.Helper()
	items := append([]string{annotatedAssistantMessage}, extra...)
	raw := `{"id":"resp_1","object":"response","status":"completed","model":"openai/test","output":[` + strings.Join(items, ",") + `]}`
	var resp components.OpenResponsesResult
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("decode fixture response: %v", err)
	}
	return resp
}

// assertAnnotatedMessage checks that the marshaled input message still holds
// both content blocks, in order, and the url_citation annotation.
func assertAnnotatedMessage(t *testing.T, item json.RawMessage) {
	t.Helper()
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Annotations []struct {
				Type       string `json:"type"`
				URL        string `json:"url"`
				Title      string `json:"title"`
				StartIndex int    `json:"start_index"`
				EndIndex   int    `json:"end_index"`
			} `json:"annotations"`
		} `json:"content"`
	}
	if err := json.Unmarshal(item, &msg); err != nil {
		t.Fatalf("assistant message content is not the upstream array shape: %v\n%s", err, item)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Fatalf("expected assistant message, got type=%q role=%q", msg.Type, msg.Role)
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != "output_text" || msg.Content[1].Text != " More." {
		t.Fatalf("content blocks not preserved as-is: %s", item)
	}
	anns := msg.Content[0].Annotations
	if len(anns) != 1 {
		t.Fatalf("expected 1 annotation on the first block, got %d: %s", len(anns), item)
	}
	a := anns[0]
	if a.Type != "url_citation" || a.URL != "https://www.apple.com/newsroom/x" || a.Title != "Newsroom" || a.StartIndex != 18 || a.EndIndex != 63 {
		t.Fatalf("url_citation annotation not preserved: %+v", a)
	}
}

func TestResponseInputItemsPreservesURLCitationAnnotations(t *testing.T) {
	items, err := responseInputItemsWithError(annotatedResponse(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(items))
	}
	b, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	assertAnnotatedMessage(t, b)
}

// End to end: an annotated assistant message followed by a function call. The
// follow-up request must echo the message back with its annotations intact.
func TestFollowUpRequestCarriesURLCitationAnnotations(t *testing.T) {
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"query\":\"go\"}","status":"completed"}`
	first := annotatedResponse(t, call)
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	sender := twoTurnSender(first, second)

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{searchTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(sender.requests))
	}

	b, err := json.Marshal(sender.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(b, &input); err != nil {
		t.Fatalf("follow-up input is not an item array: %v\n%s", err, b)
	}
	var found bool
	for _, item := range input {
		var head struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		_ = json.Unmarshal(item, &head)
		if head.Type == "message" && head.Role == "assistant" {
			assertAnnotatedMessage(t, item)
			found = true
		}
	}
	if !found {
		t.Fatalf("follow-up input dropped the assistant message: %s", b)
	}
}
