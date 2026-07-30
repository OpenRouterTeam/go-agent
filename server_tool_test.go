package agent

// Coverage for NewServerTool, a contract-Required public API entry point whose
// whole implementation (serverToolImpl and its nine methods) previously sat at
// 0%. Server tools are executed by OpenRouter, not locally, so the only
// behavior that matters is that the wrapped SDK tool union reaches the request
// verbatim and that the agent never tries to run it locally.

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

func webSearchServerTool() components.ResponsesRequestToolUnion {
	return components.CreateResponsesRequestToolUnionWebSearch20250826(
		components.WebSearchServerTool{Type: components.WebSearchServerToolTypeWebSearch20250826},
	)
}

func TestNewServerToolExposesServerToolShape(t *testing.T) {
	cfg := ServerToolConfig{Name: "web_search", Config: webSearchServerTool()}
	tool := NewServerTool(cfg)

	if tool.ToolName() != "web_search" {
		t.Fatalf("ToolName = %q, want web_search", tool.ToolName())
	}
	if tool.ToolType() != ToolTypeServer {
		t.Fatalf("ToolType = %v, want %v", tool.ToolType(), ToolTypeServer)
	}
	// Server tools carry no local schemas: OpenRouter owns validation and
	// execution, so advertising a local schema here would be a lie.
	if tool.InputSchema() != nil || tool.OutputSchema() != nil || tool.EventSchema() != nil {
		t.Fatal("server tools must not advertise local input/output/event schemas")
	}
	if tool.ToolDescription() != "" {
		t.Fatalf("ToolDescription = %q, want empty", tool.ToolDescription())
	}

	approval, err := tool.RequiresApproval(context.Background(), ParsedToolCall{Name: "web_search"}, TurnContext{})
	if err != nil {
		t.Fatal(err)
	}
	if approval {
		t.Fatal("server tools cannot pause for local approval")
	}

	// HandleResponseReceived is a passthrough: there is no local output to rewrite.
	out, err := tool.HandleResponseReceived(context.Background(), "untouched", ToolExecuteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "untouched" {
		t.Fatalf("HandleResponseReceived mutated output: %v", out)
	}
}

// TestNewServerToolPassesConfigThroughUnmodified is the assertion that actually
// matters: ToAPITool must hand back exactly the union it was given. A dropped or
// rewritten field here silently changes the request OpenRouter receives.
func TestNewServerToolPassesConfigThroughUnmodified(t *testing.T) {
	maxResults := int64(3)
	engine := components.WebSearchEngineEnumExa
	original := components.CreateResponsesRequestToolUnionWebSearch20250826(
		components.WebSearchServerTool{
			Type:       components.WebSearchServerToolTypeWebSearch20250826,
			Engine:     &engine,
			MaxResults: &maxResults,
		},
	)

	got := NewServerTool(ServerToolConfig{Name: "web_search", Config: original}).ToAPITool()

	if got.WebSearchServerTool == nil {
		t.Fatalf("ToAPITool lost the web_search union member: %+v", got)
	}
	if got.WebSearchServerTool.Engine == nil || *got.WebSearchServerTool.Engine != engine {
		t.Fatalf("engine not passed through: %+v", got.WebSearchServerTool.Engine)
	}
	if got.WebSearchServerTool.MaxResults == nil || *got.WebSearchServerTool.MaxResults != maxResults {
		t.Fatalf("max_results not passed through: %+v", got.WebSearchServerTool.MaxResults)
	}
}

// TestServerToolReachesRequestThroughCallModel closes the loop end-to-end: a
// server tool passed to CallModel must appear in the outgoing request's Tools,
// and must not be executed locally.
func TestServerToolReachesRequestThroughCallModel(t *testing.T) {
	ctx := context.Background()
	server := NewServerTool(ServerToolConfig{Name: "web_search", Config: webSearchServerTool()})

	resp := completedResponse("done")
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test",
		Input: "search the web",
		Tools: []Tool{server},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}

	if len(sender.requests) == 0 {
		t.Fatal("no request was sent")
	}
	tools := sender.requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("expected exactly 1 tool on the request, got %d", len(tools))
	}
	if tools[0].WebSearchServerTool == nil {
		t.Fatalf("server tool did not reach the request as a web_search union: %+v", tools[0])
	}
	// A server tool must not be sent as a local function definition.
	if tools[0].ResponsesRequestToolFunction != nil {
		t.Fatal("server tool was serialized as a local function tool")
	}
}
