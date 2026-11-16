package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// NextTurnParamsContext is what a nextTurnParams function sees: the call
// that succeeded, its execution result, and the request as composed so far
// (earlier functions' values are already applied, so functions compose).
type NextTurnParamsContext struct {
	ToolCall ParsedToolCall
	Result   ToolExecutionResult
	Request  components.ResponsesRequest
}

// NextTurnParamsFunctions maps a request parameter name to a function that
// computes its value for the next model turn. Valid keys mirror upstream:
// "input", "toolChoice", "model", "models", "temperature",
// "maxOutputTokens", "topP", "topK", "instructions". Unknown keys are
// skipped with a warning. A nil value clears the parameter.
//
// Functions run only for calls whose tool body ran and returned without
// error; a call that was blocked by a hook, denied, failed validation or
// errored never changes the next request. Tools are processed in tools-list
// order, calls in call order, and keys in sorted order (Go maps are
// unordered; upstream uses object insertion order).
type NextTurnParamsFunctions map[string]func(context.Context, NextTurnParamsContext) (any, error)

var validNextTurnParamKeys = map[string]bool{
	"input": true, "toolChoice": true, "model": true, "models": true, "temperature": true,
	"maxOutputTokens": true, "topP": true, "topK": true, "instructions": true,
}

func BuildNextTurnParamsContext(call ParsedToolCall, result ToolExecutionResult, request components.ResponsesRequest) NextTurnParamsContext {
	return NextTurnParamsContext{ToolCall: call, Result: result, Request: request}
}

// ExecuteNextTurnParamsFunctions runs funcs for one call (keys in sorted
// order), composing each value onto ntctx.Request before the next function
// runs. Returns the computed values keyed by parameter name.
func ExecuteNextTurnParamsFunctions(ctx context.Context, funcs NextTurnParamsFunctions, ntctx NextTurnParamsContext) (map[string]any, error) {
	out := map[string]any{}
	keys := make([]string, 0, len(funcs))
	for k := range funcs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fn := funcs[k]
		if fn == nil {
			continue
		}
		if !validNextTurnParamKeys[k] {
			log.Printf("Invalid nextTurnParams key %q in tool %q. Valid keys: input, toolChoice, model, models, temperature, maxOutputTokens, topP, topK, instructions", k, ntctx.ToolCall.Name)
			continue
		}
		v, err := fn(ctx, ntctx)
		if err != nil {
			return nil, err
		}
		out[k] = v
		next, err := applyNextTurnParam(ntctx.Request, k, v)
		if err != nil {
			return nil, err
		}
		ntctx.Request = next
	}
	return out, nil
}

// ApplyNextTurnParamsToRequest returns request with params applied. Values
// of an unsupported Go type for their key are ignored.
func ApplyNextTurnParamsToRequest(request components.ResponsesRequest, params map[string]any) components.ResponsesRequest {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if next, err := applyNextTurnParam(request, k, params[k]); err == nil {
			request = next
		}
	}
	return request
}

func applyNextTurnParam(request components.ResponsesRequest, key string, value any) (components.ResponsesRequest, error) {
	switch key {
	case "model":
		switch v := value.(type) {
		case nil:
			request.Model = nil
		case string:
			request.Model = openrouter.String(v)
		case *string:
			request.Model = v
		default:
			return request, nextTurnTypeError(key, value)
		}
	case "models":
		switch v := value.(type) {
		case nil:
			request.Models = nil
		case []string:
			request.Models = append([]string{}, v...)
		default:
			return request, nextTurnTypeError(key, value)
		}
	case "temperature", "topP":
		f, isNil, ok := toFloat(value)
		if !ok {
			return request, nextTurnTypeError(key, value)
		}
		var field optionalnullable.OptionalNullable[float64]
		if !isNil {
			field = optionalnullable.From(&f)
		}
		if key == "temperature" {
			request.Temperature = field
		} else {
			request.TopP = field
		}
	case "maxOutputTokens":
		f, isNil, ok := toFloat(value)
		if !ok {
			return request, nextTurnTypeError(key, value)
		}
		if isNil {
			request.MaxOutputTokens = nil
		} else {
			n := int64(f)
			request.MaxOutputTokens = optionalnullable.From(&n)
		}
	case "topK":
		f, isNil, ok := toFloat(value)
		if !ok {
			return request, nextTurnTypeError(key, value)
		}
		if isNil {
			request.TopK = nil
		} else {
			n := int64(f)
			request.TopK = &n
		}
	case "instructions":
		switch v := value.(type) {
		case nil:
			request.Instructions = nil
		case string:
			request.Instructions = optionalnullable.From(openrouter.String(v))
		default:
			return request, nextTurnTypeError(key, value)
		}
	case "toolChoice":
		switch v := value.(type) {
		case nil:
			request.ToolChoice = nil
		case components.OpenAIResponsesToolChoiceUnion:
			request.ToolChoice = &v
		case *components.OpenAIResponsesToolChoiceUnion:
			request.ToolChoice = v
		case string, map[string]any:
			choice, err := toolChoiceFromJSONValue(v)
			if err != nil {
				return request, err
			}
			request.ToolChoice = choice
		default:
			return request, nextTurnTypeError(key, value)
		}
	case "input":
		switch v := value.(type) {
		case nil:
			request.Input = nil
		case components.InputsUnion:
			request.Input = &v
		case *components.InputsUnion:
			request.Input = v
		case []components.InputsUnion1:
			request.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(append([]components.InputsUnion1{}, v...)))
		case string:
			request.Input = openrouter.Pointer(components.CreateInputsUnionStr(v))
		default:
			return request, nextTurnTypeError(key, value)
		}
	default:
		return request, fmt.Errorf("invalid nextTurnParams key %q", key)
	}
	return request, nil
}

func nextTurnTypeError(key string, value any) error {
	return fmt.Errorf("nextTurnParams %q: unsupported value type %T", key, value)
}

func toFloat(v any) (f float64, isNil bool, ok bool) {
	switch n := v.(type) {
	case nil:
		return 0, true, true
	case float64:
		return n, false, true
	case float32:
		return float64(n), false, true
	case int:
		return float64(n), false, true
	case int64:
		return float64(n), false, true
	case int32:
		return float64(n), false, true
	}
	return 0, false, false
}

// toolChoiceFromJSONValue decodes a JSON-shaped tool choice ("auto",
// "required", {"type":"function","name":...}, {"type":"allowed_tools",...}).
func toolChoiceFromJSONValue(v any) (*components.OpenAIResponsesToolChoiceUnion, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("nextTurnParams toolChoice: %w", err)
	}
	var choice components.OpenAIResponsesToolChoiceUnion
	if err := json.Unmarshal(b, &choice); err != nil {
		return nil, fmt.Errorf("nextTurnParams toolChoice: %w", err)
	}
	return &choice, nil
}
