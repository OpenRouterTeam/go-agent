package agent

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/types/stream"
)

// Upstream consumers stop at the terminal event (streamTerminationEvents):
// a provider that keeps the connection open after response.completed must
// not stall the run waiting for the close frame.
func TestRunFinishesAtTerminalEventWhileConnectionStaysOpen(t *testing.T) {
	resp := completedResponse("hello")
	frames := sseFrames(t, []components.StreamEvents{textDelta("hello", 1), completedEvent(resp, 2)}, false)
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		_, _ = io.Copy(pw, frames)
		// Deliberately never close: the connection stays open.
	}()
	es := stream.NewEventStream(context.Background(), pr, func(b []byte) (components.ResponsesStreamingResponse, error) {
		var v components.ResponsesStreamingResponse
		err := json.Unmarshal(b, &v)
		return v, err
	}, "[DONE]")
	created := operations.CreateCreateResponsesResponseEventStream(es)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var text string
	var textErr error
	go func() {
		defer close(done)
		text, textErr = result.Text(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run stalled waiting for the stream to close after response.completed")
	}
	if textErr != nil || text != "hello" {
		t.Fatalf("text=%q err=%v", text, textErr)
	}
}
