package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/call-model-active-tools.test.ts (#31).

func activeToolsRequest(t *testing.T, active []string, tools ...Tool) (components.ResponsesRequest, *ModelResult) {
	t.Helper()
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{plainTextResponse("ok")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: tools, ActiveTools: active})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	return sender.requests[0], result
}

func toolNames(req components.ResponsesRequest) []string {
	var names []string
	for _, tl := range req.Tools {
		if tl.ResponsesRequestToolFunction != nil {
			names = append(names, tl.ResponsesRequestToolFunction.Name)
		} else {
			names = append(names, "<server>")
		}
	}
	return names
}

func namedTool(name string) Tool {
	return MustNewTool(ToolConfig[struct{}]{Name: name, Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) { return "ok", nil }})
}

func TestActiveToolsSendsOnlyActiveTools(t *testing.T) {
	req, _ := activeToolsRequest(t, []string{"a", "missing"}, namedTool("a"), namedTool("b"))
	if got := strings.Join(toolNames(req), ","); got != "a" {
		t.Fatalf("tools = %s, want only a (unknown names ignored)", got)
	}
}

func TestActiveToolsOmittedSendsAll(t *testing.T) {
	req, _ := activeToolsRequest(t, nil, namedTool("a"), namedTool("b"))
	if got := strings.Join(toolNames(req), ","); got != "a,b" {
		t.Fatalf("tools = %s, want a,b", got)
	}
}

func TestActiveToolsKeepsServerTools(t *testing.T) {
	server := NewServerTool(ServerToolConfig{Name: "web_search", Config: webSearchServerTool()})
	req, _ := activeToolsRequest(t, []string{"b"}, namedTool("a"), server, namedTool("b"))
	if got := strings.Join(toolNames(req), ","); got != "<server>,b" {
		t.Fatalf("tools = %s, want server tool kept plus b", got)
	}
}

func TestActiveToolsFilteringEverythingOmitsToolsKey(t *testing.T) {
	req, _ := activeToolsRequest(t, []string{"missing"}, namedTool("a"), namedTool("b"))
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["tools"]; present {
		t.Fatalf("outbound request must omit tools entirely, got %s", b)
	}
}

// A filtered-out tool is not registered for execution either: a call to it
// is treated as unresolved rather than executed.
func TestActiveToolsFilteredToolIsNotExecuted(t *testing.T) {
	ran := false
	b := MustNewTool(ToolConfig[struct{}]{Name: "b", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
		ran = true
		return "ok", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "c1", Name: "b", Arguments: `{}`}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created, plainTextResponse("done")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{namedTool("a"), b}, ActiveTools: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a tool filtered out by ActiveTools must not execute")
	}
}
