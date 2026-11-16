package agent

// Ports tests/unit/get-usage.test.ts: ModelResult.Usage aggregates every
// model call of one CallModel run.

import (
	"context"
	"math"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// guWithUsage attaches upstream's usageBlock() shape (25 cached, 10
// reasoning) with the given token counts and cost.
func guWithUsage(resp components.OpenResponsesResult, in, out int64, cost *float64) components.OpenResponsesResult {
	resp.Usage = phUsage(in, out, 25, 10, cost)
	return resp
}

func guEcho() Tool {
	type empty struct{}
	return MustNewTool(ToolConfig[empty]{
		Name: "echo", InputSchema: map[string]any{"type": "object"},
		Execute: func(context.Context, empty, ToolExecuteContext) (any, error) { return map[string]any{"ok": true}, nil },
	})
}

func guToolCall(id string) components.OpenResponsesResult {
	return guWithUsage(phResp(id, phFnCall("call_"+id, "echo", `{}`)), 100, 50, phFloat(0.002))
}

func guRisky(rec *phCounter) Tool {
	return phOKTool("risky", rec, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
}

func guAssertTotals(t *testing.T, got SessionUsageTotals, calls int, in, out, cached, reasoning int64, cost *float64) {
	t.Helper()
	if got.ModelCalls != calls || got.InputTokens != in || got.OutputTokens != out || got.TotalTokens != in+out || got.CachedTokens != cached || got.ReasoningTokens != reasoning {
		t.Fatalf("usage = %+v, want calls=%d in=%d out=%d total=%d cached=%d reasoning=%d", got, calls, in, out, in+out, cached, reasoning)
	}
	switch {
	case cost == nil && got.Cost != nil:
		t.Fatalf("cost = %v, want nil (no call reported cost)", *got.Cost)
	case cost != nil && (got.Cost == nil || math.Abs(*got.Cost-*cost) > 1e-12):
		t.Fatalf("cost = %v, want %v", got.Cost, *cost)
	}
}

// upstream: "sums usage across every round of a multi-round tool loop"
func TestUsageSumsEveryRound(t *testing.T) {
	ctx := context.Background()
	sender := phSender(guToolCall("r1"), guWithUsage(phTextResp("r2", "hello back"), 200, 100, phFloat(0.003)))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guAssertTotals(t, result.Usage(ctx), 2, 300, 150, 50, 20, phFloat(0.005))
	// The gap this closes: the final response alone reports only round 2.
	if u, _ := resp.Usage.GetOrZero(); u.TotalTokens != 300 {
		t.Fatalf("final response usage = %d, want 300", u.TotalTokens)
	}
}

// upstream: "awaits run completion when called without awaiting getResponse() first"
func TestUsageAwaitsRunCompletion(t *testing.T) {
	ctx := context.Background()
	sender := phSender(guToolCall("r1"), guWithUsage(phTextResp("r2", "hi"), 100, 50, phFloat(0.002)))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}})
	if err != nil {
		t.Fatal(err)
	}
	if u := result.Usage(ctx); u.ModelCalls != 2 || u.TotalTokens != 300 {
		t.Fatalf("usage = %+v", u)
	}
}

// upstream: "accumulates with no hooks configured (pull-based, not hook-driven)"
// and "reports complete totals after consuming the no-tools getItemsStream()"
func TestUsageWithoutHooks(t *testing.T) {
	ctx := context.Background()
	result, err := CallModel(ctx, phSender(guWithUsage(phTextResp("r1", "hello back"), 100, 50, phFloat(0.002))), CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	ch, wait := result.ItemsStream(ctx)
	for range ch {
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	guAssertTotals(t, result.Usage(ctx), 1, 100, 50, 25, 10, phFloat(0.002))
}

// upstream: "agrees with the SessionEnd.totalUsage hook payload"
func TestUsageMatchesSessionEndTotals(t *testing.T) {
	ctx := context.Background()
	hooks := NewHooksManager()
	var ends phCounter
	hooks.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		if p.TotalUsage != nil {
			ends.add(*p.TotalUsage)
		} else {
			ends.add(nil)
		}
		return VoidResult[EmptyHookResult](), nil
	}})
	sender := phSender(guToolCall("r1"), guWithUsage(phTextResp("r2", "hi"), 100, 50, phFloat(0.002)))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	got := ends.values()
	if len(got) != 1 {
		t.Fatalf("SessionEnd fired %d times", len(got))
	}
	end, ok := got[0].(SessionUsageTotals)
	if !ok {
		t.Fatal("SessionEnd carried no totals")
	}
	usage := result.Usage(ctx)
	if phJSON(t, usage) != phJSON(t, end) {
		t.Fatalf("Usage %s != SessionEnd.TotalUsage %s", phJSON(t, usage), phJSON(t, end))
	}
}

// upstream: "counts usage-less responses in modelCalls and omits cost"
func TestUsageCountsUsagelessResponses(t *testing.T) {
	ctx := context.Background()
	result, err := CallModel(ctx, phSender(phTextResp("r1", "hi")), CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	guAssertTotals(t, result.Usage(ctx), 1, 0, 0, 0, 0, nil)

	// Cost is present only when at least one call reported it.
	mixed := phSender(guWithUsage(guToolCall("r1"), 10, 5, nil), guWithUsage(phTextResp("r2", "x"), 10, 5, phFloat(0.25)))
	r2, err := CallModel(ctx, mixed, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}})
	if err != nil {
		t.Fatal(err)
	}
	guAssertTotals(t, r2.Usage(ctx), 2, 20, 10, 50, 20, phFloat(0.25))
}

// upstream: "returns zeroed totals when no model call completed" — Usage
// never errors, even on a failed run.
func TestUsageZeroWhenNoCallCompleted(t *testing.T) {
	ctx := context.Background()
	created := operations.CreateCreateResponsesResponseEventStream(failingEventStream())
	result, err := CallModel(ctx, &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err == nil {
		t.Fatal("Text must surface the transport failure")
	}
	guAssertTotals(t, result.Usage(ctx), 0, 0, 0, 0, 0, nil)
}

// A run that fails mid-loop reports the totals accrued before the failure.
func TestUsagePartialTotalsOnFailure(t *testing.T) {
	ctx := context.Background()
	first := operations.CreateCreateResponsesResponseOpenResponsesResult(guToolCall("r1"))
	failing := operations.CreateCreateResponsesResponseEventStream(failingEventStream())
	result, err := CallModel(ctx, &fakeSender{responses: []*operations.CreateResponsesResponse{&first, &failing}}, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(ctx); err == nil {
		t.Fatal("the failed follow-up must surface")
	}
	guAssertTotals(t, result.Usage(ctx), 1, 100, 50, 25, 10, phFloat(0.002))
}

// The AllowFinalResponse final turn and the empty-final retry are model
// calls too.
func TestUsageCountsFinalTurnAndRetry(t *testing.T) {
	ctx := context.Background()
	t.Run("final response turn", func(t *testing.T) {
		sender := phSender(guToolCall("r1"), guWithUsage(phTextResp("r2", "final"), 7, 3, nil))
		result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}, StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true})
		if err != nil {
			t.Fatal(err)
		}
		guAssertTotals(t, result.Usage(ctx), 2, 107, 53, 50, 20, phFloat(0.002))
	})
	t.Run("empty-final retry", func(t *testing.T) {
		sender := phSender(guToolCall("r1"), guWithUsage(phResp("r2"), 1, 1, nil), guWithUsage(phTextResp("r3", "answer"), 2, 2, nil))
		result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{guEcho()}})
		if err != nil {
			t.Fatal(err)
		}
		if text, _ := result.Text(ctx); text != "answer" || sender.calls != 3 {
			t.Fatalf("text=%q calls=%d; the retry fixture did not run", text, sender.calls)
		}
		guAssertTotals(t, result.Usage(ctx), 3, 103, 53, 75, 30, phFloat(0.002))
	})
}

// upstream: "paused at awaiting_approval" — "reports only the model calls that
// completed before the pause" and "is idempotent while paused and drives no
// further model requests"
func TestUsageWhilePausedAtApproval(t *testing.T) {
	ctx := context.Background()
	var runs phCounter
	sender := phSender(guWithUsage(phResp("r1", phFnCall("call_r1", "risky", `{"target":"prod"}`)), 100, 50, phFloat(0.002)), phTextResp("should-not-be-consumed", "x"))
	mem := &phMemState{}
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "do the risky thing", Tools: []Tool{guRisky(&runs)}, StateAccessor: mem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.PendingToolCalls(ctx); err != nil {
		t.Fatal(err)
	}
	if mem.get().Status != ConversationStatusAwaitingApproval || sender.calls != 1 {
		t.Fatalf("status=%s calls=%d", mem.get().Status, sender.calls)
	}
	first := result.Usage(ctx)
	guAssertTotals(t, first, 1, 100, 50, 25, 10, phFloat(0.002))
	if again := result.Usage(ctx); phJSON(t, again) != phJSON(t, first) || sender.calls != 1 {
		t.Fatalf("Usage must be idempotent and send nothing: %+v calls=%d", again, sender.calls)
	}
}

// upstream: "scopes totals per ModelResult across an approval resume" and
// "counts a STREAMING resume generation"
func TestUsageScopedPerResultAcrossResume(t *testing.T) {
	ctx := context.Background()
	for _, streamed := range []bool{false, true} {
		var runs phCounter
		mem := &phMemState{}
		first, err := CallModel(ctx, phSender(guWithUsage(phResp("r1", phFnCall("call_r1", "risky", `{"target":"prod"}`)), 100, 50, phFloat(0.002))), CallModelInput{Model: "openai/test", Input: "do it", Tools: []Tool{guRisky(&runs)}, StateAccessor: mem})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.PendingToolCalls(ctx); err != nil {
			t.Fatal(err)
		}
		var resumeSender *fakeSender
		if streamed {
			resp := completedResponse("hello back")
			resp.ID = "r2_streamed"
			resp.Usage = phUsage(200, 100, 25, 10, phFloat(0.003))
			resumeSender = &fakeSender{responses: streamingResponsesOf(t, []components.StreamEvents{textDelta("hello back", 1), completedEvent(resp, 2)})}
		} else {
			resumeSender = phSender(guWithUsage(phTextResp("r2", "hello back"), 200, 100, phFloat(0.003)))
		}
		resumed, err := CallModel(ctx, resumeSender, CallModelInput{Model: "openai/test", Tools: []Tool{guRisky(&runs)}, StateAccessor: mem, ApproveToolCalls: []string{"call_r1"}})
		if err != nil {
			t.Fatal(err)
		}
		guAssertTotals(t, resumed.Usage(ctx), 1, 200, 100, 25, 10, phFloat(0.003))
		guAssertTotals(t, first.Usage(ctx), 1, 100, 50, 25, 10, phFloat(0.002))
		if runs.count() != 1 {
			t.Fatalf("streamed=%v: approved call ran %d times", streamed, runs.count())
		}
	}
}
