package agent

// Ports tests/unit/hitl-tool.test.ts #91 cases: the executed tool call is
// threaded into the execute context (ToolExecuteContext.Turn.ToolCall) even
// though streaming-loop turn contexts carry none, and it is not fabricated
// for OnResponseReceived.

import (
	"context"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type htAmount struct {
	Amount float64 `json:"amount"`
}

var htAmountSchema = map[string]any{"type": "object", "required": []any{"amount"}, "properties": map[string]any{"amount": map[string]any{"type": "number"}}}

// htSeenCall is what a tool observed about its own call.
type htSeenCall struct {
	turnCall *components.OutputFunctionCallItem
	callID   string
}

func htRecord(rec *phCounter, execCtx ToolExecuteContext) {
	var copied *components.OutputFunctionCallItem
	if execCtx.Turn.ToolCall != nil {
		c := *execCtx.Turn.ToolCall
		copied = &c
	}
	rec.add(htSeenCall{turnCall: copied, callID: execCtx.ToolCall.CallID})
}

func htAssertThreaded(t *testing.T, label string, rec *phCounter, callID, name, args string) {
	t.Helper()
	seen := rec.values()
	if len(seen) != 1 {
		t.Fatalf("%s: tool observed %d calls", label, len(seen))
	}
	s := seen[0].(htSeenCall)
	if s.turnCall == nil {
		t.Fatalf("%s: Turn.ToolCall must carry the executed call", label)
	}
	if s.turnCall.CallID != callID || s.turnCall.Name != name || phCanonicalJSON(t, s.turnCall.Arguments) != phCanonicalJSON(t, args) || s.turnCall.Type != components.OutputFunctionCallItemTypeFunctionCall {
		t.Fatalf("%s: Turn.ToolCall = %s, want call_id=%s name=%s args=%s", label, phJSON(t, s.turnCall), callID, name, args)
	}
	if s.callID != callID {
		t.Fatalf("%s: ToolCall.CallID = %q", label, s.callID)
	}
}

// upstream: "threads the executed tool call into execute context for regular
// tools too" — through the real streaming loop.
func TestExecutedToolCallThreadedInStreamingLoop(t *testing.T) {
	ctx := context.Background()
	var rec phCounter
	tool := MustNewTool(ToolConfig[htAmount]{
		Name: "regular", InputSchema: htAmountSchema,
		Execute: func(_ context.Context, _ htAmount, execCtx ToolExecuteContext) (any, error) {
			htRecord(&rec, execCtx)
			return map[string]any{"y": 1}, nil
		},
	})
	first := completedResponse("")
	first.OutputText = nil
	first.ID = "resp_stream_1"
	first.Output = []components.OutputItems{phFnCall("call_reg_1", "regular", `{"amount":2}`)}
	second := completedResponse("done")
	sender := &fakeSender{responses: append(
		streamingResponsesOf(t, []components.StreamEvents{completedEvent(first, 1)}),
		streamingResponsesOf(t, []components.StreamEvents{textDelta("done", 1), completedEvent(second, 2)})...,
	)}
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(ctx); err != nil || text != "done" {
		t.Fatalf("text = %q, %v", text, err)
	}
	htAssertThreaded(t, "streaming loop", &rec, "call_reg_1", "regular", `{"amount":2}`)
}

// upstream: "threads the executed tool call into the hook context (streaming
// turn contexts omit it)" — OnToolCalled sees it, both in a turn and on an
// approved resume; a PreToolUse mutation is reflected.
func TestExecutedToolCallThreadedIntoOnToolCalled(t *testing.T) {
	var rec phCounter
	hitl := MustNewTool(ToolConfig[htAmount]{
		Name: "approve", InputSchema: htAmountSchema,
		OnToolCalled: func(_ context.Context, _ htAmount, execCtx ToolExecuteContext) (any, bool, error) {
			htRecord(&rec, execCtx)
			return nil, false, nil
		},
	})
	if _, err := mustRun(t, phSender(phResp("r1", phFnCall("call_stream_1", "approve", `{"amount":5}`))), CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{hitl}}); err != nil {
		t.Fatal(err)
	}
	htAssertThreaded(t, "loop turn", &rec, "call_stream_1", "approve", `{"amount":5}`)

	t.Run("approved on resume, after a PreToolUse mutation", func(t *testing.T) {
		var rec2 phCounter
		gated := MustNewTool(ToolConfig[htAmount]{
			Name: "approve", InputSchema: htAmountSchema, RequireApproval: true,
			OnToolCalled: func(_ context.Context, _ htAmount, execCtx ToolExecuteContext) (any, bool, error) {
				htRecord(&rec2, execCtx)
				return map[string]any{"ok": true}, true, nil
			},
		})
		mem := &phMemState{}
		if _, err := mustRun(t, phSender(phResp("r1", phFnCall("call_gated", "approve", `{"amount":5}`))), CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{gated}, StateAccessor: mem}); err != nil {
			t.Fatal(err)
		}
		hooks := NewHooksManager()
		agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
			return PreToolUseResult{MutatedInput: map[string]any{"amount": 7}}
		})
		if _, err := mustRun(t, phSender(phTextResp("r2", "ok")), CallModelInput{Model: "openai/test", Tools: []Tool{gated}, Hooks: hooks, StateAccessor: mem, ApproveToolCalls: []string{"call_gated"}}); err != nil {
			t.Fatal(err)
		}
		htAssertThreaded(t, "approved resume", &rec2, "call_gated", "approve", `{"amount":7}`)
	})
}

// upstream: "prefers a caller-provided turn-context toolCall over the
// synthesized one"
func TestCallerProvidedTurnToolCallWins(t *testing.T) {
	orchestrator := &components.OutputFunctionCallItem{CallID: "call_rich_1", Name: "approve", Arguments: `{"amount":5}`, Type: components.OutputFunctionCallItemTypeFunctionCall}
	execCtx := BuildToolExecuteContext(ParsedToolCall{ID: "call_rich_1", CallID: "call_rich_1", Name: "approve", Arguments: map[string]any{"amount": 5}}, TurnContext{NumberOfTurns: 1, ToolCall: orchestrator}, nil, nil)
	if execCtx.Turn.ToolCall != orchestrator {
		t.Fatalf("a caller-provided ToolCall must be kept as-is, got %s", phJSON(t, execCtx.Turn.ToolCall))
	}
	synth := BuildToolExecuteContext(ParsedToolCall{ID: "call_x", CallID: "call_x", Name: "approve", RawArgs: `{"amount":1}`}, TurnContext{NumberOfTurns: 1}, nil, nil)
	if synth.Turn.ToolCall == nil || synth.Turn.ToolCall.CallID != "call_x" || synth.Turn.ToolCall.ID == nil || *synth.Turn.ToolCall.ID != "call_x" || synth.Turn.ToolCall.Arguments != `{"amount":1}` {
		t.Fatalf("synthesized ToolCall = %s", phJSON(t, synth.Turn.ToolCall))
	}
	if none := BuildToolExecuteContext(ParsedToolCall{}, TurnContext{}, nil, nil); none.Turn.ToolCall != nil {
		t.Fatal("an empty call must not fabricate a ToolCall")
	}
}

// upstream: "does not synthesize a toolCall for onResponseReceived (only the
// output item is in scope)"
func TestOnResponseReceivedGetsNoFabricatedToolCall(t *testing.T) {
	var rec phCounter
	hitl := MustNewTool(ToolConfig[htAmount]{
		Name: "approve", InputSchema: htAmountSchema,
		OnToolCalled: func(context.Context, htAmount, ToolExecuteContext) (any, bool, error) { return nil, false, nil },
		OnResponseReceived: func(_ context.Context, raw any, execCtx ToolExecuteContext) (any, error) {
			htRecord(&rec, execCtx)
			return raw, nil
		},
	})
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("r1", phFnCall("c1", "approve", `{"amount":5}`))), CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{hitl}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if _, err := mustRun(t, phSender(phTextResp("r2", "ok")), CallModelInput{Model: "openai/test", Tools: []Tool{hitl}, StateAccessor: mem, Input: []components.InputsUnion1{agdCallerOutput("c1", `{"ok":true}`)}}); err != nil {
		t.Fatal(err)
	}
	seen := rec.values()
	if len(seen) != 1 {
		t.Fatalf("OnResponseReceived ran %d times", len(seen))
	}
	if s := seen[0].(htSeenCall); s.turnCall != nil || s.callID != "c1" {
		t.Fatalf("OnResponseReceived must not get a fabricated Turn.ToolCall (got %s), but knows its call id (%q)", phJSON(t, s.turnCall), s.callID)
	}
}
