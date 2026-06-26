package agent

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

func TestStopConditionsUseUpstreamSemantics(t *testing.T) {
	ctx := context.Background()
	cost := 0.25
	steps := []StepResult{{
		ToolCalls:    []ParsedToolCall{{Name: "search"}},
		Usage:        &components.Usage{InputTokens: 3, OutputTokens: 3, TotalTokens: 5, Cost: optionalnullable.From(&cost)},
		FinishReason: "tool_calls",
	}}

	ok, err := MaxTokensUsed(11)(ctx, steps)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("MaxTokensUsed must use total_tokens only, not input+output+total")
	}

	ok, err = IsStopConditionMet(ctx, []StopCondition{StepCountIs(2), HasToolCall("search")}, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("stop conditions should be ORed")
	}

	ok, err = MaxCost(0.25)(ctx, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected max cost to trigger at threshold")
	}
}
