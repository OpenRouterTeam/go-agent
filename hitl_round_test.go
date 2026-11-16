package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// hitlRoundTool pauses (OnToolCalled returns proceed=false) and counts calls.
func hitlRoundTool(name string, calls *phCounter) *TypedTool[phTargetInput] {
	return MustNewTool(ToolConfig[phTargetInput]{Name: name, InputSchema: phObjectSchema("target"), OnToolCalled: func(_ context.Context, in phTargetInput, _ ToolExecuteContext) (any, bool, error) {
		calls.add(in)
		return nil, false, nil
	}})
}

func functionOutputItem(callID, output string) components.InputsUnion1 {
	return components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: callID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(output), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput})
}

// assertEveryCallAnswered fails when any function_call in items lacks a
// function_call_output (the live API rejects such history with a 400).
func assertEveryCallAnswered(t *testing.T, items []components.InputsUnion1) {
	t.Helper()
	calls := map[string]bool{}
	outputs := map[string]bool{}
	for _, it := range items {
		switch {
		case it.FunctionCallItem != nil:
			calls[it.FunctionCallItem.CallID] = true
		case it.OutputFunctionCallItem != nil:
			calls[it.OutputFunctionCallItem.CallID] = true
		case it.FunctionCallOutputItem != nil:
			outputs[it.FunctionCallOutputItem.CallID] = true
		}
	}
	for id := range calls {
		if !outputs[id] {
			t.Fatalf("function_call %s has no function_call_output in the request: %v", id, phItemKinds(t, items))
		}
	}
}

// hitl-tool.test.ts / upstream executeToolRound: a HITL pause does not stop
// the round. A regular call AFTER the paused HITL call still runs, its output
// is persisted, only the HITL call is pending, and the HITL resume (a
// function_call_output for the paused call in Input) sends a request in
// which every function_call has its output.
func TestHITLPauseFirstStillRunsLaterRegularCall(t *testing.T) {
	hitlCalls, regular := &phCounter{}, &phCounter{}
	tools := []Tool{hitlRoundTool("confirm", hitlCalls), phOKTool("fetch", regular, nil)}
	sender := phSender(phResp("resp_1", phFnCall("call_hitl", "confirm", `{"target":"a"}`), phFnCall("call_reg", "fetch", `{"target":"b"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingHITL || regular.count() != 1 || hitlCalls.count() != 1 {
		t.Fatalf("status=%s regular ran %d hitl ran %d", state.Status, regular.count(), hitlCalls.count())
	}
	if ids := phPendingIDs(state.PendingToolCalls); !phEqualStrings(ids, []string{"call_hitl"}) {
		t.Fatalf("only the HITL call may be pending, got %v", ids)
	}
	if got := countFunctionCallOutputs(state.Messages); got != 1 {
		t.Fatalf("the regular call's output must be persisted, outputs=%d", got)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("no follow-up may be sent with an incomplete output set, sent %d", len(sender.requests))
	}

	resume := phSender(phTextResp("resp_2", "done"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{Model: "m", Tools: tools, State: &state, Input: []components.InputsUnion1{functionOutputItem("call_hitl", `{"approved":true}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := resumed.Text(context.Background()); err != nil || text != "done" {
		t.Fatalf("resume text=%q err=%v", text, err)
	}
	assertEveryCallAnswered(t, requestInputItems(resume.requests[0].Input))
	if regular.count() != 1 {
		t.Fatalf("the regular call must not re-run on resume, ran %d", regular.count())
	}
}

// Two HITL calls in one round both pause and both become pending.
func TestTwoHITLCallsInOneRoundBothPending(t *testing.T) {
	hitlCalls := &phCounter{}
	tools := []Tool{hitlRoundTool("confirm", hitlCalls)}
	sender := phSender(phResp("resp_1", phFnCall("call_1", "confirm", `{"target":"a"}`), phFnCall("call_2", "confirm", `{"target":"b"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingHITL || hitlCalls.count() != 2 {
		t.Fatalf("status=%s hitl ran %d", state.Status, hitlCalls.count())
	}
	if ids := phPendingIDs(state.PendingToolCalls); !phEqualStrings(ids, []string{"call_1", "call_2"}) {
		t.Fatalf("both HITL calls must be pending in call order, got %v", ids)
	}
	resume := phSender(phTextResp("resp_2", "done"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{Model: "m", Tools: tools, State: &state, Input: []components.InputsUnion1{functionOutputItem("call_1", "yes"), functionOutputItem("call_2", "no")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEveryCallAnswered(t, requestInputItems(resume.requests[0].Input))
}

// tool-cancellation.test.ts "unbounded by default (all calls run at once)":
// a round's calls execute concurrently, while outputs keep call order.
func TestRoundCallsRunConcurrentlyAndKeepOutputOrder(t *testing.T) {
	var mu sync.Mutex
	started := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	waitFor := map[string]string{"a": "b", "b": "a"}
	tool := MustNewTool(ToolConfig[phTargetInput]{Name: "rendezvous", InputSchema: phObjectSchema("target"), Execute: func(_ context.Context, in phTargetInput, _ ToolExecuteContext) (any, error) {
		mu.Lock()
		close(started[in.Target])
		other := started[waitFor[in.Target]]
		mu.Unlock()
		select {
		case <-other:
			return "saw " + waitFor[in.Target], nil
		case <-time.After(5 * time.Second):
			return nil, context.DeadlineExceeded
		}
	}})
	sender := phSender(phResp("resp_1", phFnCall("call_a", "rendezvous", `{"target":"a"}`), phFnCall("call_b", "rendezvous", `{"target":"b"}`)), phTextResp("resp_2", "done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	outs := phOutputs(t, sender, 1)
	if len(outs) != 2 || outs[0].CallID != "call_a" || outs[1].CallID != "call_b" {
		t.Fatalf("outputs must keep call order, got %v", phOutputCallIDs(t, sender, 1))
	}
	if phOutputString(outs[0]) != "saw b" || phOutputString(outs[1]) != "saw a" {
		t.Fatalf("both calls must run at once (each saw the other start): %q %q", phOutputString(outs[0]), phOutputString(outs[1]))
	}
}

// Resuming from awaiting_client_tools: once the resumed request succeeds,
// the manual calls are resolved and no longer pending.
func TestClientToolsResumeClearsPendingCalls(t *testing.T) {
	manual := MustNewTool(ToolConfig[phTargetInput]{Name: "manual", InputSchema: phObjectSchema("target"), Manual: true})
	sender := phSender(phResp("resp_1", phFnCall("call_m", "manual", `{"target":"x"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{manual}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingClientTools || len(state.PendingToolCalls) != 1 {
		t.Fatalf("status=%s pending=%d", state.Status, len(state.PendingToolCalls))
	}
	resume := phSender(phTextResp("resp_2", "thanks"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{Model: "m", Tools: []Tool{manual}, State: &state, Input: []components.InputsUnion1{functionOutputItem("call_m", "42")}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := resumed.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != ConversationStatusComplete || len(after.PendingToolCalls) != 0 {
		t.Fatalf("status=%s pending=%v", after.Status, after.PendingToolCalls)
	}
	if ok, _ := resumed.RequiresApproval(context.Background()); ok {
		t.Fatal("a resolved client-tools resume must not report RequiresApproval")
	}
	assertEveryCallAnswered(t, requestInputItems(resume.requests[0].Input))
}

// failSecondSender serves its first response and fails every later send.
type failSecondSender struct {
	*fakeSender
	err error
}

func (s *failSecondSender) SendResponse(ctx context.Context, req components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	if len(s.fakeSender.requests) >= 1 {
		s.fakeSender.requests = append(s.fakeSender.requests, req)
		return nil, s.err
	}
	return s.fakeSender.SendResponse(ctx, req, metadata, opts...)
}

// Upstream saves state after every response (saveResponseToState) and every
// tool round (saveToolResultsToState): a StateAccessor caller whose run fails
// on turn 2 still has turn 1's response and its executed tool output.
func TestStateAccessorPersistsEachTurnBeforeALaterFailure(t *testing.T) {
	rec := &phCounter{}
	store := &phMemState{}
	boom := errors.New("rate limited")
	sender := &failSecondSender{fakeSender: phSender(phResp("resp_1", phFnCall("call_1", "fetch", `{"target":"a"}`))), err: boom}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{phOKTool("fetch", rec, nil)}, StateAccessor: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("expected the turn-2 failure, got %v", err)
	}
	saved := store.get()
	if saved == nil {
		t.Fatal("state must have been saved before the failure")
	}
	kinds := phItemKinds(t, saved.Messages)
	if countFunctionCallOutputs(saved.Messages) != 1 || len(saved.Messages) != 3 {
		t.Fatalf("saved state must hold user input, function_call and its output, got %v", kinds)
	}
	if rec.count() != 1 {
		t.Fatalf("tool ran %d times", rec.count())
	}
}

// A HITL resume via Input that leads to another tool round keeps looping: the
// loaded awaiting_hitl status must not be mistaken for a fresh pause.
func TestHITLResumeViaInputContinuesToolLoop(t *testing.T) {
	hitlCalls, regular := &phCounter{}, &phCounter{}
	tools := []Tool{hitlRoundTool("confirm", hitlCalls), phOKTool("fetch", regular, nil)}
	first := phSender(phResp("resp_1", phFnCall("call_hitl", "confirm", `{"target":"a"}`)))
	paused, err := CallModel(context.Background(), first, CallModelInput{Model: "m", Input: "hi", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := paused.State(context.Background())
	if state.Status != ConversationStatusAwaitingHITL {
		t.Fatalf("status = %s", state.Status)
	}
	resume := phSender(phResp("resp_2", phFnCall("call_f", "fetch", `{"target":"b"}`)), phTextResp("resp_3", "all done"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{Model: "m", Tools: tools, State: &state, Input: []components.InputsUnion1{functionOutputItem("call_hitl", "ok")}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := resumed.Text(context.Background())
	if err != nil || text != "all done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if regular.count() != 1 || len(resume.requests) != 2 {
		t.Fatalf("the resumed run must execute the next tool round and follow up: ran=%d requests=%d", regular.count(), len(resume.requests))
	}
}

// persistHitlPause / persistClientToolsPause REPLACE the pending list: after
// a HITL resume via Input (which leaves the answered call on the stale
// pending list, as upstream does), a second pause in the same run must list
// only the newly paused call, and approving it resumes normally.
func TestSecondPauseAfterInputResumeReplacesPendingCalls(t *testing.T) {
	cases := []struct {
		name       string
		secondTool string
		wantStatus ConversationStatus
	}{
		{name: "second HITL pause", secondTool: "confirm", wantStatus: ConversationStatusAwaitingHITL},
		{name: "manual pause", secondTool: "manual", wantStatus: ConversationStatusAwaitingClientTools},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hitlCalls, regular := &phCounter{}, &phCounter{}
			manual := MustNewTool(ToolConfig[phTargetInput]{Name: "manual", InputSchema: phObjectSchema("target"), Manual: true})
			tools := []Tool{hitlRoundTool("confirm", hitlCalls), phOKTool("fetch", regular, nil), manual}
			first := phSender(phResp("resp_1", phFnCall("call_hitl", "confirm", `{"target":"a"}`)))
			paused, err := CallModel(context.Background(), first, CallModelInput{Model: "m", Input: "hi", Tools: tools})
			if err != nil {
				t.Fatal(err)
			}
			state, _ := paused.State(context.Background())
			resume := phSender(phResp("resp_2", phFnCall("call_f", "fetch", `{"target":"b"}`)), phResp("resp_3", phFnCall("call_2", tc.secondTool, `{"target":"c"}`)))
			resumed, err := CallModel(context.Background(), resume, CallModelInput{Model: "m", Tools: tools, State: &state, Input: []components.InputsUnion1{functionOutputItem("call_hitl", "ok")}})
			if err != nil {
				t.Fatal(err)
			}
			state2, _ := resumed.State(context.Background())
			if state2.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", state2.Status, tc.wantStatus)
			}
			if ids := phPendingIDs(state2.PendingToolCalls); !phEqualStrings(ids, []string{"call_2"}) {
				t.Fatalf("pending must be only the newly paused call, got %v", ids)
			}
			if tc.secondTool != "confirm" {
				return
			}
			// Approving the new pause re-runs OnToolCalled; it pauses again,
			// so the conversation stays awaiting_hitl on call_2 only.
			again := phSender(phTextResp("resp_4", "done"))
			approved, err := CallModel(context.Background(), again, CallModelInput{Model: "m", Tools: tools, State: &state2, ApproveToolCalls: []string{"call_2"}})
			if err != nil {
				t.Fatal(err)
			}
			state3, _ := approved.State(context.Background())
			if ids := phPendingIDs(state3.PendingToolCalls); !phEqualStrings(ids, []string{"call_2"}) || state3.Status != ConversationStatusAwaitingHITL {
				t.Fatalf("after approving call_2: status=%s pending=%v", state3.Status, ids)
			}
			// Answering it via Input resumes with a well-formed request.
			final := phSender(phTextResp("resp_5", "finished"))
			done, err := CallModel(context.Background(), final, CallModelInput{Model: "m", Tools: tools, State: &state3, Input: []components.InputsUnion1{functionOutputItem("call_2", "fine")}})
			if err != nil {
				t.Fatal(err)
			}
			if text, err := done.Text(context.Background()); err != nil || text != "finished" {
				t.Fatalf("text=%q err=%v", text, err)
			}
			assertEveryCallAnswered(t, requestInputItems(final.requests[0].Input))
		})
	}
}

// Upstream executeToolRound broadcasts tool.call_output for every output,
// including hook-blocked and rejected ones.
func TestToolCallOutputEventForBlockedCall(t *testing.T) {
	hooks := NewHooksManager()
	hooks.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		return HookHandlerResult[PreToolUseResult]{Result: PreToolUseResult{BlockReason: "nope"}, HasResult: true}, nil
	}})
	rec := &phCounter{}
	sender := phSender(phResp("resp_1", phFnCall("call_1", "fetch", `{"target":"a"}`)), phTextResp("resp_2", "ok"))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{phOKTool("fetch", rec, nil)}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	events, err := phDrainToolStream(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	var sawOutput bool
	for _, ev := range events {
		if IsToolCallOutputEvent(ev) && ev.CallID == "call_1" && ev.Error != "" {
			sawOutput = true
		}
	}
	if !sawOutput || rec.count() != 0 {
		t.Fatalf("blocked call must emit tool.call_output with its error (ran=%d), events=%+v", rec.count(), events)
	}
}

// Upstream gates the initial response and pauses for client tools before
// consulting stopWhen: an all-manual first response pauses even when the
// stop condition is already met.
func TestAllManualFirstResponsePausesEvenWhenStopConditionMet(t *testing.T) {
	manual := MustNewTool(ToolConfig[phTargetInput]{Name: "manual", InputSchema: phObjectSchema("target"), Manual: true})
	sender := phSender(phResp("resp_1", phFnCall("call_m", "manual", `{"target":"x"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{manual}, StopWhen: []StopCondition{StepCountIs(1)}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingClientTools || !phEqualStrings(phPendingIDs(state.PendingToolCalls), []string{"call_m"}) {
		t.Fatalf("status=%s pending=%v", state.Status, phPendingIDs(state.PendingToolCalls))
	}
	if len(sender.requests) != 1 {
		t.Fatalf("no final-response request may be sent, sent %d", len(sender.requests))
	}
}

// processApprovalDecisions: approving only one of two paused HITL calls
// leaves the other still owing a decision, so the status is
// awaiting_approval; an Input-only resume then stays paused (no request, no
// dangling call); approving the second call sends a request in which every
// function_call has its output, including the first call's executed result.
func TestPartialApprovalOfMultiHITLPause(t *testing.T) {
	var mu sync.Mutex
	pauseNext := map[string]bool{"a": true, "b": true}
	hitl := MustNewTool(ToolConfig[phTargetInput]{Name: "confirm", InputSchema: phObjectSchema("target"), OnToolCalled: func(_ context.Context, in phTargetInput, _ ToolExecuteContext) (any, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if pauseNext[in.Target] {
			pauseNext[in.Target] = false
			return nil, false, nil
		}
		return "approved " + in.Target, true, nil
	}})
	tools := []Tool{hitl}
	first := phSender(phResp("resp_1", phFnCall("h1", "confirm", `{"target":"a"}`), phFnCall("h2", "confirm", `{"target":"b"}`)))
	paused, err := CallModel(context.Background(), first, CallModelInput{Model: "m", Input: "hi", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := paused.State(context.Background())
	if state.Status != ConversationStatusAwaitingHITL || len(state.PendingToolCalls) != 2 {
		t.Fatalf("status=%s pending=%v", state.Status, phPendingIDs(state.PendingToolCalls))
	}

	approveOne := phSender(phTextResp("never", "never"))
	r1, err := CallModel(context.Background(), approveOne, CallModelInput{Model: "m", Tools: tools, State: &state, ApproveToolCalls: []string{"h1"}})
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := r1.State(context.Background())
	if s1.Status != ConversationStatusAwaitingApproval || !phEqualStrings(phPendingIDs(s1.PendingToolCalls), []string{"h2"}) {
		t.Fatalf("after approving h1: status=%s pending=%v", s1.Status, phPendingIDs(s1.PendingToolCalls))
	}
	if len(approveOne.requests) != 0 {
		t.Fatalf("a partial decision must not send a request, sent %d", len(approveOne.requests))
	}

	inputOnly := phSender(phTextResp("never", "never"))
	r2, err := CallModel(context.Background(), inputOnly, CallModelInput{Model: "m", Tools: tools, State: &s1, Input: []components.InputsUnion1{functionOutputItem("h2", "manual answer")}})
	if err != nil {
		t.Fatal(err)
	}
	var paErr *ConversationPausedError
	if _, err := r2.Response(context.Background()); !errors.As(err, &paErr) || paErr.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("an Input-only resume of awaiting_approval must stay paused, got %v", err)
	}
	if len(inputOnly.requests) != 0 {
		t.Fatalf("no request may be sent, sent %d", len(inputOnly.requests))
	}

	approveTwo := phSender(phTextResp("resp_2", "done"))
	r3, err := CallModel(context.Background(), approveTwo, CallModelInput{Model: "m", Tools: tools, State: &s1, ApproveToolCalls: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := r3.Text(context.Background()); err != nil || text != "done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	items := requestInputItems(approveTwo.requests[0].Input)
	assertEveryCallAnswered(t, items)
	var h1Out string
	for _, it := range items {
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.CallID == "h1" {
			h1Out = phOutputString(*it.FunctionCallOutputItem)
		}
	}
	if h1Out != "approved a" {
		t.Fatalf("h1's executed result must be replayed, got %q", h1Out)
	}
}

// Upstream runs the initial response's approval check before stop
// conditions: an approval-gated call pauses without firing the Stop hook
// and without injecting an AppendPrompt between the function_call and its
// future output.
func TestInitialApprovalPauseBeforeStopHook(t *testing.T) {
	hooks := NewHooksManager()
	stopFired := &phCounter{}
	hooks.OnStop(HookEntry[StopPayload, StopResult]{Handler: func(p StopPayload, _ LifecycleHookContext) (HookHandlerResult[StopResult], error) {
		stopFired.add(p)
		return HookHandlerResult[StopResult]{Result: StopResult{AppendPrompt: "keep going"}, HasResult: true}, nil
	}})
	gated := phOKTool("deploy", &phCounter{}, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	sender := phSender(phResp("resp_1", phFnCall("call_d", "deploy", `{"target":"prod"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{gated}, Hooks: hooks, StopWhen: []StopCondition{StepCountIs(1)}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s", state.Status)
	}
	if stopFired.count() != 0 {
		t.Fatalf("the Stop hook must not fire before the initial approval pause (fired %d)", stopFired.count())
	}
	for _, text := range phUserTexts(state.Messages) {
		if text == "keep going" {
			t.Fatal("no AppendPrompt may be injected into a paused turn")
		}
	}
}
