package agent

import (
	"context"

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
	// DoomLoop enables doom-loop detection (upstream `doomLoop`): catch runs
	// that repeat the same tool call with identical arguments (including
	// repeated empty calls), repeat identical server-tool requests, or emit
	// the same text over and over, and respond with a graduated ladder
	// (observe → steer → escalate → block → stop; defaults observe@2,
	// block@3, stop@6). nil (the default) disables detection;
	// &DoomLoopConfig{} is upstream's `doomLoop: true`. Tools declare
	// precise call identity via ToolConfig.LoopKey. Pass it again on a
	// resuming call for persisted streaks to keep counting.
	DoomLoop *DoomLoopConfig
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
