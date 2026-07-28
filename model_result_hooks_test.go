package agent

import (
	"context"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

func searchTool(t *testing.T) *TypedTool[sampleInput] {
	t.Helper()
	return MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "tool output", nil
	}})
}

func twoTurnSender(first, second components.OpenResponsesResult) *fakeSender {
	createdFirst := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	createdSecond := operations.CreateCreateResponsesResponseOpenResponsesResult(second)
	return &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFirst, &createdSecond}}
}

// TestAllowFinalResponseDefaultOnOmitted verifies upstream #68: omitting
// AllowFinalResponse behaves like bare `true` (default-on), not like `false`.
func TestAllowFinalResponseDefaultOnOmitted(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("final")}
	sender := twoTurnSender(first, second)

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{searchTool(t)},
		StopWhen: []StopCondition{StepCountIs(1)},
		// AllowFinalResponse intentionally omitted (nil).
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "final" {
		t.Fatalf("expected the default-on final turn to run, got text=%q", text)
	}
	if sender.calls != 2 {
		t.Fatalf("expected initial + forced-final request even with AllowFinalResponse omitted, got %d calls", sender.calls)
	}
}

// TestAllowFinalResponseEmptyStringSuppressesDirective verifies `”` still
// enables the final turn (tool calls forbidden) but appends no directive message.
func TestAllowFinalResponseEmptyStringSuppressesDirective(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("final")}
	sender := twoTurnSender(first, second)

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{searchTool(t)},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 {
		t.Fatalf("expected a forced-final request even for '' (calls forbidden, no message), got %d", sender.calls)
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	for _, item := range items {
		if item.EasyInputMessage != nil {
			content, ok := item.EasyInputMessage.Content.GetOrZero()
			if ok && content.Str != nil && *content.Str == DefaultFinalResponseDirective {
				t.Fatalf("'' should not append the default directive")
			}
		}
	}
}

// TestAllowFinalResponseFalseDisablesFinalTurn verifies explicit `false`
// stops the run on the halted tool-call turn with no forced final request.
func TestAllowFinalResponseFalseDisablesFinalTurn(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("should not be requested")}
	sender := twoTurnSender(first, second)

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{searchTool(t)},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("AllowFinalResponse=false must not issue a forced-final request, got %d calls", sender.calls)
	}
}

// TestAllowFinalResponseCustomStringOverridesWording verifies a non-empty
// string replaces the default directive wording.
func TestAllowFinalResponseCustomStringOverridesWording(t *testing.T) {
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("final")}
	sender := twoTurnSender(first, second)

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{searchTool(t)},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: "please summarize",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	var found bool
	for _, item := range items {
		if item.EasyInputMessage != nil {
			content, ok := item.EasyInputMessage.Content.GetOrZero()
			if ok && content.Str != nil && *content.Str == "please summarize" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected custom directive string to be appended, got %#v", items)
	}
}

// TestAwaitingClientToolsStatusForUnresolvedManualTool covers upstream #64:
// an unresolved manual tool call (no execute fn, no OnToolCalled) pauses the
// loop with the distinct `awaiting_client_tools` status, and a regular tool
// in the same turn still executes and persists its output before the pause.
func TestAwaitingClientToolsStatusForUnresolvedManualTool(t *testing.T) {
	manual := MustNewTool(ToolConfig[sampleInput]{Name: "manual_tool", Manual: true})
	regular := MustNewTool(ToolConfig[sampleInput]{Name: "auto", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	autoCall := components.OutputFunctionCallItem{CallID: "call_auto", Name: "auto", Arguments: `{"query":"x"}`}
	manualCall := components.OutputFunctionCallItem{CallID: "call_manual", Name: "manual_tool", Arguments: `{"query":"y"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(autoCall),
		components.CreateOutputItemsFunctionCall(manualCall),
	}}
	createdFirst := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFirst}}

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{manual, regular},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingClientTools {
		t.Fatalf("expected status awaiting_client_tools, got %q", state.Status)
	}
	if len(state.PendingToolCalls) != 1 || state.PendingToolCalls[0].Name != "manual_tool" {
		t.Fatalf("expected the manual call to be pending, got %+v", state.PendingToolCalls)
	}
	if countFunctionCallOutputs(state.Messages) != 1 {
		t.Fatalf("expected the regular tool's output to be persisted before the pause, messages=%+v", state.Messages)
	}
}

// TestHooksSessionAndPostModelCallLifecycle covers the required parity
// surface: SessionStart/SessionEnd fire exactly once, PostModelCall fires
// per model response with usage folded into SessionEnd's totals, and
// PreToolUse/PostToolUse fire around tool execution with a threaded session id.
func TestHooksSessionAndPostModelCallLifecycle(t *testing.T) {
	tool := searchTool(t)
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{
		ID: "resp_1", Model: "openai/test",
		Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)},
		Usage:  optionalUsage(10, 5),
	}
	second := components.OpenResponsesResult{ID: "resp_2", Model: "openai/test", OutputText: openrouter.String("done"), Usage: optionalUsage(3, 2)}
	sender := twoTurnSender(first, second)

	manager := NewHooksManager()
	var sessionStarts, sessionEnds, postModelCalls, preToolUses, postToolUses int
	var sessionEndUsageCalls int
	var sawSessionID string
	manager.OnSessionStart(HookEntry[SessionStartPayload, EmptyHookResult]{
		Handler: func(payload SessionStartPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			sessionStarts++
			sawSessionID = hctx.SessionID
			return VoidResult[EmptyHookResult](), nil
		},
	})
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{
		Handler: func(payload SessionEndPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			sessionEnds++
			if payload.TotalUsage != nil {
				sessionEndUsageCalls = payload.TotalUsage.ModelCalls
			}
			if hctx.SessionID != sawSessionID {
				t.Errorf("SessionEnd session id %q should match SessionStart's %q", hctx.SessionID, sawSessionID)
			}
			return VoidResult[EmptyHookResult](), nil
		},
	})
	manager.OnPostModelCall(HookEntry[PostModelCallPayload, EmptyHookResult]{
		Handler: func(payload PostModelCallPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			postModelCalls++
			return VoidResult[EmptyHookResult](), nil
		},
	})
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			preToolUses++
			return VoidResult[PreToolUseResult](), nil
		},
	})
	manager.OnPostToolUse(HookEntry[PostToolUsePayload, EmptyHookResult]{
		Handler: func(payload PostToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			postToolUses++
			return VoidResult[EmptyHookResult](), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true,
		Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}

	if sessionStarts != 1 {
		t.Fatalf("expected exactly one SessionStart, got %d", sessionStarts)
	}
	if sessionEnds != 1 {
		t.Fatalf("expected exactly one SessionEnd, got %d", sessionEnds)
	}
	if postModelCalls != 2 {
		t.Fatalf("expected one PostModelCall per model response (initial + final), got %d", postModelCalls)
	}
	if preToolUses != 1 || postToolUses != 1 {
		t.Fatalf("expected PreToolUse/PostToolUse to fire once around the tool execution, got pre=%d post=%d", preToolUses, postToolUses)
	}
	if sessionEndUsageCalls != 2 {
		t.Fatalf("expected SessionEnd totalUsage.modelCalls to fold both model calls, got %d", sessionEndUsageCalls)
	}
	if sawSessionID == "" {
		t.Fatalf("expected a non-empty session id to be threaded through hook emits")
	}
}

// TestHooksPreToolUseBlockSynthesizesRejection verifies a PreToolUse block
// prevents the tool from executing and synthesizes a rejected output instead.
func TestHooksPreToolUseBlockSynthesizesRejection(t *testing.T) {
	var executed bool
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		executed = true
		return "should not run", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "danger", Arguments: `{"query":"x"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	createdFirst := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFirst}}

	manager := NewHooksManager()
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			return SyncResult(PreToolUseResult{BlockReason: "not allowed"}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatalf("PreToolUse block must prevent tool execution")
	}
	if countFunctionCallOutputs(state.Messages) < 1 {
		t.Fatalf("expected at least one synthesized rejected output, messages=%+v", state.Messages)
	}
}

func optionalUsage(input, output int64) optionalnullable.OptionalNullable[components.Usage] {
	usage := components.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}
	return optionalnullable.From(&usage)
}

// TestHooksStopForceResumeContinuesLoopPastStopWhen covers upstream #7/#67's
// Stop hook: when stopWhen halts the loop mid-tool-call, a handler returning
// ForceResume must keep the run going (through CallModel, not just via a
// direct manager.EmitStop call) instead of forcing the final-response turn.
func TestHooksStopForceResumeContinuesLoopPastStopWhen(t *testing.T) {
	tool := searchTool(t)
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	sender := twoTurnSender(first, second)

	manager := NewHooksManager()
	var stopCalls int
	manager.OnStop(HookEntry[StopPayload, StopResult]{
		Handler: func(payload StopPayload, hctx LifecycleHookContext) (HookHandlerResult[StopResult], error) {
			stopCalls++
			return SyncResult(StopResult{ForceResume: true}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool},
		StopWhen: []StopCondition{StepCountIs(1)}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stopCalls == 0 {
		t.Fatalf("expected the Stop hook to fire when stopWhen halted the loop")
	}
	// ForceResume means the run must NOT take the forced-final-response path
	// (no directive message, tool executed normally, second turn is a
	// genuine follow-up model call rather than a toolChoice:none coercion).
	if text != "done" {
		t.Fatalf("expected the forced-resume turn's real response text, got %q", text)
	}
	if sender.calls != 2 {
		t.Fatalf("expected exactly two model calls (resumed past the stop), got %d", sender.calls)
	}
}

// TestHooksStopAppendPromptInjectsUserMessage verifies AppendPrompt is
// honored independently of ForceResume: the prompt lands in both state and
// the next request's input.
func TestHooksStopAppendPromptInjectsUserMessage(t *testing.T) {
	tool := searchTool(t)
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	sender := twoTurnSender(first, second)

	manager := NewHooksManager()
	manager.OnStop(HookEntry[StopPayload, StopResult]{
		Handler: func(payload StopPayload, hctx LifecycleHookContext) (HookHandlerResult[StopResult], error) {
			return SyncResult(StopResult{ForceResume: true, AppendPrompt: "please wrap up"}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool},
		StopWhen: []StopCondition{StepCountIs(1)}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range requestInputItems(sender.requests[1].Input) {
		if item.EasyInputMessage == nil {
			continue
		}
		content, ok := item.EasyInputMessage.Content.GetOrZero()
		if ok && content.Str != nil && *content.Str == "please wrap up" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected AppendPrompt message to be injected into the next request")
	}
}

// TestHooksStopForceResumeCapsConsecutiveOverrides verifies the
// maxForceResumeOverrides cap: a Stop hook that always forces resume without
// any tool ever producing output eventually stops instead of looping forever
// (bounded, additionally, by MaxTurns either way).
func TestHooksStopForceResumeCapsConsecutiveOverrides(t *testing.T) {
	manual := MustNewTool(ToolConfig[sampleInput]{Name: "manual_tool", Manual: true})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "manual_tool", Arguments: `{"query":"go"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	manager := NewHooksManager()
	manager.OnStop(HookEntry[StopPayload, StopResult]{
		Handler: func(payload StopPayload, hctx LifecycleHookContext) (HookHandlerResult[StopResult], error) {
			return SyncResult(StopResult{ForceResume: true}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{manual},
		StopWhen: []StopCondition{StepCountIs(1)}, MaxTurns: 50, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Manual tool calls never produce outputs, so forceResumeCount climbs on
	// every stop; the run must terminate well before MaxTurns=50 turns.
	if sender.calls > maxForceResumeOverrides+2 {
		t.Fatalf("expected the forceResume cap to bound the loop, got %d model calls", sender.calls)
	}
}

// TestHooksPermissionRequestAllowBypassesApprovalGate verifies a
// PermissionRequest handler returning `allow` promotes an approval-gated
// call past the human-approval pause so it executes normally this round.
func TestHooksPermissionRequestAllowBypassesApprovalGate(t *testing.T) {
	var executed bool
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		executed = true
		return "done", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "danger", Arguments: `{"query":"x"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	final := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	sender := twoTurnSender(resp, final)

	manager := NewHooksManager()
	var sawRisk RiskLevel
	manager.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{
		Handler: func(payload PermissionRequestPayload, hctx LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
			sawRisk = payload.RiskLevel
			return SyncResult(PermissionRequestResult{Decision: PermissionDecisionAllow}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatalf("PermissionRequest 'allow' should let the tool execute without pausing")
	}
	if state.Status == ConversationStatusAwaitingApproval {
		t.Fatalf("run should not pause for approval when the hook allows the call")
	}
	if sawRisk == "" {
		t.Fatalf("expected a non-empty risk level to be derived and passed to the hook")
	}
}

// TestHooksPermissionRequestDenySkipsExecutionWithoutPausing verifies a
// PermissionRequest handler returning `deny` synthesizes a rejected output
// (sent to the model this round) instead of executing the tool or pausing.
func TestHooksPermissionRequestDenySkipsExecutionWithoutPausing(t *testing.T) {
	var executed bool
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		executed = true
		return "done", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "danger", Arguments: `{"query":"x"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	final := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	sender := twoTurnSender(resp, final)

	manager := NewHooksManager()
	manager.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{
		Handler: func(payload PermissionRequestPayload, hctx LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
			return SyncResult(PermissionRequestResult{Decision: PermissionDecisionDeny, Reason: "not today"}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatalf("PermissionRequest 'deny' must prevent tool execution")
	}
	if state.Status == ConversationStatusAwaitingApproval {
		t.Fatalf("run should not pause for approval when the hook denies the call")
	}
	if countFunctionCallOutputs(state.Messages) < 1 {
		t.Fatalf("expected a synthesized rejected output to be sent, messages=%+v", state.Messages)
	}
	if sender.calls != 2 {
		t.Fatalf("expected the run to continue to a second model call carrying the denial, got %d", sender.calls)
	}
}

// TestHooksPermissionRequestAskUserFallsThroughToApproval verifies the
// default decision (ask_user, and the behavior with no HooksManager at all)
// preserves the existing human-approval pause.
func TestHooksPermissionRequestAskUserFallsThroughToApproval(t *testing.T) {
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "done", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "danger", Arguments: `{"query":"x"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	manager := NewHooksManager()
	manager.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{
		Handler: func(payload PermissionRequestPayload, hctx LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
			return SyncResult(PermissionRequestResult{Decision: PermissionDecisionAskUser}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{tool}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected ask_user to fall through to the human approval pause, got %q", state.Status)
	}
}

// TestHooksUserPromptSubmitMutatesInitialInput verifies UserPromptSubmit can
// rewrite the initial string input before the first model request is sent.
func TestHooksUserPromptSubmitMutatesInitialInput(t *testing.T) {
	resp := components.OpenResponsesResult{ID: "resp_1", OutputText: openrouter.String("done")}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}

	manager := NewHooksManager()
	var sawPrompt string
	manager.OnUserPromptSubmit(HookEntry[UserPromptSubmitPayload, UserPromptSubmitResult]{
		Handler: func(payload UserPromptSubmitPayload, hctx LifecycleHookContext) (HookHandlerResult[UserPromptSubmitResult], error) {
			sawPrompt = payload.Prompt
			mutated := "MUTATED: " + payload.Prompt
			return SyncResult(UserPromptSubmitResult{MutatedPrompt: &mutated}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "original prompt", Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawPrompt != "original prompt" {
		t.Fatalf("expected the hook to see the original prompt text, got %q", sawPrompt)
	}
	items := requestInputItems(sender.requests[0].Input)
	var found bool
	for _, item := range items {
		if item.EasyInputMessage == nil {
			continue
		}
		content, ok := item.EasyInputMessage.Content.GetOrZero()
		if ok && content.Str != nil && *content.Str == "MUTATED: original prompt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the mutated prompt to reach the model request, items=%#v", items)
	}
}

// TestHooksUserPromptSubmitRejectionAbortsRun verifies a UserPromptSubmit
// handler can reject the whole turn before any model request is sent.
func TestHooksUserPromptSubmitRejectionAbortsRun(t *testing.T) {
	sender := &fakeSender{}
	manager := NewHooksManager()
	manager.OnUserPromptSubmit(HookEntry[UserPromptSubmitPayload, UserPromptSubmitResult]{
		Handler: func(payload UserPromptSubmitPayload, hctx LifecycleHookContext) (HookHandlerResult[UserPromptSubmitResult], error) {
			return SyncResult(UserPromptSubmitResult{RejectReason: "blocked prompt"}), nil
		},
	})

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "bad prompt", Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err == nil {
		t.Fatalf("expected UserPromptSubmit rejection to surface as an error")
	}
	if sender.calls != 0 {
		t.Fatalf("expected no model request to be sent after a UserPromptSubmit rejection, got %d calls", sender.calls)
	}
}

// TestPostModelCallTurnTypeDistinguishesResumeFromInitial verifies upstream's
// `continueWithUnsentResults` telemetry tag: the model call issued right
// after an approval resume is tagged "resume", not "initial".
func TestPostModelCallTurnTypeDistinguishesResumeFromInitial(t *testing.T) {
	autoTool := MustNewTool(ToolConfig[sampleInput]{Name: "auto", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	approveTool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "done", nil
	}})
	autoCall := components.OutputFunctionCallItem{CallID: "call_auto", Name: "auto", Arguments: `{"query":"a"}`}
	dangerCall := components.OutputFunctionCallItem{CallID: "call_danger", Name: "danger", Arguments: `{"query":"b"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(autoCall),
		components.CreateOutputItemsFunctionCall(dangerCall),
	}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}

	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{autoTool, approveTool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := paused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}

	manager := NewHooksManager()
	var sawTurnTypes []ModelCallTurnType
	manager.OnPostModelCall(HookEntry[PostModelCallPayload, EmptyHookResult]{
		Handler: func(payload PostModelCallPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			sawTurnTypes = append(sawTurnTypes, payload.TurnType)
			return VoidResult[EmptyHookResult](), nil
		},
	})

	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{
		Model: "openai/test", Tools: []Tool{autoTool, approveTool}, State: &state,
		ApproveToolCalls: []string{"call_danger"}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sawTurnTypes) != 1 || sawTurnTypes[0] != ModelCallTurnTypeResume {
		t.Fatalf("expected the post-resume model call to be tagged %q, got %v", ModelCallTurnTypeResume, sawTurnTypes)
	}
}

// TestHooksSessionStartEndDoNotRefireOnApprovalResume verifies upstream's
// resume design (model-result.ts:2506-2515): an approval/HITL
// resume-with-decisions is a continuation of the same logical session, not a
// fresh one. SessionStart/SessionEnd must not fire a second time on the
// resume call — only PreToolUse/PostToolUse/PostModelCall do.
func TestHooksSessionStartEndDoNotRefireOnApprovalResume(t *testing.T) {
	autoTool := MustNewTool(ToolConfig[sampleInput]{Name: "auto", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	approveTool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "done", nil
	}})
	autoCall := components.OutputFunctionCallItem{CallID: "call_auto", Name: "auto", Arguments: `{"query":"a"}`}
	dangerCall := components.OutputFunctionCallItem{CallID: "call_danger", Name: "danger", Arguments: `{"query":"b"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(autoCall),
		components.CreateOutputItemsFunctionCall(dangerCall),
	}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}

	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{autoTool, approveTool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := paused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}

	manager := NewHooksManager()
	var sessionStarts, sessionEnds, postToolUses int
	manager.OnSessionStart(HookEntry[SessionStartPayload, EmptyHookResult]{
		Handler: func(payload SessionStartPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			sessionStarts++
			return VoidResult[EmptyHookResult](), nil
		},
	})
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{
		Handler: func(payload SessionEndPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			sessionEnds++
			return VoidResult[EmptyHookResult](), nil
		},
	})
	manager.OnPostToolUse(HookEntry[PostToolUsePayload, EmptyHookResult]{
		Handler: func(payload PostToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			postToolUses++
			return VoidResult[EmptyHookResult](), nil
		},
	})

	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{
		Model: "openai/test", Tools: []Tool{autoTool, approveTool}, State: &state,
		ApproveToolCalls: []string{"call_danger"}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}

	if sessionStarts != 0 {
		t.Fatalf("SessionStart must not re-fire on an approval-resume call, fired %d times", sessionStarts)
	}
	if sessionEnds != 0 {
		t.Fatalf("SessionEnd must not re-fire on an approval-resume call, fired %d times", sessionEnds)
	}
	if postToolUses != 1 {
		t.Fatalf("PostToolUse should still fire during the resume's tool execution, fired %d times", postToolUses)
	}
}

// memoryStateAccessor is a minimal in-memory StateAccessor test double.
type memoryStateAccessor struct {
	state ConversationState
	saves int
}

func (a *memoryStateAccessor) Load(context.Context) (*ConversationState, error) {
	s := a.state
	return &s, nil
}
func (a *memoryStateAccessor) Save(_ context.Context, s ConversationState) error {
	a.saves++
	a.state = s
	return nil
}

// TestStateAccessorPersistsWhenResumeRepauses covers a StateAccessor-backed
// approval flow where a resume itself leaves calls pending (not every
// pending call is decided in one resume). The updated state — including
// which calls already executed — must be persisted so a later CallModel
// call that reloads from the SAME accessor does not re-execute an
// already-approved, already-executed tool.
func TestStateAccessorPersistsWhenResumeRepauses(t *testing.T) {
	var d1Executions, d2Executions int
	danger1 := MustNewTool(ToolConfig[sampleInput]{Name: "danger1", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		d1Executions++
		return "d1 done", nil
	}})
	danger2 := MustNewTool(ToolConfig[sampleInput]{Name: "danger2", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		d2Executions++
		return "d2 done", nil
	}})

	call1 := components.OutputFunctionCallItem{CallID: "call_d1", Name: "danger1", Arguments: `{"query":"a"}`}
	call2 := components.OutputFunctionCallItem{CallID: "call_d2", Name: "danger2", Arguments: `{"query":"b"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(call1),
		components.CreateOutputItemsFunctionCall(call2),
	}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}

	accessor := &memoryStateAccessor{state: CreateInitialState()}
	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{danger1, danger2}, StateAccessor: accessor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paused.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.saves == 0 {
		t.Fatalf("expected the initial pause to persist state via the accessor")
	}

	// Resume, deciding only call_d1. call_d2 remains pending, so the resume
	// itself re-pauses.
	repauseSender := &fakeSender{}
	savesBeforeRepause := accessor.saves
	repaused, err := CallModel(context.Background(), repauseSender, CallModelInput{
		Model: "openai/test", Tools: []Tool{danger1, danger2}, StateAccessor: accessor,
		ApproveToolCalls: []string{"call_d1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	repausedState, err := repaused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repausedState.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected the resume to re-pause on the undecided call, got %q", repausedState.Status)
	}
	if accessor.saves == savesBeforeRepause {
		t.Fatalf("expected the re-pausing resume to persist its updated state via the accessor")
	}
	if d1Executions != 1 {
		t.Fatalf("danger1 should have executed exactly once during the first resume, got %d", d1Executions)
	}

	// A THIRD CallModel call reloads from the SAME accessor (as a caller
	// relying solely on StateAccessor would) and decides the remaining call.
	// It must not re-execute danger1.
	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	finalSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}
	final, err := CallModel(context.Background(), finalSender, CallModelInput{
		Model: "openai/test", Tools: []Tool{danger1, danger2}, StateAccessor: accessor,
		ApproveToolCalls: []string{"call_d2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := final.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d1Executions != 1 {
		t.Fatalf("danger1 must not re-execute on a later resume from the same accessor, got %d executions", d1Executions)
	}
	if d2Executions != 1 {
		t.Fatalf("danger2 should have executed exactly once, got %d", d2Executions)
	}
}
