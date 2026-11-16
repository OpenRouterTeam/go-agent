package agent

// Ports tests/unit/next-turn-params.test.ts, src/lib/next-turn-params.test.ts
// and tests/unit/next-turn-tool-choice-loop.test.ts. Loop-level cases assert
// the request actually dispatched to the sender, not the helper in isolation.

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

type ntpPattern struct {
	Pattern string `json:"pattern"`
}

type ntpDepth struct {
	Depth float64 `json:"depth"`
}

func ntpFloat(o optionalnullable.OptionalNullable[float64]) (float64, bool) { return o.GetOrZero() }

func ntpInstructions(r components.ResponsesRequest) (string, bool) { return r.Instructions.GetOrZero() }

func ntpAllowed(names ...string) map[string]any {
	tools := make([]any, 0, len(names))
	for _, n := range names {
		tools = append(tools, map[string]any{"type": "function", "name": n})
	}
	return map[string]any{"type": "allowed_tools", "mode": "auto", "tools": tools}
}

func ntpAllowedJSON(t *testing.T, names ...string) string {
	t.Helper()
	return phCanonicalJSON(t, phJSON(t, ntpAllowed(names...)))
}

// The parameters a tool computes reach the follow-up request after its call
// succeeds; the initial request is untouched.
func TestNextTurnParamsAppliedToFollowUpRequest(t *testing.T) {
	ctx := context.Background()
	search := MustNewTool(ToolConfig[ntpDepth]{
		Name:        "search",
		InputSchema: map[string]any{"type": "object", "required": []any{"depth"}, "properties": map[string]any{"depth": map[string]any{"type": "number"}}},
		Execute:     func(context.Context, ntpDepth, ToolExecuteContext) (any, error) { return "done", nil },
		NextTurnParams: NextTurnParamsFunctions{
			"temperature": func(_ context.Context, c NextTurnParamsContext) (any, error) {
				if c.ToolCall.Arguments.(map[string]any)["depth"].(float64) > 3 {
					return 0.1, nil
				}
				return 0.9, nil
			},
			"instructions": func(_ context.Context, c NextTurnParamsContext) (any, error) {
				base, _ := c.Request.Instructions.GetOrZero()
				return base + " depth=5", nil
			},
			"model":           func(context.Context, NextTurnParamsContext) (any, error) { return "openai/gpt-5", nil },
			"models":          func(context.Context, NextTurnParamsContext) (any, error) { return []string{"a/one", "b/two"}, nil },
			"maxOutputTokens": func(context.Context, NextTurnParamsContext) (any, error) { return 500, nil },
			"topP":            func(context.Context, NextTurnParamsContext) (any, error) { return 0.5, nil },
			"topK":            func(context.Context, NextTurnParamsContext) (any, error) { return 40, nil },
		},
	})
	sender := phSender(phResp("r1", phFnCall("call_1", "search", `{"depth":5}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{search}, AdditionalInstructions: "Base.",
		Request: components.ResponsesRequest{Temperature: optionalnullable.From(openrouter.Float64(0.7))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 {
		t.Fatalf("calls = %d", sender.calls)
	}
	first, follow := sender.requests[0], sender.requests[1]
	if v, _ := ntpFloat(first.Temperature); v != 0.7 || *first.Model != "openai/test" {
		t.Fatalf("initial request must be untouched: temp=%v model=%s", v, *first.Model)
	}
	if v, ok := ntpFloat(follow.Temperature); !ok || v != 0.1 {
		t.Fatalf("temperature = %v (%v), want 0.1", v, ok)
	}
	if s, _ := ntpInstructions(follow); s != "Base. depth=5" {
		t.Fatalf("instructions = %q, want composed %q", s, "Base. depth=5")
	}
	if follow.Model == nil || *follow.Model != "openai/gpt-5" {
		t.Fatalf("model = %v", follow.Model)
	}
	if !phEqualStrings(follow.Models, []string{"a/one", "b/two"}) {
		t.Fatalf("models = %v", follow.Models)
	}
	if n, _ := follow.MaxOutputTokens.GetOrZero(); n != 500 {
		t.Fatalf("maxOutputTokens = %d", n)
	}
	if v, _ := ntpFloat(follow.TopP); v != 0.5 {
		t.Fatalf("topP = %v", v)
	}
	if follow.TopK == nil || *follow.TopK != 40 {
		t.Fatalf("topK = %v", follow.TopK)
	}
}

// upstream: "composes: later tools see modifications made by earlier tools"
// — in tools-list order, independent of call order.
func TestNextTurnParamsComposeInToolsListOrder(t *testing.T) {
	ctx := context.Background()
	type empty struct{}
	mk := func(name string, fn func(context.Context, NextTurnParamsContext) (any, error)) Tool {
		return MustNewTool(ToolConfig[empty]{
			Name: name, InputSchema: map[string]any{"type": "object"},
			Execute:        func(context.Context, empty, ToolExecuteContext) (any, error) { return "done", nil },
			NextTurnParams: NextTurnParamsFunctions{"temperature": fn},
		})
	}
	first := mk("first", func(context.Context, NextTurnParamsContext) (any, error) { return 0.5, nil })
	second := mk("second", func(_ context.Context, c NextTurnParamsContext) (any, error) {
		v, _ := c.Request.Temperature.GetOrZero()
		return v + 0.3, nil
	})
	uncalled := mk("uncalled", func(context.Context, NextTurnParamsContext) (any, error) { return 0.9, nil })
	// The model calls `second` before `first`.
	sender := phSender(phResp("r1", phFnCall("c2", "second", `{}`), phFnCall("c1", "first", `{}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{first, second, uncalled},
		Request: components.ResponsesRequest{Temperature: optionalnullable.From(openrouter.Float64(0.7))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := ntpFloat(sender.requests[1].Temperature)
	if math.Abs(v-0.8) > 1e-9 {
		t.Fatalf("temperature = %v, want 0.8 (first sets 0.5, second sees it and adds 0.3; uncalled tools ignored)", v)
	}
}

// upstream: "runs a tool once per call when it was called multiple times"
func TestNextTurnParamsRunOncePerCall(t *testing.T) {
	ctx := context.Background()
	var invocations phCounter
	type empty struct{}
	counter := MustNewTool(ToolConfig[empty]{
		Name: "counter", InputSchema: map[string]any{"type": "object"},
		Execute: func(context.Context, empty, ToolExecuteContext) (any, error) { return "done", nil },
		NextTurnParams: NextTurnParamsFunctions{"maxOutputTokens": func(context.Context, NextTurnParamsContext) (any, error) {
			invocations.add(nil)
			return invocations.count(), nil
		}},
	})
	sender := phSender(phResp("r1", phFnCall("c1", "counter", `{}`), phFnCall("c2", "counter", `{}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{counter}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := sender.requests[1].MaxOutputTokens.GetOrZero(); invocations.count() != 2 || n != 2 {
		t.Fatalf("invocations=%d maxOutputTokens=%d, want 2/2", invocations.count(), n)
	}
}

// upstream #153 (next-turn-tool-choice-loop.test.ts "nextTurnParams only runs
// for calls whose tool body succeeded"): blocked, denied, invalid and errored
// calls never change the next request.
func TestNextTurnParamsOnlyForExecutedCalls(t *testing.T) {
	type scenario struct {
		name  string
		args  string
		exec  func() (any, error)
		gate  bool
		hooks func(*HooksManager)
	}
	scenarios := []scenario{
		{name: "PreToolUse blocks", args: `{"pattern":"weather"}`, hooks: func(m *HooksManager) {
			m.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Matcher: "tool_search", Handler: func(PreToolUsePayload, LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
				return SyncResult(PreToolUseResult{BlockReason: "blocked by policy"}), nil
			}})
		}},
		{name: "PermissionRequest denies", args: `{"pattern":"weather"}`, gate: true, hooks: func(m *HooksManager) {
			agrOnPermission(m, &phCounter{}, PermissionDecisionDeny, "no")
		}},
		{name: "arguments fail inputSchema", args: `{"pattern":7}`},
		{name: "execute errors", args: `{"pattern":"weather"}`, exec: func() (any, error) { return nil, errors.New("search backend unavailable") }},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			ctx := context.Background()
			var executed, computed phCounter
			search := MustNewTool(ToolConfig[ntpPattern]{
				Name: "tool_search", InputSchema: phObjectSchema("pattern"), RequireApproval: sc.gate,
				Execute: func(context.Context, ntpPattern, ToolExecuteContext) (any, error) {
					executed.add(nil)
					if sc.exec != nil {
						return sc.exec()
					}
					return map[string]any{"found": true}, nil
				},
				NextTurnParams: NextTurnParamsFunctions{
					"toolChoice": func(context.Context, NextTurnParamsContext) (any, error) {
						computed.add(nil)
						return ntpAllowed("tool_search", "get_weather"), nil
					},
					"temperature": func(context.Context, NextTurnParamsContext) (any, error) {
						computed.add(nil)
						return 0.01, nil
					},
				},
			})
			hooks := NewHooksManager()
			if sc.hooks != nil {
				sc.hooks(hooks)
			}
			sender := phSender(phResp("resp_tool", phFnCall("call_1", "tool_search", sc.args)), phTextResp("resp_text", "done"))
			result, err := CallModel(ctx, sender, CallModelInput{
				Model: "openai/test", Input: "hi", Tools: []Tool{search}, Hooks: hooks,
				Request: components.ResponsesRequest{ToolChoice: phChoice(t, phJSON(t, ntpAllowed("tool_search")))},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := result.Text(ctx); err != nil {
				t.Fatal(err)
			}
			if sc.exec == nil && executed.count() != 0 {
				t.Fatalf("the tool body must not run, ran %d", executed.count())
			}
			if computed.count() != 0 {
				t.Fatalf("nextTurnParams must not run for a call that did not succeed, ran %d", computed.count())
			}
			if sender.calls < 2 {
				t.Fatalf("expected a follow-up request, got %d", sender.calls)
			}
			if got := phWireChoice(t, sender, 1); got != ntpAllowedJSON(t, "tool_search") {
				t.Fatalf("follow-up tool choice must stay the caller's, got %s", got)
			}
			if _, ok := ntpFloat(sender.requests[1].Temperature); ok {
				t.Fatal("follow-up temperature must not be set")
			}
		})
	}
}

// next-turn-tool-choice-loop.test.ts: "reaches the dispatched follow-up
// request", "leaves the tools array untouched while the choice widens", and
// a tool-computed toolChoice becomes the policy for later turns.
func TestNextTurnParamsToolChoiceReachesDispatchedRequests(t *testing.T) {
	ctx := context.Background()
	search := MustNewTool(ToolConfig[ntpPattern]{
		Name: "tool_search", InputSchema: phObjectSchema("pattern"),
		Execute: func(context.Context, ntpPattern, ToolExecuteContext) (any, error) {
			return map[string]any{"found": []string{"tool_search", "get_weather"}}, nil
		},
		NextTurnParams: NextTurnParamsFunctions{"toolChoice": func(context.Context, NextTurnParamsContext) (any, error) {
			return ntpAllowed("tool_search", "get_weather"), nil
		}},
	})
	type empty struct{}
	weather := MustNewTool(ToolConfig[empty]{
		Name: "get_weather", InputSchema: map[string]any{"type": "object"},
		Execute: func(context.Context, empty, ToolExecuteContext) (any, error) { return map[string]any{"ok": true}, nil },
	})
	sender := phSender(
		phResp("resp_tool", phFnCall("call_1", "tool_search", `{"pattern":"weather"}`)),
		phResp("resp_weather", phFnCall("call_2", "get_weather", `{}`)),
		phTextResp("resp_text", "done"),
	)
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{search, weather},
		Request: components.ResponsesRequest{ToolChoice: phChoice(t, phJSON(t, ntpAllowed("tool_search")))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 3 {
		t.Fatalf("calls = %d", sender.calls)
	}
	narrow, wide := ntpAllowedJSON(t, "tool_search"), ntpAllowedJSON(t, "tool_search", "get_weather")
	for i, want := range []string{narrow, wide, wide} {
		if got := phWireChoice(t, sender, i); got != want {
			t.Fatalf("request %d tool choice = %s, want %s", i, got, want)
		}
	}
	for i := 1; i < 3; i++ {
		if !phEqualStrings(phToolNames(t, sender, i), phToolNames(t, sender, 0)) {
			t.Fatalf("tools array must be identical across turns: %v vs %v", phToolNames(t, sender, i), phToolNames(t, sender, 0))
		}
	}
}

// next-turn-tool-choice-loop.test.ts: "does not disturb toolChoice when no
// tool computes one"
func TestToolChoiceUndisturbedWithoutNextTurnParams(t *testing.T) {
	ctx := context.Background()
	plain := MustNewTool(ToolConfig[ntpPattern]{
		Name: "tool_search", InputSchema: phObjectSchema("pattern"),
		Execute: func(context.Context, ntpPattern, ToolExecuteContext) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	})
	sender := phSender(phResp("resp_tool", phFnCall("call_1", "tool_search", `{"pattern":"weather"}`)), phTextResp("resp_text", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{plain},
		Request: components.ResponsesRequest{ToolChoice: phChoice(t, phJSON(t, ntpAllowed("tool_search")))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < sender.calls; i++ {
		if got := phWireChoice(t, sender, i); got != ntpAllowedJSON(t, "tool_search") {
			t.Fatalf("request %d tool choice = %s", i, got)
		}
	}
}

// A forced toolChoice computed by nextTurnParams is itself one-shot: it
// forces the next turn, then relaxes once that turn called a tool.
func TestNextTurnParamsForcedToolChoiceIsOneShot(t *testing.T) {
	ctx := context.Background()
	search := MustNewTool(ToolConfig[ntpPattern]{
		Name: "tool_search", InputSchema: phObjectSchema("pattern"),
		Execute: func(context.Context, ntpPattern, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: NextTurnParamsFunctions{"toolChoice": func(context.Context, NextTurnParamsContext) (any, error) {
			return "required", nil
		}},
	})
	sender := phSender(
		phResp("r1", phFnCall("c1", "tool_search", `{"pattern":"a"}`)),
		phResp("r2", phFnCall("c2", "tool_search", `{"pattern":"b"}`)),
		phTextResp("r3", "done"),
	)
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{search}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"", tcrRequired, tcrAuto} {
		if got := phWireChoice(t, sender, i); got != want {
			t.Fatalf("request %d tool choice = %q, want %q", i, got, want)
		}
	}
}

// next-turn-tool-choice-loop.test.ts: "passes the PreToolUse-mutated input
// that the tool ran on"
func TestNextTurnParamsSeePreToolUseMutatedInput(t *testing.T) {
	ctx := context.Background()
	var seen phCounter
	search := MustNewTool(ToolConfig[ntpPattern]{
		Name: "tool_search", InputSchema: phObjectSchema("pattern"),
		Execute: func(_ context.Context, in ntpPattern, _ ToolExecuteContext) (any, error) {
			return map[string]any{"found": []string{in.Pattern}}, nil
		},
		NextTurnParams: NextTurnParamsFunctions{"toolChoice": func(_ context.Context, c NextTurnParamsContext) (any, error) {
			args := c.ToolCall.Arguments.(map[string]any)
			seen.add(phCopyAny(args))
			if c.Result.Error != nil {
				t.Errorf("nextTurnParams received a failed result: %v", c.Result.Error)
			}
			return ntpAllowed("tool_search", args["pattern"].(string)), nil
		}},
	})
	hooks := NewHooksManager()
	agrOnPreToolUse(hooks, &phCounter{}, func(PreToolUsePayload) PreToolUseResult {
		return PreToolUseResult{MutatedInput: map[string]any{"pattern": "get_weather"}}
	})
	sender := phSender(phResp("resp_tool", phFnCall("call_1", "tool_search", `{"pattern":"weather"}`)), phTextResp("resp_text", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{search}, Hooks: hooks,
		Request: components.ResponsesRequest{ToolChoice: phChoice(t, phJSON(t, ntpAllowed("tool_search")))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(ctx); err != nil {
		t.Fatal(err)
	}
	if got := seen.values(); len(got) != 1 || got[0] != `{"pattern":"get_weather"}` {
		t.Fatalf("nextTurnParams must see the mutated input once, saw %v", got)
	}
	if got := phWireChoice(t, sender, 1); got != ntpAllowedJSON(t, "tool_search", "get_weather") {
		t.Fatalf("follow-up tool choice = %s", got)
	}
}

// nextTurnParams from the halted turn apply to the AllowFinalResponse final
// request (tool_choice is still forced to none there).
func TestNextTurnParamsAppliedBeforeFinalResponseRequest(t *testing.T) {
	ctx := context.Background()
	search := MustNewTool(ToolConfig[ntpPattern]{
		Name: "tool_search", InputSchema: phObjectSchema("pattern"),
		Execute: func(context.Context, ntpPattern, ToolExecuteContext) (any, error) { return "ok", nil },
		NextTurnParams: NextTurnParamsFunctions{
			"temperature":  func(context.Context, NextTurnParamsContext) (any, error) { return 0.2, nil },
			"instructions": func(context.Context, NextTurnParamsContext) (any, error) { return "Summarize.", nil },
		},
	})
	sender := phSender(phResp("r1", phFnCall("c1", "tool_search", `{"pattern":"a"}`)), phTextResp("r2", "final"))
	result, err := CallModel(ctx, sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{search},
		StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(ctx); err != nil || text != "final" {
		t.Fatalf("text = %q, %v", text, err)
	}
	final := sender.requests[1]
	if v, _ := ntpFloat(final.Temperature); v != 0.2 {
		t.Fatalf("final request temperature = %v, want 0.2", v)
	}
	if s, _ := ntpInstructions(final); s != "Summarize." {
		t.Fatalf("final request instructions = %q", s)
	}
	if got := phWireChoice(t, sender, 1); got != tcrNone {
		t.Fatalf("final request tool choice = %s", got)
	}
}

// --- ExecuteNextTurnParamsFunctions / ApplyNextTurnParamsToRequest units ---

// upstream: "warns and skips invalid nextTurnParams keys outside production"
// and "ignores non-function values in nextTurnParams" (a nil func in Go).
func TestExecuteNextTurnParamsSkipsInvalidKeysAndNilFuncs(t *testing.T) {
	out, err := ExecuteNextTurnParamsFunctions(context.Background(), NextTurnParamsFunctions{
		"bogusKey":    func(context.Context, NextTurnParamsContext) (any, error) { return 42, nil },
		"temperature": func(context.Context, NextTurnParamsContext) (any, error) { return 0.3, nil },
		"topP":        nil,
	}, BuildNextTurnParamsContext(ParsedToolCall{Name: "odd"}, ToolExecutionResult{}, components.ResponsesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out["temperature"] != 0.3 {
		t.Fatalf("computed = %v, want only temperature", out)
	}
}

// Functions for one call compose: a later key sees an earlier key's value
// already applied to the context request.
func TestExecuteNextTurnParamsComposeWithinCall(t *testing.T) {
	out, err := ExecuteNextTurnParamsFunctions(context.Background(), NextTurnParamsFunctions{
		"instructions": func(context.Context, NextTurnParamsContext) (any, error) { return "first", nil },
		"model": func(_ context.Context, c NextTurnParamsContext) (any, error) {
			s, _ := c.Request.Instructions.GetOrZero()
			return "model-for-" + s, nil
		},
	}, BuildNextTurnParamsContext(ParsedToolCall{Name: "t"}, ToolExecutionResult{}, components.ResponsesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "model-for-first" {
		t.Fatalf("model = %v; keys compose in sorted order (instructions before model)", out["model"])
	}
	_, err = ExecuteNextTurnParamsFunctions(context.Background(), NextTurnParamsFunctions{
		"temperature": func(context.Context, NextTurnParamsContext) (any, error) { return nil, errors.New("boom") },
	}, NextTurnParamsContext{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a function error must propagate, got %v", err)
	}
}

func ntpBase() components.ResponsesRequest {
	return components.ResponsesRequest{Model: openrouter.String("openai/gpt-4")}
}

// src/lib/next-turn-params.test.ts applyNextTurnParamsToRequest cases.
func TestApplyNextTurnParamsToRequest(t *testing.T) {
	t.Run("passes through non-null values", func(t *testing.T) {
		r := ApplyNextTurnParamsToRequest(ntpBase(), map[string]any{"temperature": 0.5, "maxOutputTokens": 1000, "instructions": "Be helpful"})
		if v, _ := ntpFloat(r.Temperature); v != 0.5 {
			t.Fatalf("temperature = %v", v)
		}
		if n, _ := r.MaxOutputTokens.GetOrZero(); n != 1000 {
			t.Fatalf("maxOutputTokens = %d", n)
		}
		if s, _ := ntpInstructions(r); s != "Be helpful" {
			t.Fatalf("instructions = %q", s)
		}
	})
	t.Run("null clears", func(t *testing.T) {
		base := ntpBase()
		base.Temperature = optionalnullable.From(openrouter.Float64(0.7))
		base.TopP = optionalnullable.From(openrouter.Float64(0.9))
		r := ApplyNextTurnParamsToRequest(base, map[string]any{"temperature": nil, "maxOutputTokens": nil, "instructions": nil, "topP": 0.5})
		if _, ok := ntpFloat(r.Temperature); ok {
			t.Fatal("temperature must be cleared")
		}
		if _, ok := r.MaxOutputTokens.GetOrZero(); ok {
			t.Fatal("maxOutputTokens must be cleared")
		}
		if _, ok := ntpInstructions(r); ok {
			t.Fatal("instructions must be cleared")
		}
		if v, _ := ntpFloat(r.TopP); v != 0.5 {
			t.Fatalf("topP = %v", v)
		}
	})
	t.Run("preserves untouched fields", func(t *testing.T) {
		base := ntpBase()
		base.Model = openrouter.String("anthropic/claude-3")
		base.Temperature = optionalnullable.From(openrouter.Float64(0.9))
		r := ApplyNextTurnParamsToRequest(base, map[string]any{"maxOutputTokens": 500})
		if v, _ := ntpFloat(r.Temperature); *r.Model != "anthropic/claude-3" || v != 0.9 {
			t.Fatalf("model=%s temperature=%v", *r.Model, v)
		}
		if r2 := ApplyNextTurnParamsToRequest(base, map[string]any{}); *r2.Model != "anthropic/claude-3" {
			t.Fatal("empty params must not change the request")
		}
	})
	t.Run("zero and empty string are values", func(t *testing.T) {
		r := ApplyNextTurnParamsToRequest(ntpBase(), map[string]any{"temperature": 0, "maxOutputTokens": 0, "instructions": ""})
		if v, ok := ntpFloat(r.Temperature); !ok || v != 0 {
			t.Fatalf("temperature = %v (%v)", v, ok)
		}
		if n, ok := r.MaxOutputTokens.GetOrZero(); !ok || n != 0 {
			t.Fatalf("maxOutputTokens = %d (%v)", n, ok)
		}
		if s, ok := ntpInstructions(r); !ok || s != "" {
			t.Fatalf("instructions = %q (%v)", s, ok)
		}
	})
	t.Run("allowed_tools toolChoice replaced, tools untouched", func(t *testing.T) {
		base := ntpBase()
		base.ToolChoice = phChoice(t, phJSON(t, ntpAllowed("tool_search")))
		base.Tools = []components.ResponsesRequestToolUnion{functionToolDefinition("tool_search", "", map[string]any{}, nil, nil), functionToolDefinition("get_weather", "", map[string]any{}, nil, nil)}
		before := phJSON(t, base.Tools)
		r := ApplyNextTurnParamsToRequest(base, map[string]any{"toolChoice": ntpAllowed("tool_search", "get_weather")})
		if got := phCanonicalJSON(t, phJSON(t, r.ToolChoice)); got != ntpAllowedJSON(t, "tool_search", "get_weather") {
			t.Fatalf("toolChoice = %s", got)
		}
		if phJSON(t, r.Tools) != before {
			t.Fatal("tools must be untouched")
		}
		r = ApplyNextTurnParamsToRequest(base, map[string]any{"temperature": 0.2})
		if got := phCanonicalJSON(t, phJSON(t, r.ToolChoice)); got != ntpAllowedJSON(t, "tool_search") {
			t.Fatalf("toolChoice must stay when not computed, got %s", got)
		}
	})
	t.Run("unsupported value types are ignored", func(t *testing.T) {
		base := ntpBase()
		base.Temperature = optionalnullable.From(openrouter.Float64(0.7))
		r := ApplyNextTurnParamsToRequest(base, map[string]any{"temperature": "hot", "model": 42})
		if v, _ := ntpFloat(r.Temperature); v != 0.7 || *r.Model != "openai/gpt-4" {
			t.Fatalf("temperature=%v model=%s", v, *r.Model)
		}
	})
}

// upstream "should convert null values to undefined": a null parameter is
// dropped from the request, so nothing (not an explicit null) reaches the wire.
func TestApplyNextTurnParamsNullOmittedOnWire(t *testing.T) {
	base := ntpBase()
	base.Temperature = optionalnullable.From(openrouter.Float64(0.7))
	r := ApplyNextTurnParamsToRequest(base, map[string]any{"temperature": nil, "maxOutputTokens": nil, "instructions": nil, "topP": nil})
	tier := components.ResponsesRequestServiceTierAuto
	r.ServiceTier = optionalnullable.From(&tier) // SDK marshalling workaround, as CallModel does
	wire := phJSONMap(t, r)
	for _, key := range []string{"temperature", "max_output_tokens", "instructions", "top_p"} {
		if v, ok := wire[key]; ok {
			t.Fatalf("a cleared %s must be omitted from the wire, got %v", key, v)
		}
	}
}
