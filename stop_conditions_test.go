package agent

import (
	"context"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
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

// TestFinishReasonIsMatchesStepFinishReason covers FinishReasonIs, which is in
// the contract's required public API but previously had no test at all: it was
// exported (so the verifier's presence check passed) and never exercised.
//
// The values it must match are whatever run() puts in StepResult.FinishReason,
// which comes from string(resp.Status) -- so this asserts against a real SDK
// status string rather than an invented token.
func TestFinishReasonIsMatchesStepFinishReason(t *testing.T) {
	ctx := context.Background()
	completed := string(components.OpenAIResponsesResponseStatusCompleted)
	steps := []StepResult{{FinishReason: completed}}

	ok, err := FinishReasonIs(completed)(ctx, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("FinishReasonIs(%q) should match a step with that finish reason", completed)
	}

	ok, err = FinishReasonIs("incomplete")(ctx, steps)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("FinishReasonIs must not match a different finish reason")
	}

	// Matches if ANY step carries the reason, not only the last one.
	multi := []StepResult{{FinishReason: "in_progress"}, {FinishReason: completed}}
	ok, err = FinishReasonIs(completed)(ctx, multi)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("FinishReasonIs should match when any step has the finish reason")
	}

	// No steps means nothing to match.
	ok, err = FinishReasonIs(completed)(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("FinishReasonIs must be false for an empty step list")
	}
}

// TestFinishReasonIsObservesRealRunStatus ties FinishReasonIs to what the loop
// actually records, so the constant it matches can't drift from run()'s value
// without a test failing.
func TestFinishReasonIsObservesRealRunStatus(t *testing.T) {
	ctx := context.Background()
	resp := components.OpenResponsesResult{
		ID:         "resp_1",
		Status:     components.OpenAIResponsesResponseStatusCompleted,
		OutputText: openrouter.String("done"),
	}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}

	// m.steps is unexported with no public accessor; these tests are in-package,
	// and Text() above already waited for the run to finish, so reading it here
	// is safe and needs no production change.
	steps := result.steps
	if len(steps) == 0 {
		t.Fatal("expected at least one step")
	}
	ok, err := FinishReasonIs(string(components.OpenAIResponsesResponseStatusCompleted))(ctx, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("FinishReasonIs should match the status the loop recorded, got %q", steps[len(steps)-1].FinishReason)
	}
}
