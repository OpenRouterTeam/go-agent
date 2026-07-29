package agent

// Live end-to-end tests against the real OpenRouter API, mirroring upstream
// typescript-agent's packages/agent/tests/e2e coverage: streaming, a real
// tool round, approval pause/resume across two CallModel invocations,
// lifecycle hooks firing on live traffic, and conversation-state
// serialization surviving a round trip.
//
// Skipped without OPENROUTER_API_KEY. These assert behavior (a tool ran, a
// hook fired, state advanced), never model quality, so prompts pin outputs
// as hard as possible. Model overridable via OPENROUTER_E2E_MODEL.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func e2eModel() string {
	if m := os.Getenv("OPENROUTER_E2E_MODEL"); m != "" {
		return m
	}
	return "anthropic/claude-haiku-4.5"
}

func e2eSetup(t *testing.T) (context.Context, *OpenRouter) {
	t.Helper()
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY is not set")
	}
	// Double gate: a plain `go test ./...` with a key exported must not spend
	// money by surprise. CI's e2e job sets this explicitly.
	if os.Getenv("OPENROUTER_AGENT_E2E") != "1" {
		t.Skip("set OPENROUTER_AGENT_E2E=1 to run live OpenRouter e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx, NewOpenRouter(OpenRouterOptions{})
}

// requiredToolChoice pins the first turn to a tool call so model
// nondeterminism can't skip an approval pause the test is asserting.
func requiredToolChoice() *components.OpenAIResponsesToolChoiceUnion {
	choice := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceRequired(
		components.OpenAIResponsesToolChoiceRequiredRequired,
	)
	return &choice
}

type e2eToolInput struct {
	Text string `json:"text,omitempty"`
	ID   int    `json:"id,omitempty"`
}

type e2eMemoryState struct {
	current *ConversationState
	saves   int
}

func (a *e2eMemoryState) Load(context.Context) (*ConversationState, error) {
	return a.current, nil
}

func (a *e2eMemoryState) Save(_ context.Context, s ConversationState) error {
	a.saves++
	a.current = &s
	return nil
}

func TestE2ETextAndStreamAgree(t *testing.T) {
	ctx, client := e2eSetup(t)

	result, err := CallModel(ctx, client, CallModelInput{
		Model: e2eModel(),
		Input: "Reply with exactly the word: pong",
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, wait := result.TextStream(ctx)
	var chunks []string
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("expected pong in response, got %q", text)
	}
	if joined := strings.Join(chunks, ""); joined != text {
		t.Fatalf("stream chunks %q != final text %q", joined, text)
	}
}

func TestE2EToolLoopExecutesAndFeedsResultBack(t *testing.T) {
	ctx, client := e2eSetup(t)

	var calls int
	secretTool := MustNewTool(ToolConfig[e2eToolInput]{
		Name:        "get_secret",
		Description: "Returns the secret code. Call this to answer any question about the secret code.",
		Execute: func(context.Context, e2eToolInput, ToolExecuteContext) (any, error) {
			calls++
			return map[string]any{"secret": "BANANA-42"}, nil
		},
	})

	result, err := CallModel(ctx, client, CallModelInput{
		Model: e2eModel(),
		Input: "What is the secret code? Use the get_secret tool, then repeat the code back verbatim.",
		Tools: []Tool{secretTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if calls == 0 {
		t.Fatal("model never called the tool")
	}
	if !strings.Contains(text, "BANANA-42") {
		t.Fatalf("tool output did not reach the final answer: %q", text)
	}
}

func TestE2EApprovalPauseAndResumeAcrossCalls(t *testing.T) {
	ctx, client := e2eSetup(t)

	var executed int
	deleteTool := MustNewTool(ToolConfig[e2eToolInput]{
		Name:            "delete_record",
		Description:     "Deletes the record. Requires approval.",
		RequireApproval: true,
		Execute: func(context.Context, e2eToolInput, ToolExecuteContext) (any, error) {
			executed++
			return map[string]any{"deleted": true}, nil
		},
	})

	accessor := &e2eMemoryState{}
	first, err := CallModel(ctx, client, CallModelInput{
		Model:         e2eModel(),
		Input:         "Delete the record with id 7 using the delete_record tool.",
		Tools:         []Tool{deleteTool},
		StateAccessor: accessor,
		Request:       components.ResponsesRequest{ToolChoice: requiredToolChoice()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Response(ctx); err != nil {
		t.Fatal(err)
	}

	if accessor.current == nil {
		t.Fatal("no state was saved")
	}
	if accessor.current.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", accessor.current.Status)
	}
	if executed != 0 {
		t.Fatal("tool must not run before approval")
	}

	pending, err := first.PendingToolCalls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending call, got %d", len(pending))
	}

	resumed, err := CallModel(ctx, client, CallModelInput{
		Model:            e2eModel(),
		Tools:            []Tool{deleteTool},
		StateAccessor:    accessor,
		ApproveToolCalls: []string{pending[0].CallID},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := resumed.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if executed != 1 {
		t.Fatalf("approved tool executed %d times, want 1", executed)
	}
	if accessor.current.Status != ConversationStatusComplete {
		t.Fatalf("expected complete, got %s", accessor.current.Status)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("expected a non-empty final answer after resume")
	}
}

func TestE2EHooksFireOnRealTraffic(t *testing.T) {
	ctx, client := e2eSetup(t)

	var fired []string
	var totals *SessionUsageTotals

	manager := NewHooksManager()
	manager.OnSessionStart(HookEntry[SessionStartPayload, EmptyHookResult]{
		Handler: func(_ SessionStartPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			fired = append(fired, "SessionStart")
			return HookHandlerResult[EmptyHookResult]{}, nil
		},
	})
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(_ PreToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			fired = append(fired, "PreToolUse")
			return HookHandlerResult[PreToolUseResult]{}, nil
		},
	})
	manager.OnPostToolUse(HookEntry[PostToolUsePayload, EmptyHookResult]{
		Handler: func(_ PostToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			fired = append(fired, "PostToolUse")
			return HookHandlerResult[EmptyHookResult]{}, nil
		},
	})
	manager.OnPostModelCall(HookEntry[PostModelCallPayload, EmptyHookResult]{
		Handler: func(_ PostModelCallPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			fired = append(fired, "PostModelCall")
			return HookHandlerResult[EmptyHookResult]{}, nil
		},
	})
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{
		Handler: func(payload SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			fired = append(fired, "SessionEnd")
			totals = payload.TotalUsage
			return HookHandlerResult[EmptyHookResult]{}, nil
		},
	})

	echoTool := MustNewTool(ToolConfig[e2eToolInput]{
		Name:        "echo",
		Description: "Echoes back the given text.",
		Execute: func(_ context.Context, in e2eToolInput, _ ToolExecuteContext) (any, error) {
			return map[string]any{"echoed": in.Text}, nil
		},
	})

	result, err := CallModel(ctx, client, CallModelInput{
		Model: e2eModel(),
		Input: "Use the echo tool with text 'hi', then say done.",
		Tools: []Tool{echoTool},
		Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}

	if len(fired) == 0 || fired[0] != "SessionStart" {
		t.Fatalf("SessionStart must fire first, got %v", fired)
	}
	if fired[len(fired)-1] != "SessionEnd" {
		t.Fatalf("SessionEnd must fire last, got %v", fired)
	}
	for _, want := range []string{"PreToolUse", "PostToolUse", "PostModelCall"} {
		found := false
		for _, name := range fired {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("hook %s never fired: %v", want, fired)
		}
	}
	// SessionEnd carries aggregated real usage — a live call must cost tokens.
	if totals == nil || totals.TotalTokens <= 0 || totals.ModelCalls <= 0 {
		t.Fatalf("SessionEnd usage totals empty: %+v", totals)
	}
}

func TestE2EStateSerializationRoundTripResumes(t *testing.T) {
	ctx, client := e2eSetup(t)

	var executed int
	launchTool := MustNewTool(ToolConfig[e2eToolInput]{
		Name:            "launch",
		Description:     "Launches the rocket. Requires approval.",
		RequireApproval: true,
		Execute: func(context.Context, e2eToolInput, ToolExecuteContext) (any, error) {
			executed++
			return map[string]any{"launched": true}, nil
		},
	})

	accessor := &e2eMemoryState{}
	first, err := CallModel(ctx, client, CallModelInput{
		Model:         e2eModel(),
		Input:         "Launch the rocket using the launch tool.",
		Tools:         []Tool{launchTool},
		StateAccessor: accessor,
		Request:       components.ResponsesRequest{ToolChoice: requiredToolChoice()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Response(ctx); err != nil {
		t.Fatal(err)
	}
	if accessor.current == nil || accessor.current.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval pause, got %+v", accessor.current)
	}
	pending, err := first.PendingToolCalls(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending call, got %d (err %v)", len(pending), err)
	}

	// Round-trip through the wire format, as a durable store would.
	raw, err := SerializeConversationState(*accessor.current)
	if err != nil {
		t.Fatal(err)
	}
	restoredState, err := DeserializeConversationState(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored := &e2eMemoryState{current: &restoredState}

	resumed, err := CallModel(ctx, client, CallModelInput{
		Model:            e2eModel(),
		Tools:            []Tool{launchTool},
		StateAccessor:    restored,
		ApproveToolCalls: []string{pending[0].CallID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(ctx); err != nil {
		t.Fatal(err)
	}

	if executed != 1 {
		t.Fatalf("approved tool executed %d times after round-trip, want 1", executed)
	}
	if restored.current.Status != ConversationStatusComplete {
		t.Fatalf("expected complete, got %s", restored.current.Status)
	}
}
