package agent

import "context"

// LifecycleHookContext is provided to every lifecycle-hook handler invocation.
//
// Ctx is the idiomatic-Go stand-in for upstream's `AbortSignal`: it is
// canceled if the manager's AbortInflight is called while the emit is still
// running (idiomatic divergence: context.Context for cancellation, not
// AbortSignal — see upstreamer.md). Handlers that kick off background work
// via AsyncOutput should observe Ctx.Done() for cancellation.
type LifecycleHookContext struct {
	Ctx context.Context
	// HookName is the name of the hook currently emitting (useful for shared
	// handlers registered against multiple hooks).
	HookName string
	// SessionID is the current session id. This is the single source for
	// session identity in handlers — payloads deliberately do not repeat it.
	// The engine threads it per emit (safe for a manager shared across
	// concurrent runs); direct Emit callers get the manager-level default
	// from SetSessionID unless they pass a per-emit override.
	SessionID string
}

// AsyncOutput signals fire-and-forget mode: the chain proceeds immediately
// without waiting for completion. Any background work the handler kicked off
// should be attached via Work so the manager can track it for Drain and
// enforce AsyncTimeout.
//
// Work is a channel (Go's stream-oriented stand-in for upstream's
// `work?: Promise<unknown>`) that the handler's background goroutine should
// close (optionally sending a non-nil error first) when it finishes.
type AsyncOutput struct {
	// Work is optional; nil means "no work to track".
	Work <-chan error
	// AsyncTimeout bounds how long the manager waits for Work before giving
	// up and logging a warning. Zero means DefaultAsyncTimeout.
	AsyncTimeout int64 // milliseconds
}

// DefaultAsyncTimeout is the default number of milliseconds the manager
// waits for a handler's detached AsyncOutput.Work before abandoning the wait.
const DefaultAsyncTimeout int64 = 30_000

// HookHandlerResult is what a hook handler returns: either a synchronous
// result (HasResult true), a fire-and-forget AsyncOutput signal (Async
// non-nil), or neither (void/observation-only handlers).
type HookHandlerResult[R any] struct {
	Result    R
	HasResult bool
	Async     *AsyncOutput
}

// SyncResult wraps a synchronous handler result.
func SyncResult[R any](result R) HookHandlerResult[R] {
	return HookHandlerResult[R]{Result: result, HasResult: true}
}

// VoidResult signals a side-effect-only handler outcome (no result to collect).
func VoidResult[R any]() HookHandlerResult[R] {
	return HookHandlerResult[R]{}
}

// AsyncResult wraps a fire-and-forget AsyncOutput signal.
func AsyncResult[R any](async AsyncOutput) HookHandlerResult[R] {
	return HookHandlerResult[R]{Async: &async}
}

// HookHandler receives the payload and context for one hook invocation.
type HookHandler[P, R any] func(payload P, hctx LifecycleHookContext) (HookHandlerResult[R], error)

// ToolMatcher filters tool-scoped hook invocation by tool name. Accepted
// dynamic values: nil (wildcard), string (exact match), *regexp.Regexp
// (MatchString), or func(string) bool (arbitrary predicate). See MatchesTool.
type ToolMatcher = any

// HookEntry is one registered handler for a hook.
type HookEntry[P, R any] struct {
	Handler HookHandler[P, R]
	Matcher ToolMatcher
	Filter  func(P) bool
}

// EmitResult is the result of emitting a hook through its handler chain.
type EmitResult[R, P any] struct {
	// Results are the sync results returned by handlers that produced one.
	Results []R
	// Pending are handles to detached async handler work (see Drain).
	Pending []<-chan struct{}
	// FinalPayload is the payload after all mutation piping has been applied.
	FinalPayload P
	// Blocked is true if any handler triggered a block/reject short-circuit.
	Blocked bool
	// BlockReason is the first non-empty block/reject reason, if any.
	BlockReason string
	// Mutated is true if any handler's result actually piped a mutation into
	// the payload (e.g. PreToolUse MutatedInput, UserPromptSubmit MutatedPrompt).
	Mutated bool
}

// HooksManagerOptions configures a HooksManager.
type HooksManagerOptions struct {
	// ThrowOnHandlerError: if true, a handler error stops the chain and
	// propagates the error. If false (default), the error is logged as a
	// warning and execution continues.
	ThrowOnHandlerError bool
}

// HookDefinition describes a hook's payload/result Go types. Go has no
// runtime schema equivalent to Zod (see hooks_schemas.go); this exists so a
// HookRegistry can be introspected the way upstream's HookRegistry can,
// without carrying runtime validators.
type HookDefinition struct {
	PayloadType string
	ResultType  string
}

// HookRegistry maps custom hook names to their definitions. Provided for API
// parity with upstream's HookRegistry; Go's custom-hook registration (see On)
// is fully generic and does not require populating a HookRegistry up front.
type HookRegistry = map[string]HookDefinition
