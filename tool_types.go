package agent

import (
	"context"
	"encoding/json"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// ToolType identifies which upstream tool shape a Go tool represents.
type ToolType string

const (
	ToolTypeRegular   ToolType = "regular"
	ToolTypeGenerator ToolType = "generator"
	ToolTypeManual    ToolType = "manual"
	ToolTypeHITL      ToolType = "hitl"
	ToolTypeServer    ToolType = "server"
)

type ToolApprovalCheck func(context.Context, ParsedToolCall, TurnContext) (bool, error)
type ToModelOutputFunction func(any) (any, error)
type ToModelOutputResult struct{ Output any }

type Tool interface {
	ToolName() string
	ToolDescription() string
	ToolType() ToolType
	InputSchema() map[string]any
	OutputSchema() map[string]any
	EventSchema() map[string]any
	RequiresApproval(context.Context, ParsedToolCall, TurnContext) (bool, error)
	HandleResponseReceived(context.Context, any, ToolExecuteContext) (any, error)
	ToAPITool() components.ResponsesRequestToolUnion
}

type ToolWithExecute interface {
	Tool
	Execute(context.Context, json.RawMessage, ToolExecuteContext) (ToolExecutionResult, error)
}

type ToolWithGenerator interface {
	ToolWithExecute
}

type ClientTool interface{ Tool }
type ManualTool interface{ Tool }
type HITLTool interface{ Tool }
type ServerTool interface{ Tool }
type ToolHasApproval interface{ Tool }
type HasApprovalTools interface{ Tool }

type ToolCallOutputEvent struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Output any    `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type ToolPreliminaryResultEvent struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Event  any    `json:"event"`
}

// ToolSource discriminates a tool result's origin. "mcp" identifies a tool
// wrapped from a remote MCP server (see McpBranded/MarkMcp/IsMcpTool);
// "client" is every locally-defined tool. Mirrors upstream's `source` field
// added to ToolExecutionResult/ToolResultEvent so a consumer can identify
// MCP-originated (dynamically typed) results without that collapsing every
// other tool's result to `unknown`.
type ToolSource string

const (
	ToolSourceClient ToolSource = "client"
	ToolSourceMCP    ToolSource = "mcp"
)

type ToolResultEvent struct {
	Type   string     `json:"type"`
	CallID string     `json:"call_id"`
	Name   string     `json:"name"`
	Source ToolSource `json:"source,omitempty"`
	Result any        `json:"result,omitempty"`
	Error  string     `json:"error,omitempty"`
}

type TurnStartEvent struct {
	Type string `json:"type"`
	Turn int    `json:"turn"`
}

type TurnEndEvent struct {
	Type string `json:"type"`
	Turn int    `json:"turn"`
}

type ResponseStreamEvent struct {
	Type     string                          `json:"type"`
	Turn     int                             `json:"turn,omitempty"`
	Response *components.OpenResponsesResult `json:"response,omitempty"`
	Event    *components.StreamEvents        `json:"event,omitempty"`
	Text     string                          `json:"text,omitempty"`
}

type EnhancedResponseStreamEvent = ResponseStreamEvent

type ToolStreamEvent struct {
	Type   string     `json:"type"`
	CallID string     `json:"call_id,omitempty"`
	Name   string     `json:"name,omitempty"`
	Source ToolSource `json:"source,omitempty"`
	Event  any        `json:"event,omitempty"`
	Result any        `json:"result,omitempty"`
	Output any        `json:"output,omitempty"`
	Error  string     `json:"error,omitempty"`
	Turn   int        `json:"turn,omitempty"`
}

type ChatStreamEvent = ResponseStreamEvent

type ParsedToolCall struct {
	ID        string
	CallID    string
	Name      string
	Arguments any
	RawArgs   string
}

type TypedToolCall = ParsedToolCall
type TypedToolCallUnion = ParsedToolCall

type ToolExecutionResult struct {
	CallID string
	Name   string
	// Source is "mcp" for a tool marked via MarkMcp, "client" otherwise.
	// Zero value ("") means the executor path did not set it (treat as client).
	Source ToolSource
	Output any
	Error  error
	Events []any
}

type ToolExecutionResultUnion = ToolExecutionResult

type ToolOutputContentItem = components.FunctionCallOutputItemOutputUnion1
type ToolResultItem = components.FunctionCallOutputItem
type ServerToolResultItem = components.OutputItems
type UnsentToolResult struct {
	CallID string
	Name   string
	Output any
	Error  string
}

type PartialResponse struct {
	Response components.OpenResponsesResult
}

type Warning struct{ Message string }

type StepResult struct {
	Text              string
	ToolCalls         []ParsedToolCall
	ClientToolResults []ToolExecutionResult
	ServerToolResults []ServerToolResultItem
	Response          components.OpenResponsesResult
	Usage             *components.Usage
	FinishReason      string
}

type StopCondition func(context.Context, []StepResult) (bool, error)
type StopWhen []StopCondition

type TurnContext struct {
	ToolCall      *components.OutputFunctionCallItem
	NumberOfTurns int
	TurnRequest   *components.ResponsesRequest
}

type ConversationStatus string

const (
	ConversationStatusComplete    ConversationStatus = "complete"
	ConversationStatusInterrupted ConversationStatus = "interrupted"
	// ConversationStatusAwaitingApproval means one or more tool calls need
	// human approval before executing (see ApproveToolCalls/RejectToolCalls).
	ConversationStatusAwaitingApproval ConversationStatus = "awaiting_approval"
	// ConversationStatusAwaitingHITL means a HITL tool returned nil from its
	// OnToolCalled hook, pausing execution so the caller can supply an output.
	ConversationStatusAwaitingHITL ConversationStatus = "awaiting_hitl"
	// ConversationStatusAwaitingClientTools means one or more manual
	// (execute:false / no OnToolCalled) tool calls are unresolved; the loop
	// stopped so the caller can execute them client-side and continue.
	// Distinct from ConversationStatusAwaitingHITL — HITL tools have an
	// OnToolCalled hook, manual tools do not (upstream #64).
	ConversationStatusAwaitingClientTools ConversationStatus = "awaiting_client_tools"
	ConversationStatusInProgress          ConversationStatus = "in_progress"
)

// ConversationStateVersion is the current supported ConversationState
// serialization-contract version (upstream #66). Bump this, add a migration
// branch in DeserializeConversationState, and update the supported-versions
// list when the wire shape changes.
const ConversationStateVersion = 1

type ConversationState struct {
	// Version is the serialization-contract version for this state blob.
	// Zero (the Go zero value) is treated as version 1 by
	// DeserializeConversationState/SerializeConversationState, matching
	// upstream's "absence means v1" legacy-blob policy.
	Version            int
	ID                 string
	Messages           []components.InputsUnion1
	PreviousResponseID *string
	PendingToolCalls   []ParsedToolCall
	UnsentToolResults  []UnsentToolResult
	PartialResponse    *PartialResponse
	InterruptedBy      *string
	// DoomLoop is the doom-loop detector state (see CallModelInput.DoomLoop),
	// persisted so repetition streaks survive serialize → resume. nil when
	// detection is off. Additive within ConversationStateVersion 1.
	DoomLoop  *DoomLoopSerializedState `json:",omitempty"`
	Status    ConversationStatus
	CreatedAt int64
	UpdatedAt int64
}

type StateAccessor interface {
	Load(context.Context) (*ConversationState, error)
	Save(context.Context, ConversationState) error
}

type InferToolInput = any
type InferToolOutput = any
type InferToolEvent = any
type InferToolOutputsUnion = any
type InferToolEventsUnion = any

func IsServerTool(t Tool) bool         { return t != nil && t.ToolType() == ToolTypeServer }
func IsClientTool(t Tool) bool         { return t != nil && t.ToolType() != ToolTypeServer }
func IsGeneratorTool(t Tool) bool      { return t != nil && t.ToolType() == ToolTypeGenerator }
func IsManualTool(t Tool) bool         { return t != nil && t.ToolType() == ToolTypeManual }
func IsHITLTool(t Tool) bool           { return t != nil && t.ToolType() == ToolTypeHITL }
func IsAutoResolvableTool(t Tool) bool { return HasExecuteFunction(t) && !IsManualTool(t) }
func HasExecuteFunction(t Tool) bool   { _, ok := t.(ToolWithExecute); return ok }
func IsRegularExecuteTool(t Tool) bool {
	return t != nil && t.ToolType() == ToolTypeRegular && HasExecuteFunction(t)
}
func ToolHasApprovalConfigured(t Tool) bool {
	b, err := t.RequiresApproval(context.Background(), ParsedToolCall{}, TurnContext{})
	if err != nil {
		return false
	}
	return b
}
func HasApprovalRequiredTools(tools []Tool) bool {
	for _, t := range tools {
		if ToolHasApprovalConfigured(t) {
			return true
		}
	}
	return false
}
func IsToolCallOutputEvent(e ToolStreamEvent) bool        { return e.Type == "tool.call.output" }
func IsToolPreliminaryResultEvent(e ToolStreamEvent) bool { return e.Type == "tool.preliminary" }
func IsToolResultEvent(e ToolStreamEvent) bool            { return e.Type == "tool.result" }
func IsTurnStartEvent(e ResponseStreamEvent) bool         { return e.Type == "turn.start" }
func IsTurnEndEvent(e ResponseStreamEvent) bool           { return e.Type == "turn.end" }
