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
	// WireInputSchema is a caller-owned JSON Schema serialized as the tool's
	// wire `parameters` instead of the schema generated from In (or
	// InputSchema). Manual tools only (upstream #118): NewTool rejects it on
	// an executable tool. It is deep-copied and tilde-prefixed keys are
	// sanitized, so the caller's map is never mutated.
	WireInputSchema map[string]any
	// NextTurnParams computes request parameters for the turn after this
	// tool runs (see NextTurnParamsFunctions), e.g. widening an
	// allowed_tools toolChoice once a tool-search tool found what the model
	// needs.
	NextTurnParams NextTurnParamsFunctions
	// Strict sets the function tool's `strict` flag on the wire, letting a
	// provider enforce structured-outputs-style schema adherence on tool-call
	// arguments (upstream #99). nil serializes as `strict: null` (provider
	// default), matching upstream's `strict ?? null`.
	Strict *bool
}

// ErrWireInputSchemaNotManual is returned by NewTool when WireInputSchema is
// set on a tool that is not manual (upstream types it `never` there).
var ErrWireInputSchemaNotManual = errors.New("WireInputSchema is only supported on manual tools")

type TypedTool[In any] struct {
	config     ToolConfig[In]
	schema     map[string]any
	wireSchema map[string]any
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
	if config.WireInputSchema != nil {
		if t.ToolType() != ToolTypeManual {
			return nil, ErrWireInputSchemaNotManual
		}
		t.wireSchema = SanitizeSchema(config.WireInputSchema)
	}
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

// NextTurnParams returns the tool's next-turn parameter functions.
func (t *TypedTool[In]) NextTurnParams() NextTurnParamsFunctions { return t.config.NextTurnParams }

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

// RequiresApproval evaluates the tool-level approval gate. A predicate
// (Approval) receives the call with Arguments normalized through the same
// decode Execute uses — validated against the input schema and decoded into
// In, then re-encoded — so it decides on exactly the values Execute will
// receive (upstream #94). If the arguments fail validation or decoding there
// is no trustworthy value to judge, so the gate fails closed (true): a
// PreToolUse hook may later repair the input, and schema-invalid wire
// arguments must not bypass approval.
func (t *TypedTool[In]) RequiresApproval(ctx context.Context, call ParsedToolCall, turn TurnContext) (bool, error) {
	if t.config.Approval != nil {
		normalized, ok := t.normalizeArguments(call)
		if !ok {
			return true, nil
		}
		call.Arguments = normalized
		return t.config.Approval(ctx, call, turn)
	}
	return t.config.RequireApproval, nil
}

// normalizeArguments decodes call's arguments exactly the way Execute does
// (schema validation, then json.Unmarshal into In) and re-encodes the typed
// value to its JSON-object form. ok is false when the arguments are invalid
// or the normalized value is not a JSON object.
func (t *TypedTool[In]) normalizeArguments(call ParsedToolCall) (any, bool) {
	raw, err := rawToolArguments(call)
	if err != nil {
		return nil, false
	}
	if err := ValidateAgainstSchema(raw, t.schema); err != nil {
		return nil, false
	}
	var input In
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, false
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}

// hasApprovalPredicate reports whether the tool-level approval gate is a
// predicate (as opposed to a static bool), which is what makes a
// PreToolUse-mutated input need a second approval check.
func (t *TypedTool[In]) hasApprovalPredicate() bool { return t.config.Approval != nil }

// approvalPredicateTool is implemented by tools whose approval gate may be a
// per-call predicate.
type approvalPredicateTool interface{ hasApprovalPredicate() bool }

// argumentNormalizer is implemented by tools that can normalize call
// arguments the way their Execute path will see them.
type argumentNormalizer interface {
	normalizeArguments(ParsedToolCall) (any, bool)
}

// rawToolArguments returns the JSON arguments ExecuteTool would hand to
// Execute for call.
func rawToolArguments(call ParsedToolCall) (json.RawMessage, error) {
	if call.RawArgs != "" {
		return json.RawMessage(call.RawArgs), nil
	}
	if call.Arguments != nil {
		return json.Marshal(call.Arguments)
	}
	return json.RawMessage("{}"), nil
}

func (t *TypedTool[In]) HandleResponseReceived(ctx context.Context, output any, execCtx ToolExecuteContext) (any, error) {
	if t.config.OnResponseReceived == nil {
		return output, nil
	}
	return t.config.OnResponseReceived(ctx, output, execCtx)
}

func (t *TypedTool[In]) ToAPITool() components.ResponsesRequestToolUnion {
	params := t.InputSchema()
	if t.wireSchema != nil {
		params = copyMap(t.wireSchema)
	}
	fn := components.ResponsesRequestToolFunction{
		Name:       t.config.Name,
		Parameters: params,
		Type:       components.ResponsesRequestTypeFunction,
	}
	if t.config.Description != "" {
		fn.Description = optionalnullable.From(openrouter.String(t.config.Description))
	}
	var strict *bool
	if t.config.Strict != nil {
		v := *t.config.Strict
		strict = &v
	}
	fn.Strict = optionalnullable.From(strict)
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
		// Yields are broadcast live via Emit but no longer accumulated onto
		// the result (upstream #110): a long-running generator must not
		// retain its full yield history.
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

func (b mcpBrand) Execute(ctx context.Context, raw json.RawMessage, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	exec, ok := b.Tool.(ToolWithExecute)
	if !ok {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: b.Tool.ToolName(), Error: ErrManualTool}, nil
	}
	return exec.Execute(ctx, raw, execCtx)
}

func (b mcpBrand) normalizeArguments(call ParsedToolCall) (any, bool) {
	if n, ok := b.Tool.(argumentNormalizer); ok {
		return n.normalizeArguments(call)
	}
	return nil, false
}

func (b mcpBrand) hasApprovalPredicate() bool {
	p, ok := b.Tool.(approvalPredicateTool)
	return ok && p.hasApprovalPredicate()
}

func (b mcpBrand) NextTurnParams() NextTurnParamsFunctions {
	if nt, ok := b.Tool.(nextTurnParamsTool); ok {
		return nt.NextTurnParams()
	}
	return nil
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
