package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// Ported from upstream tests/unit/approval-gate-regressions.test.ts (#94/#54).

// deployInput defaults Environment to "production" when omitted — the Go
// analogue of a Zod `.default(...)`: Execute decodes through UnmarshalJSON,
// so approval predicates must see the same defaulted value.
type deployInput struct {
	Environment string `json:"environment"`
}

func (d *deployInput) UnmarshalJSON(b []byte) error {
	type alias deployInput
	a := alias{Environment: "production"}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*d = deployInput(a)
	return nil
}

func singleCallSender(name, callID, args string) *fakeSender {
	call := components.OutputFunctionCallItem{CallID: callID, Name: name, Arguments: args}
	first := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	final := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")})
	return &fakeSender{responses: []*operations.CreateResponsesResponse{&first, &final}}
}

func TestApprovalPredicateSeesDecodedDefaults(t *testing.T) {
	var seen any
	executed := 0
	deploy := MustNewTool(ToolConfig[deployInput]{
		Name: "deploy",
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			seen = call.Arguments
			args, _ := call.Arguments.(map[string]any)
			return args["environment"] == "production", nil
		},
		Execute: func(context.Context, deployInput, ToolExecuteContext) (any, error) {
			executed++
			return "deployed", nil
		},
	})
	result, err := CallModel(context.Background(), singleCallSender("deploy", "call_deploy", `{}`), CallModelInput{Model: "m", Input: "deploy", Tools: []Tool{deploy}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, map[string]any{"environment": "production"}) {
		t.Fatalf("predicate saw %#v, want the decoded default {environment: production}", seen)
	}
	if executed != 0 || state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("executed=%d status=%s, want the defaulted call gated", executed, state.Status)
	}
	// The executable call itself is not rewritten by normalization.
	if len(state.PendingToolCalls) != 1 || state.PendingToolCalls[0].RawArgs != `{}` {
		t.Fatalf("pending call was mutated: %#v", state.PendingToolCalls)
	}
}

func TestApprovalPredicateFailsClosedOnSchemaInvalidArguments(t *testing.T) {
	called := false
	deploy := MustNewTool(ToolConfig[deployInput]{
		Name: "deploy",
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
			called = true
			return false, nil
		},
		Execute: func(context.Context, deployInput, ToolExecuteContext) (any, error) { return "x", nil },
	})
	result, err := CallModel(context.Background(), singleCallSender("deploy", "c1", `{"environment":5}`), CallModelInput{Model: "m", Input: "x", Tools: []Tool{deploy}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := result.State(context.Background())
	if called {
		t.Fatal("predicate must not be called with untrustworthy (schema-invalid) arguments")
	}
	if state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s, want fail-closed awaiting_approval", state.Status)
	}
}

func TestCallLevelCheckHonoredWithRawArgsWhenInvalidAndNormalizedWhenValid(t *testing.T) {
	var seen []any
	executed := 0
	deploy := MustNewTool(ToolConfig[deployInput]{Name: "deploy", Execute: func(context.Context, deployInput, ToolExecuteContext) (any, error) {
		executed++
		return "x", nil
	}})
	check := func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
		seen = append(seen, call.Arguments)
		return false, nil
	}
	// Invalid: call-level check still runs (unconditional) with raw args.
	result, err := CallModel(context.Background(), singleCallSender("deploy", "c1", `{"environment":5}`), CallModelInput{Model: "m", Input: "x", Tools: []Tool{deploy}, Approval: check})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !reflect.DeepEqual(seen[0], map[string]any{"environment": float64(5)}) {
		t.Fatalf("call-level check should see raw args on parse failure, saw %#v", seen)
	}
	// Valid: normalized copy with the default applied.
	seen = nil
	result, err = CallModel(context.Background(), singleCallSender("deploy", "c2", `{}`), CallModelInput{Model: "m", Input: "x", Tools: []Tool{deploy}, Approval: check})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !reflect.DeepEqual(seen[0], map[string]any{"environment": "production"}) {
		t.Fatalf("call-level check should see normalized args, saw %#v", seen)
	}
}

type guardedInput struct {
	Dangerous bool `json:"dangerous"`
}

// A PreToolUse hook must not be able to rewrite safe, auto-approved
// arguments into an unapproved dangerous call: the mutated input is re-gated,
// the run pauses, and the pending call carries the post-hook arguments with
// the PreToolUseApplied marker so resume does not apply the hook twice.
func TestPreToolUseMutationIsReGatedAndNotReappliedOnResume(t *testing.T) {
	var mu sync.Mutex
	var predicateSaw []any
	var executedWith []guardedInput
	preToolUses := 0
	guarded := MustNewTool(ToolConfig[guardedInput]{
		Name: "guarded_action",
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			predicateSaw = append(predicateSaw, call.Arguments)
			args, _ := call.Arguments.(map[string]any)
			return args["dangerous"] == true, nil
		},
		Execute: func(_ context.Context, in guardedInput, _ ToolExecuteContext) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			executedWith = append(executedWith, in)
			return map[string]any{"ok": true}, nil
		},
	})
	manager := NewHooksManager()
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		mu.Lock()
		preToolUses++
		mu.Unlock()
		return SyncResult(PreToolUseResult{MutatedInput: map[string]any{"dangerous": true}}), nil
	}})
	accessor := &memoryStateAccessor{}
	result, err := CallModel(context.Background(), singleCallSender("guarded_action", "call_guarded", `{"dangerous":false}`), CallModelInput{Model: "m", Input: "go", Tools: []Tool{guarded}, Hooks: manager, StateAccessor: accessor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executedWith) != 0 {
		t.Fatalf("hook-mutated dangerous call executed without approval: %#v", executedWith)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s, want awaiting_approval", accessor.state.Status)
	}
	if n := len(predicateSaw); n != 2 || !reflect.DeepEqual(predicateSaw[1], map[string]any{"dangerous": true}) {
		t.Fatalf("predicate must re-check the mutated input, saw %#v", predicateSaw)
	}
	pending := accessor.state.PendingToolCalls
	if len(pending) != 1 || !pending[0].PreToolUseApplied || !reflect.DeepEqual(pending[0].Arguments, map[string]any{"dangerous": true}) {
		t.Fatalf("pending = %#v, want post-hook args + PreToolUseApplied", pending)
	}
	// The marker survives serialization.
	blob, err := SerializeConversationState(accessor.state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.PendingToolCalls[0].PreToolUseApplied {
		t.Fatal("PreToolUseApplied lost across serialize/deserialize")
	}
	accessor.state = restored

	final := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_3", OutputText: openrouter.String("ok")})
	resumed, err := CallModel(context.Background(), &fakeSender{responses: []*operations.CreateResponsesResponse{&final}}, CallModelInput{Model: "m", Tools: []Tool{guarded}, Hooks: manager, StateAccessor: accessor, ApproveToolCalls: []string{"call_guarded"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executedWith) != 1 || !executedWith[0].Dangerous {
		t.Fatalf("approved call should execute once with the post-hook args, got %#v", executedWith)
	}
	if preToolUses != 1 {
		t.Fatalf("PreToolUse ran %d times, want 1 (not re-applied on resume)", preToolUses)
	}
}

func TestPreToolUseInvalidMutationIsBlocked(t *testing.T) {
	executed := 0
	failures := 0
	tool := MustNewTool(ToolConfig[guardedInput]{Name: "guarded_action", Execute: func(context.Context, guardedInput, ToolExecuteContext) (any, error) {
		executed++
		return "x", nil
	}})
	manager := NewHooksManager()
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		return SyncResult(PreToolUseResult{MutatedInput: map[string]any{"dangerous": "yes"}}), nil
	}})
	manager.OnPostToolUseFailure(HookEntry[PostToolUseFailurePayload, EmptyHookResult]{Handler: func(PostToolUseFailurePayload, LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		failures++
		return VoidResult[EmptyHookResult](), nil
	}})
	sender := singleCallSender("guarded_action", "c1", `{"dangerous":false}`)
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go", Tools: []Tool{tool}, Hooks: manager})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed != 0 || failures != 1 {
		t.Fatalf("executed=%d failures=%d, want invalid hook input blocked with one PostToolUseFailure", executed, failures)
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	out := items[len(items)-1].FunctionCallOutputItem
	if out == nil || out.Output.Str == nil || *out.Output.Str != `{"error":"PreToolUse produced invalid input for \"guarded_action\""}` {
		t.Fatalf("blocked output = %#v", items[len(items)-1])
	}
}

// allowFinalResponse path enforces the approval gate: a requireApproval tool
// is not executed when stopWhen halts the loop; non-gated calls still run.
func TestAllowFinalResponsePathEnforcesApprovalGate(t *testing.T) {
	safeRuns, dangerRuns := 0, 0
	safe := MustNewTool(ToolConfig[sampleInput]{Name: "safe", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		safeRuns++
		return map[string]any{"ok": true}, nil
	}})
	danger := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		dangerRuns++
		return map[string]any{"ok": true}, nil
	}})
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "c_safe", Name: "safe", Arguments: `{"query":"a"}`}),
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "c_danger", Name: "danger", Arguments: `{"query":"b"}`}),
	}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go", Tools: []Tool{safe, danger}, StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dangerRuns != 0 || safeRuns != 1 {
		t.Fatalf("danger=%d safe=%d, want gated call skipped and safe call run", dangerRuns, safeRuns)
	}
	if state.Status != ConversationStatusAwaitingApproval || len(state.PendingToolCalls) != 1 || state.PendingToolCalls[0].CallID != "c_danger" {
		t.Fatalf("state = %s pending=%#v", state.Status, state.PendingToolCalls)
	}
	if sender.calls != 1 {
		t.Fatalf("no final request may be sent while a call awaits approval, got %d", sender.calls)
	}
}

// Occurrence isolation across responses (the property upstream's per-response
// approval-gate keys provide): a PreToolUse block recorded for one response's
// call must not leak onto a later response that reuses the same call id.
func TestPreparedBlockDoesNotLeakAcrossResponsesWithSameCallID(t *testing.T) {
	executed := 0
	hookCalls := 0
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		executed++
		return "ok", nil
	}})
	manager := NewHooksManager()
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		hookCalls++
		if hookCalls == 1 {
			return SyncResult(PreToolUseResult{BlockReason: "not yet"}), nil
		}
		return VoidResult[PreToolUseResult](), nil
	}})
	call := components.OutputFunctionCallItem{CallID: "c1", Name: "search", Arguments: `{"query":"go"}`}
	r1 := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	r2 := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_2", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&r1, &r2, plainTextResponse("done")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go", Tools: []Tool{tool}, Hooks: manager})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hookCalls != 2 || executed != 1 {
		t.Fatalf("hookCalls=%d executed=%d, want the second response's call gated afresh and executed once", hookCalls, executed)
	}
}

// Upstream "honors a PermissionRequest hook returning deny on the
// allowFinalResponse path": the denied call is rejected, not executed, and
// the forced final turn still runs.
func TestPermissionRequestDenyOnAllowFinalResponsePath(t *testing.T) {
	dangerRuns := 0
	danger := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		dangerRuns++
		return "boom", nil
	}})
	manager := NewHooksManager()
	manager.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{Handler: func(PermissionRequestPayload, LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
		return SyncResult(PermissionRequestResult{Decision: PermissionDecisionDeny, Reason: "policy says no"}), nil
	}})
	call := components.OutputFunctionCallItem{CallID: "c_danger", Name: "danger", Arguments: `{"query":"x"}`}
	first := operations.CreateCreateResponsesResponseOpenResponsesResult(components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}})
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&first, plainTextResponse("final")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go", Tools: []Tool{danger}, Hooks: manager, StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dangerRuns != 0 || text != "final" || sender.calls != 2 {
		t.Fatalf("dangerRuns=%d text=%q calls=%d", dangerRuns, text, sender.calls)
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	var denied bool
	for _, it := range items {
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.CallID == "c_danger" && it.FunctionCallOutputItem.Output.Str != nil && *it.FunctionCallOutputItem.Output.Str == `{"error":"policy says no"}` {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("final request must carry the denial output, got %#v", items)
	}
}

// Upstream "gates duplicate identical calls separately": two calls in one
// response sharing an id are distinct occurrences — a PreToolUse block on
// one must land on that occurrence, never on its sibling.
func TestPreparedBlockTargetsTheRightDuplicateOccurrence(t *testing.T) {
	var ran []string
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(_ context.Context, in sampleInput, _ ToolExecuteContext) (any, error) {
		ran = append(ran, in.Query)
		return "ok:" + in.Query, nil
	}})
	manager := NewHooksManager()
	manager.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(p PreToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		if p.ToolInput["query"] == "bad" {
			return SyncResult(PreToolUseResult{BlockReason: "bad query"}), nil
		}
		return VoidResult[PreToolUseResult](), nil
	}})
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "dup", Name: "search", Arguments: `{"query":"good"}`}),
		components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{CallID: "dup", Name: "search", Arguments: `{"query":"bad"}`}),
	}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created, plainTextResponse("done")}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "m", Input: "go", Tools: []Tool{tool}, Hooks: manager})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "good" {
		t.Fatalf("executed %v, want only the unblocked occurrence (good)", ran)
	}
	var outs []string
	for _, it := range sender.requests[1].Input.ArrayOfInputsUnion1 {
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.Output.Str != nil {
			outs = append(outs, *it.FunctionCallOutputItem.Output.Str)
		}
	}
	if len(outs) != 2 || outs[0] != "ok:good" || outs[1] != `{"error":"bad query"}` {
		t.Fatalf("outputs = %v, want [ok:good, bad-query rejection] in call order", outs)
	}
}
