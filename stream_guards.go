package agent

import "github.com/OpenRouterTeam/go-sdk/models/components"

func IsTextDeltaEvent(e components.StreamEvents) bool {
	return e.Type == components.StreamEventsTypeResponseOutputTextDelta
}
func IsReasoningDeltaEvent(e components.StreamEvents) bool {
	return e.Type == components.StreamEventsTypeResponseReasoningTextDelta || e.Type == components.StreamEventsTypeResponseReasoningSummaryTextDelta
}
func IsFunctionCallArgsDeltaEvent(e components.StreamEvents) bool {
	return e.Type == components.StreamEventsTypeResponseFunctionCallArgumentsDelta
}
func IsResponseCompletedEvent(e components.StreamEvents) bool {
	return e.Type == components.StreamEventsTypeResponseCompleted
}
