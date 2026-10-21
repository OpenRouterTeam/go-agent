package agent

import (
	"context"
	"log"
	"reflect"
	"sync"
)

// HooksManager is a typed, extensible hook system for agent lifecycle
// events (upstream #7/#67, the 0.8.0 headline). Supports both the ten
// built-in hooks (PreToolUse, PostToolUse, ...) and user-defined custom
// hooks registered via the package-level generic On/Emit functions (Go's
// generics stand-in for upstream's `AllHooks<Custom>` conditional-type
// merging — idiomatic divergence #5 in upstreamer.md).
type HooksManager struct {
	mu                  sync.Mutex
	entries             map[string][]*erasedEntry
	pending             map[<-chan struct{}]struct{}
	inflight            map[int]context.CancelFunc
	nextToken           int
	throwOnHandlerError bool
	sessionID           string
}

// NewHooksManager constructs a HooksManager. opts is variadic so the common
// zero-options case (agent.NewHooksManager()) needs no empty struct literal.
func NewHooksManager(opts ...HooksManagerOptions) *HooksManager {
	m := &HooksManager{
		entries:  map[string][]*erasedEntry{},
		pending:  map[<-chan struct{}]struct{}{},
		inflight: map[int]context.CancelFunc{},
	}
	if len(opts) > 0 {
		m.throwOnHandlerError = opts[0].ThrowOnHandlerError
	}
	return m
}

// SetSessionID sets the manager-level default session ID exposed as
// hctx.SessionID to handler invocations.
//
// This is a single mutable default on the manager instance: when one manager
// is shared by concurrent runs, callers MUST pass SessionID in EmitOptions
// instead (as CallModel does), otherwise the last SetSessionID call wins and
// concurrent emits observe the wrong id.
func (m *HooksManager) SetSessionID(sessionID string) {
	m.mu.Lock()
	m.sessionID = sessionID
	m.mu.Unlock()
}

func (m *HooksManager) defaultSessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

// EmitOptions configures one Emit call.
type EmitOptions struct {
	// ToolName scopes tool-matcher-gated hooks (PreToolUse, PostToolUse, ...).
	ToolName string
	// SessionID overrides the manager-level default from SetSessionID for
	// this emit only. Pass it whenever the manager instance may be shared
	// across concurrent runs (CallModel always does).
	SessionID string
}

// On registers a handler for hookName (built-in or custom) and returns an
// unsubscribe function.
func On[P, R any](m *HooksManager, hookName string, entry HookEntry[P, R]) func() {
	var filter erasedFilter
	if entry.Filter != nil {
		f := entry.Filter
		filter = func(payload any) bool {
			p, ok := payload.(P)
			if !ok {
				return false
			}
			return f(p)
		}
	}
	handler := entry.Handler
	h := func(payload any, hctx LifecycleHookContext) (erasedOutcome, error) {
		p, ok := payload.(P)
		if !ok {
			log.Printf("[HooksManager] payload type mismatch for hook %q handler; skipping", hctx.HookName)
			return erasedOutcome{}, nil
		}
		result, err := handler(p, hctx)
		if err != nil {
			return erasedOutcome{}, err
		}
		return erasedOutcome{Result: result.Result, HasResult: result.HasResult, Async: result.Async}, nil
	}
	ee := &erasedEntry{handler: h, matcher: entry.Matcher, filter: filter, orig: entry.Handler}
	return m.register(hookName, ee)
}

func (m *HooksManager) register(hookName string, entry *erasedEntry) func() {
	m.mu.Lock()
	m.entries[hookName] = append(m.entries[hookName], entry)
	m.mu.Unlock()

	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		list := m.entries[hookName]
		for i, e := range list {
			if e == entry {
				m.entries[hookName] = append(list[:i:i], list[i+1:]...)
				return
			}
		}
	}
}

// Off removes a specific handler function from a hook. Returns true if found
// and removed.
//
// Go function values are not comparable, so this is a best-effort match on
// the handler's code pointer via reflection (works for named functions and
// most closures, but two independently-created closures over identical code
// can share a pointer). The unsubscribe function returned by On is the
// precise removal path; prefer it when available.
func Off[P, R any](m *HooksManager, hookName string, handler HookHandler[P, R]) bool {
	target := reflect.ValueOf(handler).Pointer()
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.entries[hookName]
	for i, e := range list {
		h, ok := e.orig.(HookHandler[P, R])
		if !ok {
			continue
		}
		if reflect.ValueOf(h).Pointer() == target {
			m.entries[hookName] = append(list[:i:i], list[i+1:]...)
			return true
		}
	}
	return false
}

// RemoveAll removes all handlers for a specific hook.
func (m *HooksManager) RemoveAll(hookName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, hookName)
}

// RemoveAllHooks removes every handler for every hook.
func (m *HooksManager) RemoveAllHooks() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[string][]*erasedEntry{}
}

// HasHandlers reports whether any handlers are registered for hookName.
func (m *HooksManager) HasHandlers(hookName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries[hookName]) > 0
}

// Drain awaits all in-flight async handler work. Used for graceful shutdown;
// CallModel calls this unconditionally on every exit path so fire-and-forget
// hook work is never silently dropped.
func (m *HooksManager) Drain() {
	for {
		m.mu.Lock()
		if len(m.pending) == 0 {
			m.mu.Unlock()
			return
		}
		snapshot := make([]<-chan struct{}, 0, len(m.pending))
		for ch := range m.pending {
			snapshot = append(snapshot, ch)
		}
		m.mu.Unlock()
		for _, ch := range snapshot {
			<-ch
		}
	}
}

// AbortInflight cancels the context passed to every in-flight Emit call.
// Handlers that kick off async work should observe hctx.Ctx to honor this.
//
// Does not remove pending async work from the drain set — callers that want
// to wait for handlers to wind down should still call Drain afterward.
func (m *HooksManager) AbortInflight() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.inflight))
	for _, c := range m.inflight {
		cancels = append(cancels, c)
	}
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// emitAny validates nothing at runtime (Go's type system is the validation
// layer — see hooks_schemas.go), invokes matching handlers in registration
// order, and returns the erased outcome.
func (m *HooksManager) emitAny(hookName string, payload any, opts EmitOptions) (chainOutcome, error) {
	m.mu.Lock()
	list := append([]*erasedEntry{}, m.entries[hookName]...)
	m.mu.Unlock()

	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = m.defaultSessionID()
	}

	ctx, cancel := context.WithCancel(context.Background())
	token := m.trackInflight(cancel)

	hctx := LifecycleHookContext{Ctx: ctx, HookName: hookName, SessionID: sessionID}

	outcome, err := executeHandlerChain(list, payload, hctx, chainOptions{
		hookName:            hookName,
		throwOnHandlerError: m.throwOnHandlerError,
		toolName:            opts.ToolName,
		hasToolName:         opts.ToolName != "",
		onAsyncTimeout:      cancel,
	})

	if len(outcome.pending) == 0 {
		m.dropInflight(token)
	} else {
		m.trackPending(token, outcome.pending)
	}

	return outcome, err
}

func (m *HooksManager) trackInflight(cancel context.CancelFunc) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextToken++
	token := m.nextToken
	m.inflight[token] = cancel
	return token
}

func (m *HooksManager) dropInflight(token int) {
	m.mu.Lock()
	delete(m.inflight, token)
	m.mu.Unlock()
}

// trackPending keeps this emit's inflight controller registered until every
// piece of detached work settles (mirrors upstream: the controller must stay
// registered so AbortInflight can still reach fire-and-forget handlers that
// outlive the emit call itself), and tracks each channel for Drain.
func (m *HooksManager) trackPending(token int, pending []<-chan struct{}) {
	m.mu.Lock()
	for _, ch := range pending {
		m.pending[ch] = struct{}{}
	}
	remaining := len(pending)
	m.mu.Unlock()

	for _, ch := range pending {
		ch := ch
		go func() {
			<-ch
			m.mu.Lock()
			delete(m.pending, ch)
			remaining--
			if remaining == 0 {
				delete(m.inflight, token)
			}
			m.mu.Unlock()
		}()
	}
}

// emitTyped is the shared implementation behind Emit and every EmitXxx
// built-in wrapper: it erases payload to `any` for emitAny and converts the
// erased outcome back to typed results.
func emitTyped[P, R any](m *HooksManager, hookName string, payload P, opts EmitOptions) (EmitResult[R, P], error) {
	outcome, err := m.emitAny(hookName, payload, opts)
	if err != nil {
		return EmitResult[R, P]{}, err
	}
	results := make([]R, 0, len(outcome.results))
	for _, r := range outcome.results {
		if typed, ok := r.(R); ok {
			results = append(results, typed)
		}
	}
	finalPayload, ok := outcome.finalPayload.(P)
	if !ok {
		finalPayload = payload
	}
	return EmitResult[R, P]{
		Results:      results,
		Pending:      outcome.pending,
		FinalPayload: finalPayload,
		Blocked:      outcome.blocked,
		BlockReason:  outcome.blockReason,
		Mutated:      outcome.mutated,
	}, nil
}

// Emit emits a custom (non-built-in) hook. Built-in hooks have typed
// EmitXxx methods on HooksManager (EmitPreToolUse, EmitPostToolUse, ...).
func Emit[P, R any](m *HooksManager, hookName string, payload P, opts EmitOptions) (EmitResult[R, P], error) {
	return emitTyped[P, R](m, hookName, payload, opts)
}

//#region Built-in hook convenience wrappers
//
// Go cannot express upstream's `AllHooks<Custom>` merged registry as a single
// generic on()/emit() pair with per-call type inference from a string
// literal, so each built-in hook gets a concrete OnXxx/EmitXxx pair. Custom
// hooks use the package-level generic On/Emit with explicit type arguments.

func (m *HooksManager) OnPreToolUse(entry HookEntry[PreToolUsePayload, PreToolUseResult]) func() {
	return On(m, string(HookNamePreToolUse), entry)
}

func (m *HooksManager) EmitPreToolUse(payload PreToolUsePayload, opts EmitOptions) (EmitResult[PreToolUseResult, PreToolUsePayload], error) {
	return emitTyped[PreToolUsePayload, PreToolUseResult](m, string(HookNamePreToolUse), payload, opts)
}

func (m *HooksManager) OnPostToolUse(entry HookEntry[PostToolUsePayload, EmptyHookResult]) func() {
	return On(m, string(HookNamePostToolUse), entry)
}

func (m *HooksManager) EmitPostToolUse(payload PostToolUsePayload, opts EmitOptions) (EmitResult[EmptyHookResult, PostToolUsePayload], error) {
	return emitTyped[PostToolUsePayload, EmptyHookResult](m, string(HookNamePostToolUse), payload, opts)
}

func (m *HooksManager) OnPostToolUseFailure(entry HookEntry[PostToolUseFailurePayload, EmptyHookResult]) func() {
	return On(m, string(HookNamePostToolUseFailure), entry)
}

func (m *HooksManager) EmitPostToolUseFailure(payload PostToolUseFailurePayload, opts EmitOptions) (EmitResult[EmptyHookResult, PostToolUseFailurePayload], error) {
	return emitTyped[PostToolUseFailurePayload, EmptyHookResult](m, string(HookNamePostToolUseFailure), payload, opts)
}

func (m *HooksManager) OnUserPromptSubmit(entry HookEntry[UserPromptSubmitPayload, UserPromptSubmitResult]) func() {
	return On(m, string(HookNameUserPromptSubmit), entry)
}

func (m *HooksManager) EmitUserPromptSubmit(payload UserPromptSubmitPayload, opts EmitOptions) (EmitResult[UserPromptSubmitResult, UserPromptSubmitPayload], error) {
	return emitTyped[UserPromptSubmitPayload, UserPromptSubmitResult](m, string(HookNameUserPromptSubmit), payload, opts)
}

func (m *HooksManager) OnStop(entry HookEntry[StopPayload, StopResult]) func() {
	return On(m, string(HookNameStop), entry)
}

func (m *HooksManager) EmitStop(payload StopPayload, opts EmitOptions) (EmitResult[StopResult, StopPayload], error) {
	return emitTyped[StopPayload, StopResult](m, string(HookNameStop), payload, opts)
}

func (m *HooksManager) OnPermissionRequest(entry HookEntry[PermissionRequestPayload, PermissionRequestResult]) func() {
	return On(m, string(HookNamePermissionRequest), entry)
}

func (m *HooksManager) EmitPermissionRequest(payload PermissionRequestPayload, opts EmitOptions) (EmitResult[PermissionRequestResult, PermissionRequestPayload], error) {
	return emitTyped[PermissionRequestPayload, PermissionRequestResult](m, string(HookNamePermissionRequest), payload, opts)
}

func (m *HooksManager) OnSessionStart(entry HookEntry[SessionStartPayload, EmptyHookResult]) func() {
	return On(m, string(HookNameSessionStart), entry)
}

func (m *HooksManager) EmitSessionStart(payload SessionStartPayload, opts EmitOptions) (EmitResult[EmptyHookResult, SessionStartPayload], error) {
	return emitTyped[SessionStartPayload, EmptyHookResult](m, string(HookNameSessionStart), payload, opts)
}

func (m *HooksManager) OnSessionEnd(entry HookEntry[SessionEndPayload, EmptyHookResult]) func() {
	return On(m, string(HookNameSessionEnd), entry)
}

func (m *HooksManager) EmitSessionEnd(payload SessionEndPayload, opts EmitOptions) (EmitResult[EmptyHookResult, SessionEndPayload], error) {
	return emitTyped[SessionEndPayload, EmptyHookResult](m, string(HookNameSessionEnd), payload, opts)
}

func (m *HooksManager) OnPostModelCall(entry HookEntry[PostModelCallPayload, EmptyHookResult]) func() {
	return On(m, string(HookNamePostModelCall), entry)
}

func (m *HooksManager) EmitPostModelCall(payload PostModelCallPayload, opts EmitOptions) (EmitResult[EmptyHookResult, PostModelCallPayload], error) {
	return emitTyped[PostModelCallPayload, EmptyHookResult](m, string(HookNamePostModelCall), payload, opts)
}

func (m *HooksManager) OnDoomLoopDetected(entry HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]) func() {
	return On(m, string(HookNameDoomLoopDetected), entry)
}

func (m *HooksManager) EmitDoomLoopDetected(payload DoomLoopDetectedPayload, opts EmitOptions) (EmitResult[DoomLoopDetectedResult, DoomLoopDetectedPayload], error) {
	return emitTyped[DoomLoopDetectedPayload, DoomLoopDetectedResult](m, string(HookNameDoomLoopDetected), payload, opts)
}

//#endregion
