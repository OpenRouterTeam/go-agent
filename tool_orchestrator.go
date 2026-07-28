package agent

import (
	"context"
	"fmt"
)

func ExecuteToolLoop(ctx context.Context, tools []Tool, calls []ParsedToolCall, store *ToolContextStore) ([]ToolExecutionResult, error) {
	results := make([]ToolExecutionResult, 0, len(calls))
	for _, call := range calls {
		t := FindToolByName(tools, call.Name)
		if t == nil {
			results = append(results, ToolExecutionResult{
				CallID: call.CallID,
				Name:   call.Name,
				Source: ToolSourceClient,
				Error:  fmt.Errorf("tool %q not found in tool definitions", call.Name),
			})
			continue
		}
		res, err := ExecuteTool(ctx, t, call, BuildToolExecuteContext(call, TurnContext{}, store, nil))
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

func ToolResultsToMap(results []ToolExecutionResult) map[string]ToolExecutionResult {
	out := map[string]ToolExecutionResult{}
	for _, r := range results {
		out[r.CallID] = r
	}
	return out
}
func SummarizeToolExecutions(results []ToolExecutionResult) []string {
	out := []string{}
	for _, r := range results {
		if r.Error != nil {
			out = append(out, r.Name+": "+r.Error.Error())
		} else {
			out = append(out, r.Name)
		}
	}
	return out
}
func HasToolExecutionErrors(results []ToolExecutionResult) bool {
	for _, r := range results {
		if r.Error != nil {
			return true
		}
	}
	return false
}
func GetToolExecutionErrors(results []ToolExecutionResult) []error {
	var out []error
	for _, r := range results {
		if r.Error != nil {
			out = append(out, r.Error)
		}
	}
	return out
}
