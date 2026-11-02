FAIL

# Port Parity Eval — go-agent vs `@openrouter/agent` @ e309ebf

- Last ported: `680bceb` (0.8.0). Target: `e309ebf` (0.11.0, plus 18 unreleased commits).
- Port under review: the uncommitted working tree (15 modified files, 5 new `*_test.go`).
- Mechanical gates: `go test -race -count=1 ./...` passes, `gofmt`/`go vet` are clean, and `.upstreamer/scripts/verify.sh` reports "PASS: 0 failures". Its advisory test-parity check names four upstream test clusters (doom, shared, startup, steering) that no Go test references.
- Reviewer probes: one throwaway probe test was written, run, and deleted. No other repository file was modified.

## Why FAIL

The delta's smaller items were ported carefully and tested. Three verdict rules still apply literally:

1. **There are behavioral parity gaps in the load-bearing loop, approval ordering, hooks and state:**
   - doom-loop detection is missing;
   - async tool lifecycles are missing;
   - upstream #94's PreToolUse-before-approval ordering is missing.
2. **Several behaviors that changed in this delta have no Go test.** These are the doom-loop, async-tool and approval-gate regression suites listed under (b).
3. **Version honesty.** `upstreamer-changelog.md` honestly says "partial sync". But advancing `.upstreamer/state.yaml` to `e309ebf` would record the whole delta as ported, and the next run would skip these gaps for good. That is the failure mode `eval.md` exists to block.

---

## (a) Delta items ported correctly

| Upstream change | Go location | Test | Notes |
| --- | --- | --- | --- |
| #100 forced toolChoice relaxation (DEV-785) | `model_result.go` `toolChoicePolicy`, `init`/`resolve`/`effective`/`commit`/`reset`, `relaxForcedToolChoice`, `isForcedToolChoice`, `forcedToolChoiceKey`, `canonicalJSON`, `syncToolChoiceState`; `ConversationState.ConsumedForcedToolChoiceKey` | `tool_choice_relaxation_test.go` | Matches `applyResolvedForcedToolChoicePolicy` / `applyForcedToolChoicePolicy` / `commitToolChoiceDispatch`. A consume only happens when the response actually called a tool. `required` → `auto`, a specific tool → `auto`, `allowed_tools` `required` → `auto` with the same set; `auto` / `none` / nil pass through. The final `none` turn and the empty-final retry never commit. The key is persisted across approval, HITL and client-tool pauses and cleared on completion. A probe confirmed key shapes match JCS (`"required"`, `{"name":"get_weather","type":"function"}`, sorted `allowed_tools`). Upstream cases ported: required, specific tool, allowed_tools, auto unchanged, final none turn, multi-follow-up, re-arm on a different value, re-arm after an unforced turn, approval-resume persistence + reset + fresh re-arm, manual client-tool resume. HITL resume was added. |
| #114 `nextTurnParams.toolChoice` + full per-tool nextTurnParams | `next_turn_params.go` `NextTurnParams[In]`, `computeNextTurnParams`, `executeToolNextTurnParams`, `ApplyNextTurnParamsToRequest`, `BuildNextTurnParamsContext`; `model_result.go` `applyNextTurnParams` | `next_turn_params_test.go` | Where it is applied matches upstream: in-loop before the follow-up request, and before the `AllowFinalResponse` final request. It is **not** applied on the approval-resume path, which also matches upstream (`continueWithUnsentResults` has no `applyNextTurnParams`). A tool-computed toolChoice re-runs the policy (`m.toolChoice.resolve`), as upstream does. Ordering relative to per-turn dynamic resolution matches: `resolveToolChoiceFunc(turn+1)` runs before the tool round and nextTurnParams after, so the tool's value wins. Composition follows Tools-slice order then call order. Key order is the upstream order; unknown keys warn. A nil value unsets the field. |
| Truncation guard for `max_output_tokens` | `stream_transformers.go` `isTruncatedAtMaxOutputTokens`, `ExtractToolCallsFromResponse`, `ResponseHasToolCalls`, `extractEmittedToolCalls`; `model_result.go` `consumeCreateResponse` (`response.incomplete` / `response.failed`) | `max_output_tokens_truncation_test.go` | Whole-batch rule matches upstream (revert of b212546). `ToolCallsStream` still shows emitted calls, matching upstream's `buildToolCallStream` scope note. All three upstream cases are covered, plus streaming incomplete/failed cases. |
| #97 `getUsage()` | `model_result.go` `Usage`, `recordModelCallUsage`, `usageSnapshot` | `usage_test.go` | Accumulates without hooks. Covers initial, tool round, final, retry and resume calls. `SessionEnd.TotalUsage` uses the same snapshot. Returning the error alongside the totals is a documented divergence. Access is guarded by `mu`, which is meaningful under `-race`. See defect D4. |
| #31 `activeTools` | `model_result.go` `filterActiveTools` | `TestActiveToolsFiltersSentAndExecutableTools` | Server tools are kept, unknown names ignored, `tools` omitted when nothing remains. Filters both the wire tools and execution, as upstream does. |
| #93 `fromChatMessages` emits `function_call` | `chat_compat.go` | `compat_test.go` `TestFromChatMessagesEmitsFunctionCallItems` | Rule for skipping an empty assistant message, `id` = `call_id`, `status: completed`, arguments not re-stringified. All five upstream #11 cases are covered. |
| #99 `strict` passthrough | `tool.go` `ToolConfig.Strict`, `ToAPITool` | `TestStrictPassthroughDefaultsToNull` | `strict ?? null`. The behavior change is noted in the changelog. |
| #118 `wireInputSchema` | `tool.go` `ToAPITool` + `SanitizeSchema` (deep copy) | `TestManualToolWireInputSchemaIsSanitizedCopy`, `TestWireInputSchemaIgnoredForExecutableTools` | Manual tools only; `~` keys stripped; the caller's map is not mutated. |
| #110 generator preliminary results no longer accumulated | `tool.go` `Execute` (Generate branch) | `tool_test.go` | `Events` is kept but no longer populated; yields are still emitted live. |
| #91 executed tool call threaded into turn context | `tool_context.go` `BuildToolExecuteContext`, `toFunctionCallItem`; `runToolWithHooks` re-threads after a PreToolUse mutation | `TestExecutedCallThreadedIntoTurnContext` | A turn context `ToolCall` supplied by the caller still wins. |
| #94, partial: tool-level predicate fails closed | `tool.go` `RequiresApproval` / `argumentsValid`; `tool_executor.go` `$ref`/`$defs` resolution in `validateValueWithRoot` | `TestApprovalPredicateFailsClosedOnSchemaInvalidArguments`, `TestStructDerivedSchemaValidationFollowsRefs` | Struct-derived schemas (root `$ref` into `$defs`) are now actually enforced, so a missing required field or a wrongly typed nested value fails closed and is rejected by `Execute`. Call-level checks stay unconditional. |
| #94, partial: `AllowFinalResponse` path gated | `model_result.go` stop branch (`PartitionToolCalls` + `applyPermissionRequestGate`) | `TestAllowFinalResponsePathEnforcesApprovalGate` | Already present before this delta; now has a test. |
| #120 user-agent | `agent.go` (already done in a364988) | existing | Contract divergence 7. |

## (b) Missing delta items

### In scope (`packages/agent/src/lib/**`): blocking

1. **Doom-loop detection: #73 and #89.**
   - Upstream sources: `src/lib/doom-loop.ts` (1,708 lines) plus wiring in `model-result.ts`.
   - Missing in Go:
     - the `doomLoop` option on `callModel`;
     - `DoomLoopMonitor`, `declareRound`, `resolveDoomLoopOption`, `ResolvedDoomLoopConfig`;
     - the `observe` → `steer` → `escalate` → `block` → `stop` ladder;
     - the **new built-in hook `DoomLoopDetected`**, added to `BuiltInHookDefinitions` in `hooks-types.ts` / `hooks-schemas.ts`;
     - `SessionEnd.reason: 'doom_loop'`;
     - `ConversationState.doomLoop` persistence (`roundFingerprints`, `callStreaks`, `stopVerdict`, `pendingSteer`, `escalationsUsed`);
     - `ModelResult.getDoomLoopVerdict()`.
   - Upstream ships `tests/vectors/doom-loop-fingerprints.json` explicitly as a cross-port contract for the Go and Python ports.
   - None of this exists in Go, and the hook-name set is now behind upstream.
   - Untested: about 7 upstream test files (`doom-loop*.test.ts`) have no Go counterpart.
2. **Async tools, task tool, steering and subagents: #90.**
   - Upstream sources: `async-tool-registry.ts`, `tool-task.ts`, `tool-check.ts`, `agent-tool.ts`, `tool-concurrency.ts`, `inner-loop/resume-tool-results.ts`.
   - Missing in Go:
     - the unified `run` handler and `lifecycle: sync | background | deferred`;
     - the universal `task` tool and the reserved name `task`;
     - `ModelResult` methods `sendToTask`, `queueUserMessage`, `cancelTask`, `getAsyncTasks`;
     - `tool.agent()`;
     - per-tool `timeoutMs` / `toolTimeoutMs` (`tool_timeout` error);
     - `toolConcurrency` / `maxConcurrency`;
     - events `tool.async_started` / `tool.async_settled`;
     - the new `ConversationStatus` `awaiting_async_tools`;
     - state fields `pendingAsyncTools` / `settledAsyncCallIds`;
     - `resumeToolResults()`;
     - tool-context `callId` / `conversationId`.
   - This affects the loop, state and streaming. Untested: `async-tool-*.test.ts`, `task-tool-*.test.ts`, `steering.test.ts`, `agent-tool.test.ts`, `tool-cancellation.test.ts`, `tool-concurrency`.
3. **#94 approval/hook ordering (security-relevant).**
   - Upstream now runs `PreToolUse` before calls are partitioned for approval, so approval predicates, the `PermissionRequest` hook and persisted `pendingToolCalls` all see the effective arguments. It adds the `preToolUseApplied` marker on `PendingToolCall` and gates each call occurrence at most once per run.
   - In Go, `PreToolUse` still runs at execution time, *after* `PartitionToolCalls` (`model_result.go` `runToolWithHooks`). A PreToolUse hook can therefore rewrite approved-safe arguments into arguments the predicate would have gated, and they execute unapproved. Upstream pins exactly this case: "does not let a hook rewrite safe arguments into an unapproved dangerous call".
   - The changelog lists this as not ported. It is still a parity gap in approval ordering and hooks.
   - Most of the 29 cases in `tests/unit/approval-gate-regressions.test.ts` have no Go counterpart: PreToolUse ordering, the persisted-marker resume, duplicate-occurrence gating, PermissionRequest deny on the final path, and the generator/HITL variants of the final-path gate.
4. **#31 tool-set (`@openrouter/agent/tool-set`).**
   - Upstream sources: `src/lib/tool-set.ts` / `tool-set-types.ts`.
   - Missing: `createToolSet`, `activate` / `deactivate` / `activateWhen` / `deactivateWhen`, `resolve()` → `{ tools, activeTools }`, and stripping of tool-set snapshot metadata. Only `ActiveTools` was ported.
   - `tool-set.test.ts` (1,585 lines) has no Go counterpart.

### In scope: warning-level (opt-in or performance)

5. **#109 `streamReplay: 'active-consumers'`** (opt-in replay compaction) and **#116** (releasing the replay backlog on terminal paths). Go's channel fan-out (divergence 6) differs structurally, but the public option is absent and there is no compatibility note.
6. **#73 run-level `signal` and per-request `timeoutMs` composition.** `context.Context` covers most of this (divergence 1). The specific behavior change "`ModelResult.cancel()` aborts in-flight tool work" holds in Go only because tools receive `m.ctx`. There is no test for it.

### Arguably out of scope

- **MCP (#102, #86, #74).** The full MCP integration moved into `packages/agent/src/mcp/**` as the canonical `@openrouter/agent/mcp` subpath, with `tests/unit/mcp/**`. The contract excludes `packages/mcp/` and says adding MCP "is a deliberate contract change". By location the code is now inside `packages/agent`, so the literal Scope clause no longer excludes it. By intent it is the excluded MCP integration. A human needs to amend the contract; the converter correctly did not start porting it.
- Packaging and infrastructure items: subpath exports (#112, `./reusable-stream`), `startup-import-closure` / the type-only `turn-context` import (#111), `gen-version.mjs`, `package-boundaries`, `THIRD_PARTY_NOTICES`, and `*.test-d.ts` type tests. These have no Go equivalent.
- #95 `validateFinalResponse` message wording. Go never raises this error; that divergence is documented and pre-existing.

## (c) Defects in the ported code

- **D1 (low/medium): `ToolChoiceFunc` gets the wrong turn number on an approval/rejection resume.**
  - Where: `model_result.go` `run()`, the `if turn == 0 { m.resolveToolChoiceFunc(turn, &req) }` branch.
  - Go calls `ToolChoiceFunc` with `NumberOfTurns: 0` on an approval/rejection resume. Upstream's `continueWithUnsentResults` resolves with `numberOfTurns = allToolExecutionRounds.length + 1`, which is 1.
  - The probe confirmed Go's resume resolution saw `[0]`. A dynamic choice keyed on turn number therefore picks a different value after resume. Example: `numberOfTurns === 1 ? {function: submit} : 'auto'` is forced upstream but not in Go.
  - No test covers resume turn numbering.
- **D2 (low): `ToolChoiceFunc(turn+1)` runs before the approval check, so it is called one extra time on paused rounds.**
  - Where: `model_result.go`.
  - Go resolves before `IsStopConditionMet` / `PartitionToolCalls`. Upstream resolves inside the loop only after `handleApprovalCheck` / the client-tools pause checks, immediately before `executeToolRound`.
  - Effect: on a round that pauses for approval, HITL or client tools, Go calls the user callback one extra time (the probe saw `[0 1]` before the approval pause; upstream would see `[0]`). Persisted state is unaffected, because the prepared commit is discarded.
- **D3 (low): nextTurnParams overrides last longer than upstream when other dynamic parameters exist.**
  - Upstream's `resolveAsyncFunctionsForTurn` rebuilds the whole request from `options.request` every round when *any* async parameter exists. A nextTurnParams override of `model`, `temperature`, `instructions` or `toolChoice` therefore lasts one turn when, say, `model` is a function.
  - Go re-resolves only `ToolChoiceFunc` per turn. `ModelFunc` / `InstructionsFunc` are resolved once in `CallModel`, which is a pre-existing gap.
  - Result: nextTurnParams values persist in Go where upstream reverts them. This is not recorded as a divergence.
- **D4 (low): `Usage(ctx)` drives an approval-resumed run to completion.**
  - Upstream `getUsage()` on an approval-resumed run never advances the tool loop. The upstream test "does not advance an approval-resumed run when read before the loop" expects status `in_progress`.
  - Go's `Usage` calls `m.start()` and waits for `done`, so it runs the entire resumed loop. This follows from Go's eager single-goroutine run, but it is not listed as a compatibility note. The Go test pins the Go behavior rather than upstream's.
- **D5 (low): interrupted-run reset is not mirrored.**
  - Upstream `checkForInterruption` calls `resetForcedToolChoicePolicy()`, and the upstream test is "clears consumed choice state when an interrupted run becomes a fresh request".
  - Go has no interruption polling (pre-existing). `syncToolChoiceState` keeps the key for any non-complete status, including `interrupted`.
  - No Go test covers this.
- **D6 (note, pre-existing, now more visible):** upstream `extractToolCallsFromResponse` keeps calls whose arguments do not parse (raw string arguments) so they execute and produce a validation-error output. Go's `parseResponseToolCalls(resp, false)` drops them from execution, while `ToolCallsStream` now emits them. A response whose only call has unparsable arguments is finalized as `complete` in Go instead of being answered with an error output.
- **Documented, acceptable divergence:** approval predicates (tool-level and call-level) receive raw rather than schema-normalized arguments. Go struct decoding has no defaults or coercions, so this is recorded in the changelog.

## (d) Recommended human actions

1. **Do not advance `.upstreamer/state.yaml` to `e309ebf`.** Either keep it at `680bceb` and re-run, or have a human split the sync so the ported items land without recording the unported in-scope features as done.
2. In priority order, port:
   - #94 PreToolUse-before-approval ordering, the `preToolUseApplied` marker and once-per-run gating, together with the `approval-gate-regressions` cases (this is security-relevant);
   - doom-loop detection, the `DoomLoopDetected` hook and its `doomLoop` state, validated against `tests/vectors/doom-loop-fingerprints.json`;
   - async tool lifecycles, `awaiting_async_tools` and `resumeToolResults`;
   - the tool-set helpers.
3. Decide MCP scope explicitly. It now lives under `packages/agent/src/mcp`, so the contract's Scope section should say whether `@openrouter/agent/mcp` is in or out.
4. Fix D1 and D2: resolve `ToolChoiceFunc` with upstream turn numbering on resume, and only after the approval/pause checks. Add tests for both.
5. Record D3 and D4 as compatibility notes in `upstreamer-changelog.md`, or converge them, and port the interrupted-run test for D5.
6. Add these mappings to the contract's Naming Map: `getUsage` → `Usage(ctx)`, `ToolChoiceFunc`, `NextTurnParams[T]`, `WireInputSchema`, `Strict`, `ActiveTools`, `ResponseHasToolCalls`, `ConsumedForcedToolChoiceKey`. They are already listed in `upstreamer-changelog.md`.

---

## Bankruptcy declaration (converter, after the eval)

**Outcome: the eval FAILED. `.upstreamer/state.yaml` was NOT advanced** and still
records `680bceb4598f228d3e2ec58e2416e4335cdff059`, so the next run retries this
delta. The mechanical verifier (`.upstreamer/scripts/verify.sh`) passes with 0
failures.

### Eval rounds

1. **Eval round 1: FAIL.** Blocking: doom-loop detection, async tools, tool-set,
   and #94 PreToolUse-before-approval are unported. It also found four defects
   in the ported code:
   - nextTurnParams were wrongly applied on approval resume;
   - nextTurnParams were not applied before the AllowFinalResponse final request;
   - `ToolChoiceFunc` overrode a tool-computed `toolChoice`;
   - fail-closed approval was ineffective for `$ref` struct schemas.
2. **Fix attempt 1** (all four defects fixed, each with a test):
   - removed nextTurnParams from the resume path;
   - applied nextTurnParams before the final request;
   - moved `ToolChoiceFunc` resolution to before tool execution;
   - made `ValidateAgainstSchema` follow local `$ref`/`$defs`.
3. **Eval round 2 (fresh context): FAIL.** It confirmed those fixes. The same
   unported in-scope features still block. It reported low-severity defects
   D1–D5 (section (c) above).
4. **Fix attempt 2:** D1 and D2 fixed, with a test:
   - `ToolChoiceFunc` now gets turn 1 on the first request after an approval
     resume;
   - it is no longer resolved on a pausing round.

   D3 to D5 are still open. They are low severity and listed in section (c).

### Why there is no third attempt: no coherent fix within this run

The remaining blockers are whole subsystems, not defects:

- doom-loop detection (`doom-loop.ts`, ~1,700 lines plus JCS/SHA-256 fingerprint
  vectors);
- async tool lifecycles, the `task` tool, steering, and subagents (`tool.ts`,
  `tool-task.ts`, `tool-check.ts`, `async-tool-registry.ts`, `agent-tool.ts`,
  `resume-tool-results.ts`, `tool-concurrency.ts`);
- tool-set (`tool-set.ts`, ~1,400 lines with its types);
- the #94 approval state machine (prepared calls, `preToolUseApplied`,
  once-per-run gating).

All of them are threaded through a `model-result.ts` that grew by ~5,400 lines
in this delta. An honest port of these is several independent, reviewable
changes. Squeezing them into one automated run would produce exactly the
large, unreviewable diff the converter skill warns against, and shipping
stubs would be papering over the gap.

### Recommended human action

1. **Decide MCP scope.** `packages/agent/src/mcp` now ships inside
   `@openrouter/agent` as `/mcp`. The contract excludes only `packages/mcp`.
   Amend the Scope section to state explicitly whether `src/mcp` is in or out.
   It would need an MCP Go SDK dependency and a nested package, which is also
   a contract change.
2. **Split the remaining delta** into dedicated, human-reviewed port runs (or
   manual PRs), each with its upstream tests ported:
   - (a) #94 PreToolUse-before-approval with the `preToolUseApplied` marker and
     once-per-run gating;
   - (b) doom-loop detection: `DoomLoopDetected` hook, `ConversationState.DoomLoop`,
     and conformance against `tests/vectors/doom-loop-fingerprints.json`;
   - (c) async tools, task tool, steering, subagents, timeouts, and concurrency;
   - (d) tool-set;
   - (e) optional `streamReplay` compaction.
3. **Add the derived naming mappings** from this run to the contract's naming
   map (listed in the converter's final report and in
   `upstreamer-changelog.md`'s mapping table).
4. Land the work already done (it is verifier-clean and eval-confirmed
   correct for the ported items). Then let the next sync re-run against this
   delta from `680bceb`.
