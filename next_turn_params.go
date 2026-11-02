package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// NextTurnParamsContext carries the current request parameters a
// nextTurnParams function may read and replace (upstream
// NextTurnParamsContext: input, toolChoice, model, models, temperature,
// maxOutputTokens, topP, topK, instructions).
//
// When several executed tools compute the same parameter, they compose: each
// later function sees the value produced by the earlier ones (tools in the
// Tools-slice order, calls in response order).
type NextTurnParamsContext struct {
	ToolCall ParsedToolCall
	Result   ToolExecutionResult
	Request  components.ResponsesRequest

	// Input is the current accumulated input.
	Input components.InputsUnion
	// ToolChoice is the current tool choice. Returning a new value changes
	// which tools the model may call next turn without touching the `tools`
	// array — the hook a tool-search tool uses to widen an `allowed_tools`
	// choice. Leaving `tools` alone preserves the provider's prompt-cache
	// prefix. A tool-computed choice becomes the new caller-level policy, so
	// the forced-choice relaxation re-arms for it.
	ToolChoice      *components.OpenAIResponsesToolChoiceUnion
	Model           string
	Models          []string
	Temperature     *float64
	MaxOutputTokens *int64
	TopP            *float64
	TopK            *int64
	Instructions    *string
}

// NextTurnParams declares per-tool functions that compute request parameters
// for the turn after this tool executes (upstream tool `nextTurnParams`).
// Each function receives the tool's decoded input and the current
// NextTurnParamsContext. A nil pointer return unsets that request field
// (upstream maps `null` to `undefined`).
type NextTurnParams[In any] struct {
	Input           func(context.Context, In, NextTurnParamsContext) (components.InputsUnion, error)
	ToolChoice      func(context.Context, In, NextTurnParamsContext) (*components.OpenAIResponsesToolChoiceUnion, error)
	Model           func(context.Context, In, NextTurnParamsContext) (string, error)
	Models          func(context.Context, In, NextTurnParamsContext) ([]string, error)
	Temperature     func(context.Context, In, NextTurnParamsContext) (*float64, error)
	MaxOutputTokens func(context.Context, In, NextTurnParamsContext) (*int64, error)
	TopP            func(context.Context, In, NextTurnParamsContext) (*float64, error)
	TopK            func(context.Context, In, NextTurnParamsContext) (*int64, error)
	Instructions    func(context.Context, In, NextTurnParamsContext) (*string, error)
}

type NextTurnParamsFunctions map[string]func(context.Context, NextTurnParamsContext) (any, error)

// nextTurnParamKeys is the set of valid nextTurnParams keys, in the order
// they are evaluated (upstream isValidNextTurnParamKey).
var nextTurnParamKeys = []string{"input", "toolChoice", "model", "models", "temperature", "maxOutputTokens", "topP", "topK", "instructions"}

func isValidNextTurnParamKey(key string) bool {
	for _, k := range nextTurnParamKeys {
		if k == key {
			return true
		}
	}
	return false
}

// BuildNextTurnParamsContext builds a NextTurnParamsContext from the current
// request, extracting the fields nextTurnParams functions may modify.
func BuildNextTurnParamsContext(call ParsedToolCall, result ToolExecutionResult, request components.ResponsesRequest) NextTurnParamsContext {
	ntc := NextTurnParamsContext{ToolCall: call, Result: result, Request: request, ToolChoice: request.ToolChoice, Models: append([]string{}, request.Models...), TopK: request.TopK}
	if request.Input != nil {
		ntc.Input = *request.Input
	} else {
		ntc.Input = components.CreateInputsUnionArrayOfInputsUnion1([]components.InputsUnion1{})
	}
	if request.Model != nil {
		ntc.Model = *request.Model
	}
	if v, ok := request.Temperature.GetOrZero(); ok {
		ntc.Temperature = &v
	}
	if v, ok := request.MaxOutputTokens.GetOrZero(); ok {
		ntc.MaxOutputTokens = &v
	}
	if v, ok := request.TopP.GetOrZero(); ok {
		ntc.TopP = &v
	}
	if v, ok := request.Instructions.GetOrZero(); ok {
		ntc.Instructions = &v
	}
	return ntc
}

// ExecuteNextTurnParamsFunctions runs a key-addressed set of nextTurnParams
// functions against ntctx. Keys are evaluated in the upstream key order;
// later keys see earlier results. Unknown keys are skipped with a warning.
func ExecuteNextTurnParamsFunctions(ctx context.Context, funcs NextTurnParamsFunctions, ntctx NextTurnParamsContext) (map[string]any, error) {
	out := map[string]any{}
	for k := range funcs {
		if !isValidNextTurnParamKey(k) {
			log.Printf("Invalid nextTurnParams key %q. Valid keys: input, toolChoice, model, models, temperature, maxOutputTokens, topP, topK, instructions", k)
		}
	}
	for _, k := range nextTurnParamKeys {
		fn, ok := funcs[k]
		if !ok || fn == nil {
			continue
		}
		v, err := fn(ctx, ntctx)
		if err != nil {
			return nil, err
		}
		out[k] = v
		setNextTurnParam(&ntctx, k, v)
	}
	return out, nil
}

// setNextTurnParam writes one computed value into a working context so later
// functions compose on it.
func setNextTurnParam(ntc *NextTurnParamsContext, key string, value any) {
	req := ApplyNextTurnParamsToRequest(ntc.Request, map[string]any{key: value})
	updated := BuildNextTurnParamsContext(ntc.ToolCall, ntc.Result, req)
	*ntc = updated
}

// ApplyNextTurnParamsToRequest returns a copy of request with the computed
// parameters applied. A nil value unsets the field (upstream strips `null` to
// `undefined`). Values of an unexpected type for their key are ignored.
func ApplyNextTurnParamsToRequest(request components.ResponsesRequest, params map[string]any) components.ResponsesRequest {
	for key, value := range params {
		switch key {
		case "input":
			switch v := value.(type) {
			case components.InputsUnion:
				request.Input = &v
			case *components.InputsUnion:
				request.Input = v
			case []components.InputsUnion1:
				in := components.CreateInputsUnionArrayOfInputsUnion1(append([]components.InputsUnion1{}, v...))
				request.Input = &in
			case nil:
				request.Input = nil
			}
		case "toolChoice":
			switch v := value.(type) {
			case *components.OpenAIResponsesToolChoiceUnion:
				request.ToolChoice = v
			case components.OpenAIResponsesToolChoiceUnion:
				request.ToolChoice = &v
			case nil:
				request.ToolChoice = nil
			}
		case "model":
			switch v := value.(type) {
			case string:
				request.Model = &v
			case *string:
				request.Model = v
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
			request.Temperature = optionalFloat(value, request.Temperature)
		case "topP":
			request.TopP = optionalFloat(value, request.TopP)
		case "maxOutputTokens":
			switch v := value.(type) {
			case int64:
				request.MaxOutputTokens = optionalnullable.From(&v)
			case int:
				n := int64(v)
				request.MaxOutputTokens = optionalnullable.From(&n)
			case *int64:
				request.MaxOutputTokens = optionalFromPtr(v)
			case nil:
				request.MaxOutputTokens = nil
			}
		case "topK":
			switch v := value.(type) {
			case int64:
				request.TopK = &v
			case int:
				n := int64(v)
				request.TopK = &n
			case *int64:
				request.TopK = v
			case nil:
				request.TopK = nil
			}
		case "instructions":
			switch v := value.(type) {
			case string:
				request.Instructions = optionalnullable.From(&v)
			case *string:
				request.Instructions = optionalFromPtr(v)
			case nil:
				request.Instructions = nil
			}
		}
	}
	return request
}

func optionalFloat(value any, current optionalnullable.OptionalNullable[float64]) optionalnullable.OptionalNullable[float64] {
	switch v := value.(type) {
	case float64:
		return optionalnullable.From(&v)
	case *float64:
		return optionalFromPtr(v)
	case nil:
		return nil
	}
	return current
}

// optionalFromPtr maps a nil pointer to an unset field (not an explicit null),
// mirroring upstream's null → undefined sanitization.
func optionalFromPtr[T any](v *T) optionalnullable.OptionalNullable[T] {
	if v == nil {
		return nil
	}
	return optionalnullable.From(v)
}

// nextTurnParamsProvider is implemented by tools that declare NextTurnParams.
type nextTurnParamsProvider interface {
	computeNextTurnParams(ctx context.Context, call ParsedToolCall, working *NextTurnParamsContext) (map[string]any, error)
}

func (t *TypedTool[In]) computeNextTurnParams(ctx context.Context, call ParsedToolCall, working *NextTurnParamsContext) (map[string]any, error) {
	p := t.config.NextTurnParams
	if p == nil {
		return nil, nil
	}
	input, err := decodeCallInput[In](call)
	if err != nil {
		return nil, fmt.Errorf("tool call arguments for %s must be an object: %w", t.config.Name, err)
	}
	out := map[string]any{}
	run := func(key string, fn func() (any, error)) error {
		v, err := fn()
		if err != nil {
			return err
		}
		out[key] = v
		setNextTurnParam(working, key, v)
		return nil
	}
	steps := []struct {
		key string
		set bool
		fn  func() (any, error)
	}{
		{"input", p.Input != nil, func() (any, error) { return p.Input(ctx, input, *working) }},
		{"toolChoice", p.ToolChoice != nil, func() (any, error) { return p.ToolChoice(ctx, input, *working) }},
		{"model", p.Model != nil, func() (any, error) { return p.Model(ctx, input, *working) }},
		{"models", p.Models != nil, func() (any, error) { return p.Models(ctx, input, *working) }},
		{"temperature", p.Temperature != nil, func() (any, error) { return p.Temperature(ctx, input, *working) }},
		{"maxOutputTokens", p.MaxOutputTokens != nil, func() (any, error) { return p.MaxOutputTokens(ctx, input, *working) }},
		{"topP", p.TopP != nil, func() (any, error) { return p.TopP(ctx, input, *working) }},
		{"topK", p.TopK != nil, func() (any, error) { return p.TopK(ctx, input, *working) }},
		{"instructions", p.Instructions != nil, func() (any, error) { return p.Instructions(ctx, input, *working) }},
	}
	for _, s := range steps {
		if !s.set {
			continue
		}
		if err := run(s.key, s.fn); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// decodeCallInput decodes a call's arguments into the tool's input type.
func decodeCallInput[In any](call ParsedToolCall) (In, error) {
	var input In
	raw := []byte("{}")
	if call.RawArgs != "" {
		raw = []byte(call.RawArgs)
	} else if call.Arguments != nil {
		b, err := json.Marshal(call.Arguments)
		if err != nil {
			return input, err
		}
		raw = b
	}
	if string(raw) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return input, err
	}
	return input, nil
}

// executeToolNextTurnParams runs the NextTurnParams of every tool that was
// called in calls (tools in slice order, calls in response order), composing
// results through a shared working context, and returns the computed params
// (upstream executeNextTurnParamsFunctions).
func executeToolNextTurnParams(ctx context.Context, calls []ParsedToolCall, tools []Tool, request components.ResponsesRequest) (map[string]any, error) {
	result := map[string]any{}
	if len(calls) == 0 {
		return result, nil
	}
	working := BuildNextTurnParamsContext(ParsedToolCall{}, ToolExecutionResult{}, request)
	for _, t := range tools {
		if IsServerTool(t) {
			continue
		}
		provider, ok := t.(nextTurnParamsProvider)
		if !ok {
			continue
		}
		for _, call := range calls {
			if call.Name != t.ToolName() {
				continue
			}
			working.ToolCall = call
			computed, err := provider.computeNextTurnParams(ctx, call, &working)
			if err != nil {
				return nil, err
			}
			for k, v := range computed {
				result[k] = v
			}
		}
	}
	return result, nil
}
