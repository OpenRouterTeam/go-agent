package agent

// Ports upstream packages/agent/tests/unit/tool-set.test.ts (runtime cases;
// expectTypeOf-only cases have no Go analogue) and the tool-set-level
// activeTools filtering cases of call-model-active-tools.test.ts.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func tsMakeTool(name string) Tool {
	return MustNewTool(ToolConfig[struct{}]{
		Name:        name,
		Description: name + " tool",
		Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
			return map[string]any{"name": name}, nil
		},
	})
}

func tsWebSearchConfig() components.ResponsesRequestToolUnion {
	return components.CreateResponsesRequestToolUnionWebSearch20250826(
		components.WebSearchServerTool{Type: components.WebSearchServerToolTypeWebSearch20250826},
	)
}

func tsDatetimeConfig() components.ResponsesRequestToolUnion {
	return components.CreateResponsesRequestToolUnionOpenrouterDatetime(
		components.DatetimeServerTool{Type: components.DatetimeServerToolTypeOpenrouterDatetime},
	)
}

// tsHandWrittenServerTool is a server Tool implementation that does not expose
// ServerToolID (upstream: a hand-written `{ _brand, config }` without `id`).
// Embedding the Tool interface (not serverToolImpl) hides ServerToolID.
type tsHandWrittenServerTool struct{ Tool }

var (
	tsA = tsMakeTool("a")
	tsB = tsMakeTool("b")
	tsC = tsMakeTool("c")

	tsWebSearch    = NewServerTool(ServerToolConfig{Name: "web_search", Config: tsWebSearchConfig()})
	tsDatetime     = NewServerTool(ServerToolConfig{Name: "datetime", Config: tsDatetimeConfig()})
	tsPublicSearch = NewServerTool(ServerToolConfig{Name: "public_search", Config: tsWebSearchConfig(), ID: "server:public_search"})
)

func always(v bool) ActivationPredicate {
	return func(context.Context, ActivationInput) (bool, error) { return v, nil }
}

func ctxFlag(key string) ActivationPredicate {
	return func(_ context.Context, in ActivationInput) (bool, error) {
		return in.Context[key] == true, nil
	}
}

func mustToolSet(t *testing.T, cfg ToolSetConfig) *ToolSet {
	t.Helper()
	ts, err := NewToolSet(cfg)
	if err != nil {
		t.Fatalf("NewToolSet: %v", err)
	}
	return ts
}

// must adapts a (*ToolSet, error) result: must(t)(ts.Deactivate("a")).
func must(t *testing.T) func(*ToolSet, error) *ToolSet {
	return func(ts *ToolSet, err error) *ToolSet {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return ts
	}
}

func resolve(t *testing.T, ts *ToolSet, in ActivationInput) ResolvedToolSet {
	t.Helper()
	r, err := ts.Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return r
}

func resolveSit(t *testing.T, ts *ToolSet, name string, in ActivationInput) ResolvedToolSet {
	t.Helper()
	r, err := ts.ResolveSituation(context.Background(), name, in)
	if err != nil {
		t.Fatalf("ResolveSituation(%q): %v", name, err)
	}
	return r
}

func eqStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", label, got, want)
	}
}

func eqTools(t *testing.T, label string, got, want []Tool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len %d (%q), want %d (%q)", label, len(got), tsIDs(got), len(want), tsIDs(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q", label, i, toolSetID(got[i]), toolSetID(want[i]))
		}
	}
}

func tsIDs(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, toolSetID(tl))
	}
	return out
}

func wantErr(t *testing.T, err, sentinel error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error wrapping %v containing %q, got nil", sentinel, substr)
	}
	if sentinel != nil && !errors.Is(err, sentinel) {
		t.Fatalf("error %v does not wrap %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}

func TestToolSetConstruction(t *testing.T) {
	t.Run("preserves tool order via Tools", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}})
		eqTools(t, "Tools", ts.Tools(), []Tool{tsA, tsB, tsC})
	})
	t.Run("Tools returns a defensive copy", func(t *testing.T) {
		in := []Tool{tsA, tsB}
		ts := mustToolSet(t, ToolSetConfig{Tools: in})
		in[0] = tsC
		got := ts.Tools()
		got[1] = tsC
		eqTools(t, "Tools", ts.Tools(), []Tool{tsA, tsB})
	})
	t.Run("errors on duplicate tool names", func(t *testing.T) {
		_, err := NewToolSet(ToolSetConfig{Tools: []Tool{tsA, tsMakeTool("a")}})
		wantErr(t, err, ErrDuplicateToolID, `duplicate tool ID: "a"`)
	})
	t.Run("errors on nil tool", func(t *testing.T) {
		_, err := NewToolSet(ToolSetConfig{Tools: []Tool{tsA, nil}})
		wantErr(t, err, ErrNilTool, "index 1")
	})
	t.Run("constructs an empty set", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{})
		if len(ts.Tools()) != 0 {
			t.Fatal("expected no tools")
		}
		r := resolve(t, ts, ActivationInput{})
		if len(r.Tools) != 0 || len(r.ActiveTools) != 0 || len(r.Enabled) != 0 || len(r.Disabled) != 0 || len(r.StatusByTool) != 0 {
			t.Fatalf("expected empty snapshot, got %+v", r)
		}
		if r.ActiveTools == nil || r.Tools == nil || r.StatusByTool == nil {
			t.Fatal("snapshot slices/maps must be non-nil (empty ActiveTools still filters)")
		}
	})
	t.Run("defaults all tools to active", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}})
		r := resolve(t, ts, ActivationInput{})
		eqTools(t, "Tools", r.Tools, []Tool{tsA, tsB})
		eqStrings(t, "ActiveTools", r.ActiveTools, []string{"a", "b"})
		eqStrings(t, "Enabled", r.Enabled, []string{"a", "b"})
		eqStrings(t, "Disabled", r.Disabled, nil)
		want := map[string]ToolStatusEntry{
			"a": {Enabled: true, Reason: StatusReasonDefault},
			"b": {Enabled: true, Reason: StatusReasonDefault},
		}
		if !reflect.DeepEqual(r.StatusByTool, want) {
			t.Fatalf("StatusByTool = %+v, want %+v", r.StatusByTool, want)
		}
	})
}

func TestToolSetDirectives(t *testing.T) {
	base := func(t *testing.T) *ToolSet {
		return mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}})
	}
	cases := []struct {
		name       string
		build      func(t *testing.T, ts *ToolSet) *ToolSet
		in         ActivationInput
		wantActive []string
	}{
		{"deactivates a single tool", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.Deactivate("b"))
		}, ActivationInput{}, []string{"a", "c"}},
		{"activates/deactivates lists", func(t *testing.T, ts *ToolSet) *ToolSet {
			ts = must(t)(ts.Deactivate("a", "b"))
			return must(t)(ts.Activate("b"))
		}, ActivationInput{}, []string{"b", "c"}},
		{"activateWhen defaults inactive", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.ActivateWhen("a", ctxFlag("enabled")))
		}, ActivationInput{}, []string{"b", "c"}},
		{"activateWhen flips on predicate", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.ActivateWhen("a", ctxFlag("enabled")))
		}, ActivationInput{Context: map[string]any{"enabled": true}}, []string{"a", "b", "c"}},
		{"activateWhen predicate map", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.ActivateWhenMap(map[string]ActivationPredicate{"a": always(true), "b": always(false)}))
		}, ActivationInput{}, []string{"a", "c"}},
		{"activateWhen map skips nil predicates", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.ActivateWhenMap(map[string]ActivationPredicate{"a": nil, "b": always(false)}))
		}, ActivationInput{}, []string{"a", "c"}},
		{"deactivateWhen defaults active, flips off", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.DeactivateWhen("a", always(true)))
		}, ActivationInput{}, []string{"b", "c"}},
		{"deactivateWhen predicate map", func(t *testing.T, ts *ToolSet) *ToolSet {
			return must(t)(ts.DeactivateWhenMap(map[string]ActivationPredicate{"a": always(true), "b": always(false)}))
		}, ActivationInput{}, []string{"b", "c"}},
		{"last call wins: activate then deactivateWhen", func(t *testing.T, ts *ToolSet) *ToolSet {
			ts = must(t)(ts.Activate("a"))
			return must(t)(ts.DeactivateWhen("a", always(true)))
		}, ActivationInput{}, []string{"b", "c"}},
		{"last call wins: deactivateWhen then activate", func(t *testing.T, ts *ToolSet) *ToolSet {
			ts = must(t)(ts.DeactivateWhen("a", always(true)))
			return must(t)(ts.Activate("a"))
		}, ActivationInput{}, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := tc.build(t, base(t))
			eqStrings(t, "ActiveTools", resolve(t, ts, tc.in).ActiveTools, tc.wantActive)
		})
	}
}

func TestToolSetDirectiveErrors(t *testing.T) {
	ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}})
	cases := []struct {
		name     string
		call     func() (*ToolSet, error)
		sentinel error
		substr   string
	}{
		{"activate unknown", func() (*ToolSet, error) { return ts.Activate("missing") }, ErrUnknownTool, `unknown tool: "missing"`},
		{"deactivate list with unknown", func() (*ToolSet, error) { return ts.Deactivate("a", "missing") }, ErrUnknownTool, `unknown tool: "missing"`},
		{"activateWhen unknown", func() (*ToolSet, error) { return ts.ActivateWhen("missing", always(true)) }, ErrUnknownTool, `"missing"`},
		{"activateWhen map unknown", func() (*ToolSet, error) {
			return ts.ActivateWhenMap(map[string]ActivationPredicate{"a": always(true), "nope": always(true)})
		}, ErrUnknownTool, `unknown tool: "nope"`},
		{"deactivateWhen map unknown", func() (*ToolSet, error) {
			return ts.DeactivateWhenMap(map[string]ActivationPredicate{"nope": always(true)})
		}, ErrUnknownTool, `"nope"`},
		{"activateWhen nil predicate", func() (*ToolSet, error) { return ts.ActivateWhen("a", nil) }, ErrMissingPredicate, "requires a predicate"},
		{"deactivateWhen nil predicate", func() (*ToolSet, error) { return ts.DeactivateWhen("a", nil) }, ErrMissingPredicate, "requires a predicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call()
			wantErr(t, err, tc.sentinel, tc.substr)
			if got != nil {
				t.Fatal("expected nil ToolSet on error")
			}
		})
	}
	// Validation happens before applying: the original stays untouched.
	eqStrings(t, "ActiveTools", resolve(t, ts, ActivationInput{}).ActiveTools, []string{"a", "b"})

	t.Run("mutable set is untouched by a failed batch", func(t *testing.T) {
		m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}, Mutable: true})
		if _, err := m.Deactivate("a", "missing"); err == nil {
			t.Fatal("expected error")
		}
		eqStrings(t, "ActiveTools", resolve(t, m, ActivationInput{}).ActiveTools, []string{"a", "b"})
	})
}

func TestToolSetImmutabilityAndClone(t *testing.T) {
	t.Run("immutable by default: mutators return a new instance", func(t *testing.T) {
		base := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}})
		next := must(t)(base.Deactivate("a"))
		if next == base {
			t.Fatal("immutable mutator returned the receiver")
		}
		if base.Mutable() || next.Mutable() {
			t.Fatal("expected immutable sets")
		}
		eqStrings(t, "base", resolve(t, base, ActivationInput{}).ActiveTools, []string{"a", "b"})
		eqStrings(t, "next", resolve(t, next, ActivationInput{}).ActiveTools, []string{"b"})
	})
	t.Run("immutable chain yields distinct instances", func(t *testing.T) {
		base := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}})
		d := must(t)(base.Deactivate("b"))
		w := must(t)(d.ActivateWhen("a", always(true)))
		s := must(t)(w.DefineSituations(map[string]SituationConfig{"x": {}}))
		if d == base || w == d || s == w {
			t.Fatal("immutable steps must be distinct instances")
		}
		if _, err := w.ResolveSituation(context.Background(), "x", ActivationInput{}); !errors.Is(err, ErrUnknownSituation) {
			t.Fatalf("DefineSituations leaked into source: %v", err)
		}
	})
	t.Run("mutates in place when Mutable", func(t *testing.T) {
		base := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}, Mutable: true})
		alias := base
		steps := []*ToolSet{
			must(t)(alias.Activate("a")),
			must(t)(alias.Deactivate("b")),
			must(t)(alias.ActivateWhen("c", always(true))),
			must(t)(alias.DeactivateWhen("c", always(false))),
			must(t)(alias.ActivateWhenMap(map[string]ActivationPredicate{"c": always(true)})),
			must(t)(alias.DeactivateWhenMap(map[string]ActivationPredicate{"c": always(false)})),
			must(t)(alias.DefineSituations(map[string]SituationConfig{"s": {Disabled: []string{"a"}}})),
		}
		for i, s := range steps {
			if s != base {
				t.Fatalf("step %d did not return the receiver", i)
			}
		}
		eqStrings(t, "ActiveTools", resolve(t, base, ActivationInput{}).ActiveTools, []string{"a", "c"})
		eqStrings(t, "situation", resolveSit(t, base, "s", ActivationInput{}).ActiveTools, []string{"c"})
	})
	t.Run("clone copies state and can flip mode", func(t *testing.T) {
		immutable := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}}).Deactivate("a"))
		mutableCopy := immutable.CloneAs(true)
		if !mutableCopy.Mutable() {
			t.Fatal("expected mutable clone")
		}
		if got := must(t)(mutableCopy.Activate("a")); got != mutableCopy {
			t.Fatal("mutable clone must mutate in place")
		}
		eqStrings(t, "clone", resolve(t, mutableCopy, ActivationInput{}).ActiveTools, []string{"a", "b"})
		eqStrings(t, "original", resolve(t, immutable, ActivationInput{}).ActiveTools, []string{"b"})
	})
	t.Run("clone inherits mode when not overridden", func(t *testing.T) {
		m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}, Mutable: true})
		clone := m.Clone()
		if clone == m {
			t.Fatal("clone must be a new instance")
		}
		after := must(t)(clone.Deactivate("a"))
		if after != clone {
			t.Fatal("clone of a mutable set must stay mutable")
		}
		// The source is independent of the clone.
		eqStrings(t, "source", resolve(t, m, ActivationInput{}).ActiveTools, []string{"a"})
	})
	t.Run("clone to immutable from mutable", func(t *testing.T) {
		m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}, Mutable: true})
		must(t)(m.DefineSituations(map[string]SituationConfig{"s": {Disabled: []string{"b"}}}))
		im := m.CloneAs(false)
		must(t)(m.Deactivate("a"))
		must(t)(m.DefineSituations(nil))
		if next := must(t)(im.Deactivate("b")); next == im {
			t.Fatal("immutable clone returned receiver")
		}
		eqStrings(t, "immutable clone", resolve(t, im, ActivationInput{}).ActiveTools, []string{"a", "b"})
		eqStrings(t, "immutable clone situation", resolveSit(t, im, "s", ActivationInput{}).ActiveTools, []string{"a"})
	})
	t.Run("mutable alias observes mutations through another alias", func(t *testing.T) {
		m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}, Mutable: true})
		alias := m
		must(t)(m.Deactivate("a"))
		eqStrings(t, "alias", resolve(t, alias, ActivationInput{}).ActiveTools, []string{"b"})
	})
}

func TestToolSetMutableConcurrentUse(t *testing.T) {
	m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}, Mutable: true})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = m.Deactivate("a")
			} else {
				_, err = m.ActivateWhen("b", always(true))
			}
			if err == nil {
				_, err = m.DefineSituations(map[string]SituationConfig{"s": {Enabled: []string{"c"}}})
			}
			errs <- err
		}(i)
		go func() {
			defer wg.Done()
			_, err := m.Resolve(context.Background(), ActivationInput{})
			if err == nil {
				_ = m.Clone()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	eqStrings(t, "ActiveTools", resolve(t, m, ActivationInput{}).ActiveTools, []string{"b", "c"})
}

func TestToolSetResolveInput(t *testing.T) {
	t.Run("zero input reaches predicate with nil state and context", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}}).ActivateWhen("a",
			func(_ context.Context, in ActivationInput) (bool, error) {
				return in.State == nil && in.Context == nil, nil
			}))
		eqStrings(t, "ActiveTools", resolve(t, ts, ActivationInput{}).ActiveTools, []string{"a"})
	})
	t.Run("passes state, shared context, and ctx to the predicate", func(t *testing.T) {
		type key struct{}
		state := &ConversationState{ID: "conv_test", Status: ConversationStatusComplete}
		shared := map[string]any{"foo": "bar"}
		var calls int
		var got ActivationInput
		var gotCtx any
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}}).ActivateWhen("a",
			func(ctx context.Context, in ActivationInput) (bool, error) {
				calls++
				got = in
				gotCtx = ctx.Value(key{})
				return true, nil
			}))
		ctx := context.WithValue(context.Background(), key{}, "v")
		if _, err := ts.Resolve(ctx, ActivationInput{State: state, Context: shared}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || got.State != state || !reflect.DeepEqual(got.Context, shared) || gotCtx != "v" {
			t.Fatalf("predicate got calls=%d input=%+v ctx=%v", calls, got, gotCtx)
		}
	})
	t.Run("predicate errors abort resolution", func(t *testing.T) {
		boom := errors.New("boom")
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}}).DeactivateWhen("b",
			func(context.Context, ActivationInput) (bool, error) { return false, boom }))
		_, err := ts.Resolve(context.Background(), ActivationInput{})
		wantErr(t, err, boom, `"b"`)
	})
	t.Run("InferTools is an alias of Resolve", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}}).Deactivate("a"))
		viaResolve := resolve(t, ts, ActivationInput{})
		viaInfer, err := ts.InferTools(context.Background(), ActivationInput{})
		if err != nil {
			t.Fatal(err)
		}
		eqTools(t, "Tools", viaInfer.Tools, viaResolve.Tools)
		eqStrings(t, "ActiveTools", viaInfer.ActiveTools, viaResolve.ActiveTools)
		eqStrings(t, "Enabled", viaInfer.Enabled, viaResolve.Enabled)
		eqStrings(t, "Disabled", viaInfer.Disabled, viaResolve.Disabled)
		if !reflect.DeepEqual(viaInfer.StatusByTool, viaResolve.StatusByTool) {
			t.Fatal("StatusByTool differs")
		}
	})
}

func TestToolSetStatusByTool(t *testing.T) {
	t.Run("exhaustive with reason/directive/predicate metadata", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}}).Deactivate("b"))
		ts = must(t)(ts.ActivateWhen("c", always(true)))
		r := resolve(t, ts, ActivationInput{})
		want := map[string]ToolStatusEntry{
			"a": {Enabled: true, Reason: StatusReasonDefault},
			"b": {Enabled: false, Reason: StatusReasonDeactivate, Directive: ToolDirectiveDeactivate},
			"c": {Enabled: true, Reason: StatusReasonActivateWhen, Directive: ToolDirectiveActivateWhen, Predicate: true},
		}
		if !reflect.DeepEqual(r.StatusByTool, want) {
			t.Fatalf("StatusByTool = %+v", r.StatusByTool)
		}
		eqStrings(t, "Enabled", r.Enabled, []string{"a", "c"})
		eqStrings(t, "Disabled", r.Disabled, []string{"b"})
	})
	t.Run("prototype-sensitive IDs are ordinary keys", func(t *testing.T) {
		proto, ctor, prototype := tsMakeTool("__proto__"), tsMakeTool("constructor"), tsMakeTool("prototype")
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, proto, ctor, prototype, tsB}}).Deactivate("constructor"))
		r := resolve(t, ts, ActivationInput{})
		if len(r.StatusByTool) != 5 {
			t.Fatalf("StatusByTool has %d keys", len(r.StatusByTool))
		}
		if r.StatusByTool["constructor"] != (ToolStatusEntry{Enabled: false, Reason: StatusReasonDeactivate, Directive: ToolDirectiveDeactivate}) {
			t.Fatalf("constructor = %+v", r.StatusByTool["constructor"])
		}
		for _, id := range []string{"a", "b", "__proto__", "prototype"} {
			if r.StatusByTool[id] != (ToolStatusEntry{Enabled: true, Reason: StatusReasonDefault}) {
				t.Fatalf("%s = %+v", id, r.StatusByTool[id])
			}
		}
		eqStrings(t, "Enabled", r.Enabled, []string{"a", "__proto__", "prototype", "b"})
		eqStrings(t, "Disabled", r.Disabled, []string{"constructor"})
		eqTools(t, "Tools", r.Tools, []Tool{tsA, proto, prototype, tsB})
		eqStrings(t, "ActiveTools", r.ActiveTools, []string{"a", "__proto__", "prototype", "b"})
	})
	t.Run("prototype-sensitive IDs via activate/deactivate/activateWhen", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsMakeTool("__proto__"), tsMakeTool("constructor"), tsMakeTool("prototype")}})
		ts = must(t)(ts.Activate("__proto__"))
		ts = must(t)(ts.Deactivate("prototype"))
		ts = must(t)(ts.ActivateWhen("constructor", always(false)))
		r := resolve(t, ts, ActivationInput{})
		want := map[string]ToolStatusEntry{
			"__proto__":   {Enabled: true, Reason: StatusReasonActivate, Directive: ToolDirectiveActivate},
			"constructor": {Enabled: false, Reason: StatusReasonActivateWhen, Directive: ToolDirectiveActivateWhen, Predicate: true},
			"prototype":   {Enabled: false, Reason: StatusReasonDeactivate, Directive: ToolDirectiveDeactivate},
		}
		if !reflect.DeepEqual(r.StatusByTool, want) {
			t.Fatalf("StatusByTool = %+v", r.StatusByTool)
		}
		eqStrings(t, "Enabled", r.Enabled, []string{"__proto__"})
		eqStrings(t, "Disabled", r.Disabled, []string{"constructor", "prototype"})
	})
	t.Run("conditional id lands in Disabled when its predicate is false", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}}).Deactivate("b"))
		ts = must(t)(ts.ActivateWhen("a", always(false)))
		eqStrings(t, "Disabled", resolve(t, ts, ActivationInput{}).Disabled, []string{"a", "b"})
	})
	t.Run("deactivateWhen status entry", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}}).DeactivateWhen("a", always(false)))
		got := resolve(t, ts, ActivationInput{}).StatusByTool["a"]
		if got != (ToolStatusEntry{Enabled: true, Reason: StatusReasonDeactivateWhen, Directive: ToolDirectiveDeactivateWhen, Predicate: true}) {
			t.Fatalf("a = %+v", got)
		}
	})
	t.Run("static partition returns exact active tools", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}}).Deactivate("b"))
		r := resolve(t, ts, ActivationInput{})
		eqTools(t, "Tools", r.Tools, []Tool{tsA, tsC})
		eqStrings(t, "ActiveTools", r.ActiveTools, []string{"a", "c"})
	})
}

func TestToolSetServerTools(t *testing.T) {
	t.Run("default server IDs from config type; override via ID", func(t *testing.T) {
		cases := map[string]Tool{
			"server:web_search_2025_08_26": tsWebSearch,
			"server:openrouter:datetime":   tsDatetime,
			"server:public_search":         tsPublicSearch,
		}
		for want, tool := range cases {
			if got := ToolID(tool); got != want {
				t.Fatalf("ToolID = %q, want %q", got, want)
			}
		}
	})
	t.Run("preserves server tools in construction order", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsWebSearch, tsB, tsDatetime}})
		eqTools(t, "Tools", ts.Tools(), []Tool{tsA, tsWebSearch, tsB, tsDatetime})
	})
	t.Run("active server tools are in Tools/Enabled/StatusByTool but not ActiveTools", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsWebSearch, tsB}}).Deactivate("a"))
		r := resolve(t, ts, ActivationInput{})
		eqTools(t, "Tools", r.Tools, []Tool{tsWebSearch, tsB})
		eqStrings(t, "ActiveTools", r.ActiveTools, []string{"b"})
		eqStrings(t, "Enabled", r.Enabled, []string{"server:web_search_2025_08_26", "b"})
		if r.StatusByTool["server:web_search_2025_08_26"] != (ToolStatusEntry{Enabled: true, Reason: StatusReasonDefault}) {
			t.Fatalf("status = %+v", r.StatusByTool["server:web_search_2025_08_26"])
		}
	})
	t.Run("deactivate server tools by stable ID", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsWebSearch, tsB}}).Deactivate("server:web_search_2025_08_26"))
		r := resolve(t, ts, ActivationInput{})
		eqTools(t, "Tools", r.Tools, []Tool{tsA, tsB})
		eqStrings(t, "Enabled", r.Enabled, []string{"a", "b"})
		eqStrings(t, "Disabled", r.Disabled, []string{"server:web_search_2025_08_26"})
		if r.StatusByTool["server:web_search_2025_08_26"] != (ToolStatusEntry{Enabled: false, Reason: StatusReasonDeactivate, Directive: ToolDirectiveDeactivate}) {
			t.Fatalf("status = %+v", r.StatusByTool["server:web_search_2025_08_26"])
		}
	})
	t.Run("override IDs and duplicate rejection", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsPublicSearch}})
		eqStrings(t, "Enabled", resolve(t, ts, ActivationInput{}).Enabled, []string{"a", "server:public_search"})
		_, err := NewToolSet(ToolSetConfig{Tools: []Tool{tsWebSearch, NewServerTool(ServerToolConfig{Config: tsWebSearchConfig()})}})
		wantErr(t, err, ErrDuplicateToolID, `"server:web_search_2025_08_26"`)
		// A custom ID frees the default ID: both can coexist.
		mustToolSet(t, ToolSetConfig{Tools: []Tool{tsWebSearch, tsPublicSearch}})
	})
	t.Run("rejects raw type strings", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsWebSearch}})
		_, err := ts.Activate("web_search_2025_08_26")
		wantErr(t, err, ErrUnknownTool, "unknown tool")
		// The server tool's display Name is not its ID either.
		_, err = ts.Activate("web_search")
		wantErr(t, err, ErrUnknownTool, "unknown tool")
	})
	t.Run("hand-written server tool without an ID uses the synthesized ID", func(t *testing.T) {
		hw := tsHandWrittenServerTool{NewServerTool(ServerToolConfig{Name: "hand_written", Config: tsWebSearchConfig()})}
		if _, ok := Tool(hw).(interface{ ServerToolID() string }); ok {
			t.Fatal("fixture must not expose ServerToolID")
		}
		const id = "server:web_search_2025_08_26"
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, hw}}).Deactivate(id))
		r := resolve(t, ts, ActivationInput{})
		eqStrings(t, "Enabled", r.Enabled, []string{"a"})
		eqStrings(t, "Disabled", r.Disabled, []string{id})
		if r.StatusByTool[id] != (ToolStatusEntry{Enabled: false, Reason: StatusReasonDeactivate, Directive: ToolDirectiveDeactivate}) {
			t.Fatalf("status = %+v", r.StatusByTool[id])
		}
		eqTools(t, "Tools", r.Tools, []Tool{tsA})
	})
	t.Run("custom-ID server tool: real ID works, synthesized default is unknown", func(t *testing.T) {
		ts := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsPublicSearch}})
		d := must(t)(ts.Deactivate("server:public_search"))
		r := resolve(t, d, ActivationInput{})
		eqStrings(t, "Enabled", r.Enabled, []string{"a"})
		eqStrings(t, "Disabled", r.Disabled, []string{"server:public_search"})
		_, err := ts.Deactivate("server:web_search_2025_08_26")
		wantErr(t, err, ErrUnknownTool, "unknown tool")
	})
}

func TestToolSetSituations(t *testing.T) {
	abc := func(t *testing.T) *ToolSet { return mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC}}) }

	t.Run("overlays static enabled/disabled", func(t *testing.T) {
		ts := must(t)(abc(t).Deactivate("c"))
		ts = must(t)(ts.DefineSituations(map[string]SituationConfig{
			"guest": {Enabled: []string{"a"}, Disabled: []string{"b", "c"}},
			"full":  {Enabled: []string{"a", "b", "c"}},
		}))
		guest := resolveSit(t, ts, "guest", ActivationInput{})
		eqTools(t, "guest.Tools", guest.Tools, []Tool{tsA})
		eqStrings(t, "guest.ActiveTools", guest.ActiveTools, []string{"a"})
		eqStrings(t, "guest.Enabled", guest.Enabled, []string{"a"})
		eqStrings(t, "guest.Disabled", guest.Disabled, []string{"b", "c"})
		want := map[string]ToolStatusEntry{
			"a": {Enabled: true, Reason: StatusReasonSituation, Directive: ToolDirectiveActivate},
			"b": {Enabled: false, Reason: StatusReasonSituation, Directive: ToolDirectiveDeactivate},
			"c": {Enabled: false, Reason: StatusReasonSituation, Directive: ToolDirectiveDeactivate},
		}
		if !reflect.DeepEqual(guest.StatusByTool, want) {
			t.Fatalf("guest.StatusByTool = %+v", guest.StatusByTool)
		}
		eqTools(t, "full.Tools", resolveSit(t, ts, "full", ActivationInput{}).Tools, []Tool{tsA, tsB, tsC})
		// The base partition is unaffected by situations.
		eqStrings(t, "base", resolve(t, ts, ActivationInput{}).ActiveTools, []string{"a", "b"})
	})
	t.Run("conditional rules with runtime-exact status", func(t *testing.T) {
		ts := must(t)(abc(t).DefineSituations(map[string]SituationConfig{
			"authed": {
				Enabled:     []string{"a"},
				Disabled:    []string{"b"},
				Conditional: map[string]*SituationConditionalRule{"c": {Predicate: ctxFlag("admin")}},
			},
		}))
		denied := resolveSit(t, ts, "authed", ActivationInput{Context: map[string]any{"admin": false}})
		eqTools(t, "denied.Tools", denied.Tools, []Tool{tsA})
		eqStrings(t, "denied.Enabled", denied.Enabled, []string{"a"})
		eqStrings(t, "denied.Disabled", denied.Disabled, []string{"b", "c"})
		if denied.StatusByTool["c"] != (ToolStatusEntry{Enabled: false, Reason: StatusReasonSituation, Directive: ToolDirectiveActivateWhen, Predicate: true}) {
			t.Fatalf("c = %+v", denied.StatusByTool["c"])
		}
		allowed := resolveSit(t, ts, "authed", ActivationInput{Context: map[string]any{"admin": true}})
		eqTools(t, "allowed.Tools", allowed.Tools, []Tool{tsA, tsC})
		eqStrings(t, "allowed.Enabled", allowed.Enabled, []string{"a", "c"})
	})
	t.Run("deactivateWhen situation rules", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}}).DefineSituations(map[string]SituationConfig{
			"guarded": {Conditional: map[string]*SituationConditionalRule{
				"a": {Mode: SituationModeDeactivateWhen, Predicate: ctxFlag("blocked")},
			}},
		}))
		eqStrings(t, "Enabled", resolveSit(t, ts, "guarded", ActivationInput{}).Enabled, []string{"a"})
		blocked := resolveSit(t, ts, "guarded", ActivationInput{Context: map[string]any{"blocked": true}})
		eqStrings(t, "Disabled", blocked.Disabled, []string{"a"})
		if blocked.StatusByTool["a"] != (ToolStatusEntry{Enabled: false, Reason: StatusReasonSituation, Directive: ToolDirectiveDeactivateWhen, Predicate: true}) {
			t.Fatalf("a = %+v", blocked.StatusByTool["a"])
		}
	})
	t.Run("explicit activateWhen mode", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}}).DefineSituations(map[string]SituationConfig{
			"s": {Conditional: map[string]*SituationConditionalRule{"a": {Mode: SituationModeActivateWhen, Predicate: always(false)}}},
		}))
		eqStrings(t, "Disabled", resolveSit(t, ts, "s", ActivationInput{}).Disabled, []string{"a"})
	})
	t.Run("ignores nil conditional entries", func(t *testing.T) {
		ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}}).DefineSituations(map[string]SituationConfig{
			"optional": {Conditional: map[string]*SituationConditionalRule{"a": nil, "b": {Predicate: always(false)}}},
		}))
		eqStrings(t, "ActiveTools", resolveSit(t, ts, "optional", ActivationInput{}).ActiveTools, []string{"a"})
	})
	t.Run("leaves unmentioned IDs on the base partition", func(t *testing.T) {
		ts := must(t)(abc(t).Deactivate("c"))
		ts = must(t)(ts.DefineSituations(map[string]SituationConfig{"onlyB": {Disabled: []string{"b"}}}))
		r := resolveSit(t, ts, "onlyB", ActivationInput{})
		eqStrings(t, "Enabled", r.Enabled, []string{"a"})
		eqStrings(t, "Disabled", r.Disabled, []string{"b", "c"})
		if r.StatusByTool["c"].Reason != StatusReasonDeactivate {
			t.Fatalf("c keeps base reason, got %+v", r.StatusByTool["c"])
		}
	})
	t.Run("redefining replaces previous situations", func(t *testing.T) {
		ts := must(t)(abc(t).DefineSituations(map[string]SituationConfig{"one": {}}))
		ts = must(t)(ts.DefineSituations(map[string]SituationConfig{"two": {}}))
		_, err := ts.ResolveSituation(context.Background(), "one", ActivationInput{})
		wantErr(t, err, ErrUnknownSituation, `"one"`)
		resolveSit(t, ts, "two", ActivationInput{})
	})
	t.Run("situation predicate errors abort resolution", func(t *testing.T) {
		boom := errors.New("boom")
		ts := must(t)(abc(t).DefineSituations(map[string]SituationConfig{
			"s": {Conditional: map[string]*SituationConditionalRule{"a": {Predicate: func(context.Context, ActivationInput) (bool, error) { return false, boom }}}},
		}))
		_, err := ts.ResolveSituation(context.Background(), "s", ActivationInput{})
		wantErr(t, err, boom, `"a"`)
	})
}

func TestToolSetSituationErrors(t *testing.T) {
	base := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB}})
	malformed := `situation "checkout": conditional rule for tool "a" must be a function or { mode, predicate } object`
	cases := []struct {
		name     string
		cfg      map[string]SituationConfig
		sentinel error
		substr   string
	}{
		{"unknown id", map[string]SituationConfig{"bad": {Enabled: []string{"nope"}}}, ErrUnknownTool, `unknown tool: "nope"`},
		{"unknown conditional id", map[string]SituationConfig{"bad": {Conditional: map[string]*SituationConditionalRule{"nope": {Predicate: always(true)}}}}, ErrUnknownTool, `"nope"`},
		{"enabled+disabled", map[string]SituationConfig{"bad": {Enabled: []string{"a"}, Disabled: []string{"a"}}}, ErrInvalidSituation, `lists tool "a" more than once`},
		{"enabled twice", map[string]SituationConfig{"bad": {Enabled: []string{"a", "a"}}}, ErrInvalidSituation, `lists tool "a" more than once`},
		{"enabled+conditional", map[string]SituationConfig{"bad": {Enabled: []string{"a"}, Conditional: map[string]*SituationConditionalRule{"a": {Predicate: always(true)}}}}, ErrInvalidSituation, `lists tool "a" more than once`},
		{"invalid mode", map[string]SituationConfig{"checkout": {Conditional: map[string]*SituationConditionalRule{"a": {Mode: "invalid", Predicate: always(true)}}}}, ErrInvalidSituation, malformed},
		{"missing predicate", map[string]SituationConfig{"checkout": {Conditional: map[string]*SituationConditionalRule{"a": {Mode: SituationModeActivateWhen}}}}, ErrInvalidSituation, malformed},
		{"empty rule", map[string]SituationConfig{"checkout": {Conditional: map[string]*SituationConditionalRule{"a": {}}}}, ErrInvalidSituation, malformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base.DefineSituations(tc.cfg)
			wantErr(t, err, tc.sentinel, tc.substr)
			if got != nil {
				t.Fatal("expected nil ToolSet on error")
			}
		})
	}
	t.Run("unknown situation at resolve time", func(t *testing.T) {
		ts := must(t)(base.DefineSituations(map[string]SituationConfig{"guest": {Enabled: []string{"a"}}}))
		_, err := ts.ResolveSituation(context.Background(), "missing", ActivationInput{})
		wantErr(t, err, ErrUnknownSituation, `unknown situation: "missing"`)
	})
	t.Run("failed define leaves mutable set untouched", func(t *testing.T) {
		m := mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA}, Mutable: true})
		must(t)(m.DefineSituations(map[string]SituationConfig{"ok": {Disabled: []string{"a"}}}))
		if _, err := m.DefineSituations(map[string]SituationConfig{"bad": {Enabled: []string{"nope"}}}); err == nil {
			t.Fatal("expected error")
		}
		eqStrings(t, "ok", resolveSit(t, m, "ok", ActivationInput{}).ActiveTools, nil)
	})
}

func TestToolSetCallModelShape(t *testing.T) {
	ts := must(t)(mustToolSet(t, ToolSetConfig{Tools: []Tool{tsA, tsB, tsC, tsWebSearch}}).Deactivate("b"))
	r := resolve(t, ts, ActivationInput{})
	eqTools(t, "CallModel.Tools", r.CallModel.Tools, []Tool{tsA, tsC, tsWebSearch})
	eqStrings(t, "CallModel.ActiveTools", r.CallModel.ActiveTools, []string{"a", "c"})

	// Feeding the snapshot through CallModel's ActiveTools filter is a no-op:
	// the server tool survives even though its ID is not in ActiveTools.
	filtered, err := FilterToolsByIDs(r.CallModel.Tools, r.CallModel.ActiveTools)
	if err != nil {
		t.Fatal(err)
	}
	eqTools(t, "filtered", filtered, r.CallModel.Tools)

	// A fully-deactivated client set still yields a non-nil (filtering)
	// ActiveTools so CallModel does not fall back to "all tools".
	none := must(t)(ts.Deactivate("a", "c", "server:web_search_2025_08_26"))
	rn := resolve(t, none, ActivationInput{})
	if rn.CallModel.ActiveTools == nil || len(rn.CallModel.ActiveTools) != 0 || len(rn.CallModel.Tools) != 0 {
		t.Fatalf("expected empty non-nil ActiveTools, got %#v", rn.CallModel)
	}
	if out, _ := FilterToolsByIDs(rn.CallModel.Tools, rn.CallModel.ActiveTools); out != nil {
		t.Fatal("empty result must collapse to nil so tools is omitted")
	}
}

func TestFilterToolsByIDs(t *testing.T) {
	cases := []struct {
		name  string
		tools []Tool
		ids   []string
		want  []Tool
	}{
		{"sends only active tools", []Tool{tsA, tsB}, []string{"a"}, []Tool{tsA}},
		{"silently ignores unknown names", []Tool{tsA, tsB}, []string{"a", "missing"}, []Tool{tsA}},
		{"nil ids keeps all tools", []Tool{tsA, tsB}, nil, []Tool{tsA, tsB}},
		{"empty tools collapses to nil", []Tool{}, nil, nil},
		{"nil tools collapses to nil", nil, []string{"a"}, nil},
		{"all filtered collapses to nil", []Tool{tsA, tsB}, []string{"missing"}, nil},
		{"empty non-nil ids filters every client tool", []Tool{tsA, tsB}, []string{}, nil},
		{"server tools always kept", []Tool{tsA, tsWebSearch, tsB}, []string{"b"}, []Tool{tsWebSearch, tsB}},
		{"server tools kept with empty ids", []Tool{tsA, tsDatetime}, []string{}, []Tool{tsDatetime}},
		{"server IDs do not select client tools", []Tool{tsA, tsWebSearch}, []string{"server:web_search_2025_08_26"}, []Tool{tsWebSearch}},
		{"order follows tools, duplicates ignored", []Tool{tsA, tsB, tsC}, []string{"c", "a", "c"}, []Tool{tsA, tsC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FilterToolsByIDs(tc.tools, tc.ids)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("got %q, want nil", tsIDs(got))
			}
			eqTools(t, "filtered", got, tc.want)
		})
	}
	t.Run("does not mutate the input slice", func(t *testing.T) {
		in := []Tool{tsA, tsB, tsC}
		if _, err := FilterToolsByIDs(in, []string{"c"}); err != nil {
			t.Fatal(err)
		}
		eqTools(t, "input", in, []Tool{tsA, tsB, tsC})
	})
	t.Run("nil tool entry errors", func(t *testing.T) {
		_, err := FilterToolsByIDs([]Tool{tsA, nil}, nil)
		wantErr(t, err, ErrNilTool, "index 1")
	})
}
