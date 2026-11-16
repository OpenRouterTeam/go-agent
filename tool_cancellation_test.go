package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// tool-cancellation.test.ts: "passes an unaborted ctx.signal to tool execute
// by default" — in Go the body's context is live, and the execute context
// carries the executing call id and the conversation id.
func TestToolExecuteContextCarriesCallAndConversationIDs(t *testing.T) {
	var mu sync.Mutex
	var gotCallID, gotConv string
	var ctxErr error
	tool := MustNewTool(ToolConfig[phTargetInput]{Name: "probe", InputSchema: phObjectSchema("target"), Execute: func(ctx context.Context, _ phTargetInput, tc ToolExecuteContext) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		gotCallID, gotConv, ctxErr = tc.CallID, tc.ConversationID, ctx.Err()
		return "ok", nil
	}})
	sender := phSender(phResp("r1", phFnCall("call_1", "probe", `{"target":"x"}`)), phTextResp("r2", "done"))
	state := CreateInitialState()
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}, State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotCallID != "call_1" || gotConv != state.ID || ctxErr != nil {
		t.Fatalf("callID=%q conversationID=%q (want %q) ctxErr=%v", gotCallID, gotConv, state.ID, ctxErr)
	}
}

// tool-cancellation.test.ts: "per-tool timeoutMs produces a tool_timeout
// error output and the loop continues", and "run-level toolTimeoutMs applies
// as the default; tool-level overrides it". The abandoned body's context is
// cancelled.
func TestToolTimeoutProducesToolTimeoutOutputAndLoopContinues(t *testing.T) {
	cases := []struct {
		name      string
		toolLimit time.Duration
		runLimit  time.Duration
		wantMs    string
	}{
		{name: "per-tool", toolLimit: 20 * time.Millisecond, wantMs: "20ms"},
		{name: "run default", runLimit: 30 * time.Millisecond, wantMs: "30ms"},
		{name: "tool overrides run", toolLimit: 10 * time.Millisecond, runLimit: time.Hour, wantMs: "10ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			cancelled := make(chan struct{})
			slow := MustNewTool(ToolConfig[phTargetInput]{Name: "slow", InputSchema: phObjectSchema("target"), Timeout: tc.toolLimit, Execute: func(ctx context.Context, _ phTargetInput, _ ToolExecuteContext) (any, error) {
				select {
				case <-ctx.Done():
					close(cancelled)
				case <-release:
				}
				return "late", nil
			}})
			defer close(release)
			sender := phSender(phResp("r1", phFnCall("call_1", "slow", `{"target":"x"}`)), phTextResp("r2", "recovered"))
			result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{slow}, ToolTimeout: tc.runLimit})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(context.Background())
			if err != nil || text != "recovered" {
				t.Fatalf("loop must continue after a timeout: text=%q err=%v", text, err)
			}
			outs := phOutputs(t, sender, 1)
			if len(outs) != 1 || !strings.Contains(phOutputError(outs[0]), `Tool "slow" timed out after `+tc.wantMs) {
				t.Fatalf("expected a tool_timeout error output, got %#v", outs)
			}
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("the abandoned body's context must be cancelled")
			}
		})
	}
	var te *ToolTimeoutError
	err := error(&ToolTimeoutError{ToolName: "x", Timeout: time.Second})
	if !errors.As(err, &te) || te.Code() != "tool_timeout" {
		t.Fatalf("ToolTimeoutError code = %v", err)
	}
}

// tool-cancellation.test.ts: "ModelResult.cancel() aborts in-flight tool
// signals"; run-cancellation.test.ts: cancelling stops the loop with the
// cancellation error.
func TestCancelAbortsInFlightToolAndFailsRun(t *testing.T) {
	started := make(chan struct{})
	tool := MustNewTool(ToolConfig[phTargetInput]{Name: "wait", InputSchema: phObjectSchema("target"), Execute: func(ctx context.Context, _ phTargetInput, _ ToolExecuteContext) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	sender := phSender(phResp("r1", phFnCall("call_1", "wait", `{"target":"x"}`)), phTextResp("r2", "never"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	ch, wait := result.TextStream(context.Background())
	<-started
	result.Cancel()
	for range ch {
	}
	if err := wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run must surface context.Canceled, got %v", err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("no follow-up request may be sent after cancellation, sent %d", len(sender.requests))
	}
}

// ModelResult.queueUserMessage: a message queued while the run is in flight
// is injected as a user message into the next follow-up request.
func TestQueueUserMessageInjectedIntoNextFollowUp(t *testing.T) {
	var result *ModelResult
	tool := MustNewTool(ToolConfig[phTargetInput]{Name: "step", InputSchema: phObjectSchema("target"), Execute: func(context.Context, phTargetInput, ToolExecuteContext) (any, error) {
		result.QueueUserMessage("please also check the tests")
		return "ok", nil
	}})
	sender := phSender(phResp("r1", phFnCall("call_1", "step", `{"target":"x"}`)), phTextResp("r2", "done"))
	var err error
	result, err = CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	texts := phUserTexts(requestInputItems(sender.requests[1].Input))
	found := false
	for _, s := range texts {
		if s == "please also check the tests" {
			found = true
		}
	}
	if !found {
		t.Fatalf("queued message must reach the follow-up request, user texts=%q", texts)
	}
	if got := phUserTexts(requestInputItems(sender.requests[0].Input)); len(got) != 1 {
		t.Fatalf("initial request must carry only the original prompt, got %q", got)
	}
}

// requiresApproval(): true for every caller-owed pause, not only approval.
func TestRequiresApprovalForEveryCallerPause(t *testing.T) {
	manual := MustNewTool(ToolConfig[phTargetInput]{Name: "manual", InputSchema: phObjectSchema("target"), Manual: true})
	sender := phSender(phResp("r1", phFnCall("call_1", "manual", `{"target":"x"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{manual}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingClientTools {
		t.Fatalf("status = %s", state.Status)
	}
	if ok, err := result.RequiresApproval(context.Background()); err != nil || !ok {
		t.Fatalf("awaiting_client_tools must report RequiresApproval=true, got %v %v", ok, err)
	}
}
