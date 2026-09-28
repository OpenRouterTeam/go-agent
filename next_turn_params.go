package agent

import (
	"context"
	"fmt"
	"log"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// NextTurnParamsContext is what a NextTurnParams function sees: the executed
// call (its Arguments are the tool's params), its result, and the working
// request for the next turn. Later tools see modifications made by earlier
// ones (composition, in Tools order), mirroring upstream's working context.
type NextTurnParamsContext struct {
	ToolCall ParsedToolCall
	Result   ToolExecutionResult
	Request  components.ResponsesRequest
}

// NextTurnParamsFunctions maps a next-turn parameter key to a function
// computing its new value. Valid keys mirror upstream: "input",
// "toolChoice", "model", "models", "temperature", "maxOutputTokens",
// "topP", "topK", "instructions". Setting "toolChoice" changes which tools
// the model may call next turn without touching `tools` (upstream #114), so
// the provider's prompt-cache prefix survives; the new choice becomes the
// caller-level policy (a forced value is relaxed after it produces a call).
type NextTurnParamsFunctions map[string]func(context.Context, NextTurnParamsContext) (any, error)

// nextTurnParamKeys is the valid key set, in the deterministic order keys
// are applied (Go maps are unordered; upstream follows declaration order).
var nextTurnParamKeys = []string{"input", "toolChoice", "model", "models", "temperature", "maxOutputTokens", "topP", "topK", "instructions"}

func isValidNextTurnParamKey(key string) bool {
	for _, k := range nextTurnParamKeys {
		if k == key {
			return true
		}
	}
	return false
}

// nextTurnParamsTool is implemented by tools that declare NextTurnParams.
type nextTurnParamsTool interface {
	NextTurnParams() NextTurnParamsFunctions
}

func BuildNextTurnParamsContext(call ParsedToolCall, result ToolExecutionResult, request components.ResponsesRequest) NextTurnParamsContext {
	return NextTurnParamsContext{ToolCall: call, Result: result, Request: request}
}

// ExecuteNextTurnParamsFunctions runs one tool call's NextTurnParams
// functions, in canonical key order, returning the computed values. Invalid
// keys are skipped with a warning, as upstream does.
func ExecuteNextTurnParamsFunctions(ctx context.Context, funcs NextTurnParamsFunctions, ntctx NextTurnParamsContext) (map[string]any, error) {
	out := map[string]any{}
	for key := range funcs {
		if !isValidNextTurnParamKey(key) {
			log.Printf("Invalid nextTurnParams key %q in tool %q. Valid keys: input, toolChoice, model, models, temperature, maxOutputTokens, topP, topK, instructions", key, ntctx.ToolCall.Name)
		}
	}
	for _, key := range nextTurnParamKeys {
		fn, ok := funcs[key]
		if !ok || fn == nil {
			continue
		}
		v, err := fn(ctx, ntctx)
		if err != nil {
			return nil, fmt.Errorf("nextTurnParams %q for %s: %w", key, ntctx.ToolCall.Name, err)
		}
		out[key] = v
		// Compose: later keys and tools see this change.
		ntctx.Request = ApplyNextTurnParamsToRequest(ntctx.Request, map[string]any{key: v})
	}
	return out, nil
}

// ApplyNextTurnParamsToRequest returns request with the computed next-turn
// parameters applied. A nil value clears the field (upstream strips null to
// undefined). Values of an unexpected type are ignored.
func ApplyNextTurnParamsToRequest(request components.ResponsesRequest, params map[string]any) components.ResponsesRequest {
	for key, value := range params {
		switch key {
		case "model":
			switch v := value.(type) {
			case string:
				request.Model = &v
			case nil:
				request.Model = nil
			}
		case "models":
			switch v := value.(type) {
			case []string:
				request.Models = append([]string{}, v...)
			case nil:
				request.Models = nil
			}
		case "temperature":
			if f, ok := toFloat(value); ok {
				request.Temperature = optionalnullable.From(&f)
			} else if value == nil {
				request.Temperature.Unset()
			}
		case "topP":
			if f, ok := toFloat(value); ok {
				request.TopP = optionalnullable.From(&f)
			} else if value == nil {
				request.TopP.Unset()
			}
		case "maxOutputTokens":
			if n, ok := toInt64(value); ok {
				request.MaxOutputTokens = optionalnullable.From(&n)
			} else if value == nil {
				request.MaxOutputTokens.Unset()
			}
		case "topK":
			if n, ok := toInt64(value); ok {
				request.TopK = &n
			} else if value == nil {
				request.TopK = nil
			}
		case "instructions":
			switch v := value.(type) {
			case string:
				request.Instructions = optionalnullable.From(&v)
			case nil:
				request.Instructions.Unset()
			}
		case "toolChoice":
			switch v := value.(type) {
			case components.OpenAIResponsesToolChoiceUnion:
				request.ToolChoice = &v
			case *components.OpenAIResponsesToolChoiceUnion:
				request.ToolChoice = v
			case nil:
				request.ToolChoice = nil
			}
		case "input":
			switch v := value.(type) {
			case []components.InputsUnion1:
				in := components.CreateInputsUnionArrayOfInputsUnion1(append([]components.InputsUnion1{}, v...))
				request.Input = &in
			case components.InputsUnion:
				request.Input = &v
			case string:
				in := components.CreateInputsUnionStr(v)
				request.Input = &in
			}
		}
	}
	return request
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case *float64:
		if x != nil {
			return *x, true
		}
	}
	return 0, false
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case float64:
		return int64(x), true
	case *int64:
		if x != nil {
			return *x, true
		}
	}
	return 0, false
}

// applyToolNextTurnParams runs NextTurnParams for every executed call, in
// Tools order then call order (upstream executeNextTurnParamsFunctions),
// composing onto request. changedToolChoice reports whether a tool set the
// toolChoice key, so the caller can re-derive its forced-choice policy.
func applyToolNextTurnParams(ctx context.Context, tools []Tool, executed []ToolExecutionResult, calls []ParsedToolCall, request components.ResponsesRequest) (components.ResponsesRequest, bool, error) {
	changedToolChoice := false
	for _, t := range tools {
		nt, ok := t.(nextTurnParamsTool)
		if !ok || IsServerTool(t) {
			continue
		}
		funcs := nt.NextTurnParams()
		if len(funcs) == 0 {
			continue
		}
		for i, call := range calls {
			if call.Name != t.ToolName() {
				continue
			}
			if call.Arguments != nil {
				if _, isObject := call.Arguments.(map[string]any); !isObject {
					return request, false, fmt.Errorf("tool call arguments for %s must be an object, got %s", t.ToolName(), describeJSONType(call.Arguments))
				}
			}
			var result ToolExecutionResult
			if i < len(executed) {
				result = executed[i]
			}
			computed, err := ExecuteNextTurnParamsFunctions(ctx, funcs, BuildNextTurnParamsContext(call, result, request))
			if err != nil {
				return request, false, err
			}
			if _, ok := computed["toolChoice"]; ok {
				changedToolChoice = true
			}
			request = ApplyNextTurnParamsToRequest(request, computed)
		}
	}
	return request, changedToolChoice, nil
}
