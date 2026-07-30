package agent

import "github.com/OpenRouterTeam/go-sdk/models/components"

// Stream event predicates. These classify events by the union's `type`
// discriminator, which the SDK sets alongside the corresponding member pointer
// in both its Create* constructors and UnmarshalJSON -- so a type check and a
// pointer check agree for any event the SDK produced.
//
// The loop in run() uses the *Text helpers below rather than nil-checking union
// fields inline, so "which events carry a text/reasoning delta" is answered in
// exactly one place. Before that these predicates were exported but unused,
// which meant the answer existed twice and only the unused copy was tested.

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

// textDeltaText returns the text delta carried by e, if it is a text delta event.
func textDeltaText(e components.StreamEvents) (string, bool) {
	if !IsTextDeltaEvent(e) || e.TextDeltaEvent == nil {
		return "", false
	}
	return e.TextDeltaEvent.Delta, true
}

// reasoningDeltaText returns the reasoning delta carried by e, if it is either
// reasoning delta shape. Both feed the same reasoning stream upstream, but they
// are distinct union members, so the value comes from whichever is populated.
func reasoningDeltaText(e components.StreamEvents) (string, bool) {
	if !IsReasoningDeltaEvent(e) {
		return "", false
	}
	if e.ReasoningDeltaEvent != nil {
		return e.ReasoningDeltaEvent.Delta, true
	}
	if e.ReasoningSummaryTextDeltaEvent != nil {
		return e.ReasoningSummaryTextDeltaEvent.Delta, true
	}
	return "", false
}
