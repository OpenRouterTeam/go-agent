package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
	"github.com/invopop/jsonschema"
)

var ErrReservedToolName = errors.New("tool name 'shared' is reserved")

// ErrWireInputSchemaRequiresManualTool is returned by NewTool when
// WireInputSchema is set on a tool that executes (Execute, Generate, or
// OnToolCalled). Upstream restricts `wireInputSchema` to manual
// (`execute: false`) tool configs at the type level; Go enforces the same
// rule at construction time.
var ErrWireInputSchemaRequiresManualTool = errors.New("WireInputSchema is only supported on manual tools (no Execute, Generate, or OnToolCalled)")

// ToolNextTurnParams maps a request parameter name to a function computing
// its value for the request that follows a round in which this tool ran
// (upstream `nextTurnParams`). Valid keys are the upstream names: "input",
// "toolChoice", "model", "models", "temperature", "maxOutputTokens", "topP",
// "topK", and "instructions"; any other key is ignored with a warning.
//
// Each function receives the input the tool actually ran on (after schema
// decoding and any PreToolUse mutation) and the working request, which
// already reflects values set by tools processed earlier in the same round,
// so multiple tools compose. A function only runs for calls whose tool body
// ran and returned without error.
//
// Value types per key: "input" — components.InputsUnion, *components.InputsUnion,
// []components.InputsUnion1, or string; "toolChoice" —
// components.OpenAIResponsesToolChoiceUnion or a pointer to one; "model" and
// "instructions" — string or *string; "models" — []string; "temperature" and
// "topP" — float64 or *float64; "maxOutputTokens" and "topK" — an integer or
// pointer to int64. A nil value clears the parameter where the request field
// is optional.
type ToolNextTurnParams[In any] map[string]func(context.Context, In, NextTurnParamsContext) (any, error)

type ToolConfig[In any] struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	EventSchema  map[string]any
	// WireInputSchema, when set, is sent to the model verbatim (sanitized of
	// `~`-prefixed keys) instead of the schema derived from In/InputSchema,
	// which keeps validating the call. Manual tools only.
	WireInputSchema map[string]any
	// Strict is serialized onto the wire tool definition's `strict` field.
	// nil leaves it unset so the provider default applies.
	Strict             *bool
	Execute            func(context.Context, In, ToolExecuteContext) (any, error)
	Generate           func(context.Context, In, ToolExecuteContext, func(any) error) (any, error)
	Manual             bool
	RequireApproval    bool
	Approval           ToolApprovalCheck
	OnToolCalled       func(context.Context, In, ToolExecuteContext) (any, bool, error)
	OnResponseReceived func(context.Context, any, ToolExecuteContext) (any, error)
	// NextTurnParams adjusts the next request after a successful call. See
	// ToolNextTurnParams.
	NextTurnParams ToolNextTurnParams[In]
	// ToModelOutput maps a successful result to the content the model sees.
	// It receives the execute result and the decoded input the tool ran on.
	// Returning a nil slice sends the JSON-encoded result unchanged.
	ToModelOutput func(ctx context.Context, output any, input In) ([]ToolOutputContentItem, error)
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
	if config.WireInputSchema != nil && (config.Execute != nil || config.Generate != nil || config.OnToolCalled != nil) {
		return nil, ErrWireInputSchemaRequiresManualTool
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

// RequiresApproval reports whether call needs human approval. A tool-level
// Approval predicate sees the call's arguments as the tool will execute
// them — decoded through the tool's input type and schema, then re-encoded —
// rather than the raw wire payload, so the predicate and Execute agree.
// Arguments that fail schema validation fail closed (approval required):
// there is no trustworthy value to judge, and a PreToolUse hook could later
// repair them into executable input (upstream #94).
func (t *TypedTool[In]) RequiresApproval(ctx context.Context, call ParsedToolCall, turn TurnContext) (bool, error) {
	if t.config.Approval != nil {
		normalized, ok := t.normalizedCall(call)
		if !ok {
			return true, nil
		}
		return t.config.Approval(ctx, normalized, turn)
	}
	return t.config.RequireApproval, nil
}

// rawArguments returns the JSON payload a call would execute with.
func rawArguments(call ParsedToolCall) (json.RawMessage, error) {
	if call.RawArgs != "" {
		return json.RawMessage(call.RawArgs), nil
	}
	if call.Arguments == nil {
		return json.RawMessage("{}"), nil
	}
	if s, ok := call.Arguments.(string); ok {
		// Unparseable wire arguments are carried as the raw string.
		return json.RawMessage(s), nil
	}
	b, err := json.Marshal(call.Arguments)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// decodeInput validates raw against the tool schema and decodes it into In,
// mirroring exactly what Execute does before running the tool body.
func (t *TypedTool[In]) decodeInput(raw json.RawMessage) (In, error) {
	var input In
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return input, err
	}
	if err := ValidateAgainstSchema(raw, t.schema); err != nil {
		return input, err
	}
	return input, nil
}

// parsedInput decodes call's arguments the way Execute would.
func (t *TypedTool[In]) parsedInput(call ParsedToolCall) (In, bool) {
	raw, err := rawArguments(call)
	if err != nil {
		var zero In
		return zero, false
	}
	input, err := t.decodeInput(raw)
	if err != nil {
		return input, false
	}
	return input, true
}

// normalizedCall returns a copy of call whose Arguments are the decoded input
// re-encoded as a JSON object — the Go analogue of upstream handing approval
// predicates the inputSchema-parsed copy. ok is false when the arguments do
// not decode/validate or do not re-encode to an object.
func (t *TypedTool[In]) normalizedCall(call ParsedToolCall) (ParsedToolCall, bool) {
	input, ok := t.parsedInput(call)
	if !ok {
		return call, false
	}
	b, err := json.Marshal(input)
	if err != nil {
		return call, false
	}
	var args map[string]any
	if err := json.Unmarshal(b, &args); err != nil || args == nil {
		return call, false
	}
	out := call
	out.Arguments = args
	out.RawArgs = ""
	return out, true
}

// toolInputParser is implemented by tools that can decode a call's arguments
// the way their executor will (TypedTool). The loop uses it to hand
// call-level approval predicates a parsed copy and to validate
// PreToolUse-mutated input before execution.
type toolInputParser interface {
	normalizedCall(ParsedToolCall) (ParsedToolCall, bool)
}

// toolInputDependentApproval reports whether a tool's approval answer can
// depend on its input (a predicate rather than a constant), which is when a
// PreToolUse input mutation must be re-gated.
type toolInputDependentApproval interface {
	hasApprovalPredicate() bool
}

func (t *TypedTool[In]) hasApprovalPredicate() bool { return t.config.Approval != nil }

// toolNextTurnParamsRunner is implemented by tools that declare
// NextTurnParams.
type toolNextTurnParamsRunner interface {
	applyNextTurnParams(ctx context.Context, call ParsedToolCall, req components.ResponsesRequest) (components.ResponsesRequest, []string, error)
}

// nextTurnParamKeyOrder is the order NextTurnParams keys are processed in for
// one call. Go maps are unordered, so a fixed order (upstream's list of valid
// keys) keeps composition deterministic.
var nextTurnParamKeyOrder = []string{"input", "toolChoice", "model", "models", "temperature", "maxOutputTokens", "topP", "topK", "instructions"}

func (t *TypedTool[In]) applyNextTurnParams(ctx context.Context, call ParsedToolCall, req components.ResponsesRequest) (components.ResponsesRequest, []string, error) {
	if len(t.config.NextTurnParams) == 0 {
		return req, nil, nil
	}
	for key := range t.config.NextTurnParams {
		if !isValidNextTurnParamKey(key) {
			log.Printf("Invalid nextTurnParams key %q in tool %q. Valid keys: %s", key, t.config.Name, strings.Join(nextTurnParamKeyOrder, ", "))
		}
	}
	input, ok := t.parsedInput(call)
	if !ok {
		return req, nil, fmt.Errorf("tool call arguments for %s must decode to the tool input type", t.config.Name)
	}
	var applied []string
	for _, key := range nextTurnParamKeyOrder {
		fn, ok := t.config.NextTurnParams[key]
		if !ok || fn == nil {
			continue
		}
		v, err := fn(ctx, input, NextTurnParamsContext{ToolCall: call, Request: req})
		if err != nil {
			return req, applied, fmt.Errorf("nextTurnParams %s for tool %s: %w", key, t.config.Name, err)
		}
		next, err := applyNextTurnParam(req, key, v)
		if err != nil {
			return req, applied, fmt.Errorf("nextTurnParams %s for tool %s: %w", key, t.config.Name, err)
		}
		req = next
		applied = append(applied, key)
	}
	return req, applied, nil
}

// toolModelOutputMapper is implemented by tools that declare ToModelOutput.
type toolModelOutputMapper interface {
	mapModelOutput(ctx context.Context, call ParsedToolCall, output any) ([]ToolOutputContentItem, bool, error)
}

func (t *TypedTool[In]) mapModelOutput(ctx context.Context, call ParsedToolCall, output any) ([]ToolOutputContentItem, bool, error) {
	if t.config.ToModelOutput == nil {
		return nil, false, nil
	}
	input, ok := t.parsedInput(call)
	if !ok {
		return nil, false, fmt.Errorf("toolCall.arguments for %q must decode to the tool input type", t.config.Name)
	}
	content, err := t.config.ToModelOutput(ctx, output, input)
	if err != nil {
		return nil, false, err
	}
	if content == nil {
		return nil, false, nil
	}
	return content, true, nil
}

func (t *TypedTool[In]) HandleResponseReceived(ctx context.Context, output any, execCtx ToolExecuteContext) (any, error) {
	if t.config.OnResponseReceived == nil {
		return output, nil
	}
	return t.config.OnResponseReceived(ctx, output, execCtx)
}

func (t *TypedTool[In]) ToAPITool() components.ResponsesRequestToolUnion {
	params := t.InputSchema()
	if t.config.WireInputSchema != nil {
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
	if t.config.Strict != nil {
		strict := *t.config.Strict
		fn.Strict = optionalnullable.From(&strict)
	}
	return components.CreateResponsesRequestToolUnionFunction(fn)
}

func (t *TypedTool[In]) Execute(ctx context.Context, raw json.RawMessage, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	input, err := t.decodeInput(raw)
	if err != nil {
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
		// Yields are broadcast live through execCtx.Emit and not accumulated:
		// the result carries only the final value (upstream #110 stopped
		// collecting preliminaryResults, which grew without bound for
		// long-running generators).
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

// McpToolAnnotations are the behaviour hints an MCP server may attach to a
// tool definition in `tools/list` (MCP ToolAnnotations). They are hints, not
// guarantees: a server can misreport them. An absent hint has the default the
// MCP specification gives it, which is not always false — DestructiveHint and
// OpenWorldHint default to true — so compare against the default rather than
// testing for an explicit true.
type McpToolAnnotations struct {
	Title           *string `json:"title,omitempty"`
	ReadOnlyHint    *bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool   `json:"openWorldHint,omitempty"`
}

// copyMcpToolAnnotations returns a fresh copy holding only the five standard
// hints, so no two tools share annotation storage.
func copyMcpToolAnnotations(in McpToolAnnotations) *McpToolAnnotations {
	out := McpToolAnnotations{}
	if in.Title != nil {
		v := *in.Title
		out.Title = &v
	}
	for _, pair := range []struct{ src, dst **bool }{
		{&in.ReadOnlyHint, &out.ReadOnlyHint},
		{&in.DestructiveHint, &out.DestructiveHint},
		{&in.IdempotentHint, &out.IdempotentHint},
		{&in.OpenWorldHint, &out.OpenWorldHint},
	} {
		if *pair.src != nil {
			v := **pair.src
			*pair.dst = &v
		}
	}
	return &out
}

// MarkMcpOptions configures MarkMcpWithOptions.
type MarkMcpOptions struct {
	// WireInputSchema is the MCP server's own JSON Schema for the tool's
	// arguments. It is sent to the model (sanitized of `~`-prefixed keys) in
	// place of the schema the Go tool would otherwise serialize; the wrapped
	// tool's own schema keeps validating the call. A schema whose root is
	// not an object is ignored and the tool's own schema is sent.
	WireInputSchema map[string]any
	// Annotations records the server's tools/list annotations on the
	// branded tool. Read them back with McpAnnotations.
	Annotations *McpToolAnnotations
}

// mcpBrand wraps a Tool with the additive MCP marker (see MarkMcp/IsMcpTool).
// Non-mutating: it forwards every Tool method to the wrapped tool unchanged,
// so runtime behavior and wire shape (ToAPITool) are identical to the
// unmarked tool unless a WireInputSchema option overrides the parameters.
// Execute is forwarded explicitly (rather than inherited via embedding,
// which only promotes the narrow Tool interface) so a marked executable tool
// keeps executing normally.
type mcpBrand struct {
	Tool
	wireInputSchema map[string]any
	annotations     *McpToolAnnotations
}

func (b mcpBrand) isMcpTool() {}

func (b mcpBrand) Execute(ctx context.Context, raw json.RawMessage, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	exec, ok := b.Tool.(ToolWithExecute)
	if !ok {
		return ToolExecutionResult{CallID: execCtx.ToolCall.CallID, Name: b.Tool.ToolName(), Error: ErrManualTool}, nil
	}
	return exec.Execute(ctx, raw, execCtx)
}

func (b mcpBrand) ToAPITool() components.ResponsesRequestToolUnion {
	apiTool := b.Tool.ToAPITool()
	if b.wireInputSchema == nil || apiTool.ResponsesRequestToolFunction == nil {
		return apiTool
	}
	fn := *apiTool.ResponsesRequestToolFunction
	fn.Parameters = SanitizeSchema(b.wireInputSchema)
	return components.CreateResponsesRequestToolUnionFunction(fn)
}

func (b mcpBrand) normalizedCall(call ParsedToolCall) (ParsedToolCall, bool) {
	if p, ok := b.Tool.(toolInputParser); ok {
		return p.normalizedCall(call)
	}
	return call, true
}

func (b mcpBrand) hasApprovalPredicate() bool {
	p, ok := b.Tool.(toolInputDependentApproval)
	return ok && p.hasApprovalPredicate()
}

func (b mcpBrand) applyNextTurnParams(ctx context.Context, call ParsedToolCall, req components.ResponsesRequest) (components.ResponsesRequest, []string, error) {
	if r, ok := b.Tool.(toolNextTurnParamsRunner); ok {
		return r.applyNextTurnParams(ctx, call, req)
	}
	return req, nil, nil
}

func (b mcpBrand) mapModelOutput(ctx context.Context, call ParsedToolCall, output any) ([]ToolOutputContentItem, bool, error) {
	if m, ok := b.Tool.(toolModelOutputMapper); ok {
		return m.mapModelOutput(ctx, call, output)
	}
	return nil, false, nil
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
	return MarkMcpWithOptions(t, MarkMcpOptions{})
}

// MarkMcpWithOptions is MarkMcp with upstream `markMcp`'s options: the
// server's own wire input schema and its tools/list annotations.
func MarkMcpWithOptions(t Tool, opts MarkMcpOptions) Tool {
	brand := mcpBrand{Tool: t}
	if existing, ok := t.(mcpBrand); ok {
		brand = existing
	}
	if opts.WireInputSchema != nil && opts.WireInputSchema["type"] == "object" {
		brand.wireInputSchema = copyMap(opts.WireInputSchema)
	}
	if opts.Annotations != nil {
		brand.annotations = copyMcpToolAnnotations(*opts.Annotations)
	}
	return brand
}

// McpAnnotations returns the tools/list annotations recorded on an
// MCP-branded tool, or nil when the tool is not MCP-branded or the server
// sent none.
func McpAnnotations(t Tool) *McpToolAnnotations {
	b, ok := t.(mcpBrand)
	if !ok || b.annotations == nil {
		return nil
	}
	return copyMcpToolAnnotations(*b.annotations)
}

// IsMcpTool reports whether tool carries the additive MCP brand (see MarkMcp).
func IsMcpTool(t Tool) bool {
	if t == nil {
		return false
	}
	_, ok := t.(mcpMarked)
	return ok
}
