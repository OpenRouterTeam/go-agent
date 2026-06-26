package agent

import (
	"context"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type NextTurnParamsContext struct {
	ToolCall ParsedToolCall
	Result   ToolExecutionResult
	Request  components.ResponsesRequest
}
type NextTurnParamsFunctions map[string]func(context.Context, NextTurnParamsContext) (any, error)

func BuildNextTurnParamsContext(call ParsedToolCall, result ToolExecutionResult, request components.ResponsesRequest) NextTurnParamsContext {
	return NextTurnParamsContext{ToolCall: call, Result: result, Request: request}
}
func ExecuteNextTurnParamsFunctions(ctx context.Context, funcs NextTurnParamsFunctions, ntctx NextTurnParamsContext) (map[string]any, error) {
	out := map[string]any{}
	for k, fn := range funcs {
		v, err := fn(ctx, ntctx)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}
func ApplyNextTurnParamsToRequest(request components.ResponsesRequest, params map[string]any) components.ResponsesRequest {
	if v, ok := params["model"].(string); ok {
		request.Model = &v
	}
	return request
}
