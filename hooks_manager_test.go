package agent

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestHooksManagerOnEmitBasic(t *testing.T) {
	m := NewHooksManager()
	var seen PreToolUsePayload
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			seen = payload
			return VoidResult[PreToolUseResult](), nil
		},
	})

	result, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "search", ToolInput: map[string]any{"q": "go"}}, EmitOptions{ToolName: "search"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.ToolName != "search" {
		t.Fatalf("handler did not observe payload: %+v", seen)
	}
	if result.Blocked || result.Mutated {
		t.Fatalf("unexpected blocked/mutated: %+v", result)
	}
}

func TestHooksManagerPreToolUseMutation(t *testing.T) {
	m := NewHooksManager()
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			return SyncResult(PreToolUseResult{MutatedInput: map[string]any{"q": "mutated"}}), nil
		},
	})

	result, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "search", ToolInput: map[string]any{"q": "original"}}, EmitOptions{ToolName: "search"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Mutated {
		t.Fatalf("expected mutated=true")
	}
	if result.FinalPayload.ToolInput["q"] != "mutated" {
		t.Fatalf("expected mutated input to be piped into final payload, got %+v", result.FinalPayload)
	}
}

func TestHooksManagerPreToolUseBlock(t *testing.T) {
	m := NewHooksManager()
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			return SyncResult(PreToolUseResult{BlockReason: "not allowed"}), nil
		},
	})
	// A second handler must never run once the chain is blocked.
	var secondRan bool
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			secondRan = true
			return VoidResult[PreToolUseResult](), nil
		},
	})

	result, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "danger", ToolInput: map[string]any{}}, EmitOptions{ToolName: "danger"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Blocked {
		t.Fatalf("expected blocked=true")
	}
	if result.BlockReason != "not allowed" {
		t.Fatalf("expected block reason to surface, got %q", result.BlockReason)
	}
	if secondRan {
		t.Fatalf("chain should short-circuit on block")
	}
}

func TestHooksManagerToolMatcherFailsClosed(t *testing.T) {
	m := NewHooksManager()
	var ran bool
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Matcher: "search",
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			ran = true
			return VoidResult[PreToolUseResult](), nil
		},
	})

	// No ToolName in EmitOptions -> matcher fails closed, handler must not run.
	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "search"}, EmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatalf("matcher should fail closed without a toolName")
	}

	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "other"}, EmitOptions{ToolName: "other"}); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatalf("matcher should skip a non-matching tool name")
	}

	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "search"}, EmitOptions{ToolName: "search"}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatalf("matcher should run for a matching tool name")
	}
}

func TestHooksManagerFilterGate(t *testing.T) {
	m := NewHooksManager()
	var ran bool
	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Filter: func(p PreToolUsePayload) bool { return p.ToolInput["risky"] == true },
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			ran = true
			return VoidResult[PreToolUseResult](), nil
		},
	})

	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "t", ToolInput: map[string]any{}}, EmitOptions{ToolName: "t"}); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatalf("filter should skip when predicate is false")
	}

	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "t", ToolInput: map[string]any{"risky": true}}, EmitOptions{ToolName: "t"}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatalf("filter should run when predicate is true")
	}
}

func TestHooksManagerThrowOnHandlerError(t *testing.T) {
	boom := errors.New("boom")

	lenient := NewHooksManager()
	var secondRan bool
	lenient.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			return HookHandlerResult[PreToolUseResult]{}, boom
		},
	})
	lenient.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			secondRan = true
			return VoidResult[PreToolUseResult](), nil
		},
	})
	if _, err := lenient.EmitPreToolUse(PreToolUsePayload{ToolName: "t"}, EmitOptions{ToolName: "t"}); err != nil {
		t.Fatalf("default mode should not propagate handler errors: %v", err)
	}
	if !secondRan {
		t.Fatalf("default mode should continue the chain after a handler error")
	}

	strict := NewHooksManager(HooksManagerOptions{ThrowOnHandlerError: true})
	strict.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			return HookHandlerResult[PreToolUseResult]{}, boom
		},
	})
	if _, err := strict.EmitPreToolUse(PreToolUsePayload{ToolName: "t"}, EmitOptions{ToolName: "t"}); !errors.Is(err, boom) {
		t.Fatalf("strict mode should propagate the handler error, got %v", err)
	}
}

func TestHooksManagerSessionIDThreading(t *testing.T) {
	m := NewHooksManager()
	m.SetSessionID("default-session")

	var seenDefault, seenOverride string
	m.OnSessionStart(HookEntry[SessionStartPayload, EmptyHookResult]{
		Handler: func(payload SessionStartPayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			if seenDefault == "" {
				seenDefault = hctx.SessionID
			} else {
				seenOverride = hctx.SessionID
			}
			return VoidResult[EmptyHookResult](), nil
		},
	})

	if _, err := m.EmitSessionStart(SessionStartPayload{}, EmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if seenDefault != "default-session" {
		t.Fatalf("expected manager-level default session id, got %q", seenDefault)
	}

	if _, err := m.EmitSessionStart(SessionStartPayload{}, EmitOptions{SessionID: "run-specific"}); err != nil {
		t.Fatal(err)
	}
	if seenOverride != "run-specific" {
		t.Fatalf("expected per-emit session id override, got %q", seenOverride)
	}
}

func TestHooksManagerAsyncDrain(t *testing.T) {
	m := NewHooksManager()
	work := make(chan error)
	// handlerCompleted is written by the detached goroutine below and read by
	// this test goroutine, so it needs a mutex — without one this test races
	// and `go test -race` fails. The assertions are about ordering (emit
	// returns before the work settles; Drain waits for it), which the lock
	// preserves.
	var mu sync.Mutex
	handlerCompleted := false
	completed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return handlerCompleted
	}
	m.OnPostToolUse(HookEntry[PostToolUsePayload, EmptyHookResult]{
		Handler: func(payload PostToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			go func() {
				time.Sleep(10 * time.Millisecond)
				mu.Lock()
				handlerCompleted = true
				mu.Unlock()
				close(work)
			}()
			return AsyncResult[EmptyHookResult](AsyncOutput{Work: work}), nil
		},
	})

	if _, err := m.EmitPostToolUse(PostToolUsePayload{ToolName: "t"}, EmitOptions{ToolName: "t"}); err != nil {
		t.Fatal(err)
	}
	// Async handler must not block emit from returning.
	if completed() {
		t.Fatalf("async handler should not have completed synchronously")
	}
	m.Drain()
	if !completed() {
		t.Fatalf("Drain should wait for detached async work to settle")
	}
}

func TestHooksManagerRemoveAllAndUnsubscribe(t *testing.T) {
	m := NewHooksManager()
	var calls int
	unsubscribe := m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			calls++
			return VoidResult[PreToolUseResult](), nil
		},
	})
	if !m.HasHandlers(string(HookNamePreToolUse)) {
		t.Fatalf("expected registered handler to be visible via HasHandlers")
	}
	unsubscribe()
	if m.HasHandlers(string(HookNamePreToolUse)) {
		t.Fatalf("unsubscribe should remove the handler")
	}

	m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{
		Handler: func(payload PreToolUsePayload, hctx LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
			calls++
			return VoidResult[PreToolUseResult](), nil
		},
	})
	m.RemoveAll(string(HookNamePreToolUse))
	if m.HasHandlers(string(HookNamePreToolUse)) {
		t.Fatalf("RemoveAll should clear every handler for the hook")
	}
	if _, err := m.EmitPreToolUse(PreToolUsePayload{ToolName: "t"}, EmitOptions{ToolName: "t"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("expected no handler calls after removal, got %d", calls)
	}
}

func TestHooksManagerCustomHook(t *testing.T) {
	type customPayload struct{ Value int }
	type customResult struct{ Doubled int }

	m := NewHooksManager()
	unsubscribe := On(m, "CustomDoubler", HookEntry[customPayload, customResult]{
		Handler: func(payload customPayload, hctx LifecycleHookContext) (HookHandlerResult[customResult], error) {
			return SyncResult(customResult{Doubled: payload.Value * 2}), nil
		},
	})
	defer unsubscribe()

	result, err := Emit[customPayload, customResult](m, "CustomDoubler", customPayload{Value: 21}, EmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Doubled != 42 {
		t.Fatalf("expected custom hook result Doubled=42, got %+v", result.Results)
	}
}

func TestHooksManagerStopForceResumeAndAppendPrompt(t *testing.T) {
	m := NewHooksManager()
	m.OnStop(HookEntry[StopPayload, StopResult]{
		Handler: func(payload StopPayload, hctx LifecycleHookContext) (HookHandlerResult[StopResult], error) {
			return SyncResult(StopResult{ForceResume: true, AppendPrompt: "keep going"}), nil
		},
	})

	result, err := m.EmitStop(StopPayload{Reason: StopReasonMaxTurns}, EmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || !result.Results[0].ForceResume {
		t.Fatalf("expected ForceResume=true, got %+v", result.Results)
	}
	if result.Results[0].AppendPrompt != "keep going" {
		t.Fatalf("expected AppendPrompt to round-trip, got %q", result.Results[0].AppendPrompt)
	}
}

func TestMatchesTool(t *testing.T) {
	if !MatchesTool(nil, "anything") {
		t.Fatalf("nil matcher should match everything")
	}
	if !MatchesTool("search", "search") || MatchesTool("search", "other") {
		t.Fatalf("string matcher should require an exact match")
	}
	if !MatchesTool(func(name string) bool { return len(name) > 2 }, "search") {
		t.Fatalf("predicate matcher should evaluate the function")
	}
}
