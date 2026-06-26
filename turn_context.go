package agent

import "github.com/OpenRouterTeam/go-sdk/models/components"

func BuildTurnContext(call *components.OutputFunctionCallItem, turns int, req *components.ResponsesRequest) TurnContext {
	return TurnContext{ToolCall: call, NumberOfTurns: turns, TurnRequest: req}
}

func NormalizeInputToArray(input components.InputsUnion) []components.InputsUnion1 {
	if input.Type == components.InputsUnionTypeArrayOfInputsUnion1 {
		return append([]components.InputsUnion1{}, input.ArrayOfInputsUnion1...)
	}
	return []components.InputsUnion1{}
}
