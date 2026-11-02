package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// Ported from upstream tests/unit/get-usage.test.ts: ModelResult.Usage
// aggregates token/cost usage across every model call a run made.

func usageWithCost(input, output, cached, reasoning int64, cost *float64) optionalnullable.OptionalNullable[components.Usage] {
	u := components.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}
	u.InputTokensDetails.CachedTokens = cached
	u.OutputTokensDetails.ReasoningTokens = reasoning
	if cost != nil {
		u.Cost = optionalnullable.From(cost)
	}
	return optionalnullable.From(&u)
}

func TestUsageSumsEveryRoundOfToolLoop(t *testing.T) {
	c1, c2 := 0.001, 0.002
	first := weatherToolCallResponse("resp_1", "call_1")
	first.Usage = usageWithCost(10, 5, 2, 1, &c1)
	second := weatherToolCallResponse("resp_2", "call_2")
	second.Usage = usageWithCost(20, 7, 3, 2, &c2)
	final := textOnlyResponse("done")
	final.Usage = usageWithCost(30, 9, 4, 0, nil)
	sender := sequenceSender(first, second, final)
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{weatherTool(t, false)}, StopWhen: []StopCondition{StepCountIs(5)}})
	if err != nil {
		t.Fatal(err)
	}
	// Called without awaiting Response first: Usage gates on run completion.
	usage, err := result.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.ModelCalls != 3 || usage.InputTokens != 60 || usage.OutputTokens != 21 || usage.TotalTokens != 81 || usage.CachedTokens != 9 || usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Cost == nil || *usage.Cost < 0.0029 || *usage.Cost > 0.0031 {
		t.Fatalf("cost = %v", usage.Cost)
	}
	// Response is only the final round; Usage is the aggregate.
	resp, _ := result.Response(context.Background())
	if u, _ := resp.Usage.GetOrZero(); u.InputTokens != 30 {
		t.Fatalf("final response usage = %+v", u)
	}
}

func TestUsageAgreesWithSessionEndTotals(t *testing.T) {
	first := weatherToolCallResponse("resp_1", "call_1")
	first.Usage = optionalUsage(10, 5)
	final := textOnlyResponse("done")
	final.Usage = optionalUsage(3, 4)
	manager := NewHooksManager()
	var mu sync.Mutex
	var fromHook *SessionUsageTotals
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		mu.Lock()
		fromHook = p.TotalUsage
		mu.Unlock()
		return VoidResult[EmptyHookResult](), nil
	}})
	result, err := CallModel(context.Background(), sequenceSender(first, final), CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{weatherTool(t, false)}, Hooks: manager})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := result.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fromHook == nil || *fromHook != usage {
		t.Fatalf("SessionEnd totals %+v != Usage %+v", fromHook, usage)
	}
}

func TestUsageCountsUsagelessResponsesAndOmitsCost(t *testing.T) {
	result, err := CallModel(context.Background(), sequenceSender(textOnlyResponse("r1")), CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	usage, err := result.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.ModelCalls != 1 || usage.TotalTokens != 0 || usage.Cost != nil {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestUsageReturnsZeroedTotalsAndCauseWhenNoCallCompleted(t *testing.T) {
	apiDown := errors.New("api down")
	result, err := CallModel(context.Background(), &fakeSender{err: apiDown}, CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, apiDown) {
		t.Fatalf("Text err = %v", err)
	}
	usage, err := result.Usage(context.Background())
	// Totals are always returned; the failure is reported, not swallowed.
	if !errors.Is(err, apiDown) {
		t.Fatalf("Usage must report the run failure, got %v", err)
	}
	if usage.ModelCalls != 0 || usage.TotalTokens != 0 || usage.Cost != nil {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestUsageWhilePausedAndScopedPerResultAcrossResume(t *testing.T) {
	first := weatherToolCallResponse("resp_1", "call_1")
	first.Usage = optionalUsage(10, 5)
	tool := weatherTool(t, true)
	pauseSender := sequenceSender(first)
	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent while paused, drives no further requests
		usage, err := paused.Usage(context.Background())
		if err != nil || usage.ModelCalls != 1 || usage.TotalTokens != 15 {
			t.Fatalf("paused usage = %+v err=%v", usage, err)
		}
	}
	if pauseSender.calls != 1 {
		t.Fatalf("reading usage must not drive more requests: %d", pauseSender.calls)
	}
	state, _ := paused.State(context.Background())
	final := textOnlyResponse("22 degrees")
	final.Usage = optionalUsage(7, 1)
	resumed, err := CallModel(context.Background(), sequenceSender(final), CallModelInput{Model: "test-model", Tools: []Tool{tool}, State: &state, ApproveToolCalls: []string{"call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := resumed.Usage(context.Background())
	if err != nil || usage.ModelCalls != 1 || usage.TotalTokens != 8 {
		t.Fatalf("resumed usage must be scoped to its own ModelResult: %+v err=%v", usage, err)
	}
}

func TestUsageCountsStreamingResponses(t *testing.T) {
	resp := completedResponse("hello")
	resp.Usage = optionalUsage(4, 6)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{streamingResponse(t, []components.StreamEvents{textDelta("hello", 1), completedEvent(resp, 2)})}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := result.Usage(context.Background())
	if err != nil || usage.ModelCalls != 1 || usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v err=%v", usage, err)
	}
}

func TestUsageHonorsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	result, err := CallModel(context.Background(), blockingSender{block: block}, CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	usage, err := result.Usage(ctx)
	if !errors.Is(err, context.Canceled) || usage.ModelCalls != 0 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	result.Cancel()
}

// blockingSender blocks until block closes or the request ctx ends.
type blockingSender struct{ block chan struct{} }

func (b blockingSender) SendResponse(ctx context.Context, _ components.ResponsesRequest, _ *components.MetadataLevel, _ ...operations.Option) (*operations.CreateResponsesResponse, error) {
	select {
	case <-b.block:
	case <-ctx.Done():
	}
	return nil, context.Canceled
}
