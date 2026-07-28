package agent

import (
	"log"
	"time"
)

// erasedHandler is a type-erased HookHandler, used internally so a single
// HooksManager registry can hold entries of many different (P, R) type pairs.
// Payload/result values cross this boundary as `any`; the type-safe surface
// is On/Off/EmitXxx in hooks_manager.go.
type erasedHandler func(payload any, hctx LifecycleHookContext) (erasedOutcome, error)

type erasedOutcome struct {
	Result    any
	HasResult bool
	Async     *AsyncOutput
}

type erasedFilter func(any) bool

type erasedEntry struct {
	handler erasedHandler
	matcher ToolMatcher
	filter  erasedFilter
	// orig holds the original typed HookHandler[P, R], boxed as `any`, so Off
	// can attempt a best-effort reflect.Pointer() match against it.
	orig any
}

// hookBehavior is the erased, data-driven per-hook mutation/block table,
// mirroring upstream's HOOK_BEHAVIOR. Hooks absent from hookBehaviors are
// observation-only: their handlers' results are collected but never alter
// the payload or short-circuit the chain. Custom hooks never participate.
type hookBehavior struct {
	// mutate applies a result's mutation field onto payload, returning the
	// (possibly new) payload and whether a mutation was actually applied.
	mutate func(payload any, result any) (any, bool)
	// isBlocked reports whether result triggers this hook's short-circuit,
	// and the reason (if any) to surface to the caller.
	isBlocked func(result any) (bool, string)
}

var hookBehaviors = map[string]hookBehavior{
	string(HookNamePreToolUse): {
		mutate: func(payload any, result any) (any, bool) {
			p, ok := payload.(PreToolUsePayload)
			r, ok2 := result.(PreToolUseResult)
			if !ok || !ok2 || r.MutatedInput == nil {
				return payload, false
			}
			p.ToolInput = r.MutatedInput
			return p, true
		},
		isBlocked: func(result any) (bool, string) {
			r, ok := result.(PreToolUseResult)
			if !ok || !r.Blocked() {
				return false, ""
			}
			reason := r.BlockReason
			if reason == "" {
				reason = "Blocked by PreToolUse hook"
			}
			return true, reason
		},
	},
	string(HookNameUserPromptSubmit): {
		mutate: func(payload any, result any) (any, bool) {
			p, ok := payload.(UserPromptSubmitPayload)
			r, ok2 := result.(UserPromptSubmitResult)
			if !ok || !ok2 || r.MutatedPrompt == nil {
				return payload, false
			}
			p.Prompt = *r.MutatedPrompt
			return p, true
		},
		isBlocked: func(result any) (bool, string) {
			r, ok := result.(UserPromptSubmitResult)
			if !ok || !r.Rejected() {
				return false, ""
			}
			reason := r.RejectReason
			if reason == "" {
				reason = "Prompt rejected by hook"
			}
			return true, reason
		},
	},
}

// chainOptions configures one executeHandlerChain call.
type chainOptions struct {
	hookName            string
	throwOnHandlerError bool
	toolName            string
	hasToolName         bool
	onAsyncTimeout      func()
}

// chainOutcome is executeHandlerChain's erased result, converted to a typed
// EmitResult by the caller (HooksManager.emitAny's typed wrappers).
type chainOutcome struct {
	results      []any
	pending      []<-chan struct{}
	finalPayload any
	blocked      bool
	blockReason  string
	mutated      bool
}

// executeHandlerChain runs entries sequentially against initialPayload.
//
// Supports:
//   - ToolMatcher and filter-based skipping (matcher fails closed: an entry
//     with a matcher and no options.toolName is skipped)
//   - Sync results collected into results
//   - Async fire-and-forget via a returned AsyncOutput — the handler's Work
//     channel is tracked in pending without being awaited here
//   - Per-hook mutation piping and block short-circuit, driven by hookBehaviors
//   - Cooperative abort via hctx.Ctx: the chain checks ctx.Err() between
//     handlers and bails out when canceled, so AbortInflight has a
//     deterministic effect on the chain itself
//
// Payload isolation: results/mutation are computed by-value on Go structs
// (not shared references), so mutation piping never mutates the caller's
// payload — a stronger guarantee than upstream's "top-level-only" note (which
// exists there because JS objects are mutable references; Go struct copies
// are not).
func executeHandlerChain(entries []*erasedEntry, initialPayload any, hctx LifecycleHookContext, opts chainOptions) (chainOutcome, error) {
	var results []any
	var pending []<-chan struct{}
	currentPayload := initialPayload
	blocked := false
	blockReason := ""
	mutated := false

	behavior, hasBehavior := hookBehaviors[opts.hookName]

	for i, entry := range entries {
		if hctx.Ctx != nil && hctx.Ctx.Err() != nil {
			break
		}

		if entry == nil {
			continue
		}

		gate, err := evaluateEntryGate(entry, currentPayload, i, opts)
		if err != nil {
			return chainOutcome{}, err
		}
		if !gate {
			continue
		}

		outcome, err := entry.handler(currentPayload, hctx)
		if err != nil {
			if opts.throwOnHandlerError {
				return chainOutcome{}, err
			}
			log.Printf("[HooksManager] Handler %d for hook %q threw: %v", i, opts.hookName, err)
			continue
		}

		if outcome.Async != nil {
			if outcome.Async.Work != nil {
				pending = append(pending, trackAsyncWork(outcome.Async, opts.hookName, opts.onAsyncTimeout))
			}
			continue
		}
		if !outcome.HasResult {
			continue
		}

		results = append(results, outcome.Result)

		if hasBehavior && behavior.mutate != nil {
			newPayload, changed := behavior.mutate(currentPayload, outcome.Result)
			if changed {
				currentPayload = newPayload
				mutated = true
			}
		}

		if hasBehavior && behavior.isBlocked != nil {
			isBlocked, reason := behavior.isBlocked(outcome.Result)
			if isBlocked {
				blocked = true
				if reason != "" {
					blockReason = reason
				}
				break
			}
		}
	}

	return chainOutcome{
		results:      results,
		pending:      pending,
		finalPayload: currentPayload,
		blocked:      blocked,
		blockReason:  blockReason,
		mutated:      mutated,
	}, nil
}

// evaluateEntryGate evaluates an entry's matcher and filter under the
// chain's error policy. Matchers fail closed: if a matcher is registered and
// no toolName is available for this emit, the handler is skipped rather than
// invoked globally.
func evaluateEntryGate(entry *erasedEntry, payload any, index int, opts chainOptions) (bool, error) {
	defer func() {
		// Guard against a panicking matcher/filter function the same way
		// upstream's try/catch does for a throwing matcher or filter.
		if r := recover(); r != nil {
			log.Printf("[HooksManager] Matcher/filter for handler %d of hook %q panicked: %v", index, opts.hookName, r)
		}
	}()

	matcherPasses := entry.matcher == nil || (opts.hasToolName && MatchesTool(entry.matcher, opts.toolName))
	if !matcherPasses {
		return false, nil
	}
	if entry.filter == nil {
		return true, nil
	}
	return entry.filter(payload), nil
}

// trackAsyncWork returns a channel that closes when the handler's detached
// AsyncOutput.Work settles OR the timeout fires, whichever is first. On
// timeout, onTimeout is invoked (the manager uses it to cancel the emit's
// context) and the tracking channel still closes so Drain stops waiting.
func trackAsyncWork(async *AsyncOutput, hookName string, onTimeout func()) <-chan struct{} {
	done := make(chan struct{})
	if async.Work == nil {
		close(done)
		return done
	}
	timeoutMs := async.AsyncTimeout
	if timeoutMs <= 0 {
		timeoutMs = DefaultAsyncTimeout
	}
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)

	go func() {
		defer close(done)
		defer timer.Stop()
		select {
		case err, ok := <-async.Work:
			if ok && err != nil {
				log.Printf("[HooksManager] Async work for hook %q rejected: %v", hookName, err)
			}
		case <-timer.C:
			log.Printf("[HooksManager] Async work for hook %q exceeded its %dms timeout; abandoning wait.", hookName, timeoutMs)
			if onTimeout != nil {
				onTimeout()
			}
		}
	}()
	return done
}
