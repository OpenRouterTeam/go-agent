package agent

// Port of upstream packages/agent/src/lib/tool-set.ts and the runtime parts of
// tool-set-types.ts (@openrouter/agent/tool-set).
//
// A ToolSet is an ordered, immutable-by-default collection of tools with a
// per-tool activation directive (static activate/deactivate, or a runtime
// activateWhen/deactivateWhen predicate) and optional named "situations" that
// overlay the base directives. Resolving a ToolSet yields the active tools in
// construction order plus an exhaustive per-tool status snapshot.
//
// Upstream's compile-time partition tracking (InitialPartition,
// WidenedPartition, InferEnabledIds, ...) is pure type-level machinery with no
// Go analogue; only the runtime behavior is ported.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Tool-set errors. Returned errors wrap these sentinels so callers can use
// errors.Is; the messages carry the offending id/name like upstream's throws.
var (
	// ErrDuplicateToolID is returned by NewToolSet when two tools share an ID
	// (upstream: `Duplicate tool ID: "<id>"`).
	ErrDuplicateToolID = errors.New("duplicate tool ID")
	// ErrUnknownTool is returned when a directive or situation references an
	// ID not in the set (upstream: `Unknown tool: "<id>"`).
	ErrUnknownTool = errors.New("unknown tool")
	// ErrUnknownSituation is returned by ResolveSituation for an undefined
	// situation name (upstream: `Unknown situation: "<name>"`).
	ErrUnknownSituation = errors.New("unknown situation")
	// ErrInvalidSituation is returned by DefineSituations for a situation that
	// lists a tool more than once or carries a malformed conditional rule.
	ErrInvalidSituation = errors.New("invalid situation")
	// ErrMissingPredicate is returned by ActivateWhen/DeactivateWhen when
	// called with a name but a nil predicate.
	ErrMissingPredicate = errors.New("activateWhen/deactivateWhen requires a predicate when called with a name")
	// ErrNilTool is returned when a tool slice contains a nil Tool.
	ErrNilTool = errors.New("nil tool")
)

// ActivationInput is passed to activation predicates (upstream
// ActivationInput). Both fields are optional; a zero value mirrors calling
// resolve() with no argument.
type ActivationInput struct {
	// State is the current conversation state, if any.
	State *ConversationState
	// Context is the shared context (the map stored under SharedContextKey in
	// a ToolContextStore / ContextInput).
	Context map[string]any
}

// ActivationPredicate decides whether a conditional tool is "hit". For
// ActivateWhen the tool is active iff the predicate returns true; for
// DeactivateWhen it is inactive iff the predicate returns true. A non-nil
// error aborts resolution (upstream: a throwing predicate propagates).
type ActivationPredicate func(ctx context.Context, input ActivationInput) (bool, error)

// StatusReason explains why a tool resolved the way it did.
type StatusReason string

const (
	StatusReasonDefault        StatusReason = "default"
	StatusReasonActivate       StatusReason = "activate"
	StatusReasonDeactivate     StatusReason = "deactivate"
	StatusReasonActivateWhen   StatusReason = "activateWhen"
	StatusReasonDeactivateWhen StatusReason = "deactivateWhen"
	StatusReasonSituation      StatusReason = "situation"
)

// ToolDirective is the last directive applied to a tool before predicates
// ran. The empty string means the tool is at its construction default.
type ToolDirective string

const (
	ToolDirectiveNone           ToolDirective = ""
	ToolDirectiveActivate       ToolDirective = "activate"
	ToolDirectiveDeactivate     ToolDirective = "deactivate"
	ToolDirectiveActivateWhen   ToolDirective = "activateWhen"
	ToolDirectiveDeactivateWhen ToolDirective = "deactivateWhen"
)

// ToolStatusEntry is one entry of ResolvedToolSet.StatusByTool.
type ToolStatusEntry struct {
	Enabled bool
	Reason  StatusReason
	// Directive is empty when the tool is still at its construction default.
	Directive ToolDirective
	// Predicate is true when the final state depended on a runtime predicate.
	Predicate bool
}

// SituationRuleMode selects how a situation's conditional rule applies.
type SituationRuleMode string

const (
	// SituationModeActivateWhen (the default when Mode is empty): inactive
	// until the predicate returns true.
	SituationModeActivateWhen SituationRuleMode = "activateWhen"
	// SituationModeDeactivateWhen: active until the predicate returns true.
	SituationModeDeactivateWhen SituationRuleMode = "deactivateWhen"
)

// SituationConditionalRule is upstream's `{ mode?, predicate }` rule. The
// upstream bare-function shorthand is a rule with an empty Mode.
type SituationConditionalRule struct {
	Mode      SituationRuleMode
	Predicate ActivationPredicate
}

// SituationConfig is a declarative overlay for one named situation. IDs it
// does not mention keep the base ToolSet directive.
type SituationConfig struct {
	// Enabled tools are statically on in this situation.
	Enabled []string
	// Disabled tools are statically off in this situation.
	Disabled []string
	// Conditional tools are predicate-driven in this situation. A nil value is
	// ignored (upstream: `undefined` entries are skipped).
	Conditional map[string]*SituationConditionalRule
}

// ToolSetConfig configures NewToolSet (upstream createToolSet options).
type ToolSetConfig struct {
	Tools []Tool
	// Mutable makes directive methods mutate the receiver in place and return
	// it, instead of returning a fresh ToolSet.
	Mutable bool
}

// ToolSetCallModelInput holds exactly the fields of a resolved snapshot that
// belong in a CallModel request.
type ToolSetCallModelInput struct {
	Tools       []Tool
	ActiveTools []string
}

// ResolvedToolSet is the snapshot returned by Resolve / ResolveSituation /
// InferTools (upstream ResolvedToolSnapshot).
//
// Only Tools and ActiveTools are CallModel inputs; assign them to
// CallModelInput.Tools and CallModelInput.ActiveTools (or use the CallModel
// field, which holds exactly those two).
type ResolvedToolSet struct {
	// Tools are the active tools (client and server), construction order.
	Tools []Tool
	// ActiveTools are the active client tool names only (CallModel
	// ActiveTools wire format); server tool IDs are omitted. Never nil.
	ActiveTools []string
	// CallModel holds the same Tools/ActiveTools slices.
	CallModel ToolSetCallModelInput
	// Enabled are the IDs (client and server) that resolved active.
	Enabled []string
	// Disabled are the IDs (client and server) that resolved inactive.
	Disabled []string
	// StatusByTool has an entry for every ID in the set.
	StatusByTool map[string]ToolStatusEntry
}

type activationKind int

const (
	activationStatic activationKind = iota
	activationActivateWhen
	activationDeactivateWhen
)

type activationSource int

const (
	sourceDirective activationSource = iota
	sourceSituation
)

type activationEntry struct {
	kind      activationKind
	active    bool // for activationStatic
	predicate ActivationPredicate
	source    activationSource
}

type situationConditional struct {
	id        string
	kind      activationKind
	predicate ActivationPredicate
}

type situationRuntime struct {
	enabled     []string
	disabled    []string
	conditional []situationConditional
}

type toolSetIndex struct {
	tools    []Tool
	ids      []string
	toolByID map[string]Tool
}

// ToolSet is the Go port of upstream's ToolSet class. Construct one with
// NewToolSet. Immutable sets (the default) are safe for concurrent use;
// mutable sets serialize their own mutations and resolutions internally.
type ToolSet struct {
	index   *toolSetIndex
	mutable bool

	mu         sync.RWMutex
	activation map[string]activationEntry
	situations map[string]situationRuntime
}

// NewToolSet constructs a ToolSet (upstream createToolSet). It returns an
// error wrapping ErrDuplicateToolID when two tools share an ID (see ToolID),
// or ErrNilTool for a nil entry.
func NewToolSet(config ToolSetConfig) (*ToolSet, error) {
	idx := &toolSetIndex{
		tools:    append([]Tool(nil), config.Tools...),
		ids:      make([]string, 0, len(config.Tools)),
		toolByID: make(map[string]Tool, len(config.Tools)),
	}
	for i, t := range config.Tools {
		if t == nil {
			return nil, fmt.Errorf("tool set: %w at index %d", ErrNilTool, i)
		}
		id := toolSetID(t)
		if _, dup := idx.toolByID[id]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateToolID, id)
		}
		idx.toolByID[id] = t
		idx.ids = append(idx.ids, id)
	}
	return &ToolSet{
		index:      idx,
		mutable:    config.Mutable,
		activation: map[string]activationEntry{},
		situations: map[string]situationRuntime{},
	}, nil
}

// toolSetID is ToolID plus upstream's defaultServerId fallback for server
// tools that do not expose a ServerToolID (e.g. hand-written Tool
// implementations): "server:<wire type>".
func toolSetID(t Tool) string {
	if IsServerTool(t) {
		if _, ok := t.(interface{ ServerToolID() string }); !ok {
			return "server:" + serverToolWireType(t.ToAPITool())
		}
	}
	return ToolID(t)
}

// Tools returns every tool in construction order regardless of activation.
func (s *ToolSet) Tools() []Tool {
	return append([]Tool(nil), s.index.tools...)
}

// Mutable reports whether directive methods mutate this set in place.
func (s *ToolSet) Mutable() bool { return s.mutable }

func (s *ToolSet) assertKnown(id string) error {
	if _, ok := s.index.toolByID[id]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownTool, id)
	}
	return nil
}

func (s *ToolSet) assertAllKnown(ids []string) error {
	for _, id := range ids {
		if err := s.assertKnown(id); err != nil {
			return err
		}
	}
	return nil
}

func cloneActivation(in map[string]activationEntry) map[string]activationEntry {
	out := make(map[string]activationEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSituations(in map[string]situationRuntime) map[string]situationRuntime {
	out := make(map[string]situationRuntime, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// withActivation applies mutate in place for mutable sets (returning the
// receiver), or to a copy carried by a fresh immutable set.
func (s *ToolSet) withActivation(mutate func(map[string]activationEntry)) *ToolSet {
	if s.mutable {
		s.mu.Lock()
		mutate(s.activation)
		s.mu.Unlock()
		return s
	}
	s.mu.RLock()
	next := cloneActivation(s.activation)
	situations := s.situations
	s.mu.RUnlock()
	mutate(next)
	return &ToolSet{index: s.index, mutable: false, activation: next, situations: situations}
}

// Activate statically enables the given IDs. Every ID is validated before
// anything is applied. Immutable sets return a new ToolSet; mutable sets
// mutate and return the receiver.
func (s *ToolSet) Activate(ids ...string) (*ToolSet, error) {
	return s.setStatic(ids, true)
}

// Deactivate statically disables the given IDs. See Activate.
func (s *ToolSet) Deactivate(ids ...string) (*ToolSet, error) {
	return s.setStatic(ids, false)
}

func (s *ToolSet) setStatic(ids []string, active bool) (*ToolSet, error) {
	if err := s.assertAllKnown(ids); err != nil {
		return nil, err
	}
	list := append([]string(nil), ids...)
	return s.withActivation(func(m map[string]activationEntry) {
		for _, id := range list {
			m[id] = activationEntry{kind: activationStatic, active: active, source: sourceDirective}
		}
	}), nil
}

// ActivateWhen makes id conditional: inactive unless predicate returns true.
func (s *ToolSet) ActivateWhen(id string, predicate ActivationPredicate) (*ToolSet, error) {
	return s.setConditionalOne(id, predicate, activationActivateWhen)
}

// ActivateWhenMap is the predicate-map form of ActivateWhen. Nil predicates
// are skipped; every remaining ID is validated before anything is applied.
func (s *ToolSet) ActivateWhenMap(predicates map[string]ActivationPredicate) (*ToolSet, error) {
	return s.setConditionalMap(predicates, activationActivateWhen)
}

// DeactivateWhen makes id conditional: active unless predicate returns true.
func (s *ToolSet) DeactivateWhen(id string, predicate ActivationPredicate) (*ToolSet, error) {
	return s.setConditionalOne(id, predicate, activationDeactivateWhen)
}

// DeactivateWhenMap is the predicate-map form of DeactivateWhen.
func (s *ToolSet) DeactivateWhenMap(predicates map[string]ActivationPredicate) (*ToolSet, error) {
	return s.setConditionalMap(predicates, activationDeactivateWhen)
}

func (s *ToolSet) setConditionalOne(id string, p ActivationPredicate, kind activationKind) (*ToolSet, error) {
	if p == nil {
		return nil, ErrMissingPredicate
	}
	if err := s.assertKnown(id); err != nil {
		return nil, err
	}
	return s.withActivation(func(m map[string]activationEntry) {
		m[id] = activationEntry{kind: kind, predicate: p, source: sourceDirective}
	}), nil
}

func (s *ToolSet) setConditionalMap(predicates map[string]ActivationPredicate, kind activationKind) (*ToolSet, error) {
	ids := make([]string, 0, len(predicates))
	for id, p := range predicates {
		if p != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids) // deterministic error reporting
	if err := s.assertAllKnown(ids); err != nil {
		return nil, err
	}
	return s.withActivation(func(m map[string]activationEntry) {
		for _, id := range ids {
			m[id] = activationEntry{kind: kind, predicate: predicates[id], source: sourceDirective}
		}
	}), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DefineSituations registers named situations, replacing any previously
// defined ones. Each situation is validated: every ID must be known, and an
// ID may appear at most once across Enabled/Disabled/Conditional. Conditional
// rules must carry a predicate and a valid (or empty) mode.
func (s *ToolSet) DefineSituations(situations map[string]SituationConfig) (*ToolSet, error) {
	next := make(map[string]situationRuntime, len(situations))
	for _, name := range sortedKeys(situations) {
		config := situations[name]
		seen := map[string]bool{}
		record := func(id string) error {
			if err := s.assertKnown(id); err != nil {
				return err
			}
			if seen[id] {
				return fmt.Errorf("%w: situation %q lists tool %q more than once (across enabled/disabled/conditional)", ErrInvalidSituation, name, id)
			}
			seen[id] = true
			return nil
		}
		for _, id := range config.Enabled {
			if err := record(id); err != nil {
				return nil, err
			}
		}
		for _, id := range config.Disabled {
			if err := record(id); err != nil {
				return nil, err
			}
		}
		condIDs := make([]string, 0, len(config.Conditional))
		for _, id := range sortedKeys(config.Conditional) {
			if config.Conditional[id] == nil {
				continue
			}
			if err := record(id); err != nil {
				return nil, err
			}
			condIDs = append(condIDs, id)
		}
		rt := situationRuntime{
			enabled:  append([]string(nil), config.Enabled...),
			disabled: append([]string(nil), config.Disabled...),
		}
		for _, id := range condIDs {
			rule := config.Conditional[id]
			var kind activationKind
			switch rule.Mode {
			case "", SituationModeActivateWhen:
				kind = activationActivateWhen
			case SituationModeDeactivateWhen:
				kind = activationDeactivateWhen
			default:
				return nil, malformedRule(name, id)
			}
			if rule.Predicate == nil {
				return nil, malformedRule(name, id)
			}
			rt.conditional = append(rt.conditional, situationConditional{id: id, kind: kind, predicate: rule.Predicate})
		}
		next[name] = rt
	}

	if s.mutable {
		s.mu.Lock()
		s.situations = next
		s.mu.Unlock()
		return s, nil
	}
	s.mu.RLock()
	activation := cloneActivation(s.activation)
	s.mu.RUnlock()
	return &ToolSet{index: s.index, mutable: false, activation: activation, situations: next}, nil
}

func malformedRule(situation, id string) error {
	return fmt.Errorf("%w: situation %q: conditional rule for tool %q must be a function or { mode, predicate } object", ErrInvalidSituation, situation, id)
}

// Resolve evaluates the base directives (no situation overlay) against input.
func (s *ToolSet) Resolve(ctx context.Context, input ActivationInput) (ResolvedToolSet, error) {
	s.mu.RLock()
	activation := cloneActivation(s.activation)
	s.mu.RUnlock()
	return s.resolveWith(ctx, activation, input)
}

// InferTools is upstream's back-compat alias for Resolve.
func (s *ToolSet) InferTools(ctx context.Context, input ActivationInput) (ResolvedToolSet, error) {
	return s.Resolve(ctx, input)
}

// ResolveSituation resolves a previously defined situation overlaid on the
// base directives. Unknown names return an error wrapping
// ErrUnknownSituation.
func (s *ToolSet) ResolveSituation(ctx context.Context, name string, input ActivationInput) (ResolvedToolSet, error) {
	s.mu.RLock()
	situation, ok := s.situations[name]
	activation := cloneActivation(s.activation)
	s.mu.RUnlock()
	if !ok {
		return ResolvedToolSet{}, fmt.Errorf("%w: %q", ErrUnknownSituation, name)
	}
	for _, id := range situation.enabled {
		activation[id] = activationEntry{kind: activationStatic, active: true, source: sourceSituation}
	}
	for _, id := range situation.disabled {
		activation[id] = activationEntry{kind: activationStatic, active: false, source: sourceSituation}
	}
	for _, c := range situation.conditional {
		activation[c.id] = activationEntry{kind: c.kind, predicate: c.predicate, source: sourceSituation}
	}
	return s.resolveWith(ctx, activation, input)
}

func (s *ToolSet) resolveWith(ctx context.Context, activation map[string]activationEntry, input ActivationInput) (ResolvedToolSet, error) {
	out := ResolvedToolSet{
		Tools:        []Tool{},
		ActiveTools:  []string{},
		Enabled:      []string{},
		Disabled:     []string{},
		StatusByTool: make(map[string]ToolStatusEntry, len(s.index.ids)),
	}
	for _, id := range s.index.ids {
		t := s.index.toolByID[id]
		entry, has := activation[id]
		active := true
		if has {
			switch entry.kind {
			case activationStatic:
				active = entry.active
			case activationActivateWhen, activationDeactivateWhen:
				hit, err := entry.predicate(ctx, input)
				if err != nil {
					return ResolvedToolSet{}, fmt.Errorf("tool set: predicate for tool %q: %w", id, err)
				}
				active = hit == (entry.kind == activationActivateWhen)
			}
		}
		out.StatusByTool[id] = toStatusEntry(active, entry, has)
		if active {
			out.Tools = append(out.Tools, t)
			out.Enabled = append(out.Enabled, id)
			if !IsServerTool(t) {
				out.ActiveTools = append(out.ActiveTools, id)
			}
		} else {
			out.Disabled = append(out.Disabled, id)
		}
	}
	out.CallModel = ToolSetCallModelInput{Tools: out.Tools, ActiveTools: out.ActiveTools}
	return out, nil
}

func toStatusEntry(active bool, entry activationEntry, has bool) ToolStatusEntry {
	if !has {
		return ToolStatusEntry{Enabled: active, Reason: StatusReasonDefault}
	}
	switch entry.kind {
	case activationStatic:
		directive := ToolDirectiveDeactivate
		if entry.active {
			directive = ToolDirectiveActivate
		}
		reason := StatusReason(directive)
		if entry.source == sourceSituation {
			reason = StatusReasonSituation
		}
		return ToolStatusEntry{Enabled: active, Reason: reason, Directive: directive}
	default:
		directive := ToolDirectiveActivateWhen
		if entry.kind == activationDeactivateWhen {
			directive = ToolDirectiveDeactivateWhen
		}
		reason := StatusReason(directive)
		if entry.source == sourceSituation {
			reason = StatusReasonSituation
		}
		return ToolStatusEntry{Enabled: active, Reason: reason, Directive: directive, Predicate: true}
	}
}

// Clone copies directives and situations into an independent ToolSet that
// inherits this set's mutability.
func (s *ToolSet) Clone() *ToolSet {
	return s.CloneAs(s.mutable)
}

// CloneAs copies directives and situations into an independent ToolSet with
// the given mutability (upstream clone({ mutable })).
func (s *ToolSet) CloneAs(mutable bool) *ToolSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &ToolSet{
		index:      s.index,
		mutable:    mutable,
		activation: cloneActivation(s.activation),
		situations: cloneSituations(s.situations),
	}
}

// FilterToolsByIDs applies CallModel's ActiveTools filter (upstream
// call-model.ts):
//
//   - ids == nil (option omitted): every tool is kept.
//   - ids != nil (including an empty, non-nil slice): server tools are always
//     kept; a client tool is kept iff its name is in ids. Unknown names and
//     duplicates are silently ignored; the result keeps tools' order.
//
// A filtered-to-empty (or empty) result is returned as nil so callers omit
// the outbound `tools` key entirely rather than sending `tools: []`. The only
// error is ErrNilTool for a nil entry in tools.
func FilterToolsByIDs(tools []Tool, ids []string) ([]Tool, error) {
	var active map[string]struct{}
	if ids != nil {
		active = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			active[id] = struct{}{}
		}
	}
	var out []Tool
	for i, t := range tools {
		if t == nil {
			return nil, fmt.Errorf("filter tools: %w at index %d", ErrNilTool, i)
		}
		if active != nil && !IsServerTool(t) {
			if _, ok := active[t.ToolName()]; !ok {
				continue
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
