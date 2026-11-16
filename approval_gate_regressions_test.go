package agent

// Ports tests/unit/approval-gate-regressions.test.ts, sections:
//   - "approval predicate argument parity with execute (#54)"
//   - "approval uses the post-PreToolUse arguments that execute would receive"
//   - "every engine execute path validates before running the tool body"
//   - "allowFinalResponse path enforces the approval gate (#54)"
// The "approval decision validation and paused resumes (#517, #518)" section
// lives in approval_gate_decisions_test.go.
//
// Step counting: Go's StopCondition sees one StepResult per model response
// (before that response's tools run), while upstream's stepCountIs counts
// executed tool rounds. Upstream stepCountIs(n) therefore corresponds to Go
// StepCountIs(n+1); fixtures below are translated accordingly.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- input shapes standing in for Zod defaults / coercions / transforms ---

// agrDefaulted: `dangerous` defaults to true when absent
// (z.boolean().default(true)).
type agrDefaulted struct {
	Dangerous bool `json:"dangerous"`
}

func (d *agrDefaulted) UnmarshalJSON(b []byte) error {
	var raw struct {
		Dangerous *bool `json:"dangerous"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d.Dangerous = true
	if raw.Dangerous != nil {
		d.Dangerous = *raw.Dangerous
	}
	return nil
}

var agrDefaultedSchema = map[string]any{"type": "object", "properties": map[string]any{"dangerous": map[string]any{"type": "boolean"}}}

// agrCoerced: `amount` accepts a numeric string (z.coerce.number()).
type agrCoerced struct {
	Amount float64 `json:"amount"`
}

func (c *agrCoerced) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch v := raw["amount"].(type) {
	case float64:
		c.Amount = v
	case string:
		var f float64
		if err := json.Unmarshal([]byte(v), &f); err != nil {
			return err
		}
		c.Amount = f
	}
	return nil
}

// agrNonObject decodes fine but re-encodes to a non-object (a schema whose
// parse yields a non-record).
type agrNonObject struct {
	Target string `json:"target"`
}

func (agrNonObject) MarshalJSON() ([]byte, error) { return []byte(`["not","a","record"]`), nil }

// agrNormalized appends ":normalized" on decode (z.string().transform).
type agrNormalized struct {
	Value string `json:"value"`
}

func (n *agrNormalized) UnmarshalJSON(b []byte) error {
	var raw struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	n.Value = raw.Value + ":normalized"
	return nil
}

// agrLength decodes a string into its length (type-changing transform).
type agrLength struct {
	Value int `json:"value"`
}

func (l *agrLength) UnmarshalJSON(b []byte) error {
	var raw struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	l.Value = len(raw.Value)
	return nil
}

type agrDangerous struct {
	Dangerous bool `json:"dangerous" jsonschema:"required"`
}

// --- hook helpers ---

func agrOnPreToolUse(m *HooksManager, rec *phCounter, fn func(PreToolUsePayload) PreToolUseResult) {
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(p PreToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		rec.add(phCopyAny(p.ToolInput))
		if fn == nil {
			return VoidResult[PreToolUseResult](), nil
		}
		return SyncResult(fn(p)), nil
	}})
}

func agrOnPermission(m *HooksManager, rec *phCounter, decision PermissionDecision, reason string) {
	m.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{Handler: func(p PermissionRequestPayload, _ LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
		rec.add(phCopyAny(p.ToolInput))
		return SyncResult(PermissionRequestResult{Decision: decision, Reason: reason}), nil
	}})
}

func agrOnPostToolUseFailure(m *HooksManager, rec *phCounter) {
	m.OnPostToolUseFailure(HookEntry[PostToolUseFailurePayload, EmptyHookResult]{Handler: func(p PostToolUseFailurePayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		msg := ""
		if p.Error != nil {
			msg = p.Error.Error()
		}
		rec.add(p.ToolName + ": " + msg)
		return VoidResult[EmptyHookResult](), nil
	}})
}

// phCopyAny snapshots a JSON-shaped value as its encoding, so recorded hook
// payloads can be compared after the run without aliasing live maps.
func phCopyAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unencodable>"
	}
	return string(b)
}

func agrArgsJSON(t *testing.T, call ParsedToolCall) string {
	t.Helper()
	if call.Arguments == nil {
		return call.RawArgs
	}
	return phJSON(t, call.Arguments)
}

// =====================================================================
// approval predicate argument parity with execute (#54)
// =====================================================================

// upstream: "applies inputSchema defaults before invoking a function-based requireApproval"
func TestApprovalPredicateSeesSchemaDefaults(t *testing.T) {
	var seen phCounter
	tool := MustNewTool(ToolConfig[agrDefaulted]{
		Name: "conditional_action", InputSchema: agrDefaultedSchema,
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			seen.add(phCopyAny(call.Arguments))
			args, _ := call.Arguments.(map[string]any)
			return args["dangerous"] == true, nil
		},
		Execute: func(context.Context, agrDefaulted, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	requires, err := ToolRequiresApproval(context.Background(), tool, ParsedToolCall{ID: "1", Name: "conditional_action", Arguments: map[string]any{}}, TurnContext{NumberOfTurns: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen.values(); len(got) != 1 || got[0] != `{"dangerous":true}` {
		t.Fatalf("predicate must see the decoded default, saw %v", got)
	}
	if !requires {
		t.Fatal("a defaulted dangerous=true must require approval")
	}
}

// upstream: "applies inputSchema coercions before invoking a function-based requireApproval"
func TestApprovalPredicateSeesCoercedArguments(t *testing.T) {
	var seen phCounter
	tool := MustNewTool(ToolConfig[agrCoerced]{
		Name:        "transfer",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"amount": map[string]any{}}},
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			seen.add(phCopyAny(call.Arguments))
			args, _ := call.Arguments.(map[string]any)
			amount, ok := args["amount"].(float64)
			return ok && amount > 100, nil
		},
		Execute: func(context.Context, agrCoerced, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	requires, err := ToolRequiresApproval(context.Background(), tool, ParsedToolCall{ID: "2", Name: "transfer", Arguments: map[string]any{"amount": "500"}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen.values(); len(got) != 1 || got[0] != `{"amount":500}` {
		t.Fatalf("predicate must see the numeric coercion, saw %v", got)
	}
	if !requires {
		t.Fatal("amount 500 (> 100) must require approval")
	}
}

// upstream: "applies schema defaults for call-level checks without mutating the executable call"
func TestCallLevelApprovalSeesNormalizedArgumentsWithoutMutatingCall(t *testing.T) {
	tool := MustNewTool(ToolConfig[agrDefaulted]{
		Name: "call_level_defaulted_action", InputSchema: agrDefaultedSchema,
		Execute: func(context.Context, agrDefaulted, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	original := map[string]any{}
	call := ParsedToolCall{ID: "call-level-default", Name: "call_level_defaulted_action", Arguments: original}
	var seen phCounter
	check := func(_ context.Context, c ParsedToolCall, _ TurnContext) (bool, error) {
		seen.add(phCopyAny(c.Arguments))
		if c.ID != "call-level-default" || c.Name != "call_level_defaulted_action" {
			t.Errorf("call-level check lost the call identity: %+v", c)
		}
		return true, nil
	}
	requires, err := ToolRequiresApproval(context.Background(), tool, call, TurnContext{}, check)
	if err != nil {
		t.Fatal(err)
	}
	if !requires {
		t.Fatal("the call-level check returned true")
	}
	if got := seen.values(); len(got) != 1 || got[0] != `{"dangerous":true}` {
		t.Fatalf("call-level check must receive the normalized arguments, saw %v", got)
	}
	if len(original) != 0 || len(call.Arguments.(map[string]any)) != 0 {
		t.Fatalf("the executable call's arguments must not be mutated: %v", call.Arguments)
	}
}

// upstream: "honors a false call-level check when arguments fail schema validation"
//
//	and "honors a true call-level check when arguments fail schema validation"
func TestCallLevelApprovalHonoredOnSchemaInvalidArguments(t *testing.T) {
	tool := MustNewTool(ToolConfig[phTargetInput]{
		Name:        "call_level_invalid_action",
		InputSchema: phObjectSchema("target"),
		Execute:     func(context.Context, phTargetInput, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	for _, want := range []bool{false, true} {
		invalid := ParsedToolCall{ID: "call-level-invalid", Name: "call_level_invalid_action", Arguments: map[string]any{}}
		var seen phCounter
		got, err := ToolRequiresApproval(context.Background(), tool, invalid, TurnContext{}, func(_ context.Context, c ParsedToolCall, _ TurnContext) (bool, error) {
			seen.add(phCopyAny(c.Arguments))
			return want, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("call-level check returned %v; ToolRequiresApproval = %v", want, got)
		}
		if v := seen.values(); len(v) != 1 || v[0] != `{}` {
			t.Fatalf("call-level check must receive the raw (unnormalizable) call, saw %v", v)
		}
	}
}

// upstream: "fails closed when arguments fail schema validation"
func TestApprovalPredicateFailsClosedOnSchemaInvalidArguments(t *testing.T) {
	var calls phCounter
	tool := MustNewTool(ToolConfig[phTargetInput]{
		Name:        "strict_action",
		InputSchema: phObjectSchema("target"),
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
			calls.add(nil)
			return false, nil
		},
		Execute: func(context.Context, phTargetInput, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	requires, err := ToolRequiresApproval(context.Background(), tool, ParsedToolCall{ID: "3", Name: "strict_action", Arguments: map[string]any{}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !requires || calls.count() != 0 {
		t.Fatalf("schema-invalid arguments must fail closed without consulting the predicate: requires=%v calls=%d", requires, calls.count())
	}
}

// upstream: "still fails closed when the schema parses to a non-object value"
func TestApprovalPredicateFailsClosedOnNonObjectNormalization(t *testing.T) {
	var calls phCounter
	tool := MustNewTool(ToolConfig[agrNonObject]{
		Name:        "weird_action",
		InputSchema: map[string]any{"type": "object", "required": []any{"target"}, "properties": map[string]any{"target": map[string]any{"type": "string"}}},
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
			calls.add(nil)
			return false, nil
		},
		Execute: func(context.Context, agrNonObject, ToolExecuteContext) (any, error) { return map[string]any{}, nil },
	})
	requires, err := ToolRequiresApproval(context.Background(), tool, ParsedToolCall{ID: "4", Name: "weird_action", Arguments: map[string]any{"target": "x"}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !requires || calls.count() != 0 {
		t.Fatalf("a non-object normalized value must fail closed: requires=%v calls=%d", requires, calls.count())
	}
}

// upstream: "still fails closed on schema-invalid arguments for manual (caller-executed) tools"
func TestApprovalPredicateFailsClosedForManualTools(t *testing.T) {
	var calls phCounter
	manual := MustNewTool(ToolConfig[phTargetInput]{
		Name:        "manual_action",
		InputSchema: phObjectSchema("target"),
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
			calls.add(nil)
			return false, nil
		},
	})
	if !IsManualTool(manual) {
		t.Fatal("fixture must be a manual tool")
	}
	requires, err := ToolRequiresApproval(context.Background(), manual, ParsedToolCall{ID: "5", Name: "manual_action", Arguments: map[string]any{}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !requires || calls.count() != 0 {
		t.Fatalf("manual tool with invalid args must fail closed: requires=%v calls=%d", requires, calls.count())
	}
}

// Call-level CallModelInput.Approval applies even to tool names the caller
// never registered (upstream toolRequiresApproval consults the call-level
// check before looking the tool up).
func TestCallLevelApprovalAppliesToUnknownToolNames(t *testing.T) {
	ctx := context.Background()
	var seen phCounter
	sender := phSender(phResp("resp_1", phFnCall("call_ghost", "ghost", `{"x":1}`)))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi",
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			seen.add(call.Name + " " + phCopyAny(call.Arguments))
			return call.Name == "ghost", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingApproval || !phEqualStrings(phPendingIDs(state.PendingToolCalls), []string{"call_ghost"}) {
		t.Fatalf("unknown tool gated by the call-level check must pause awaiting_approval, got %s %v", state.Status, phPendingIDs(state.PendingToolCalls))
	}
	if got := seen.values(); len(got) != 1 || got[0] != `ghost {"x":1}` {
		t.Fatalf("call-level check must see the raw call for an unknown tool, saw %v", got)
	}
	if sender.calls != 1 {
		t.Fatalf("a paused run sends no follow-up, got %d requests", sender.calls)
	}
}

// =====================================================================
// approval uses the post-PreToolUse arguments that execute would receive
// =====================================================================

func agrGuardedTool(rec *phCounter, predicateSeen *phCounter) *TypedTool[agrDangerous] {
	return MustNewTool(ToolConfig[agrDangerous]{
		Name:        "guarded_action",
		InputSchema: map[string]any{"type": "object", "required": []any{"dangerous"}, "properties": map[string]any{"dangerous": map[string]any{"type": "boolean"}}},
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			predicateSeen.add(phCopyAny(call.Arguments))
			args, _ := call.Arguments.(map[string]any)
			return args["dangerous"] == true, nil
		},
		Execute: func(_ context.Context, in agrDangerous, _ ToolExecuteContext) (any, error) {
			rec.add(in)
			return map[string]any{"ok": true}, nil
		},
	})
}

// agrPauseOnMutatedDangerousCall runs upstream's runMutationExploit fixture:
// the model asks for {dangerous:false}, PreToolUse rewrites it to
// {dangerous:true}. Returns the persisted state and recorders.
func agrPauseOnMutatedDangerousCall(t *testing.T, mem *phMemState) (tool *TypedTool[agrDangerous], exec, predicate, pre *phCounter) {
	t.Helper()
	exec, predicate, pre = &phCounter{}, &phCounter{}, &phCounter{}
	tool = agrGuardedTool(exec, predicate)
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, pre, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"dangerous": true}}
	})
	sender := phSender(phResp("resp_mutation", phFnCall("call_guarded", "guarded_action", `{"dangerous":false}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "run the guarded action", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("the pause must not send a follow-up, got %d requests", sender.calls)
	}
	return tool, exec, predicate, pre
}

// upstream: "does not let a hook rewrite safe arguments into an unapproved dangerous call"
func TestPreToolUseMutationRequiringApprovalGoesToSecondGate(t *testing.T) {
	mem := &phMemState{}
	_, exec, predicate, pre := agrPauseOnMutatedDangerousCall(t, mem)

	if exec.count() != 0 {
		t.Fatal("the mutated dangerous call must not execute without approval")
	}
	seen := predicate.values()
	if len(seen) == 0 || seen[len(seen)-1] != `{"dangerous":true}` {
		t.Fatalf("predicate must be re-consulted with the hook's input, saw %v", seen)
	}
	if pre.count() != 1 {
		t.Fatalf("PreToolUse runs once in the preparation phase, ran %d", pre.count())
	}
	st := mem.get()
	if st == nil || st.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %+v", st)
	}
	if len(st.PendingToolCalls) != 1 {
		t.Fatalf("expected one pending call, got %+v", st.PendingToolCalls)
	}
	p := st.PendingToolCalls[0]
	if callKey(p) != "call_guarded" || p.Name != "guarded_action" || agrArgsJSON(t, p) != `{"dangerous":true}` || !p.PreToolUseApplied {
		t.Fatalf("pending call must persist the hook's input with PreToolUseApplied: %+v", p)
	}
	// The marker is part of the persisted (serialized) contract.
	serialized, err := SerializeConversationState(*st)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(serialized)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.PendingToolCalls[0].PreToolUseApplied {
		t.Fatalf("PreToolUseApplied must survive serialization: %s", serialized)
	}
}

// A resume of a call whose PreToolUse already ran (PreToolUseApplied) must
// not run the hook again, and executes the persisted effective input.
func TestResumeOfPreparedCallDoesNotRerunPreToolUse(t *testing.T) {
	ctx := context.Background()
	mem := &phMemState{}
	tool, exec, _, pre := agrPauseOnMutatedDangerousCall(t, mem)

	hooks := NewHooksManager()
	var resumePre phCounter
	agrOnPreToolUse(hooks, &resumePre, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"dangerous": false}}
	})
	sender := phSender(phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem,
		ApproveToolCalls: []string{"call_guarded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if resumePre.count() != 0 || pre.count() != 1 {
		t.Fatalf("PreToolUse must not re-run on resume of a prepared call: resume=%d pause=%d", resumePre.count(), pre.count())
	}
	if got := exec.values(); len(got) != 1 || got[0].(agrDangerous).Dangerous != true {
		t.Fatalf("approved call must execute once with the approved (prepared) input, got %v", got)
	}
	if ids := phOutputCallIDs(t, sender, 0); !phEqualStrings(ids, []string{"call_guarded"}) {
		t.Fatalf("resume request outputs = %v", ids)
	}
	if st := mem.get(); st.Status != ConversationStatusComplete {
		t.Fatalf("status = %s, want complete", st.Status)
	}
}

// Rejecting a call that PreToolUse already prepared reports it as a
// PostToolUseFailure (the hook ran, the tool never will).
func TestRejectingPreparedCallEmitsPostToolUseFailure(t *testing.T) {
	ctx := context.Background()
	mem := &phMemState{}
	tool, exec, _, _ := agrPauseOnMutatedDangerousCall(t, mem)

	hooks := NewHooksManager()
	var failures, post phCounter
	agrOnPostToolUseFailure(hooks, &failures)
	hooks.OnPostToolUse(HookEntry[PostToolUsePayload, EmptyHookResult]{Handler: func(p PostToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		post.add(p.ToolName)
		return VoidResult[EmptyHookResult](), nil
	}})
	sender := phSender(phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem,
		RejectToolCalls: []string{"call_guarded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if exec.count() != 0 || post.count() != 0 {
		t.Fatalf("a rejected call must not run: exec=%d post=%d", exec.count(), post.count())
	}
	if got := failures.values(); len(got) != 1 || got[0] != "guarded_action: Rejected by user" {
		t.Fatalf("rejecting a prepared call must emit one PostToolUseFailure, got %v", got)
	}
	outs := phOutputs(t, sender, 0)
	if len(outs) != 1 || outs[0].CallID != "call_guarded" || !strings.Contains(phOutputString(outs[0]), "Rejected by user") {
		t.Fatalf("resume request must carry the rejection output, got %+v", outs)
	}
}

// A rejection of a call PreToolUse never prepared is not a failure of a
// prepared call: no PostToolUseFailure.
func TestRejectingUnpreparedCallEmitsNoPostToolUseFailure(t *testing.T) {
	mem := &phMemState{}
	var exec phCounter
	gated := phOKTool("gated", &exec, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	if _, err := mustRun(t, phSender(phResp("r1", phFnCall("call_1", "gated", `{"target":"x"}`))), CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{gated}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	hooks := NewHooksManager()
	var failures phCounter
	agrOnPostToolUseFailure(hooks, &failures)
	if _, err := mustRun(t, phSender(phTextResp("r2", "ok")), CallModelInput{Model: "openai/test", Tools: []Tool{gated}, Hooks: hooks, StateAccessor: mem, RejectToolCalls: []string{"call_1"}}); err != nil {
		t.Fatal(err)
	}
	if failures.count() != 0 || exec.count() != 0 {
		t.Fatalf("unprepared rejection: failures=%v exec=%d", failures.values(), exec.count())
	}
}

// mustRun drives a CallModel to completion (State) and returns the result.
func mustRun(t *testing.T, sender *fakeSender, in CallModelInput) (*ModelResult, error) {
	t.Helper()
	result, err := CallModel(context.Background(), sender, in)
	if err != nil {
		return nil, err
	}
	_, err = result.State(context.Background())
	return result, err
}

// PreToolUse runs exactly once per call: in the preparation phase, not again
// at execution. Covers the normal round and an auto-approved sibling at an
// approval pause.
func TestPreToolUseRunsOncePerAutoApprovedCall(t *testing.T) {
	ctx := context.Background()
	var exec, gatedExec, pre phCounter
	auto := phOKTool("auto", &exec, nil)
	gated := phOKTool("gated", &gatedExec, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &pre, func(p PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"target": "hooked-" + p.ToolInput["target"].(string)}}
	})

	t.Run("normal round", func(t *testing.T) {
		sender := phSender(phResp("r1", phFnCall("call_a", "auto", `{"target":"x"}`)), phTextResp("r2", "done"))
		result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{auto}, Hooks: hooks})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(ctx); err != nil {
			t.Fatal(err)
		}
		if pre.count() != 1 {
			t.Fatalf("PreToolUse ran %d times for one call", pre.count())
		}
		if got := exec.values(); len(got) != 1 || got[0].(phTargetInput).Target != "hooked-x" {
			t.Fatalf("tool must run once on the hook's input, got %v", got)
		}
	})

	t.Run("auto-approved sibling of an approval pause", func(t *testing.T) {
		var pre2, exec2 phCounter
		auto2 := phOKTool("auto", &exec2, nil)
		hooks2 := NewHooksManager()
		agrOnPreToolUse(hooks2, &pre2, nil)
		mem := &phMemState{}
		sender := phSender(phResp("r1", phFnCall("call_a", "auto", `{"target":"x"}`), phFnCall("call_g", "gated", `{"target":"y"}`)))
		if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{auto2, gated}, Hooks: hooks2, StateAccessor: mem}); err != nil {
			t.Fatal(err)
		}
		if exec2.count() != 1 || gatedExec.count() != 0 {
			t.Fatalf("auto sibling runs once before the pause, gated never: auto=%d gated=%d", exec2.count(), gatedExec.count())
		}
		// PreToolUse ran exactly once, for the auto call only; the gated
		// call (ask_user) was not prepared.
		if got := pre2.values(); len(got) != 1 || got[0] != `{"target":"x"}` {
			t.Fatalf("PreToolUse must run once for the auto-approved call only, saw %v", got)
		}
		st := mem.get()
		if st.Status != ConversationStatusAwaitingApproval || len(st.UnsentToolResults) != 1 || st.UnsentToolResults[0].CallID != "call_a" {
			t.Fatalf("auto result must be saved as unsent at the pause: %+v", st)
		}
	})
}

// A PreToolUse mutation that produces schema-invalid input blocks the call
// with a descriptive error instead of executing it.
func TestPreToolUseInvalidMutationBlocksCall(t *testing.T) {
	ctx := context.Background()
	var exec, failures phCounter
	tool := phOKTool("strict_tool", &exec, nil)
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"target": 123}}
	})
	agrOnPostToolUseFailure(hooks, &failures)
	sender := phSender(phResp("r1", phFnCall("call_1", "strict_tool", `{"target":"ok"}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	events, err := phDrainToolStream(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	if exec.count() != 0 {
		t.Fatal("a schema-invalid mutated input must never execute")
	}
	const want = `PreToolUse produced invalid input for "strict_tool"`
	outs := phOutputs(t, sender, 1)
	if len(outs) != 1 || outs[0].CallID != "call_1" || phOutputError(outs[0]) != want {
		t.Fatalf("follow-up must carry the blocked output %q, got %+v", want, outs)
	}
	var sawResultErr bool
	for _, ev := range events {
		if ev.Type == "tool.result" && ev.CallID == "call_1" && ev.Error == want {
			sawResultErr = true
		}
	}
	if !sawResultErr {
		t.Fatalf("ToolStream must report the block as a tool.result error, got %+v", events)
	}
	if got := failures.values(); len(got) != 1 || !strings.Contains(got[0].(string), want) {
		t.Fatalf("a prepared-then-blocked call reports PostToolUseFailure, got %v", got)
	}
}

// upstream: "lets normal validation handle deeply nested model input when keying cannot"
func TestApprovalGateFailsClosedOnDeeplyNestedInvalidInput(t *testing.T) {
	deep := `{}`
	for i := 0; i < 200; i++ {
		deep = `{"child":` + deep + `}`
	}
	var exec phCounter
	tool := phOKTool("deep_invalid", &exec, func(c *ToolConfig[phTargetInput]) {
		c.Approval = func(context.Context, ParsedToolCall, TurnContext) (bool, error) { return false, nil }
	})
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("r", phFnCall("call_deep_invalid", "deep_invalid", deep))), CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if st := mem.get(); st.Status != ConversationStatusAwaitingApproval || exec.count() != 0 {
		t.Fatalf("status=%s exec=%d, want awaiting_approval and no execution", st.Status, exec.count())
	}
}

// upstream: "gates identical post-hook calls independently across responses"
func TestIdenticalPostHookCallsGatedIndependentlyAcrossResponses(t *testing.T) {
	ctx := context.Background()
	var exec, predicate, perm, pre phCounter
	tool := agrGuardedTool(&exec, &predicate)
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &pre, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"dangerous": true}}
	})
	agrOnPermission(hooks, &perm, PermissionDecisionAllow, "")
	call := phResp("resp_mutation_reused", phFnCall("call_reused", "guarded_action", `{"dangerous":false}`))
	sender := phSender(call, call, phTextResp("resp_done", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "run twice", Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	dangerousChecks := 0
	for _, v := range predicate.values() {
		if v == `{"dangerous":true}` {
			dangerousChecks++
		}
	}
	if dangerousChecks < 2 || perm.count() != 2 || exec.count() != 2 {
		t.Fatalf("each response's call is gated on its own: predicate(dangerous)=%d permission=%d exec=%d", dangerousChecks, perm.count(), exec.count())
	}
	if sender.calls != 3 {
		t.Fatalf("expected 3 requests, got %d", sender.calls)
	}
	if pre.count() != 2 {
		t.Fatalf("PreToolUse runs once per call (preparation only, not again at execution), ran %d", pre.count())
	}
	// Each executed call ran on the hook's (permission-allowed) input.
	for _, v := range exec.values() {
		if v.(agrDangerous).Dangerous != true {
			t.Fatalf("executed input = %+v, want the mutated input", v)
		}
	}
}

// upstream: "keeps hook-mutated arguments raw across approval and execution transforms"
func TestHookMutatedArgumentsPersistRawAcrossTransforms(t *testing.T) {
	var predicate, exec phCounter
	tool := MustNewTool(ToolConfig[agrNormalized]{
		Name:        "transformed_mutation",
		InputSchema: map[string]any{"type": "object", "required": []any{"value"}, "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		Approval: func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			predicate.add(phCopyAny(call.Arguments))
			args, _ := call.Arguments.(map[string]any)
			return args["value"] == "hook:normalized", nil
		},
		Execute: func(_ context.Context, in agrNormalized, _ ToolExecuteContext) (any, error) {
			exec.add(in)
			return map[string]any{"value": in.Value}, nil
		},
	})
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"value": "hook"}}
	})
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("r", phFnCall("call_transformed_mutation", "transformed_mutation", `{"value":"wire"}`))), CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	seen := predicate.values()
	if len(seen) == 0 || seen[len(seen)-1] != `{"value":"hook:normalized"}` {
		t.Fatalf("predicate must last see the normalized hook input, saw %v", seen)
	}
	if exec.count() != 0 {
		t.Fatal("must not execute before approval")
	}
	st := mem.get()
	if len(st.PendingToolCalls) != 1 || agrArgsJSON(t, st.PendingToolCalls[0]) != `{"value":"hook"}` || !st.PendingToolCalls[0].PreToolUseApplied {
		t.Fatalf("pending must keep the raw hook input (not the transformed value): %+v", st.PendingToolCalls)
	}
}

// upstream: "executes a type-changing hook mutation from raw input exactly once"
func TestTypeChangingHookMutationExecutesFromRawInputOnce(t *testing.T) {
	ctx := context.Background()
	var exec phCounter
	tool := MustNewTool(ToolConfig[agrLength]{
		Name:        "type_changing_mutation",
		InputSchema: map[string]any{"type": "object", "required": []any{"value"}, "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		Approval:    func(context.Context, ParsedToolCall, TurnContext) (bool, error) { return false, nil },
		Execute: func(_ context.Context, in agrLength, _ ToolExecuteContext) (any, error) {
			exec.add(in)
			return map[string]any{"value": in.Value}, nil
		},
	})
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"value": "hook"}}
	})
	sender := phSender(phResp("r1", phFnCall("call_tc", "type_changing_mutation", `{"value":"wire"}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if got := exec.values(); len(got) != 1 || got[0].(agrLength).Value != 4 {
		t.Fatalf("tool must run exactly once with len(\"hook\")=4, got %v", got)
	}
}

// upstream: "gates schema-invalid model arguments before running PreToolUse"
func TestSchemaInvalidArgumentsGatedBeforePreToolUse(t *testing.T) {
	var exec, predicate, pre phCounter
	tool := agrGuardedTool(&exec, &predicate)
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &pre, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"dangerous": true}}
	})
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("r", phFnCall("call_invalid_original", "guarded_action", `{}`))), CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if pre.count() != 0 || exec.count() != 0 || predicate.count() != 0 {
		t.Fatalf("invalid args fail closed before any hook: pre=%d exec=%d predicate=%d", pre.count(), exec.count(), predicate.count())
	}
	if st := mem.get(); st.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status = %s", st.Status)
	}
}

// upstream: "persists unconditional approvals with post-hook arguments and
// does not reapply the hook on resume"
func TestUnconditionalApprovalPreparesOnResumeOnly(t *testing.T) {
	ctx := context.Background()
	var exec, pre, perm phCounter
	type valueInput struct {
		Value string `json:"value" jsonschema:"required"`
	}
	tool := MustNewTool(ToolConfig[valueInput]{
		Name: "always_guarded", RequireApproval: true, InputSchema: phObjectSchema("value"),
		Execute: func(_ context.Context, in valueInput, _ ToolExecuteContext) (any, error) {
			exec.add(in.Value)
			return map[string]any{"value": in.Value}, nil
		},
	})
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &pre, func(p PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"value": p.ToolInput["value"].(string) + "-prepared"}}
	})
	agrOnPermission(hooks, &perm, PermissionDecisionAskUser, "")
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("resp_unconditional", phFnCall("call_unconditional", "always_guarded", `{"value":"original"}`))), CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if got := perm.values(); len(got) != 1 || got[0] != `{"value":"original"}` {
		t.Fatalf("PermissionRequest must see the original input, saw %v", got)
	}
	st := mem.get()
	if len(st.PendingToolCalls) != 1 || agrArgsJSON(t, st.PendingToolCalls[0]) != `{"value":"original"}` || st.PendingToolCalls[0].PreToolUseApplied {
		t.Fatalf("an ask_user pause persists the unprepared call: %+v", st.PendingToolCalls)
	}
	legacy := &phMemState{}
	if err := legacy.Save(ctx, *st); err != nil {
		t.Fatal(err)
	}

	if _, err := mustRun(t, phSender(phTextResp("resp_after", "Done.")), CallModelInput{Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: mem, ApproveToolCalls: []string{"call_unconditional"}}); err != nil {
		t.Fatal(err)
	}
	if pre.count() != 1 || perm.count() != 1 {
		t.Fatalf("resume prepares once and does not re-ask: pre=%d perm=%d", pre.count(), perm.count())
	}
	if got := exec.values(); len(got) != 1 || got[0] != "original-prepared" {
		t.Fatalf("approved call must run once on the prepared input, got %v", got)
	}

	if _, err := mustRun(t, phSender(phTextResp("resp_after_legacy", "Done.")), CallModelInput{Model: "openai/test", Tools: []Tool{tool}, Hooks: hooks, StateAccessor: legacy, ApproveToolCalls: []string{"call_unconditional"}}); err != nil {
		t.Fatal(err)
	}
	if pre.count() != 2 {
		t.Fatalf("a legacy pending call (no marker) is prepared on resume, pre=%d", pre.count())
	}
	if got := exec.values(); len(got) != 2 || got[1] != "original-prepared" {
		t.Fatalf("legacy resume must execute the prepared input, got %v", got)
	}
}

// =====================================================================
// every engine execute path validates before running the tool body
// =====================================================================

func TestEveryExecutePathValidatesBeforeRunningToolBody(t *testing.T) {
	ctx := context.Background()
	invalid := ParsedToolCall{ID: "c", CallID: "c", Name: "x", Arguments: map[string]any{}}
	var ran phCounter
	tools := map[string]Tool{
		"regular": MustNewTool(ToolConfig[phTargetInput]{Name: "regular", InputSchema: phObjectSchema("target"), Execute: func(context.Context, phTargetInput, ToolExecuteContext) (any, error) {
			ran.add("regular")
			return map[string]any{}, nil
		}}),
		"generator": MustNewTool(ToolConfig[phTargetInput]{Name: "generator", InputSchema: phObjectSchema("target"), Generate: func(_ context.Context, _ phTargetInput, _ ToolExecuteContext, yield func(any) error) (any, error) {
			ran.add("generator")
			_ = yield(map[string]any{"progress": 1})
			return map[string]any{"done": true}, nil
		}}),
		"hitl": MustNewTool(ToolConfig[phTargetInput]{Name: "hitl", InputSchema: phObjectSchema("target"), OnToolCalled: func(context.Context, phTargetInput, ToolExecuteContext) (any, bool, error) {
			ran.add("hitl")
			return map[string]any{"ok": true}, true, nil
		}}),
	}
	for name, tool := range tools {
		call := invalid
		call.Name = name
		res, err := ExecuteTool(ctx, tool, call, ToolExecuteContext{})
		if err != nil {
			t.Fatalf("%s: validation failure is a tool result, not a fatal error: %v", name, err)
		}
		if res.Error == nil {
			t.Fatalf("%s: schema-invalid arguments must produce a result error", name)
		}
	}
	if ran.count() != 0 {
		t.Fatalf("no tool body may run on invalid arguments, ran %v", ran.values())
	}
}

// =====================================================================
// allowFinalResponse path enforces the approval gate (#54)
// =====================================================================

// upstream: "does not execute a requireApproval tool when stopWhen halts the loop"
func TestAllowFinalResponsePathPausesApprovalRequiredCall(t *testing.T) {
	ctx := context.Background()
	var safeRuns, dangerRuns phCounter
	safe := phOKTool("safe", &safeRuns, nil)
	danger := phOKTool("danger", &dangerRuns, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	sender := phSender(
		phResp("resp_turn_0", phFnCall("call_safe", "safe", `{"target":"staging"}`)),
		phResp("resp_turn_1", phFnCall("call_danger", "danger", `{"target":"prod"}`)),
		phTextResp("resp_final", "Done."),
	)
	mem := &phMemState{}
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "do the thing", Tools: []Tool{safe, danger}, StateAccessor: mem,
		StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requires, err := result.RequiresApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if safeRuns.count() != 1 {
		t.Fatalf("ungated tool runs in the normal round, ran %d", safeRuns.count())
	}
	if dangerRuns.count() != 0 {
		t.Fatal("the gate must hold on the AllowFinalResponse path")
	}
	st := mem.get()
	if !requires || st.Status != ConversationStatusAwaitingApproval || !phEqualStrings(phPendingIDs(st.PendingToolCalls), []string{"call_danger"}) {
		t.Fatalf("expected pause on call_danger: requires=%v status=%s pending=%v", requires, st.Status, phPendingIDs(st.PendingToolCalls))
	}
	if sender.calls != 2 {
		t.Fatalf("no final text-coercion request while paused: got %d requests, want 2", sender.calls)
	}
}

// upstream: "still runs non-gated tools on the allowFinalResponse path"
func TestAllowFinalResponsePathRunsUngatedTools(t *testing.T) {
	ctx := context.Background()
	var runs phCounter
	safe := phOKTool("safe", &runs, nil)
	sender := phSender(phResp("resp_turn_0", phFnCall("call_safe", "safe", `{"target":"staging"}`)), phTextResp("resp_final", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "do the safe thing", Tools: []Tool{safe}, StateAccessor: &phMemState{},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runs.count() != 1 || text != "Done." || sender.calls != 2 {
		t.Fatalf("runs=%d text=%q calls=%d", runs.count(), text, sender.calls)
	}
	if got := phWireChoice(t, sender, 1); got != `"none"` {
		t.Fatalf("final request must forbid tools, tool_choice=%s", got)
	}
	if ids := phOutputCallIDs(t, sender, 1); !phEqualStrings(ids, []string{"call_safe"}) {
		t.Fatalf("final request must carry the executed output, got %v", ids)
	}
}

// upstream: "honors a PermissionRequest hook returning deny on the allowFinalResponse path"
func TestAllowFinalResponsePathHonorsPermissionDeny(t *testing.T) {
	ctx := context.Background()
	var safeRuns, dangerRuns, perm phCounter
	safe := phOKTool("safe", &safeRuns, nil)
	danger := phOKTool("danger", &dangerRuns, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	hooks := NewHooksManager()
	agrOnPermission(hooks, &perm, PermissionDecisionDeny, "blocked by policy")
	sender := phSender(
		phResp("resp_turn_0", phFnCall("call_safe", "safe", `{"target":"staging"}`)),
		phResp("resp_turn_1", phFnCall("call_danger", "danger", `{"target":"prod"}`)),
		phTextResp("resp_final", "Denied."),
	)
	mem := &phMemState{}
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "do the thing", Tools: []Tool{safe, danger}, Hooks: hooks, StateAccessor: mem,
		StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(ctx); err != nil {
		t.Fatal(err)
	}
	if perm.count() != 1 || dangerRuns.count() != 0 || safeRuns.count() != 1 {
		t.Fatalf("perm=%d danger=%d safe=%d", perm.count(), dangerRuns.count(), safeRuns.count())
	}
	st := mem.get()
	if st.Status == ConversationStatusAwaitingApproval {
		t.Fatal("a hook deny resolves the gate without a human; the run must not pause")
	}
	var denied []string
	for _, item := range st.Messages {
		if o := item.FunctionCallOutputItem; o != nil && o.CallID == "call_danger" {
			denied = append(denied, phOutputString(*o))
		}
	}
	if len(denied) != 1 || !strings.Contains(denied[0], "blocked by policy") {
		t.Fatalf("expected exactly one synthesized rejection carrying the reason, got %v", denied)
	}
}

// upstream: "fails closed on schema-invalid arguments that no hook repairs"
func TestApprovalPredicateFailsClosedInLoop(t *testing.T) {
	var exec, predicate phCounter
	strict := phOKTool("strict", &exec, func(c *ToolConfig[phTargetInput]) {
		c.Approval = func(_ context.Context, call ParsedToolCall, _ TurnContext) (bool, error) {
			predicate.add(nil)
			args, _ := call.Arguments.(map[string]any)
			s, _ := args["target"].(string)
			return len(s) > 3, nil
		}
	})
	mem := &phMemState{}
	sender := phSender(phResp("resp_turn_0", phFnCall("call_strict", "strict", `{}`)), phTextResp("resp_final", "Recovered."))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "do the strict thing", Tools: []Tool{strict}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingApproval || len(st.PendingToolCalls) != 1 {
		t.Fatalf("expected awaiting_approval with one pending call, got %s %+v", st.Status, st.PendingToolCalls)
	}
	p := st.PendingToolCalls[0]
	if callKey(p) != "call_strict" || p.Name != "strict" || agrArgsJSON(t, p) != `{}` || p.PreToolUseApplied {
		t.Fatalf("pending call = %+v", p)
	}
	if exec.count() != 0 || predicate.count() != 0 || sender.calls != 1 {
		t.Fatalf("exec=%d predicate=%d calls=%d", exec.count(), predicate.count(), sender.calls)
	}
}

// upstream: "gates duplicate identical calls separately but only once across
// repeated response visits" and "gates the initial response only once when
// the stop condition fires on the first iteration"
func TestAllowFinalResponsePathGatesEachCallOnce(t *testing.T) {
	for _, n := range []int{1, 2} {
		ctx := context.Background()
		var runs, perm phCounter
		danger := phOKTool("danger", &runs, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
		hooks := NewHooksManager()
		agrOnPermission(hooks, &perm, PermissionDecisionAllow, "")
		calls := phResp("resp_turn_0")
		for i := 0; i < n; i++ {
			calls.Output = append(calls.Output, phFnCall("call_danger", "danger", `{"target":"prod"}`))
		}
		sender := phSender(calls, phTextResp("resp_final", "Done."))
		result, err := CallModel(ctx, sender, CallModelInput{
			Model: "openai/test", Input: "do it", Tools: []Tool{danger}, Hooks: hooks, StateAccessor: &phMemState{},
			StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		text, err := result.Text(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if perm.count() != n || runs.count() != n || text != "Done." {
			t.Fatalf("%d call(s): permission=%d runs=%d text=%q — one gate visit and one execution per call", n, perm.count(), runs.count(), text)
		}
	}
}

// upstream: "gates identical post-hook calls independently across responses"
// asserts the predicate sees the mutated input exactly once per call, and
// PermissionRequest derives riskLevel from the approval gate's *shape*
// (`typeof requireApproval === 'function'` => "high") without invoking it.
func TestApprovalPredicateInvokedOncePerGateDecision(t *testing.T) {
	ctx := context.Background()
	var exec, predicate phCounter
	tool := agrGuardedTool(&exec, &predicate)
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"dangerous": true}}
	})
	var risks phCounter
	hooks.OnPermissionRequest(HookEntry[PermissionRequestPayload, PermissionRequestResult]{Handler: func(p PermissionRequestPayload, _ LifecycleHookContext) (HookHandlerResult[PermissionRequestResult], error) {
		risks.add(string(p.RiskLevel))
		return SyncResult(PermissionRequestResult{Decision: PermissionDecisionAllow}), nil
	}})
	sender := phSender(phResp("r1", phFnCall("call_1", "guarded_action", `{"dangerous":false}`)), phTextResp("r2", "Done."))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "run", Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	var dangerous int
	for _, v := range predicate.values() {
		if v == `{"dangerous":true}` {
			dangerous++
		}
	}
	if dangerous != 1 {
		t.Fatalf("predicate must be consulted once for the mutated input, was %d times (%v)", dangerous, predicate.values())
	}
	if got := risks.values(); len(got) != 1 || got[0] != string(RiskLevelHigh) {
		t.Fatalf("a function-valued approval gate is high risk, got %v", got)
	}
}

// The schema derived from a struct must enforce its required fields, so the
// fail-closed approval gate and execute-time validation work for typed tools
// that do not pass an explicit InputSchema (upstream: z.object({target:
// z.string()}) rejects {} in every case of this file).
func TestDerivedSchemaEnforcesRequiredFields(t *testing.T) {
	ctx := context.Background()
	var ran, predicate phCounter
	tool := MustNewTool(ToolConfig[phTargetInput]{
		Name: "derived",
		Approval: func(context.Context, ParsedToolCall, TurnContext) (bool, error) {
			predicate.add(nil)
			return false, nil
		},
		Execute: func(context.Context, phTargetInput, ToolExecuteContext) (any, error) {
			ran.add(nil)
			return "ran", nil
		},
	})
	res, err := ExecuteTool(ctx, tool, ParsedToolCall{ID: "c", CallID: "c", Name: "derived", Arguments: map[string]any{}}, ToolExecuteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || ran.count() != 0 {
		t.Fatalf("missing required field must fail validation before the body runs: err=%v ran=%d", res.Error, ran.count())
	}
	requires, err := ToolRequiresApproval(ctx, tool, ParsedToolCall{ID: "c", Name: "derived", Arguments: map[string]any{}}, TurnContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !requires || predicate.count() != 0 {
		t.Fatalf("schema-invalid args must fail closed: requires=%v predicate=%d", requires, predicate.count())
	}
}
