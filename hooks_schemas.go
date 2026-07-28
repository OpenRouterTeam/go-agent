package agent

// HookName identifies a built-in lifecycle hook. Ported from upstream
// hooks-schemas.ts's `HookName` const object (upstream #7/#67, the 0.8.0
// headline). Custom hook names are plain strings; only the built-ins get a
// typed constant.
type HookName string

const (
	HookNamePreToolUse         HookName = "PreToolUse"
	HookNamePostToolUse        HookName = "PostToolUse"
	HookNamePostToolUseFailure HookName = "PostToolUseFailure"
	HookNameUserPromptSubmit   HookName = "UserPromptSubmit"
	HookNameStop               HookName = "Stop"
	HookNamePermissionRequest  HookName = "PermissionRequest"
	HookNameSessionStart       HookName = "SessionStart"
	HookNameSessionEnd         HookName = "SessionEnd"
	HookNamePostModelCall      HookName = "PostModelCall"
)

// builtInHookNames mirrors upstream's BUILT_IN_HOOK_NAMES set, used by
// ResolveHooks to warn about (and skip) inline entries under an unrecognized
// name instead of silently never firing.
var builtInHookNames = map[string]bool{
	string(HookNamePreToolUse):         true,
	string(HookNamePostToolUse):        true,
	string(HookNamePostToolUseFailure): true,
	string(HookNameUserPromptSubmit):   true,
	string(HookNameStop):               true,
	string(HookNamePermissionRequest):  true,
	string(HookNameSessionStart):       true,
	string(HookNameSessionEnd):         true,
	string(HookNamePostModelCall):      true,
}

// IsBuiltInHookName reports whether name is one of the nine built-in hooks.
func IsBuiltInHookName(name string) bool { return builtInHookNames[name] }

//#region Payload & Result types
//
// Go has no runtime schema library standing in for Zod (idiomatic divergence:
// "Struct tags + invopop/jsonschema replace Zod" is about *tool* input/output
// schemas; hook payloads/results are plain compile-time-typed Go structs).
// Compile-time typing replaces upstream's runtime schema validation for these
// shapes: a well-typed Go handler cannot receive a malformed payload the way
// a JS handler behind a Zod schema could. See upstreamer-changelog.md for the
// compatibility note on this divergence.
//
// TS's `block?: boolean | string` / `reject?: boolean | string` union fields
// are split into two Go fields (a bool and a string) rather than boxed in an
// `any`; either one set triggers the same short-circuit behavior as upstream
// (`=== true` or a non-empty string). This keeps the common case
// (`return PreToolUseResult{Block: true}`) idiomatic while still allowing a
// reason string (`return PreToolUseResult{BlockReason: "not allowed"}`).

// PreToolUsePayload is delivered before a client tool executes.
type PreToolUsePayload struct {
	ToolName  string
	ToolInput map[string]any
}

// PreToolUseResult can mutate the tool's input or block execution.
type PreToolUseResult struct {
	// MutatedInput replaces ToolInput for the actual tool call when non-nil.
	MutatedInput map[string]any
	// Block, if true, denies the call with a generic reason.
	Block bool
	// BlockReason, if non-empty, denies the call and doubles as Block=true.
	BlockReason string
}

// Blocked reports whether this result triggers the PreToolUse short-circuit.
func (r PreToolUseResult) Blocked() bool { return r.Block || r.BlockReason != "" }

// PostToolUsePayload is delivered after a client tool executes successfully.
type PostToolUsePayload struct {
	ToolName   string
	ToolInput  map[string]any
	ToolOutput any
	DurationMs float64
}

// PostToolUseFailurePayload is delivered when a tool execution throws or
// returns an error. Deliberately NOT fired when a tool never ran (a
// PermissionRequest deny, a user rejection on resume, or a PreToolUse block
// all synthesize a rejected result without execution).
type PostToolUseFailurePayload struct {
	ToolName  string
	ToolInput map[string]any
	Error     error
}

// StopReason is the reason the tool loop halted, passed to the Stop hook.
type StopReason string

// StopReasonMaxTurns is currently the only reason the engine emits: the
// configured stopWhen condition (default StepCountIs) fired.
const StopReasonMaxTurns StopReason = "max_turns"

// StopPayload is delivered when a stopWhen condition halts the loop.
type StopPayload struct {
	Reason StopReason
}

// StopResult lets a handler force the loop to resume and/or inject a prompt.
//
// ForceResume alone does not change any state: the stop condition typically
// fires again immediately, so a bare ForceResume burns through the
// consecutive-override cap in rapid succession and then stops. Pair it with
// AppendPrompt (which injects a user message, advancing the conversation) to
// make resumption useful. AppendPrompt is honored independently of
// ForceResume. Multiple handlers' AppendPrompt values are concatenated with
// newlines.
type StopResult struct {
	ForceResume  bool
	AppendPrompt string
}

// RiskLevel is PermissionRequest's coarse risk classification, derived from
// the approval gate's shape: a callback => high, blanket true => medium,
// otherwise low.
type RiskLevel string

const (
	RiskLevelLow    RiskLevel = "low"
	RiskLevelMedium RiskLevel = "medium"
	RiskLevelHigh   RiskLevel = "high"
)

// PermissionRequestPayload is delivered before the engine blocks for human
// approval, letting a hook allow/deny/pass-through the decision.
type PermissionRequestPayload struct {
	ToolName  string
	ToolInput map[string]any
	RiskLevel RiskLevel
}

// PermissionDecision is PermissionRequestResult's outcome.
type PermissionDecision string

const (
	// PermissionDecisionAllow promotes the call past the approval gate.
	PermissionDecisionAllow PermissionDecision = "allow"
	// PermissionDecisionDeny synthesizes a rejection without executing the tool.
	PermissionDecisionDeny PermissionDecision = "deny"
	// PermissionDecisionAskUser falls through to the normal human approval flow (default).
	PermissionDecisionAskUser PermissionDecision = "ask_user"
)

// PermissionRequestResult is a handler's decision for a gated tool call.
// Last-wins when multiple handlers disagree.
type PermissionRequestResult struct {
	Decision PermissionDecision
	Reason   string
}

// UserPromptSubmitPayload carries the extracted user prompt text for the
// current turn's input.
type UserPromptSubmitPayload struct {
	Prompt string
}

// UserPromptSubmitResult can mutate the prompt or reject it outright.
type UserPromptSubmitResult struct {
	// MutatedPrompt replaces Prompt when non-nil (a pointer distinguishes
	// "not set" from "explicitly set to the empty string").
	MutatedPrompt *string
	Reject        bool
	RejectReason  string
}

// Rejected reports whether this result triggers the UserPromptSubmit short-circuit.
func (r UserPromptSubmitResult) Rejected() bool { return r.Reject || r.RejectReason != "" }

// SessionStartPayload is delivered once per run, before the first model call.
type SessionStartPayload struct {
	Config map[string]any
}

// ModelCallUsage is the per-call usage summary handed to PostModelCall and
// folded into SessionEnd's SessionUsageTotals.
type ModelCallUsage struct {
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CachedTokens    int64
	ReasoningTokens int64
	// Cost is nil when the response carried no cost figure.
	Cost *float64
}

// SessionUsageTotals aggregates ModelCallUsage across every model call made
// during a run, plus a call count.
type SessionUsageTotals struct {
	ModelCallUsage
	ModelCalls int
}

// SessionEndReason explains why the run ended.
type SessionEndReason string

const (
	SessionEndReasonUser     SessionEndReason = "user"
	SessionEndReasonError    SessionEndReason = "error"
	SessionEndReasonMaxTurns SessionEndReason = "max_turns"
	SessionEndReasonComplete SessionEndReason = "complete"
)

// SessionEndPayload is delivered exactly once per run that reached
// SessionStart, carrying the aggregated usage totals when any model call was made.
type SessionEndPayload struct {
	Reason     SessionEndReason
	TotalUsage *SessionUsageTotals
}

// ModelCallTurnType classifies which kind of model request PostModelCall is reporting on.
type ModelCallTurnType string

const (
	ModelCallTurnTypeInitial   ModelCallTurnType = "initial"
	ModelCallTurnTypeResume    ModelCallTurnType = "resume"
	ModelCallTurnTypeToolRound ModelCallTurnType = "tool_round"
	ModelCallTurnTypeFinal     ModelCallTurnType = "final"
	ModelCallTurnTypeRetry     ModelCallTurnType = "retry"
)

// PostModelCallPayload is delivered once per materialized model response.
type PostModelCallPayload struct {
	SessionID  string
	ResponseID string
	Model      string
	DurationMs float64
	TurnType   ModelCallTurnType
	TurnNumber int
	Usage      *ModelCallUsage
}

// EmptyHookResult is the result type for observation-only hooks
// (PostToolUse, PostToolUseFailure, SessionStart, SessionEnd, PostModelCall):
// handlers have no meaningful result to return. Stands in for upstream's
// `result: undefined` / `z.void()` built-in definitions.
type EmptyHookResult struct{}

//#endregion
