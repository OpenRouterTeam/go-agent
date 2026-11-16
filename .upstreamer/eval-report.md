FAIL

# Eval Report: go-agent, `@openrouter/agent` 0.8.0 (680bceb) → 1.2.1+3 (6a8c4ea), review round 4

## Headline

Both of the previous round's blockers are fixed, tested, and guarded. I
reverted each fix in a throwaway copy, and the new test failed both times:

- **B1 (round 3).** A partial approval of a multi-call `awaiting_hitl` pause
  now leaves the status `awaiting_approval`.
- **B2 (round 3).** The turn-0 approval gate now runs before stopWhen and the
  Stop hook.

I found no regressions from those fixes in:
- turn-0 gating;
- `runFinalResponseTurn`'s reuse of the initial gate;
- `runToolJobs` concurrency;
- per-turn state saves.

The verdict is still **FAIL**. Reading the load-bearing loop directly against
upstream at the target, I found three behavioral parity gaps that earlier
rounds missed. None is documented. A probe reproduced the first two, and the
third is read straight from the code.

1. **`MaxTurns` silently caps the loop at 5 requests.** Without a `StopWhen`,
   Go stops after 5 requests and leaves the run `in_progress`. The last
   round's tool outputs are never sent, `Text` is empty, and no error is
   returned.
   - Upstream removed its implicit 5-step cap in 8e71f06, before the delta
     base. It runs until the model stops calling tools.
   - `call_model.go`'s `DefaultMaxTurns` comment says this "matches the
     upstream agent loop default". That is false.
2. **Stop conditions are evaluated on a different step set.**
   - Go passes one `StepResult` per model response, including the current
     response whose tools have not run yet.
   - Upstream passes only the executed tool rounds.
   - So `StepCountIs(n)` runs one fewer tool round than upstream's
     `stepCountIs(n)`. `HasToolCall`, `MaxCost`, `MaxTokensUsed` and
     `FinishReasonIs` fire one response earlier.
   - The port knows this: `approval_gate_regressions_test.go` translates the
     upstream fixtures from n to n+1. That is a test pinning the port's shape,
     which the eval forbids. No user-facing documentation mentions it.
3. **`SessionEnd` never reports `max_turns`.** When stopWhen halts the loop,
   Go emits `SessionEnd{Reason: complete}`. Upstream emits `max_turns`, and
   `hooks-session-lifecycle.test.ts` asserts it ("emits SessionEnd(max_turns)
   when a stop condition halts a tool loop"). `SessionEndReasonMaxTurns` is
   defined but never emitted.

All three predate this delta, but they are still gaps against the target. The
eval's FAIL criteria cover any undocumented behavioral gap in the loop or
hooks, not only gaps introduced by this delta. A false PASS here would advance
`state.yaml` past them for good.

## Method

- **Read in full:** `.upstreamer/eval.md`, the contract
  `.upstreamer/upstreamer.md`, `state.yaml`, the round-3 report,
  `upstreamer-changelog.md`, and the tail of `.upstreamer/logs/20261116T065009Z.log`.
- **Port code read:** `model_result.go` (`run`, `prepareResumeRequest`,
  `gateToolCalls`, `pauseForApproval`, `runToolJobs`,
  `executeAutoApproveTools`, `executeToolCallsForTurn`,
  `runFinalResponseTurn`, `runToolWithHooks`, `executeWithDeadline`,
  `runStopHook`, `saveState`/`saveStateFailed`), `stop_conditions.go`,
  `call_model.go`, `tool_context.go`.
- **Upstream code compared** (`packages/agent/src/lib/model-result.ts` at 6a8c4ea):
  - `executeToolsIfNeeded` (L6396–6880)
  - `handleApprovalCheck` (L3283–3393)
  - `processApprovalDecisions` (L6086–6304)
  - `staysPausedWithoutDecisions` / `initStream` (L5764–5960)
  - `shouldStopExecution` (L1409)
  - `runStopHook` (L2509)
  - `checkForInterruption` (L1316)
- **Upstream history checked:** commit 8e71f06 ("remove implicit 5-step cap"),
  which is an ancestor of 680bceb.
- **Upstream delta:**
  - Read `git diff 680bceb..6a8c4ea -- packages/agent/src`.
  - Read `git diff --stat` for the test files. I listed the new test files
    and sampled their `it` blocks: tool-orchestrator, tool-name-events,
    tool-check, claude-type-guards, final-response-validation, stop-conditions
    and hooks-session-lifecycle.
- **Repo changes:** checked `git status` and `git diff --stat`
  (22 modified files, 26 new tests, `doom_loop.go`, `tool_set.go`).
- **`go test -race -count=1 ./...`:** ok.
- **`.upstreamer/scripts/verify.sh`:** `PASS: 0 failures`.
  - Advisory notes: no Go counterpart for `startup-*` / `steering-*`, and the
    tree is 3 commits past the 1.2.1 tag.
- **Probes and mutations** ran in a throwaway copy under
  `tmp/upstreamer/probe/` (gitignored), since deleted.
  - Mutation A: restored the old `&& m.state.Status != ConversationStatusAwaitingHITL`
    guard. `TestPartialApprovalOfMultiHITLPause` **fails**.
  - Mutation B: disabled the turn-0 gate (`if turn == 0` → `if false`).
    `TestInitialApprovalPauseBeforeStopHook` **fails**.
  - Probe 1: `StepCountIs(1)`; every response is a tool call; a SessionEnd
    hook is installed. Result: `requests=2 status=complete sessionEnd=[complete]`.
    Upstream `stepCountIs(1)` gives 3 requests (initial, follow-up after
    round 1, then the final `tool_choice: none` turn) and `SessionEnd` reason
    `max_turns`.
  - Probe 2: no `StopWhen`; 7 consecutive tool-call responses, then text.
    Result: `requests=5 text="" err=<nil> status=in_progress`.
    Upstream gives 8 requests and the text `"final"`.
- `git status --porcelain` is byte-identical before and after, apart from this
  report.

## Re-verification of round-3 blockers

**B1 (partial approval of a multi-HITL pause): fixed.**
- `prepareResumeRequest` (`model_result.go:1215–1238`) now uses upstream's
  rule (`model-result.ts:6241–6249`):
  - Any remaining call that is not a re-paused HITL call gives
    `awaiting_approval`.
  - Otherwise the status is `awaiting_hitl`.
  - Second-gate-paused calls count as unresolved, which also matches upstream.
- `TestPartialApprovalOfMultiHITLPause` (`hitl_round_test.go:372`) asserts:
  - the status and pending IDs after approving `h1`;
  - zero requests on the partial decision;
  - an `Input`-only resume stays paused, with zero requests and
    `*ConversationPausedError` with status `awaiting_approval`;
  - the approve-`h2` request answers every call and replays `h1`'s executed
    output.
- Mutation A confirms the test guards the fix.

**B2 (Stop hook before the initial approval gate): fixed.**
- `run()` (`model_result.go:699–729`) gates turn 0 before stopWhen and the
  Stop hook. It computes `skipAuto` only when `AllowFinalResponse == false`,
  matching upstream L6472–6478.
- `TestInitialApprovalPauseBeforeStopHook` (`hitl_round_test.go:446`) asserts
  `awaiting_approval`, that Stop does not fire, and that no `AppendPrompt` is
  injected. Mutation B confirms the test guards the fix.

**New issues from the fixes: none found.**
- **`runFinalResponseTurn(…, initialGate)` reuse.** Upstream calls
  `handleApprovalCheck` again on the same response, but
  `classifyInitialApprovalCalls` treats already-seen calls as no-ops. Reusing
  the gate once is therefore equivalent, and it avoids double
  PermissionRequest/PreToolUse emits.
- **Stop-hook `ForceResume` on turn 0.** It falls through to execution with
  the reused gate. Upstream instead `continue`s and re-checks the stop
  condition; this pre-dates the delta (see W2).
- **Turn-0 all-manual check before the gate.** It goes through
  `gateAndMaybePause` first, so an approval-gated manual call still pauses
  `awaiting_approval`. That is equivalent to upstream's approval-check →
  `persistClientToolsPause` order.
- **`runToolJobs`.** Each goroutine writes its own `outcomes[i]` slot.
  Processing, `toolRoundsExecuted` and the state writes all happen after
  `wg.Wait()`. `ToolContextStore` and `HooksManager` are mutex-guarded, and
  the doom-loop checkpoint runs in the sequential gate. The suite is clean
  under `-race`.
- **Per-turn saves.** `saveStateFailed` runs after every response and after
  every tool round (upstream `saveResponseToState` / `saveToolResultsToState`).
  A failed save stops the run with the error. An `awaiting_client_tools`
  resume keeps its pending calls until the resumed response lands, as
  upstream does.

## Per-quality findings

**Public API completeness: PASS.**
- All Required Public API symbols are present, exported and covered
  (verify.sh presence and per-symbol gates).
- The new surface is listed in `PORTING.md` with naming mappings for
  maintainers.

**Version honesty: PASS WITH WARNING.**
- "Sync Toward `@openrouter/agent@1.2.1` (partial — see 'Not yet ported')"
  is accurate about e8d7d6d and MCP.
- I spot-checked the changelog's new B1/B2 bullets against the code; both are
  true.
- The changelog does not disclose F1–F3. `call_model.go`'s "matches the
  upstream agent loop default" is an affirmatively wrong parity claim (F1).

**Load-bearing loop: FAIL (F1, F2).**
- These match upstream:
  - accumulated input (prior input + response items + outputs);
  - `PreviousResponseID` tracked in state;
  - concurrent rounds with call-ordered outputs;
  - nextTurnParams only for successful calls (6a8c4ea);
  - toModelOutput on every result path;
  - truncated `incomplete` responses do not execute;
  - forced tool-choice relaxation;
  - duplicate tool names rejected;
  - cancellation at turn boundaries;
  - the turn-0 gate ordering.
- These differ: the implicit 5-request cap (F1) and the step set that stop
  conditions see (F2).

**Streaming: PASS.**
- Consumption ends at the terminal event, and `response.failed` surfaces as an
  error.
- Every stream completes with `getterErr()`, so a paused resume reports
  `*ConversationPausedError` on every consumer.
- Replay modes are tested.
- One adjacent issue: F1's truncated run ends every stream with a nil error
  and empty text — the "ends silently" shape this quality exists to catch.

**State: PASS.**
- The version constant and mismatch errors work, and the new fields
  round-trip.
- Replace-on-pause and the partial-approval status are both correct now.
- Pause/resume across approval, HITL and client tools is tested.

**Approval/HITL ordering: PASS.**
- Mixed turns execute the auto calls before the pause and store them as unsent.
- Resume order is `function_call` → `function_call_output`.
- `[hitl, regular]` rounds keep the regular output.
- Decisions are de-duplicated and conflicts are rejected.
- Decision-less resumes stay paused.
- The second gate after PreToolUse works.
- The turn-0 gate runs before Stop. Both of the round-3 B1/B2 fixes are
  verified.

**Hooks: FAIL (F3).**
- `SessionEnd`/drain runs on every exit path.
- The session ID is threaded per emit.
- PreToolUse preparation and `PostToolUseFailure` for prepared-then-rejected
  calls both work.
- `DoomLoopDetected` was added.
- Gap: the `SessionEnd` reason is `complete` where upstream reports
  `max_turns` (F3).
- Also: upstream's `user` reason, from `checkForInterruption`, has no Go path
  (W1).

**Compatibility helpers: PASS.**
- The six new chat-compat cases are ported.
- The Claude round trip is unchanged.

**Test parity: FAIL (via F2/F3).**
- The delta clusters are broadly ported with behavioral assertions:
  approval-gate regressions and decisions, hitl-tool, to-model-output-parity,
  next-turn-params, max-output-tokens, tool-choice-relaxation,
  duplicate-tool-names, get-usage, active-tools, manual-tool-wire-schema,
  stop-when-before-approval, doom-loop (with shared fingerprint vectors),
  tool-set, tool-cancellation, replay compaction and stream termination.
- `approval_gate_regressions_test.go:10–14` translates every upstream
  `stepCountIs(n)` to `StepCountIs(n+1)`. The tests then pass against the
  port's step semantics instead of upstream's (F2).
- No Go test covers upstream's "emits SessionEnd(max_turns)" case (F3), or a
  run longer than 5 tool rounds without `StopWhen` (F1).
- These upstream clusters have no Go counterpart, which is acceptable:
  `steering-*`, `task-tool-*`, `async-tool-*`, `agent-tool` and `tool-check`
  (deferred e8d7d6d); `startup-import-closure` and `package-boundaries`
  (TS-only); `final-response-validation` (documented: no hard
  empty-output error).

**Divergences documented: FAIL (via F1–F3).**
- These are documented in the changelog compatibility notes and
  `PORTING.md`: sorted nextTurnParams keys, the lone-surrogate fingerprint,
  no concurrency caps, no context schema, no hard empty-output error, the
  event renames, and the e8d7d6d/MCP deferrals.
- These are undocumented: F1, F2, F3 and W1.

**Repo-owned files intact: PASS.**
- Unchanged: `.github/`, `LICENSE`, `scripts/upstream`,
  `.upstreamer/upstreamer.md`, `.upstreamer/state.yaml`,
  `.upstreamer/scripts/`, `go.mod`/`go.sum` (go-sdk v0.5.4) and the
  `service_tier` pin.
- The coverage floor rose from 72.0 to 82.0, which the ratchet allows.
  Coverage is 83.4%.

## Scope judgments requested

- **MCP (`packages/agent/src/mcp/`).** Out of scope per the contract. Only the
  marking surface is kept (`MarkMcp`, `LoopKey`, `WireInputSchema`), and the
  relocation is noted in `PORTING.md`. Not a finding.
- **e8d7d6d, deferred.** Not a FAIL on its own:
  - No required symbol depends on it.
  - The default execute/manual/HITL paths are unaffected.
  - The parts that touch the loop are ported: concurrent rounds,
    `CallID`/`ConversationID`, timeouts, cancellation and `QueueUserMessage`.
  - The deferral is recorded in `PORTING.md` ("Upstream work not yet ported",
    including the dropped `awaiting_async_tools` state fields) and in the
    changelog.
  - The converter may not edit the contract, so this is adequate. A
    maintainer should copy it into the contract's Scope section (W3).

## Findings (prioritized)

### Blocking

**F1. `MaxTurns` implicit 5-request cap truncates runs silently.**
- **Where.**
  - `model_result.go` `run()` L484–487 (`maxTurns = 5` when
    `MaxTurns <= 0`).
  - The loop `for turn := 0; turn < maxTurns; turn++` (L551).
  - `call_model.go` `DefaultMaxTurns`, whose comment wrongly claims upstream
    parity.
- **Upstream.**
  - 8e71f06 "remove implicit 5-step cap in callModel" (ancestor of 680bceb).
  - `call-model.ts` L71–72: "If `stopWhen` is omitted, the loop runs until the
    model produces a turn with no tool calls."
  - `executeToolsIfNeeded` is `while (true)`, and `shouldStopExecution`
    returns false without `stopWhen`.
- **Effect (probe 2).** After the 5th response, its tools execute and their
  outputs are appended to `req`, then the loop exits. No request is sent, the
  status stays `in_progress`, `Text` returns `""`, and every consumer
  completes with a nil error.
  - Even the old upstream cap went through the stop path (Stop hook plus the
    final-response turn). Go's cap bypasses both.
- **Fix.**
  - Without `StopWhen` (and with `MaxTurns` unset), run until there are no
    tool calls, as upstream does.
  - If `MaxTurns` is kept as a Go-only safety bound, route its exhaustion
    through the same stop path (Stop hook → `AllowFinalResponse` final turn →
    `SessionEnd` `max_turns`) rather than ending silently, and document it as
    a compatibility note.
  - Fix the `DefaultMaxTurns` comment.
  - Add a test: more than 5 tool rounds with no `StopWhen` reaches the final
    text.

**F2. Stop conditions see different steps than upstream.**
- **Where.**
  - `model_result.go` `run()` L605–617: `steps` gets one `StepResult` per
    response, appended before that response's tools run.
  - It is then evaluated at L709/L730.
  - `stop_conditions.go` `StepCountIs` computes `len(steps) >= limit`.
- **Upstream.** `shouldStopExecution` (L1409) builds `steps` from
  `allToolExecutionRounds`: executed rounds only, each with its calls and
  results. So on the initial response `steps` is empty (upstream tests use
  `stepCountIs(0)` for "stop on the first response").
- **Effect.**
  - Go `StepCountIs(n)` ≡ upstream `stepCountIs(n-1)`. A TS user's
    `stepCountIs(3)` gives 3 tool rounds; the same Go code gives 2.
  - Probe 1 sends 2 requests; upstream sends 3.
  - `HasToolCall` fires on a call that has not executed yet. Go then takes the
    final-response path (executes it plus a `tool_choice: none` request with
    the directive), where upstream would run a normal round and a normal
    follow-up first.
  - `MaxCost`, `MaxTokensUsed` and `FinishReasonIs` include the current
    response's usage and finish reason. Upstream excludes them until its
    round executes.
  - Custom `StopCondition`s receive a different slice.
- **Tests.** `approval_gate_regressions_test.go:10–14` knowingly translates
  the fixtures (n → n+1) instead of flagging the gap.
- **Fix (preferred).** Evaluate stop conditions over executed rounds, as
  upstream does, and restore the upstream fixture values.
- **Fix (alternative).** If keeping the Go semantics, record it prominently as
  a compatibility note in `upstreamer-changelog.md` and the README "Stop
  Conditions" section (with the n ↔ n+1 mapping and the `HasToolCall`/usage
  effects). Add it to `PORTING.md` for a maintainer to put in the contract's
  divergences. With that documentation this finding drops to a warning.

**F3. `SessionEnd` reason `max_turns` is never emitted.**
- **Where.**
  - `model_result.go` `run()` session-teardown defer, L466–474: only
    `error`, `doom_loop` and `complete`.
  - `hooks_schemas.go:207`: `SessionEndReasonMaxTurns` is defined but unused.
- **Upstream.**
  - `executeToolsIfNeeded` L6523–6537 sets `sessionEndReason = 'max_turns'`
    when stopWhen breaks the loop and the Stop hook does not resume.
  - This holds on the final-response path and on approval/HITL pauses
    reached from it.
  - `tests/unit/hooks-session-lifecycle.test.ts:201` asserts it.
- **Effect.** Hook consumers cannot tell a stop-condition halt from a natural
  completion. Probe 1 gives `sessionEnd=[complete]`.
- **Fix.**
  - Track a `stoppedByStopWhen` flag in `run()`, set when `runStopHook`
    returns stop, and report `SessionEndReasonMaxTurns`.
  - Port the upstream case: `StepCountIs` (per F2) halts a tool loop, and the
    single `SessionEnd` has reason `max_turns`.

### Warnings

- **W1. External interruption is not ported.**
  - Upstream `checkForInterruption` (L1316) reloads state each loop iteration.
    On `interruptedBy` it saves `status: interrupted` plus `partialResponse`
    and ends with `SessionEnd` reason `user`.
  - Go has `ConversationState.InterruptedBy` and
    `ConversationStatusInterrupted`, but nothing reads or sets them.
  - Context cancellation (divergence 1) covers most uses, but the state-driven
    mechanism and its persisted shape are missing and undocumented.
  - Pre-existing. Document it, or port it.
- **W2. Stop-hook `ForceResume` path differs.**
  - Upstream `continue`s and re-evaluates `shouldStopExecution` on the same
    response, up to `MAX_FORCE_RESUME_OVERRIDES`.
  - Go falls straight through to executing the round.
  - Pre-existing (0.8.0). The outcome usually converges, but the hook-call
    count differs. Note it, or align it.
- **W3. Contract upkeep (maintainer, non-blocking).**
  - Copy the `PORTING.md` naming mappings, the e8d7d6d deferral and the
    `packages/agent/src/mcp/` exclusion into `.upstreamer/upstreamer.md`.
  - Also F2's divergence, if it is kept.
- **W4. Release tagging.** Do not cut a v1.2.1 tag from this tree. It is 3
  commits past the upstream tag, and e8d7d6d is unported.

## What must be fixed before PASS

1. **F1.** Remove the silent implicit 5-request cap, or route `MaxTurns`
   through the stop path and document it. Fix the `DefaultMaxTurns` comment.
   Add a test for a run longer than 5 rounds without `StopWhen`.
2. **F2.** Align stop-condition step semantics with upstream (executed rounds
   only) and restore the upstream fixture values. Alternatively, document the
   divergence as a compatibility note in the changelog, README and
   `PORTING.md`; with that done it would be a warning.
3. **F3.** Emit `SessionEnd` reason `max_turns` when stopWhen halts the loop,
   and port the `hooks-session-lifecycle` case.
4. (Recommended) Document or port W1 (`interruptedBy` → `interrupted` /
   `user`).

---

## Bankruptcy declaration (converter)

The parity eval returned **FAIL** on all four rounds of this run. Each round
was a fresh review context, followed by one focused fix attempt. Three fix
attempts have been used, which is the limit set by the converter skill and the
run instructions, so the port declares bankruptcy:

- `.upstreamer/state.yaml` is **not** advanced. It still records `680bceb`, so
  the next run retries the `680bceb..6a8c4ea` delta.
- The work stays in the working tree. It passes `.upstreamer/scripts/verify.sh`
  with 0 failures and 83.4% coverage against a raised floor of 82.0.

### Eval history and fix attempts

| Round | Verdict | Blocking findings | Fix applied before the next round |
| --- | --- | --- | --- |
| 1 | FAIL | A HITL pause `break`s the round, so later calls never run and are left without an output | Fix 1: the round runs concurrently (as upstream does with `Promise.allSettled`) and every paused HITL call goes pending. Also: per-turn state saves, stopping at the stream's terminal event, upstream tool-event names, and a HITL Input-resume status fix. Tests in `hitl_round_test.go` and `stream_termination_test.go`. |
| 2 | FAIL | Paused calls were *appended* to stale `PendingToolCalls` after an Input resume | Fix 2: replace instead of append. Also: `tool.call_output` for blocked or rejected calls, and pausing for client tools on an all-manual first response before checking stop conditions. Tests added. |
| 3 | FAIL | (B1) partial approval of a multi-call HITL pause kept `awaiting_hitl`; (B2) stop conditions and the Stop hook ran before the first-response approval gate | Fix 3: upstream's `processApprovalDecisions` status rule, and gating the initial response before stop or Stop-hook handling. Tests `TestPartialApprovalOfMultiHITLPause` and `TestInitialApprovalPauseBeforeStopHook`. |
| 4 | FAIL | F1, F2 and F3 below. They predate this delta and earlier rounds did not report them. | None. The attempt budget is exhausted. |

### Remaining blockers (round 4)

1. **F1: implicit turn cap.** `run()` defaults `MaxTurns` to 5 (`model_result.go`). The `DefaultMaxTurns` comment says this matches upstream, which is false. Upstream has no implicit cap. A run with more than 5 tool rounds and no `StopWhen` ends silently `in_progress`, with empty text and its last tool outputs never sent.
2. **F2: stop-condition step counting is off by one.** Go evaluates `StopWhen` over every model response including the current one, so `StepCountIs(n)` behaves like upstream `stepCountIs(n-1)`. Upstream's `shouldStopExecution` counts only tool rounds that have already run. `approval_gate_regressions_test.go` shifts upstream's fixture values to compensate, and that should be undone once this is fixed.
3. **F3: `SessionEnd` never reports `max_turns`.** It should when a stop condition halts the loop. Upstream asserts this in `hooks-session-lifecycle.test.ts`.

The round-4 warnings (W1 `interruptedBy` interruption path not ported, W2 Stop-hook forceResume not re-checking stop) also predate the delta.

### Recommended human action

1. **Fix F1–F3 in one change**, then re-run `./scripts/upstream` (or `--force`), which retries the same delta with these files as the starting point.
   - F1: remove the default cap, or make it opt-in and document it as a divergence in the contract. Caution: `fakeSender` repeats its last response forever, so tests that relied on the cap need an explicit `MaxTurns` or `StopWhen`.
   - F2: count executed tool rounds the way upstream's `shouldStopExecution` does, and restore upstream's step values in the ported fixtures.
   - F3: report `SessionEndReasonMaxTurns` on a stop-condition halt.
2. **Copy the "Upstream work not yet ported" section of `PORTING.md` into `.upstreamer/upstreamer.md`.** That covers the deferred e8d7d6d async/task/subagent work and its state-schema impact, the explicit `packages/agent/src/mcp/` exclusion, and the new naming-map rows. Port runs may not edit the contract.
3. **Do not cut a `v1.2.1` tag from this tree.**
