package agent

import (
	"context"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type DynamicValue[T any] func(context.Context, TurnContext) (T, error)

type CallModelInput struct {
	Model                  string
	ModelFunc              DynamicValue[string]
	Input                  any
	Tools                  []Tool
	StopWhen               []StopCondition
	AllowFinalResponse     any
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
