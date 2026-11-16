package agent

// Ports tests/unit/stop-when-before-approval.test.ts.

import (
	"context"
	"testing"
)

func swbaTools() (auto, gated *TypedTool[phVInput], autoRuns, gatedRuns *phCounter) {
	autoRuns, gatedRuns = &phCounter{}, &phCounter{}
	mk := func(name string, rec *phCounter, gate bool) *TypedTool[phVInput] {
		return MustNewTool(ToolConfig[phVInput]{
			Name: name, InputSchema: phObjectSchema("v"), RequireApproval: gate,
			Execute: func(_ context.Context, in phVInput, _ ToolExecuteContext) (any, error) {
				rec.add(in.V)
				return map[string]any{"ok": true}, nil
			},
		})
	}
	return mk("auto", autoRuns, false), mk("gated", gatedRuns, true), autoRuns, gatedRuns
}

func swbaSender() *fakeSender {
	return phSender(phResp("resp_1", phFnCall("call_auto", "auto", `{"v":"a"}`), phFnCall("call_gated", "gated", `{"v":"g"}`)))
}

func swbaAlways(v bool) StopCondition {
	return func(context.Context, []StepResult) (bool, error) { return v, nil }
}

// upstream: "does not run an auto-executable sibling of an approval-gated
// call when stopWhen is already met"
func TestStopWhenMetSkipsAutoSiblingsAtApprovalPause(t *testing.T) {
	ctx := context.Background()
	auto, gated, autoRuns, gatedRuns := swbaTools()
	mem := &phMemState{}
	sender := swbaSender()
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{auto, gated}, StateAccessor: mem,
		StopWhen: []StopCondition{swbaAlways(true)}, AllowFinalResponse: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	requires, err := result.RequiresApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if autoRuns.count() != 0 || gatedRuns.count() != 0 {
		t.Fatalf("no tool may run once stopWhen is met: auto=%d gated=%d", autoRuns.count(), gatedRuns.count())
	}
	if sender.calls != 1 {
		t.Fatalf("requests = %d, want 1", sender.calls)
	}
	st := mem.get()
	if !requires || st.Status != ConversationStatusAwaitingApproval || !phEqualStrings(phPendingIDs(st.PendingToolCalls), []string{"call_gated"}) {
		t.Fatalf("requires=%v status=%s pending=%v", requires, st.Status, phPendingIDs(st.PendingToolCalls))
	}
	want := UnsentToolResult{CallID: "call_auto", Name: "auto", Output: nil, Error: "Tool execution skipped: stop condition met"}
	if len(st.UnsentToolResults) != 1 || st.UnsentToolResults[0] != want {
		t.Fatalf("the skipped sibling must be saved as a skipped unsent result, got %+v", st.UnsentToolResults)
	}

	// Resuming replays the skipped result alongside the decision, so the
	// paused turn stays well-formed (every function_call answered).
	resume := phSender(phTextResp("resp_2", "done"))
	if _, err := mustRun(t, resume, CallModelInput{Model: "openai/test", Tools: []Tool{auto, gated}, StateAccessor: mem, ApproveToolCalls: []string{"call_gated"}, AllowFinalResponse: false}); err != nil {
		t.Fatal(err)
	}
	outs := phOutputs(t, resume, 0)
	if len(outs) != 2 {
		t.Fatalf("resume request must answer both calls, got %+v", outs)
	}
	byID := map[string]string{}
	for _, o := range outs {
		byID[o.CallID] = phOutputError(o)
	}
	if byID["call_auto"] != "Tool execution skipped: stop condition met" || byID["call_gated"] != "" {
		t.Fatalf("resume outputs = %v", byID)
	}
	if autoRuns.count() != 0 || gatedRuns.count() != 1 {
		t.Fatalf("after resume: auto=%d gated=%d", autoRuns.count(), gatedRuns.count())
	}
}

// upstream: "still runs the auto-executable sibling when stopWhen is not met"
func TestStopWhenNotMetRunsAutoSiblingBeforeApprovalPause(t *testing.T) {
	auto, gated, autoRuns, _ := swbaTools()
	mem := &phMemState{}
	if _, err := mustRun(t, swbaSender(), CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{auto, gated}, StateAccessor: mem,
		StopWhen: []StopCondition{swbaAlways(false)}, AllowFinalResponse: false,
	}); err != nil {
		t.Fatal(err)
	}
	if autoRuns.count() != 1 {
		t.Fatalf("auto sibling must run once, ran %d", autoRuns.count())
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingApproval || !phEqualStrings(phPendingIDs(st.PendingToolCalls), []string{"call_gated"}) {
		t.Fatalf("status=%s pending=%v", st.Status, phPendingIDs(st.PendingToolCalls))
	}
	if len(st.UnsentToolResults) != 1 || st.UnsentToolResults[0].CallID != "call_auto" || st.UnsentToolResults[0].Error != "" {
		t.Fatalf("auto result must be a successful unsent result, got %+v", st.UnsentToolResults)
	}
}

// With AllowFinalResponse=false and stopWhen met on a response with no
// approval-gated call, nothing runs and the run completes on that response.
func TestStopWhenMetWithoutGatedCallCompletesWithoutRunning(t *testing.T) {
	ctx := context.Background()
	auto, _, autoRuns, _ := swbaTools()
	sender := phSender(phResp("resp_1", phFnCall("call_auto", "auto", `{"v":"a"}`)))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{auto},
		StopWhen: []StopCondition{swbaAlways(true)}, AllowFinalResponse: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := result.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if autoRuns.count() != 0 || sender.calls != 1 || st.Status != ConversationStatusComplete || len(st.UnsentToolResults) != 0 {
		t.Fatalf("auto=%d calls=%d status=%s unsent=%+v", autoRuns.count(), sender.calls, st.Status, st.UnsentToolResults)
	}
}
