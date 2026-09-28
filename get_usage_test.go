package agent

import (
	"context"
	"errors"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// Ported from upstream tests/unit/get-usage.test.ts (#97).

func usageBlock(in, out, cached, reasoning int64, cost *float64) optionalnullable.OptionalNullable[components.Usage] {
	u := components.Usage{
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		InputTokensDetails:  components.InputTokensDetails{CachedTokens: cached},
		OutputTokensDetails: components.OutputTokensDetails{ReasoningTokens: reasoning},
	}
	if cost != nil {
		u.Cost = optionalnullable.From(cost)
	}
	return optionalnullable.From(&u)
}

func f64(v float64) *float64 { return &v }

func TestUsageSumsEveryRoundWithNoHooks(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}, Usage: usageBlock(100, 50, 25, 10, f64(0.002))}
	second := components.OpenResponsesResult{ID: "r2", OutputText: openrouter.String("done"), Usage: usageBlock(200, 100, 0, 0, f64(0.003))}
	sender := twoTurnSender(first, second)
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi", Tools: []Tool{searchTool(t)}})
	if err != nil {
		t.Fatal(err)
	}
	// Called without awaiting Response first: must wait for the run.
	usage := result.Usage(context.Background())
	if usage.ModelCalls != 2 || usage.InputTokens != 300 || usage.OutputTokens != 150 || usage.TotalTokens != 450 || usage.CachedTokens != 25 || usage.ReasoningTokens != 10 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Cost == nil || *usage.Cost < 0.00499 || *usage.Cost > 0.00501 {
		t.Fatalf("cost = %v, want 0.005", usage.Cost)
	}
	if sender.calls != 2 {
		t.Fatalf("calls = %d", sender.calls)
	}
}

func TestUsageAgreesWithSessionEndTotals(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}, Usage: usageBlock(10, 5, 0, 0, nil)}
	second := components.OpenResponsesResult{ID: "r2", OutputText: openrouter.String("done"), Usage: usageBlock(3, 2, 0, 0, nil)}
	manager := NewHooksManager()
	var ends []SessionEndPayload
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		ends = append(ends, p)
		return VoidResult[EmptyHookResult](), nil
	}})
	result, err := CallModel(context.Background(), twoTurnSender(first, second), CallModelInput{Model: "m", Input: "hi", Tools: []Tool{searchTool(t)}, Hooks: manager})
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Usage(context.Background())
	if len(ends) != 1 || ends[0].TotalUsage == nil {
		t.Fatalf("SessionEnd totals missing: %+v", ends)
	}
	got := *ends[0].TotalUsage
	if got.ModelCalls != usage.ModelCalls || got.TotalTokens != usage.TotalTokens || got.InputTokens != usage.InputTokens {
		t.Fatalf("SessionEnd %+v disagrees with Usage %+v", got, usage)
	}
}

func TestUsageCountsUsagelessResponsesAndOmitsCost(t *testing.T) {
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "r1", OutputText: openrouter.String("hi")})
	result, err := CallModel(context.Background(), &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}, CallModelInput{Model: "m", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Usage(context.Background())
	if usage.ModelCalls != 1 || usage.TotalTokens != 0 || usage.Cost != nil {
		t.Fatalf("usage = %+v, want 1 call, zero tokens, nil cost", usage)
	}
}

func TestUsageNeverFailsReturnsZeroedTotalsOnError(t *testing.T) {
	sender := &fakeSender{err: errors.New("api down")}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err == nil || err.Error() != "api down" {
		t.Fatalf("Text err = %v, want api down", err)
	}
	usage := result.Usage(context.Background())
	if usage.ModelCalls != 0 || usage.TotalTokens != 0 || usage.Cost != nil {
		t.Fatalf("usage = %+v, want zeroed totals", usage)
	}
}

func TestUsagePausedAtApprovalIsIdempotentAndScopedPerResult(t *testing.T) {
	risky := MustNewTool(ToolConfig[sampleInput]{Name: "risky", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "done", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_risky", Name: "risky", Arguments: `{"query":"x"}`}
	pause := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "r1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}, Usage: usageBlock(100, 50, 25, 10, f64(0.002))})
	extra := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "should-not-be-consumed", OutputText: openrouter.String("x")})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&pause, &extra}}
	accessor := &memoryStateAccessor{}
	first, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "do it", Tools: []Tool{risky}, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	u1 := first.Usage(context.Background())
	u2 := first.Usage(context.Background())
	if accessor.state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s", accessor.state.Status)
	}
	if u1.ModelCalls != 1 || u1.TotalTokens != 150 || u1.CachedTokens != 25 || u1.ReasoningTokens != 10 || u1.Cost == nil || *u1.Cost != 0.002 {
		t.Fatalf("paused usage = %+v", u1)
	}
	if u2.ModelCalls != u1.ModelCalls || u2.TotalTokens != u1.TotalTokens || sender.calls != 1 {
		t.Fatalf("repeated Usage re-drove the loop: %+v calls=%d", u2, sender.calls)
	}

	final := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "r2", OutputText: openrouter.String("ok"), Usage: usageBlock(200, 100, 0, 0, f64(0.003))})
	resumed, err := CallModel(context.Background(), &fakeSender{responses: []*operations.CreateResponsesResponse{&final}}, CallModelInput{Model: "m", Tools: []Tool{risky}, StateAccessor: accessor, ApproveToolCalls: []string{"call_risky"}})
	if err != nil {
		t.Fatal(err)
	}
	ru := resumed.Usage(context.Background())
	if ru.ModelCalls != 1 || ru.TotalTokens != 300 || ru.Cost == nil || *ru.Cost != 0.003 {
		t.Fatalf("resumed usage must report only its own generation, got %+v", ru)
	}
	if again := first.Usage(context.Background()); again.TotalTokens != 150 {
		t.Fatalf("first result's usage changed after resume: %+v", again)
	}
}

// Usage on the streaming path: the per-call usage rides on each streamed
// response.completed event and is aggregated the same way.
func TestUsageStreamingPath(t *testing.T) {
	final := completedResponse("hello")
	final.Usage = usageBlock(7, 3, 0, 0, f64(0.001))
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{
		streamingResponse(t, []components.StreamEvents{textDelta("hello", 1), completedEvent(final, 2)}),
	}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	ch, wait := result.TextStream(context.Background())
	for range ch {
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	usage := result.Usage(context.Background())
	if usage.ModelCalls != 1 || usage.TotalTokens != 10 || usage.Cost == nil || *usage.Cost != 0.001 {
		t.Fatalf("streaming usage = %+v", usage)
	}
}
