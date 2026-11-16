package agent

import (
	"context"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type DynamicValue[T any] func(context.Context, TurnContext) (T, error)

type CallModelInput struct {
	Model              string
	ModelFunc          DynamicValue[string]
	Input              any
	Tools              []Tool
	StopWhen           []StopCondition
	AllowFinalResponse any
	// StrictFinalResponse: when true, skips the one-shot retry that would
	// otherwise fire when a completed run's final response has no output
	// after at least one tool round. Default false: that empty final is
	// retried once via a `toolChoice: none` resend. Either way an empty
	// final response is never a hard error here — unlike upstream's
	// `validateFinalResponse`, an empty `Output` items array is not on its
	// own a reliable invalidity signal against go-sdk's response shape,
	// which also carries a separate `OutputText` convenience field. See
	// upstreamer-changelog.md's compatibility note.
	StrictFinalResponse    bool
	State                  *ConversationState
	StateAccessor          StateAccessor
	Context                ContextInput
	Approval               ToolApprovalCheck
	ApproveToolCalls       []string
	RejectToolCalls        []string
	Request                components.ResponsesRequest
	MetadataLevel          *components.MetadataLevel
	MaxTurns               int
	BeforeTurn             func(context.Context, TurnContext) error
	AfterTurn              func(context.Context, TurnContext, StepResult) error
	AdditionalInstructions string
	InstructionsFunc       DynamicValue[string]
	// Hooks accepts either a *HooksManager or an InlineHookConfig. See
	// ResolveHooks. nil means no hooks.
	Hooks any
	// ActiveTools filters which of Tools are sent to the model and may be
	// executed for this call, by tool ID (a client tool's name, or a server
	// tool's ID — see ToolID). nil means no filtering; a non-nil list (even
	// empty) keeps only the listed client tools. Server tools are always
	// kept, and unknown IDs are ignored. Typically set from a resolved
	// ToolSet (ResolvedToolSet.CallModel). Never sent on the wire.
	ActiveTools []string
	// DoomLoop enables doom-loop detection: runs that repeat the same tool
	// call with identical arguments (including repeated empty calls) or
	// emit the same text over and over get a graduated response (observe ->
	// steer -> escalate -> block -> stop). nil (the default) disables it
	// entirely (upstream `false`/absent); &DoomLoopConfig{} enables the
	// recommended defaults (upstream `true`).
	DoomLoop *DoomLoopConfig
	// StreamReplay selects how much stream history ModelResult's fan-out
	// streams retain for consumers: StreamReplayFull (the default, "") keeps
	// everything so a late subscriber replays the whole run;
	// StreamReplayActiveConsumers retains only what currently attached
	// consumers still need, so late subscribers start at the trim point.
	// Client-side only; never sent on the wire.
	StreamReplay StreamReplay
	// ToolTimeout is the default deadline for each tool execution in the
	// run; ToolConfig.Timeout overrides it per tool. Zero means none.
	ToolTimeout time.Duration
}

type CallModelInputWithState = CallModelInput
type ResolvedCallModelInput = CallModelInput

func HasAsyncFunctions(input CallModelInput) bool {
	return input.ModelFunc != nil || input.InstructionsFunc != nil
}

func ResolveAsyncFunctions(ctx context.Context, input CallModelInput, turn TurnContext) (ResolvedCallModelInput, error) {
	if input.ModelFunc != nil {
		model, err := input.ModelFunc(ctx, turn)
		if err != nil {
			return input, err
		}
		input.Model = model
	}
	if input.InstructionsFunc != nil {
		instructions, err := input.InstructionsFunc(ctx, turn)
		if err != nil {
			return input, err
		}
		input.AdditionalInstructions = instructions
	}
	return input, nil
}
