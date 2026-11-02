package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
	"github.com/invopop/jsonschema"
)

var ErrReservedToolName = errors.New("tool name 'shared' is reserved")

type ToolConfig[In any] struct {
	Name               string
	Description        string
	InputSchema        map[string]any
	OutputSchema       map[string]any
	EventSchema        map[string]any
	Execute            func(context.Context, In, ToolExecuteContext) (any, error)
	Generate           func(context.Context, In, ToolExecuteContext, func(any) error) (any, error)
	Manual             bool
	RequireApproval    bool
	Approval           ToolApprovalCheck
	OnToolCalled       func(context.Context, In, ToolExecuteContext) (any, bool, error)
	OnResponseReceived func(context.Context, any, ToolExecuteContext) (any, error)
	// Strict is forwarded as the wire tool definition's `strict` flag so
	// providers can enforce structured-outputs-style schema adherence on
	// tool-call arguments. nil sends `strict: null` (provider default).
	// OpenAI-style strict schemas require every object property to be listed
	// in `required`.
	Strict *bool
	// WireInputSchema, for manual tools only, is the JSON Schema serialized
	// as the wire tool's `parameters` instead of the InputSchema/struct-derived
	// schema. Manual tools are never executed or input-validated by the SDK,
	// so a caller that already owns a JSON Schema (including constructs such
	// as anyOf/oneOf) can forward it verbatim. It is copied and sanitized of
	// `~`-prefixed metadata keys; the caller's map is never mutated. Ignored
	// for executable tools.
	WireInputSchema map[string]any
	// NextTurnParams computes request parameters (including ToolChoice) for
	// the turn after this tool executes.
	NextTurnParams *NextTurnParams[In]
}

type TypedTool[In any] struct {
	config ToolConfig[In]
	schema map[string]any
}

func NewTool[In any](config ToolConfig[In]) (*TypedTool[In], error) {
	if config.Name == "shared" {
		return nil, ErrReservedToolName
	}
	if config.Name == "" {
		return nil, errors.New("tool name is required")
	}
	t := &TypedTool[In]{config: config}
	if config.InputSchema != nil {
		t.schema = copyMap(config.InputSchema)
	} else {
		schema, err := schemaFor[In]()
		if err != nil {
			return nil, err
		}
		t.schema = schema
	}
	t.schema = SanitizeSchema(t.schema)
	return t, nil
}

func MustNewTool[In any](config ToolConfig[In]) *TypedTool[In] {
	t, err := NewTool(config)
	if err != nil {
		panic(err)
	}
	return t
}

func (t *TypedTool[In]) ToolName() string             { return t.config.Name }
func (t *TypedTool[In]) ToolDescription() string      { return t.config.Description }
func (t *TypedTool[In]) InputSchema() map[string]any  { return copyMap(t.schema) }
func (t *TypedTool[In]) OutputSchema() map[string]any { return copyMap(t.config.OutputSchema) }
func (t *TypedTool[In]) EventSchema() map[string]any  { return copyMap(t.config.EventSchema) }

func (t *TypedTool[In]) ToolType() ToolType {
	switch {
	case t.config.OnToolCalled != nil:
		return ToolTypeHITL
	case t.config.Manual || (t.config.Execute == nil && t.config.Generate == nil):
		return ToolTypeManual
	case t.config.Generate != nil:
		return ToolTypeGenerator
	default:
		return ToolTypeRegular
	}
}

func (t *TypedTool[In]) RequiresApproval(ctx context.Context, call ParsedToolCall, turn TurnContext) (bool, error) {
	if t.config.Approval != nil {
		// The predicate must decide on exactly what Execute will receive.
		// When the arguments fail the tool's input validation there is no
		// trustworthy value to judge: fail closed (require approval) rather
		// than letting schema-invalid wire arguments bypass the gate
		// (upstream #54). A zero call (no arguments at all) is the
		// configuration probe used by ToolHasApprovalConfigured and is
		// passed through unchanged.
		if call.RawArgs != "" || call.Arguments != nil {
			if !t.argumentsValid(call) {
				return true, nil
			}
		}
		return t.config.Approval(ctx, call, turn)
	}
	return t.config.RequireApproval, nil
}

func (t *TypedTool[In]) HandleResponseReceived(ctx context.Context, output any, execCtx ToolExecuteContext) (any, error) {
	if t.config.OnResponseReceived == nil {
		return output, nil
	}
	return t.config.OnResponseReceived(ctx, output, execCtx)
}

// argumentsValid reports whether call's arguments decode into In and pass
// the tool's input schema — the same checks Execute applies.
func (t *TypedTool[In]) argumentsValid(call ParsedToolCall) bool {
	raw := []byte("{}")
	if call.RawArgs != "" {
		raw = []byte(call.RawArgs)
	} else if call.Arguments != nil {
		b, err := json.Marshal(call.Arguments)
		if err != nil {
			return false
		}
		raw = b
	}
	if string(raw) == "null" {
		raw = []byte("{}")
	}
	var input In
	if err := json.Unmarshal(raw, &input); err != nil {
		return false
	}
	var asObject map[string]any
	if err := json.Unmarshal(raw, &asObject); err != nil || asObject == nil {
		// Valid JSON that is not an object: the predicate contract is an
		// object, so there is no trustworthy value to judge.
		return false
	}
	return ValidateAgainstSchema(raw, t.schema) == nil
}

func (t *TypedTool[In]) ToAPITool() components.ResponsesRequestToolUnion {
	params := t.InputSchema()
	if t.ToolType() == ToolTypeManual && t.config.WireInputSchema != nil {
		params = SanitizeSchema(t.config.WireInputSchema)
	}
	fn := components.ResponsesRequestToolFunction{
		Name:       t.config.Name,
		Parameters: params,
		Type:       components.ResponsesRequestTypeFunction,
	}
	if t.config.Description != "" {
		fn.Description = optionalnullable.From(openrouter.String(t.config.Description))
	}
	// Pass the caller's strict flag through instead of hardcoding it; unset
	// serializes as `strict: null` (upstream `strict ?? null`).
	fn.Strict = optionalnullable.From(t.config.Strict)
	return components.CreateResponsesRequestToolUnionFunction(fn)
}

func (t *TypedTool[In]) Execute(ctx context.Context, raw json.RawMessage, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	var input In
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Error: err}, nil
	}
	if err := ValidateAgainstSchema(raw, t.schema); err != nil {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Error: err}, nil
	}
	if t.config.OnToolCalled != nil {
		out, proceed, err := t.config.OnToolCalled(ctx, input, execCtx)
		if err != nil || !proceed {
			if err == nil && !proceed {
				err = ErrHITLPause
			}
			return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out, Error: err}, nil
		}
		if err := t.validateOutput(out); err != nil {
			return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out, Error: err}, nil
		}
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out}, nil
	}
	if t.config.Generate != nil {
		// Yields are broadcast live through execCtx.Emit and are NOT
		// accumulated on the result: a long-running generator would otherwise
		// retain its whole yield history in memory (upstream dropped
		// `preliminaryResults` accumulation for the same reason).
		out, err := t.config.Generate(ctx, input, execCtx, func(v any) error {
			if err := validateAny(v, t.config.EventSchema); err != nil {
				return err
			}
			if execCtx.Emit != nil {
				execCtx.Emit(v)
			}
			return nil
		})
		if err != nil {
			return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Error: err}, nil
		}
		if err := t.validateOutput(out); err != nil {
			return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out, Error: err}, nil
		}
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out}, nil
	}
	if t.config.Execute == nil {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Error: ErrManualTool}, nil
	}
	out, err := t.config.Execute(ctx, input, execCtx)
	if err != nil {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Error: err}, nil
	}
	if err := t.validateOutput(out); err != nil {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out, Error: err}, nil
	}
	return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: t.config.Name, Output: out}, nil
}

func (t *TypedTool[In]) validateOutput(out any) error { return validateAny(out, t.config.OutputSchema) }

func validateAny(v any, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ValidateAgainstSchema(b, schema)
}

type ServerToolConfig struct {
	Name   string
	Config components.ResponsesRequestToolUnion
}

type serverToolImpl struct{ config ServerToolConfig }

func NewServerTool(config ServerToolConfig) Tool {
	return serverToolImpl{config: config}
}

func (s serverToolImpl) ToolName() string             { return s.config.Name }
func (s serverToolImpl) ToolDescription() string      { return "" }
func (s serverToolImpl) ToolType() ToolType           { return ToolTypeServer }
func (s serverToolImpl) InputSchema() map[string]any  { return nil }
func (s serverToolImpl) OutputSchema() map[string]any { return nil }
func (s serverToolImpl) EventSchema() map[string]any  { return nil }
func (s serverToolImpl) RequiresApproval(context.Context, ParsedToolCall, TurnContext) (bool, error) {
	return false, nil
}
func (s serverToolImpl) HandleResponseReceived(_ context.Context, output any, _ ToolExecuteContext) (any, error) {
	return output, nil
}
func (s serverToolImpl) ToAPITool() components.ResponsesRequestToolUnion { return s.config.Config }

func schemaFor[T any]() (map[string]any, error) {
	var zero T
	s := jsonschema.Reflect(zero)
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out["type"] == nil && reflect.TypeOf(zero).Kind() == reflect.Struct {
		out["type"] = "object"
	}
	return out, nil
}

func copyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyJSONValue(v)
	}
	return out
}

func deepCopyJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return copyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = deepCopyJSONValue(child)
		}
		return out
	default:
		return x
	}
}

// mcpBrand wraps a Tool with the additive MCP marker (see MarkMcp/IsMcpTool).
// Non-mutating: it forwards every Tool method to the wrapped tool unchanged,
// so runtime behavior and wire shape (ToAPITool) are identical to the
// unmarked tool. Execute is forwarded explicitly (rather than inherited via
// embedding, which only promotes the narrow Tool interface) so a marked
// executable tool keeps executing normally.
type mcpBrand struct{ Tool }

func (b mcpBrand) isMcpTool() {}

// computeNextTurnParams forwards a wrapped tool's NextTurnParams so marking a
// tool as MCP does not silently drop them.
func (b mcpBrand) computeNextTurnParams(ctx context.Context, call ParsedToolCall, working *NextTurnParamsContext) (map[string]any, error) {
	p, ok := b.Tool.(nextTurnParamsProvider)
	if !ok {
		return nil, nil
	}
	return p.computeNextTurnParams(ctx, call, working)
}

func (b mcpBrand) Execute(ctx context.Context, raw json.RawMessage, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	exec, ok := b.Tool.(ToolWithExecute)
	if !ok {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: b.Tool.ToolName(), Error: ErrManualTool}, nil
	}
	return exec.Execute(ctx, raw, execCtx)
}

// mcpMarked is implemented only by mcpBrand; IsMcpTool structurally checks
// for it rather than requiring a cast to a concrete type.
type mcpMarked interface{ isMcpTool() }

// MarkMcp adds the additive MCP brand to an already-built client tool. The
// tool's runtime behavior and wire shape are unchanged; only IsMcpTool's
// classification (and downstream Source discrimination on
// ToolExecutionResult/ToolStreamEvent) now identifies it as MCP-originated.
// Intended for use by an MCP integration package that wraps remote tools.
func MarkMcp(t Tool) Tool {
	return mcpBrand{Tool: t}
}

// IsMcpTool reports whether tool carries the additive MCP brand (see MarkMcp).
func IsMcpTool(t Tool) bool {
	if t == nil {
		return false
	}
	_, ok := t.(mcpMarked)
	return ok
}
