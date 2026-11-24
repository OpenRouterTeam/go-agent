package agent

import (
	"context"
	"fmt"
	"log"
	"sort"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// NextTurnParamsContext is what a NextTurnParams function sees. Request is
// the working request for the next turn — it already reflects values set by
// tools processed earlier in the same round, so its Input, ToolChoice,
// Model, Models, Temperature, MaxOutputTokens, TopP, TopK, and Instructions
// fields are the Go equivalent of upstream's NextTurnParamsContext fields.
type NextTurnParamsContext struct {
	ToolCall ParsedToolCall
	Result   ToolExecutionResult
	Request  components.ResponsesRequest
}
type NextTurnParamsFunctions map[string]func(context.Context, NextTurnParamsContext) (any, error)

func BuildNextTurnParamsContext(call ParsedToolCall, result ToolExecutionResult, request components.ResponsesRequest) NextTurnParamsContext {
	return NextTurnParamsContext{ToolCall: call, Result: result, Request: request}
}

// ExecuteNextTurnParamsFunctions runs each function in funcs against ntctx
// and returns the computed values keyed by parameter name. Keys run in
// upstream's canonical parameter order; unknown keys are skipped with a
// warning, as upstream does.
func ExecuteNextTurnParamsFunctions(ctx context.Context, funcs NextTurnParamsFunctions, ntctx NextTurnParamsContext) (map[string]any, error) {
	out := map[string]any{}
	keys := make([]string, 0, len(funcs))
	for k := range funcs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !isValidNextTurnParamKey(k) {
			log.Printf("Invalid nextTurnParams key %q. Valid keys: input, toolChoice, model, models, temperature, maxOutputTokens, topP, topK, instructions", k)
			continue
		}
	}
	for _, k := range nextTurnParamKeyOrder {
		fn, ok := funcs[k]
		if !ok || fn == nil {
			continue
		}
		v, err := fn(ctx, ntctx)
		if err != nil {
			return nil, err
		}
		out[k] = v
		// Later keys see earlier keys' values, mirroring upstream's working
		// context composition.
		if next, err := applyNextTurnParam(ntctx.Request, k, v); err == nil {
			ntctx.Request = next
		}
	}
	return out, nil
}

// ApplyNextTurnParamsToRequest returns request with every computed
// parameter applied. Unknown keys and values of an unsupported type are
// ignored; use the tool-level NextTurnParams path for error reporting.
func ApplyNextTurnParamsToRequest(request components.ResponsesRequest, params map[string]any) components.ResponsesRequest {
	for _, key := range nextTurnParamKeyOrder {
		v, ok := params[key]
		if !ok {
			continue
		}
		if next, err := applyNextTurnParam(request, key, v); err == nil {
			request = next
		}
	}
	return request
}

func isValidNextTurnParamKey(key string) bool {
	for _, k := range nextTurnParamKeyOrder {
		if k == key {
			return true
		}
	}
	return false
}

// applyNextTurnParam sets one upstream-named parameter on req. A nil value
// clears an optional field (upstream strips null to undefined).
func applyNextTurnParam(req components.ResponsesRequest, key string, v any) (components.ResponsesRequest, error) {
	switch key {
	case "input":
		switch x := v.(type) {
		case nil:
			req.Input = nil
		case components.InputsUnion:
			req.Input = &x
		case *components.InputsUnion:
			req.Input = x
		case []components.InputsUnion1:
			req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(append([]components.InputsUnion1{}, x...)))
		case string:
			req.Input = openrouter.Pointer(components.CreateInputsUnionStr(x))
		default:
			return req, fmt.Errorf("unsupported input value %T", v)
		}
	case "toolChoice":
		switch x := v.(type) {
		case nil:
			req.ToolChoice = nil
		case components.OpenAIResponsesToolChoiceUnion:
			req.ToolChoice = &x
		case *components.OpenAIResponsesToolChoiceUnion:
			req.ToolChoice = x
		default:
			return req, fmt.Errorf("unsupported toolChoice value %T", v)
		}
	case "model":
		switch x := v.(type) {
		case nil:
			req.Model = nil
		case string:
			req.Model = openrouter.Pointer(x)
		case *string:
			req.Model = x
		default:
			return req, fmt.Errorf("unsupported model value %T", v)
		}
	case "models":
		switch x := v.(type) {
		case nil:
			req.Models = nil
		case []string:
			req.Models = append([]string{}, x...)
		default:
			return req, fmt.Errorf("unsupported models value %T", v)
		}
	case "temperature", "topP":
		f, isNil, err := floatParam(v)
		if err != nil {
			return req, fmt.Errorf("unsupported %s value: %w", key, err)
		}
		val := optionalnullable.OptionalNullable[float64](nil)
		if !isNil {
			val = optionalnullable.From(&f)
		}
		if key == "temperature" {
			req.Temperature = val
		} else {
			req.TopP = val
		}
	case "maxOutputTokens":
		n, isNil, err := intParam(v)
		if err != nil {
			return req, fmt.Errorf("unsupported maxOutputTokens value: %w", err)
		}
		if isNil {
			req.MaxOutputTokens = nil
		} else {
			req.MaxOutputTokens = optionalnullable.From(&n)
		}
	case "topK":
		n, isNil, err := intParam(v)
		if err != nil {
			return req, fmt.Errorf("unsupported topK value: %w", err)
		}
		if isNil {
			req.TopK = nil
		} else {
			req.TopK = &n
		}
	case "instructions":
		switch x := v.(type) {
		case nil:
			req.Instructions = nil
		case string:
			req.Instructions = optionalnullable.From(openrouter.String(x))
		case *string:
			if x == nil {
				req.Instructions = nil
			} else {
				req.Instructions = optionalnullable.From(openrouter.String(*x))
			}
		default:
			return req, fmt.Errorf("unsupported instructions value %T", v)
		}
	default:
		return req, fmt.Errorf("unknown nextTurnParams key %q", key)
	}
	return req, nil
}

func floatParam(v any) (float64, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, true, nil
	case float64:
		return x, false, nil
	case float32:
		return float64(x), false, nil
	case int:
		return float64(x), false, nil
	case int64:
		return float64(x), false, nil
	case *float64:
		if x == nil {
			return 0, true, nil
		}
		return *x, false, nil
	}
	return 0, false, fmt.Errorf("%T", v)
}

func intParam(v any) (int64, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, true, nil
	case int:
		return int64(x), false, nil
	case int32:
		return int64(x), false, nil
	case int64:
		return x, false, nil
	case float64:
		return int64(x), false, nil
	case *int64:
		if x == nil {
			return 0, true, nil
		}
		return *x, false, nil
	}
	return 0, false, fmt.Errorf("%T", v)
}
