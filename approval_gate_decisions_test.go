package agent

// Ports tests/unit/approval-gate-regressions.test.ts section "approval
// decision validation and paused resumes (#517, #518)" plus the decision
// ordering rules of upstream processApprovalDecisions.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func agdGatedTool(exec *phCounter) *TypedTool[phVInput] {
	return MustNewTool(ToolConfig[phVInput]{
		Name: "gated", InputSchema: phObjectSchema("v"), RequireApproval: true,
		Execute: func(_ context.Context, in phVInput, execCtx ToolExecuteContext) (any, error) {
			exec.add(execCtx.ToolCall.CallID + ":" + in.V)
			return map[string]any{"ok": true}, nil
		},
	})
}

// agdPauseOnGatedCalls runs one turn that pauses on gated calls with ids.
func agdPauseOnGatedCalls(t *testing.T, ids ...string) (*TypedTool[phVInput], *phCounter, *phMemState) {
	t.Helper()
	exec := &phCounter{}
	tool := agdGatedTool(exec)
	resp := phResp("resp_pause")
	for _, id := range ids {
		resp.Output = append(resp.Output, phFnCall(id, "gated", `{"v":"x"}`))
	}
	mem := &phMemState{}
	sender := phSender(resp)
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "run the gated action", Tools: []Tool{tool}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if st := mem.get(); st == nil || st.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %+v", st)
	}
	if exec.count() != 0 {
		t.Fatal("gated calls must not run before approval")
	}
	return tool, exec, mem
}

type agdHitl struct {
	tool       *TypedTool[phVInput]
	onCalled   *phCounter
	onReceived *phCounter
	mem        *phMemState
}

// agdPauseOnHitlCall runs one turn in which a HITL tool pauses on call_hitl.
func agdPauseOnHitlCall(t *testing.T) agdHitl {
	t.Helper()
	h := agdHitl{onCalled: &phCounter{}, onReceived: &phCounter{}, mem: &phMemState{}}
	h.tool = MustNewTool(ToolConfig[phVInput]{
		Name: "gated", InputSchema: phObjectSchema("v"),
		OnToolCalled: func(_ context.Context, in phVInput, _ ToolExecuteContext) (any, bool, error) {
			h.onCalled.add(in.V)
			return nil, false, nil
		},
		OnResponseReceived: func(_ context.Context, raw any, _ ToolExecuteContext) (any, error) {
			h.onReceived.add(phCopyAny(raw))
			m, _ := raw.(map[string]any)
			answer, _ := m["answer"].(string)
			return map[string]any{"answer": strings.ToUpper(answer)}, nil
		},
	})
	if _, err := mustRun(t, phSender(phResp("resp_hitl", phFnCall("call_hitl", "gated", `{"v":"x"}`))), CallModelInput{Model: "openai/test", Input: "run the gated action", Tools: []Tool{h.tool}, StateAccessor: h.mem}); err != nil {
		t.Fatal(err)
	}
	if st := h.mem.get(); st.Status != ConversationStatusAwaitingHITL {
		t.Fatalf("expected awaiting_hitl, got %s", st.Status)
	}
	if h.onCalled.count() != 1 {
		t.Fatalf("OnToolCalled ran %d times", h.onCalled.count())
	}
	return h
}

func agdCallerOutput(callID, output string) components.InputsUnion1 {
	return components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{
		CallID: callID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(output),
		Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput,
	})
}

// agdPausedErr asserts err is a *ConversationPausedError and returns it.
func agdPausedErr(t *testing.T, label string, err error) *ConversationPausedError {
	t.Helper()
	var paused *ConversationPausedError
	if !errors.As(err, &paused) {
		t.Fatalf("%s: want *ConversationPausedError, got %T %v", label, err, err)
	}
	return paused
}

// upstream: "executes an approved call once when its ID is listed twice"
func TestApproveToolCallsDeduplicated(t *testing.T) {
	ctx := context.Background()
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
	sender := phSender(phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, ApproveToolCalls: []string{"call_1", "call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Done." || exec.count() != 1 {
		t.Fatalf("text=%q exec=%d; an ID listed twice must execute once", text, exec.count())
	}
	if ids := phOutputCallIDs(t, sender, 0); !phEqualStrings(ids, []string{"call_1"}) {
		t.Fatalf("resume outputs = %v, want [call_1]", ids)
	}
}

// upstream: "records a rejection once when its ID is listed twice"
func TestRejectToolCallsDeduplicated(t *testing.T) {
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
	sender := phSender(phTextResp("resp_done", "Done."))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, RejectToolCalls: []string{"call_1", "call_1"}}); err != nil {
		t.Fatal(err)
	}
	if exec.count() != 0 {
		t.Fatal("a rejected call must not run")
	}
	outs := phOutputs(t, sender, 0)
	if len(outs) != 1 || outs[0].CallID != "call_1" || phOutputError(outs[0]) != "Rejected by user" {
		t.Fatalf("resume outputs = %+v, want one rejection for call_1", outs)
	}
}

// upstream: "refuses an ID that is both approved and rejected before anything runs"
func TestConflictingApprovalDecisionsRefused(t *testing.T) {
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1", "call_2")
	before := phSnapshot(t, mem)
	sender := phSender(phTextResp("resp_done", "Done."))
	_, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem,
		ApproveToolCalls: []string{"call_1", "call_2"}, RejectToolCalls: []string{"call_2"},
	})
	if err == nil || !strings.Contains(err.Error(), "both approved and rejected: call_2") {
		t.Fatalf("want a 'both approved and rejected: call_2' error, got %v", err)
	}
	if strings.Contains(err.Error(), "call_1") {
		t.Fatalf("only the conflicting ID should be listed: %v", err)
	}
	if exec.count() != 0 || sender.calls != 0 {
		t.Fatalf("nothing may run: exec=%d calls=%d", exec.count(), sender.calls)
	}
	if after := phSnapshot(t, mem); after != before {
		t.Fatal("state must be untouched")
	}
}

// upstream: "stays awaiting_approval and sends no request when resumed without decisions"
func TestAwaitingApprovalWithoutDecisionsStaysPaused(t *testing.T) {
	ctx := context.Background()
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
	before := phSnapshot(t, mem)
	sender := phSender(phTextResp("resp_should_not_be_requested", "nope"))

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if paused.Status != ConversationStatusAwaitingApproval || !phEqualStrings(paused.PendingCallIDs, []string{"call_1"}) || paused.InputDropped {
		t.Fatalf("paused error = %+v", paused)
	}
	if !strings.Contains(err.Error(), "awaiting approval") || !strings.Contains(err.Error(), "call_1") {
		t.Fatalf("message must say awaiting approval and list call_1: %q", err)
	}
	if _, err := result.Text(ctx); !strings.Contains(fmt.Sprint(err), "awaiting approval") {
		t.Fatalf("Text error = %v", err)
	}
	requires, err := result.RequiresApproval(ctx)
	if err != nil || !requires {
		t.Fatalf("RequiresApproval = %v, %v", requires, err)
	}
	pending, err := result.PendingToolCalls(ctx)
	if err != nil || !phEqualStrings(phPendingIDs(pending), []string{"call_1"}) {
		t.Fatalf("PendingToolCalls = %v, %v", phPendingIDs(pending), err)
	}
	if sender.calls != 0 || exec.count() != 0 {
		t.Fatalf("no request and no execution: calls=%d exec=%d", sender.calls, exec.count())
	}
	if after := phSnapshot(t, mem); after != before {
		t.Fatalf("state must be untouched\nbefore=%s\nafter=%s", before, after)
	}

	// The pause still resolves normally once a decision is supplied.
	if _, err := mustRun(t, phSender(phTextResp("resp_done", "Done.")), CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, ApproveToolCalls: []string{"call_1"}}); err != nil {
		t.Fatal(err)
	}
	if exec.count() != 1 || mem.get().Status != ConversationStatusComplete {
		t.Fatalf("after approval: exec=%d status=%s", exec.count(), mem.get().Status)
	}
}

// upstream: "stays awaiting_hitl and sends no request when resumed without decisions"
func TestAwaitingHITLWithoutDecisionsStaysPaused(t *testing.T) {
	ctx := context.Background()
	h := agdPauseOnHitlCall(t)
	sender := phSender(phTextResp("resp_should_not_be_requested", "nope"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{h.tool}, StateAccessor: h.mem})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if paused.Status != ConversationStatusAwaitingHITL || !phEqualStrings(paused.PendingCallIDs, []string{"call_hitl"}) {
		t.Fatalf("paused error = %+v", paused)
	}
	if msg := err.Error(); !strings.Contains(msg, "awaiting HITL input") || !strings.Contains(msg, "call_hitl") {
		t.Fatalf("message = %q", msg)
	}
	if sender.calls != 0 || h.onCalled.count() != 1 || h.mem.get().Status != ConversationStatusAwaitingHITL {
		t.Fatalf("calls=%d onToolCalled=%d status=%s", sender.calls, h.onCalled.count(), h.mem.get().Status)
	}
}

// upstream: "reports the pause, not a generic error, when decisions cover only some pending calls"
func TestPartialDecisionsReportThePause(t *testing.T) {
	ctx := context.Background()
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1", "call_2")
	sender := phSender(phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, ApproveToolCalls: []string{"call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if !phEqualStrings(paused.PendingCallIDs, []string{"call_2"}) || !strings.Contains(err.Error(), "call_2") {
		t.Fatalf("pause must name the remaining call: %+v %q", paused, err)
	}
	if errors.Is(err, ErrResponseNotAvailable) {
		t.Fatal("a pause is not the generic response-not-available error")
	}
	if exec.count() != 1 || sender.calls != 0 || mem.get().Status != ConversationStatusAwaitingApproval {
		t.Fatalf("exec=%d calls=%d status=%s", exec.count(), sender.calls, mem.get().Status)
	}
	// The approved call's result is kept for the eventual resume request.
	if st := mem.get(); len(st.UnsentToolResults) != 1 || st.UnsentToolResults[0].CallID != "call_1" {
		t.Fatalf("approved result must be persisted as unsent: %+v", st.UnsentToolResults)
	}
}

// upstream: "resumes awaiting_hitl when input carries a function_call_output for the paused call"
func TestAwaitingHITLResumesWithCallerOutput(t *testing.T) {
	ctx := context.Background()
	h := agdPauseOnHitlCall(t)
	sender := phSender(phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{h.tool}, StateAccessor: h.mem,
		Input: []components.InputsUnion1{agdCallerOutput("call_hitl", `{"answer":"yes"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Done." {
		t.Fatalf("text = %q", text)
	}
	if got := h.onReceived.values(); len(got) != 1 || got[0] != `{"answer":"yes"}` {
		t.Fatalf("OnResponseReceived must run once on the parsed caller output, saw %v", got)
	}
	if h.onCalled.count() != 1 || sender.calls != 1 {
		t.Fatalf("the paused call is not re-run: onToolCalled=%d calls=%d", h.onCalled.count(), sender.calls)
	}
	outs := phOutputs(t, sender, 0)
	if len(outs) != 1 || outs[0].CallID != "call_hitl" || phOutputString(outs[0]) != `{"answer":"YES"}` {
		t.Fatalf("model must get the hook-transformed output, got %+v", outs)
	}
	if h.mem.get().Status != ConversationStatusComplete {
		t.Fatalf("status = %s", h.mem.get().Status)
	}
}

// upstream: "stays awaiting_hitl when input has no output for a pending call, and says the input was dropped"
func TestAwaitingHITLDropsUnrelatedInput(t *testing.T) {
	ctx := context.Background()
	h := agdPauseOnHitlCall(t)
	before := phSnapshot(t, h.mem)
	sender := phSender(phTextResp("resp_x", "nope"))
	for _, input := range []any{
		"a new user message",
		[]components.InputsUnion1{agdCallerOutput("call_other", `{"answer":"yes"}`)},
	} {
		result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{h.tool}, StateAccessor: h.mem, Input: input})
		if err != nil {
			t.Fatal(err)
		}
		_, err = result.Response(ctx)
		paused := agdPausedErr(t, fmt.Sprintf("input %T", input), err)
		if !paused.InputDropped {
			t.Fatalf("InputDropped must be set for %T input", input)
		}
		msg := err.Error()
		for _, want := range []string{"awaiting HITL input", "call_hitl", "function_call_output", "input supplied with this call was not sent"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message %q missing %q", msg, want)
			}
		}
		if i1, i2 := strings.Index(msg, "call_hitl"), strings.Index(msg, "function_call_output"); i1 > i2 {
			t.Fatalf("message order differs from upstream: %q", msg)
		}
	}
	if sender.calls != 0 || h.onReceived.count() != 0 {
		t.Fatalf("calls=%d onResponseReceived=%d", sender.calls, h.onReceived.count())
	}
	if after := phSnapshot(t, h.mem); after != before {
		t.Fatal("dropped input must not be saved to state")
	}
}

// upstream: "stays awaiting_approval when input carries a function_call_output for the gated call"
func TestAwaitingApprovalIgnoresCallerOutputForGatedCall(t *testing.T) {
	ctx := context.Background()
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
	before := phSnapshot(t, mem)
	sender := phSender(phTextResp("resp_x", "nope"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem,
		Input: []components.InputsUnion1{agdCallerOutput("call_1", `{"ok":true}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if !paused.InputDropped || paused.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("paused error = %+v", paused)
	}
	msg := err.Error()
	for _, want := range []string{"awaiting approval", "call_1", "input supplied with this call was not sent"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	if sender.calls != 0 || exec.count() != 0 {
		t.Fatalf("calls=%d exec=%d", sender.calls, exec.count())
	}
	if after := phSnapshot(t, mem); after != before {
		t.Fatal("state must be untouched")
	}
}

// upstream: "rejects every response and stream getter with the awaiting error on a paused resume"
func TestPausedResumeEveryGetterReturnsPausedError(t *testing.T) {
	ctx := context.Background()
	tool, _, mem := agdPauseOnGatedCalls(t, "call_1")
	before := phSnapshot(t, mem)
	sender := phSender(phTextResp("resp_x", "nope"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem})
	if err != nil {
		t.Fatal(err)
	}

	consumers := map[string]func() error{
		"Response": func() error { _, err := result.Response(ctx); return err },
		"Text":     func() error { _, err := result.Text(ctx); return err },
		"ToolCalls": func() error {
			_, err := result.ToolCalls(ctx)
			return err
		},
		"TextStream": func() error {
			ch, wait := result.TextStream(ctx)
			for range ch {
			}
			return wait()
		},
		"ReasoningStream": func() error {
			ch, wait := result.ReasoningStream(ctx)
			for range ch {
			}
			return wait()
		},
		"FullResponsesStream": func() error {
			ch, wait := result.FullResponsesStream(ctx)
			for range ch {
			}
			return wait()
		},
		"ItemsStream": func() error {
			ch, wait := result.ItemsStream(ctx)
			for range ch {
			}
			return wait()
		},
		"NewMessagesStream": func() error {
			ch, wait := result.NewMessagesStream(ctx)
			for range ch {
			}
			return wait()
		},
		"ToolStream": func() error {
			_, err := phDrainToolStream(ctx, result)
			return err
		},
		"ToolCallsStream": func() error {
			ch, wait := result.ToolCallsStream(ctx)
			for range ch {
			}
			return wait()
		},
	}
	for name, consume := range consumers {
		err := consume()
		paused := agdPausedErr(t, name, err)
		if paused.Status != ConversationStatusAwaitingApproval || !phEqualStrings(paused.PendingCallIDs, []string{"call_1"}) {
			t.Fatalf("%s: paused error = %+v", name, paused)
		}
		if !strings.Contains(err.Error(), "awaiting approval") {
			t.Fatalf("%s: message = %q", name, err)
		}
	}

	if requires, err := result.RequiresApproval(ctx); err != nil || !requires {
		t.Fatalf("RequiresApproval = %v, %v", requires, err)
	}
	if pending, err := result.PendingToolCalls(ctx); err != nil || !phEqualStrings(phPendingIDs(pending), []string{"call_1"}) {
		t.Fatalf("PendingToolCalls = %v, %v", phPendingIDs(pending), err)
	}
	if st, err := result.State(ctx); err != nil || st.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("State = %s, %v", st.Status, err)
	}
	if usage := result.Usage(ctx); usage.ModelCalls != 0 {
		t.Fatalf("a paused resume made no model call, Usage = %+v", usage)
	}
	if sender.calls != 0 {
		t.Fatalf("no request may be sent, got %d", sender.calls)
	}
	if after := phSnapshot(t, mem); after != before {
		t.Fatal("state must be untouched")
	}
}

// upstream: "continues a %s state whose pendingToolCalls is %j"
func TestPausedStatusWithoutPendingCallsContinues(t *testing.T) {
	cases := []struct {
		name    string
		status  ConversationStatus
		pending []ParsedToolCall
	}{
		{"awaiting_approval with empty pending", ConversationStatusAwaitingApproval, []ParsedToolCall{}},
		{"awaiting_approval with nil pending", ConversationStatusAwaitingApproval, nil},
		{"awaiting_hitl with empty pending", ConversationStatusAwaitingHITL, []ParsedToolCall{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
			stranded := *mem.get()
			stranded.Status = tc.status
			stranded.PendingToolCalls = tc.pending
			next := &phMemState{}
			if err := next.Save(ctx, stranded); err != nil {
				t.Fatal(err)
			}
			sender := phSender(phTextResp("resp_done", "Done."))
			result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "run the gated action", Tools: []Tool{tool}, StateAccessor: next})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if text != "Done." || sender.calls != 1 || exec.count() != 0 || next.get().Status != ConversationStatusComplete {
				t.Fatalf("text=%q calls=%d exec=%d status=%s", text, sender.calls, exec.count(), next.get().Status)
			}
		})
	}
}

// upstream: "caps the pending IDs listed in the awaiting error and omits the
// input note when no input was supplied"
func TestPausedErrorCapsListedIDs(t *testing.T) {
	ctx := context.Background()
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = fmt.Sprintf("call_%d", i+1)
	}
	tool, _, mem := agdPauseOnGatedCalls(t, ids...)
	result, err := CallModel(ctx, phSender(phTextResp("r", "x")), CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, Input: []components.InputsUnion1{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if len(paused.PendingCallIDs) != 25 {
		t.Fatalf("PendingCallIDs carries the full list, got %d", len(paused.PendingCallIDs))
	}
	if paused.InputDropped {
		t.Fatal("an empty input is not dropped input")
	}
	msg := err.Error()
	if !strings.Contains(msg, "call_1, call_2,") || !strings.Contains(msg, "call_10, ... (15 more)") {
		t.Fatalf("message must list the first 10 IDs then '(15 more)': %q", msg)
	}
	if strings.Contains(msg, "call_11") || strings.Contains(msg, "was not sent") {
		t.Fatalf("message must not list call_11 or the input note: %q", msg)
	}

	// Exactly 10 pending IDs: all listed, no "(N more)" suffix.
	exact := &ConversationPausedError{Status: ConversationStatusAwaitingApproval, PendingCallIDs: ids[:10]}
	if m := exact.Error(); strings.Contains(m, "more)") || !strings.Contains(m, "call_10") {
		t.Fatalf("10 IDs must be listed in full: %q", m)
	}
}

// upstream: "does not let a duplicated ID bypass re-approval after a PreToolUse mutation"
func TestDuplicatedApprovalIDDoesNotBypassSecondGate(t *testing.T) {
	ctx := context.Background()
	var exec phCounter
	tool := MustNewTool(ToolConfig[phVInput]{
		Name: "gated", InputSchema: phObjectSchema("v"),
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) { return true, nil },
		Execute: func(_ context.Context, in phVInput, _ ToolExecuteContext) (any, error) {
			exec.add(in.V)
			return map[string]any{"ok": true}, nil
		},
	})
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("resp_pause", phFnCall("call_1", "gated", `{"v":"reviewed"}`))), CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if mem.get().Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s", mem.get().Status)
	}

	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"v": "mutated"}}
	})
	sender := phSender(phTextResp("resp_x", "nope"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem, ApproveToolCalls: []string{"call_1", "call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if !phEqualStrings(paused.PendingCallIDs, []string{"call_1"}) {
		t.Fatalf("paused error = %+v", paused)
	}
	if exec.count() != 0 || sender.calls != 0 {
		t.Fatalf("the mutated input needs its own approval: exec=%d calls=%d", exec.count(), sender.calls)
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingApproval || len(st.PendingToolCalls) != 1 {
		t.Fatalf("state = %s %+v", st.Status, st.PendingToolCalls)
	}
	p := st.PendingToolCalls[0]
	if callKey(p) != "call_1" || p.Name != "gated" || agrArgsJSON(t, p) != `{"v":"mutated"}` || !p.PreToolUseApplied {
		t.Fatalf("pending call = %+v", p)
	}

	// Approving the re-gated call runs it once on the mutated input, without
	// re-running PreToolUse.
	var pre phCounter
	hooks2 := NewHooksManager()
	agrOnPreToolUse(hooks2, &pre, nil)
	if _, err := mustRun(t, phSender(phTextResp("resp_done", "Done.")), CallModelInput{Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks2, StateAccessor: mem, ApproveToolCalls: []string{"call_1"}}); err != nil {
		t.Fatal(err)
	}
	if got := exec.values(); len(got) != 1 || got[0] != "mutated" || pre.count() != 0 {
		t.Fatalf("exec=%v pre=%d", got, pre.count())
	}
}

// Approvals are processed in ApproveToolCalls order, then rejections
// (upstream processApprovalDecisions); the resume request's outputs follow
// the same order.
func TestApprovalDecisionsProcessedInOrder(t *testing.T) {
	tool, exec, mem := agdPauseOnGatedCalls(t, "c1", "c2", "c3", "c4")
	sender := phSender(phTextResp("resp_done", "Done."))
	if _, err := mustRun(t, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem,
		RejectToolCalls: []string{"c4", "c2"}, ApproveToolCalls: []string{"c3", "c1"},
	}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, v := range exec.values() {
		order = append(order, v.(string))
	}
	if !phEqualStrings(order, []string{"c3:x", "c1:x"}) {
		t.Fatalf("approved calls must run in ApproveToolCalls order, ran %v", order)
	}
	if ids := phOutputCallIDs(t, sender, 0); !phEqualStrings(ids, []string{"c3", "c1", "c4", "c2"}) {
		t.Fatalf("resume outputs must be approvals (in order) then rejections (in order): %v", ids)
	}
	// function_call items precede their outputs in the resume request.
	kinds := phItemKinds(t, requestInputItems(sender.requests[0].Input))
	if !phEqualStrings(kinds, []string{"user", "function_call", "function_call", "function_call", "function_call", "function_call_output", "function_call_output", "function_call_output", "function_call_output"}) {
		t.Fatalf("resume request item order = %v", kinds)
	}
}

// Decision IDs that match no pending call are ignored; the run stays paused
// on the calls that still owe a decision.
func TestUnknownDecisionIDsIgnored(t *testing.T) {
	ctx := context.Background()
	tool, exec, mem := agdPauseOnGatedCalls(t, "call_1")
	sender := phSender(phTextResp("resp_x", "nope"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, StateAccessor: mem, ApproveToolCalls: []string{"call_unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Response(ctx)
	paused := agdPausedErr(t, "Response", err)
	if !phEqualStrings(paused.PendingCallIDs, []string{"call_1"}) || exec.count() != 0 || sender.calls != 0 {
		t.Fatalf("paused=%+v exec=%d calls=%d", paused, exec.count(), sender.calls)
	}
}
