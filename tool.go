package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

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
	// Strict is serialized onto the wire function definition: whether the
	// provider should enforce strict schema adherence when generating this
	// tool's call arguments. nil leaves the provider default (the field is
	// omitted). OpenAI-style strict mode requires every declared property to
	// be listed in `required`; the schema is forwarded unchanged.
	Strict *bool
	// WireInputSchema is a caller-owned JSON Schema serialized to the model
	// verbatim (sanitized of `~`-prefixed keys) instead of the schema derived
	// from In / InputSchema. Only valid on manual tools (no Execute, Generate
	// or OnToolCalled); NewTool returns an error otherwise. A schema whose
	// root is not an object is ignored in favor of the derived schema.
	WireInputSchema map[string]any
	// ToModelOutput maps a successful execution result to the content sent
	// to the model in place of the JSON-encoded output. It receives the
	// decoded input the tool ran on. Applied on every result path: the
	// normal tool round, auto-approved siblings of an approval pause, and
	// calls approved on resume.
	ToModelOutput func(ctx context.Context, input In, output any) (ToModelOutputResult, error)
	// NextTurnParams computes request parameters for the next model turn
	// after a call to this tool succeeds. See NextTurnParamsFunctions.
	NextTurnParams NextTurnParamsFunctions
	// Timeout bounds one execution of this tool; it overrides
	// CallModelInput.ToolTimeout. On expiry the model receives a
	// *ToolTimeoutError output and the loop continues. Zero means no
	// per-tool deadline.
	Timeout time.Duration
	// LoopKey declares this tool's doom-loop identity (upstream `loopKey`),
	// used only when CallModelInput.DoomLoop is set. nil means the full
	// call arguments are the identity; ExemptToolLoopKey exempts the tool;
	// LoopKeyFields builds the field-subset form. See ToolLoopKey.
	LoopKey ToolLoopKey
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
		return nil, fmt.Errorf("tool %q: WireInputSchema is only supported on manual tools", config.Name)
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

func (t *TypedTool[In]) ToAPITool() components.ResponsesRequestToolUnion {
	return functionToolDefinition(t.config.Name, t.config.Description, t.InputSchema(), t.config.WireInputSchema, t.config.Strict)
}

// functionToolDefinition builds the wire function tool. wire, when its root
// is an object schema, is serialized (sanitized) instead of schema.
func functionToolDefinition(name, description string, schema, wire map[string]any, strict *bool) components.ResponsesRequestToolUnion {
	params := schema
	if wire != nil && isObjectRootSchema(wire) {
		params = SanitizeSchema(wire)
	}
	fn := components.ResponsesRequestToolFunction{
		Name:       name,
		Parameters: params,
		Type:       components.ResponsesRequestTypeFunction,
	}
	if description != "" {
		fn.Description = optionalnullable.From(openrouter.String(description))
	}
	if strict != nil {
		v := *strict
		fn.Strict = optionalnullable.From(&v)
	}
	return components.CreateResponsesRequestToolUnionFunction(fn)
}

func isObjectRootSchema(schema map[string]any) bool {
	typ, ok := schema["type"]
	if !ok {
		return false
	}
	return typ == "object"
}

// decodeInput decodes raw tool arguments into In, mirroring Execute's
// parse + schema validation.
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

// normalizeArguments returns the arguments exactly as Execute would receive
// them (decoded into In and re-encoded), so approval predicates decide on
// the same values the tool body sees. ok is false when the arguments fail
// to decode/validate or the decoded value is not a JSON object.
func (t *TypedTool[In]) normalizeArguments(raw json.RawMessage) (map[string]any, bool) {
	input, err := t.decodeInput(raw)
	if err != nil {
		return nil, false
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

func (t *TypedTool[In]) hasApprovalPredicate() bool { return t.config.Approval != nil }

func (t *TypedTool[In]) hasBlanketApproval() bool {
	return t.config.Approval == nil && t.config.RequireApproval
}

func (t *TypedTool[In]) nextTurnParams() NextTurnParamsFunctions { return t.config.NextTurnParams }

func (t *TypedTool[In]) timeout() time.Duration { return t.config.Timeout }

func (t *TypedTool[In]) loopKey() ToolLoopKey { return t.config.LoopKey }

// modelOutput applies ToModelOutput. ok is false when the tool has no
// mapper, in which case the raw output is used unchanged.
func (t *TypedTool[In]) modelOutput(ctx context.Context, call ParsedToolCall, output any) (any, bool, error) {
	if t.config.ToModelOutput == nil {
		return nil, false, nil
	}
	raw, err := callArgumentsJSON(call)
	if err != nil {
		return nil, false, err
	}
	input, err := t.decodeInput(raw)
	if err != nil {
		return nil, false, fmt.Errorf("toModelOutput input for %q: %w", call.Name, err)
	}
	res, err := t.config.ToModelOutput(ctx, input, output)
	if err != nil {
		return nil, false, fmt.Errorf("toModelOutput for %q: %w", call.Name, err)
	}
	if res.Type == "content" {
		return append([]ToolOutputContentItem{}, res.Value...), true, nil
	}
	return nil, false, nil
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
		// Yields are broadcast live through execCtx.Emit and not retained:
		// a long-running generator must not accumulate its whole yield
		// history on the result (upstream 1.0 dropped preliminaryResults).
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
	// ID is the server tool's identity for tool-set activation and
	// ActiveTools filtering. Empty means the upstream default
	// "server:<type>", where <type> is the wire `type` of Config.
	ID string
}

type serverToolImpl struct{ config ServerToolConfig }

func NewServerTool(config ServerToolConfig) Tool {
	return serverToolImpl{config: config}
}

// ServerToolID returns the configured ID, or "server:<type>" by default.
func (s serverToolImpl) ServerToolID() string {
	if s.config.ID != "" {
		return s.config.ID
	}
	return "server:" + serverToolWireType(s.config.Config)
}

// serverToolWireType reads the `type` discriminator of a server tool's wire
// config.
func serverToolWireType(cfg components.ResponsesRequestToolUnion) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return ""
	}
	return probe.Type
}

// ToolID returns the identity used by tool sets and CallModelInput.ActiveTools:
// a client tool's name, or a server tool's ID ("server:<type>" unless
// overridden by ServerToolConfig.ID).
func ToolID(t Tool) string {
	if t == nil {
		return ""
	}
	if s, ok := t.(interface{ ServerToolID() string }); ok {
		return s.ServerToolID()
	}
	if IsServerTool(t) {
		return "server:" + serverToolWireType(t.ToAPITool())
	}
	return t.ToolName()
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
	// ExpandedStruct inlines the root type so the tool's parameters are a
	// plain object schema (not a bare {"$ref": ...}); nested named types stay
	// under $defs and are resolved by the validator.
	rt := reflect.TypeOf(zero)
	r := &jsonschema.Reflector{ExpandedStruct: rt != nil && rt.Kind() == reflect.Struct && rt.Name() != ""}
	s := r.Reflect(zero)
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
type mcpBrand struct {
	Tool
	wireInputSchema map[string]any
	// loopKey, when set via McpMarkOptions.LoopKey, overrides the wrapped
	// tool's doom-loop identity.
	loopKey ToolLoopKey
}

func (b mcpBrand) isMcpTool() {}

// ToAPITool forwards the wrapped tool's wire shape, substituting the MCP
// server's own JSON Schema (sanitized) for the parameters when one was
// supplied via McpMarkOptions.WireInputSchema and its root is an object.
func (b mcpBrand) ToAPITool() components.ResponsesRequestToolUnion {
	out := b.Tool.ToAPITool()
	if b.wireInputSchema == nil || !isObjectRootSchema(b.wireInputSchema) || out.ResponsesRequestToolFunction == nil {
		return out
	}
	fn := *out.ResponsesRequestToolFunction
	fn.Parameters = SanitizeSchema(b.wireInputSchema)
	return components.CreateResponsesRequestToolUnionFunction(fn)
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
//
// McpMarkOptions.WireInputSchema is the server's own JSON Schema for the
// tool's arguments: it is serialized to the model in place of the schema
// derived for validation, which keeps validating the call.
//
// McpMarkOptions.LoopKey attaches a doom-loop identity to the wrapped tool —
// the injection point for MCP tools, whose remote definitions cannot carry
// client-side functions.
func MarkMcp(t Tool, opts ...McpMarkOptions) Tool {
	b := mcpBrand{Tool: t}
	for _, o := range opts {
		if o.WireInputSchema != nil {
			b.wireInputSchema = copyMap(o.WireInputSchema)
		}
		if o.LoopKey != nil {
			b.loopKey = o.LoopKey
		}
	}
	return b
}

// McpMarkOptions configures MarkMcp.
type McpMarkOptions struct {
	WireInputSchema map[string]any
	// LoopKey is the tool's doom-loop identity (see ToolLoopKey); nil keeps
	// the wrapped tool's own.
	LoopKey ToolLoopKey
}

// toolLoopKey returns the doom-loop identity declared for t: the outermost
// MarkMcp LoopKey, else the concrete tool's ToolConfig.LoopKey, else nil
// (full arguments).
func toolLoopKey(t Tool) ToolLoopKey {
	for {
		b, ok := t.(mcpBrand)
		if !ok {
			break
		}
		if b.loopKey != nil {
			return b.loopKey
		}
		t = b.Tool
	}
	if lk, ok := t.(interface{ loopKey() ToolLoopKey }); ok {
		return lk.loopKey()
	}
	return nil
}

// IsMcpTool reports whether tool carries the additive MCP brand (see MarkMcp).
func IsMcpTool(t Tool) bool {
	if t == nil {
		return false
	}
	_, ok := t.(mcpMarked)
	return ok
}
