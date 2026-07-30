package agent

// Deterministic streaming test support.
//
// Before this file the suite had exactly one streaming fake and it only ever
// emitted a broken frame to assert error propagation, so no test drove a
// *successful* stream. That left consumeCreateResponse's success loop, run()'s
// delta callback and ReasoningStream unreachable, and meant TextStream was only
// ever exercised through its single-chunk non-streaming fallback -- for a
// package whose headline feature is streaming fan-out.
//
// Events here are built with the SDK's own Create* constructors and then
// serialized to real SSE frames, which the SDK's decoder parses back. That
// matters: the constructors and UnmarshalJSON both set the union's `Type`
// discriminator *and* its member pointer together. A fake that hand-set struct
// fields could leave `Type` empty, which would still satisfy production's
// pointer nil-checks while disagreeing with any Type-based predicate -- a fake
// that silently diverges from production hides the bugs it is meant to catch.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/types/stream"
)

// sseFrames encodes events as an SSE body, mirroring what the Responses API
// puts on the wire, and terminates with the [DONE] sentinel the SDK expects.
func sseFrames(t *testing.T, events []components.StreamEvents, withDone bool) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range events {
		// The frame body is the bare event: the SDK's SSE scanner re-wraps the
		// `data:` payload as {"data": <event>} before handing it to the decoder,
		// so emitting that wrapper here would double-wrap it.
		//
		// Marshal the typed value, never a map[string]any: StreamEvents has a
		// custom MarshalJSON that flattens the active union member, and routing
		// through a map loses the discriminator.
		payload, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal stream event: %v", err)
		}
		fmt.Fprintf(&buf, "data: %s\n\n", payload)
	}
	if withDone {
		buf.WriteString("data: [DONE]\n\n")
	}
	return &buf
}

// eventStreamFrom builds an SDK EventStream over real SSE bytes, decoded by the
// SDK's own unmarshaller rather than a stub decoder.
func eventStreamFrom(t *testing.T, events []components.StreamEvents, withDone bool) *stream.EventStream[components.ResponsesStreamingResponse] {
	t.Helper()
	return stream.NewEventStream(
		context.Background(),
		sseFrames(t, events, withDone),
		func(b []byte) (components.ResponsesStreamingResponse, error) {
			// The SDK hands the decoder the frame already wrapped as
			// {"data": <event>}, which is exactly the ResponsesStreamingResponse
			// envelope shape -- so unmarshal it as the envelope, not as a bare
			// StreamEvents. Decoding it bare yields Type "UNKNOWN" with every
			// union pointer nil, which is silent: json.Unmarshal returns no
			// error, so the fake would look fine while carrying no event at all.
			var v components.ResponsesStreamingResponse
			if err := json.Unmarshal(b, &v); err != nil {
				return components.ResponsesStreamingResponse{}, err
			}
			return v, nil
		},
		"[DONE]",
	)
}

// streamingResponse wraps events as a streaming CreateResponsesResponse, ready
// to hand to fakeSender.
func streamingResponse(t *testing.T, events []components.StreamEvents) *operations.CreateResponsesResponse {
	t.Helper()
	created := operations.CreateCreateResponsesResponseEventStream(eventStreamFrom(t, events, true))
	return &created
}

func textDelta(delta string, seq int64) components.StreamEvents {
	return components.CreateStreamEventsResponseOutputTextDelta(components.TextDeltaEvent{
		Delta: delta, ItemID: "item_1", SequenceNumber: seq,
	})
}

func reasoningDelta(delta string, seq int64) components.StreamEvents {
	return components.CreateStreamEventsResponseReasoningTextDelta(components.ReasoningDeltaEvent{
		Delta: delta, ItemID: "item_1", SequenceNumber: seq,
	})
}

func reasoningSummaryDelta(delta string, seq int64) components.StreamEvents {
	return components.CreateStreamEventsResponseReasoningSummaryTextDelta(components.ReasoningSummaryTextDeltaEvent{
		Delta: delta, ItemID: "item_1", SequenceNumber: seq,
	})
}

func completedEvent(resp components.OpenResponsesResult, seq int64) components.StreamEvents {
	return components.CreateStreamEventsResponseCompleted(components.StreamEventsResponseCompleted{
		Response: resp, SequenceNumber: seq,
	})
}

// completedResponse is the terminal response a streaming turn resolves to. Uses
// the OutputText convenience field, matching the rest of the suite.
//
// Object and ToolChoice are set explicitly because this response is genuinely
// serialized to SSE bytes and read back through the SDK's strict decoder: an
// empty ToolChoice union fails to marshal ("all fields are null") and an empty
// Object fails to unmarshal ("invalid value for OpenResponsesResultObject").
// Tests elsewhere hand a response straight to fakeSender without a wire
// round-trip, so they can leave both zero.
func completedResponse(text string) components.OpenResponsesResult {
	return components.OpenResponsesResult{
		ID:         "resp_stream",
		Model:      "openai/test",
		Object:     components.OpenResponsesResultObjectResponse,
		Status:     components.OpenAIResponsesResponseStatusCompleted,
		OutputText: openrouter.String(text),
		ToolChoice: components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(
			components.OpenAIResponsesToolChoiceAutoAuto,
		),
	}
}

// TestSanityFakeStreamDecodesThroughSDK guards the fake itself: every event must
// round-trip through the SDK decoder with BOTH its union member pointer and its
// `Type` discriminator populated. If this fails, every other streaming test is
// asserting against a fake that does not look like real traffic.
func TestSanityFakeStreamDecodesThroughSDK(t *testing.T) {
	events := []components.StreamEvents{
		textDelta("a", 1),
		reasoningDelta("r", 2),
		reasoningSummaryDelta("s", 3),
		completedEvent(completedResponse("a"), 4),
	}
	es := eventStreamFrom(t, events, true)
	defer es.Close()

	var decoded []components.StreamEvents
	for es.Next() {
		v := es.Value()
		if v == nil {
			continue
		}
		decoded = append(decoded, v.GetData())
	}
	if err := es.Err(); err != nil {
		t.Fatalf("fake stream failed to decode: %v", err)
	}
	if len(decoded) != len(events) {
		t.Fatalf("decoded %d events, want %d", len(decoded), len(events))
	}

	if decoded[0].TextDeltaEvent == nil || decoded[0].Type != components.StreamEventsTypeResponseOutputTextDelta {
		t.Fatalf("text delta lost pointer or Type: %+v", decoded[0])
	}
	if decoded[0].TextDeltaEvent.Delta != "a" {
		t.Fatalf("text delta value = %q", decoded[0].TextDeltaEvent.Delta)
	}
	if decoded[1].ReasoningDeltaEvent == nil || decoded[1].Type != components.StreamEventsTypeResponseReasoningTextDelta {
		t.Fatalf("reasoning delta lost pointer or Type: %+v", decoded[1])
	}
	if decoded[2].ReasoningSummaryTextDeltaEvent == nil || decoded[2].Type != components.StreamEventsTypeResponseReasoningSummaryTextDelta {
		t.Fatalf("reasoning summary delta lost pointer or Type: %+v", decoded[2])
	}
	if decoded[3].StreamEventsResponseCompleted == nil || decoded[3].Type != components.StreamEventsTypeResponseCompleted {
		t.Fatalf("completed event lost pointer or Type: %+v", decoded[3])
	}
}

// TestStreamingTextDeltasArriveInOrder is the test the suite was missing: a
// genuine multi-delta stream, asserting TextStream yields each delta separately
// and in order, and that concatenating them equals Text(). Previously the only
// deterministic coverage was the non-streaming single-chunk fallback, so delta
// ordering was verified nowhere outside the paid e2e job.
func TestStreamingTextDeltasArriveInOrder(t *testing.T) {
	ctx := context.Background()
	final := completedResponse("Hello, world!")
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		streamingResponse(t, []components.StreamEvents{
			textDelta("Hello", 1),
			textDelta(", ", 2),
			textDelta("world", 3),
			textDelta("!", 4),
			completedEvent(final, 5),
		}),
	}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	ch, wait := result.TextStream(ctx)
	var got []string
	for chunk := range ch {
		got = append(got, chunk)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}

	want := []string{"Hello", ", ", "world", "!"}
	if len(got) != len(want) {
		t.Fatalf("got %d deltas %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta %d = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}

	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(got, ""); joined != text {
		t.Fatalf("concatenated deltas %q != Text() %q", joined, text)
	}
}

// TestStreamingReasoningDeltasReachReasoningStream covers ReasoningStream, a
// contract-Required consumer that had no deterministic test at all: its only
// push sites sit inside the streaming callback, so without a successful stream
// it was structurally unreachable.
//
// Both reasoning event shapes are exercised, since each has its own branch.
func TestStreamingReasoningDeltasReachReasoningStream(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		streamingResponse(t, []components.StreamEvents{
			reasoningDelta("think", 1),
			reasoningSummaryDelta("summary", 2),
			textDelta("answer", 3),
			completedEvent(completedResponse("answer"), 4),
		}),
	}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	ch, wait := result.ReasoningStream(ctx)
	var got []string
	for chunk := range ch {
		got = append(got, chunk)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}

	want := []string{"think", "summary"}
	if len(got) != len(want) {
		t.Fatalf("reasoning deltas = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reasoning delta %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStreamingSurfacesEveryEventToFullStream asserts the raw event passthrough:
// FullResponsesStream emits a "response.event" per upstream event, which is the
// escape hatch consumers use for events the typed streams don't model.
func TestStreamingSurfacesEveryEventToFullStream(t *testing.T) {
	ctx := context.Background()
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		streamingResponse(t, []components.StreamEvents{
			textDelta("a", 1),
			reasoningDelta("r", 2),
			completedEvent(completedResponse("a"), 3),
		}),
	}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	ch, wait := result.FullResponsesStream(ctx)
	var raw int
	var sawTurnStart, sawTurnEnd bool
	for ev := range ch {
		switch ev.Type {
		case "response.event":
			raw++
			if ev.Event == nil {
				t.Fatal("response.event carried a nil Event")
			}
		case "turn.start":
			sawTurnStart = true
		case "turn.end":
			sawTurnEnd = true
		}
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}

	if raw != 3 {
		t.Fatalf("got %d raw response.event entries, want 3", raw)
	}
	if !sawTurnStart || !sawTurnEnd {
		t.Fatalf("turn boundaries missing: start=%v end=%v", sawTurnStart, sawTurnEnd)
	}
}

// TestStreamEndingWithoutCompletedEventErrors covers the distinct error branch
// for a stream that terminates without a response.completed event. It must
// surface an error rather than returning an empty response as if it succeeded.
func TestStreamEndingWithoutCompletedEventErrors(t *testing.T) {
	ctx := context.Background()
	// Deltas, then [DONE] with no response.completed.
	created := operations.CreateCreateResponsesResponseEventStream(
		eventStreamFrom(t, []components.StreamEvents{textDelta("partial", 1)}, true),
	)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := result.Text(ctx); err == nil {
		t.Fatal("a stream that never sent response.completed must surface an error")
	}
}
