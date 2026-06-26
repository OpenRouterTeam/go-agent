package agent

import (
	"context"
	"encoding/json"
	"testing"
)

type sampleInput struct {
	Query string `json:"query" jsonschema:"required"`
}

func TestToolExecutionValidationAndGeneratorEvents(t *testing.T) {
	ctx := context.Background()
	regular := MustNewTool(ToolConfig[sampleInput]{
		Name:        "search",
		Description: "Search",
		InputSchema: map[string]any{
			"type":       "object",
			"required":   []any{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
		Execute: func(ctx context.Context, in sampleInput, tc ToolExecuteContext) (any, error) {
			return map[string]any{"seen": in.Query}, nil
		},
	})

	bad, err := ExecuteTool(ctx, regular, ParsedToolCall{CallID: "c1", Name: "search", RawArgs: `{}`}, ToolExecuteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Error == nil {
		t.Fatalf("expected schema validation error for missing required query")
	}

	good, err := ExecuteTool(ctx, regular, ParsedToolCall{CallID: "c2", Name: "search", RawArgs: `{"query":"go"}`}, ToolExecuteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if good.Error != nil {
		t.Fatalf("unexpected tool error: %v", good.Error)
	}

	generator := MustNewTool(ToolConfig[sampleInput]{
		Name: "analyze",
		Generate: func(ctx context.Context, in sampleInput, tc ToolExecuteContext, yield func(any) error) (any, error) {
			if err := yield(map[string]any{"progress": 0.5}); err != nil {
				return nil, err
			}
			return map[string]any{"summary": in.Query}, nil
		},
	})
	var live []any
	result, err := ExecuteTool(ctx, generator, ParsedToolCall{CallID: "c3", Name: "analyze", RawArgs: `{"query":"data"}`}, ToolExecuteContext{Emit: func(v any) { live = append(live, v) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || len(live) != 1 {
		t.Fatalf("generator events should be emitted live and recorded")
	}
	if !IsGeneratorTool(generator) || !IsRegularExecuteTool(regular) || IsManualTool(regular) {
		t.Fatalf("tool guards misclassified tools")
	}

	out := FormatToolOutput(ToolExecutionResult{CallID: "c3", Name: "analyze", Output: result.Output})
	b, _ := json.Marshal(out)
	if !json.Valid(b) {
		t.Fatalf("formatted output should marshal")
	}
}
