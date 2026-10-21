# Port Parity Eval — go-agent @ upstream 78c562e (doom-loop detection + run cancellation)

Verdict: PASS WITH WARNINGS

## Scope

- Upstream delta: `680bceb4598f228d3e2ec58e2416e4335cdff059..78c562ef53da0edd84dfbcc6d6ee38a095d72b37`, `packages/agent/` only. The behavioral commit is 78c562e "feat(agent): doom-loop detection for the tool-execution loop (#73)". 89ae3ff and fc8a7f7 are release/publish infra and out of scope. `packages/agent/package.json` is still `0.8.0` at the target. 78c562e first ships in `@openrouter/agent@0.9.0`.
- Upstream surface added: `lib/doom-loop.ts` (new), wiring in `lib/model-result.ts`, the `DoomLoopDetected` hook and `SessionEnd` reason `doom_loop` (`hooks-schemas.ts`, `hooks-types.ts`), tool `loopKey` (`tool.ts`, `tool-types.ts`), the `markMcp(tool, { loopKey })` option, `ConversationState.doomLoop`, `CallModelInput.doomLoop` and `signal` (`async-params.ts`, `inner-loop/call-model.ts`), per-dispatch signal/timeout composition (`dispatchRequestOptions`), and `getDoomLoopVerdict()`.
- Go port files reviewed: `doom_loop.go` (new), `model_result.go` (`run`, `prepareResumeRequest`, `executeAutoApproveTools`, `executeToolCallsForTurn`, `runToolWithHooks`, `sendFinalResponseRequest`, `retryCurrentRequest`, the "Run cancellation" and "Doom-loop engine" regions, `pauseBeforeFirstTurnDoomStop`), `hooks_schemas.go`, `hooks_manager.go`, `hooks_resolve.go`, `tool.go`, `tool_types.go`, `conversation_state.go`, `async_params.go`, `README.md`, `upstreamer-changelog.md`. New tests: `doom_loop_test.go`, `doom_loop_engine_test.go`, `run_cancellation_test.go`, `testdata/doom-loop-fingerprints.json`.

## Method

1. Read `.upstreamer/eval.md` and the binding contract `.upstreamer/upstreamer.md` (Required Public API, Idiomatic Divergences, Test Quality).
2. Read the full upstream source diff (2,821 lines), including all of `doom-loop.ts`, and the final `model-result.ts` `executeToolsIfNeeded`, `handleApprovalCheck`, `processApprovalDecisions`, `continueWithUnsentResults` and `initStream` restore site, so ordering could be compared in context.
3. Read the Go implementation line by line against it, case by case (listed below). I did not rely on the changelog or code comments.
4. Ran the full suite with `-race` and `.upstreamer/scripts/verify.sh`. Read every new Go test and mapped it to the upstream `it(...)` cases in `doom-loop.test.ts`, `doom-loop-integration.test.ts`, `doom-loop-remediation.test.ts`, `doom-loop-escalation.test.ts`, `run-cancellation.test.ts` and `e2e/cancellation.test.ts`.
5. Ran throwaway experiments in a copy (`tmp/upstreamer/opencode/evalcopy`, deleted afterwards):
   - a 3,008-value differential of `CanonicalizeKeyMaterial` number output against Node's `JSON.stringify`: 0 mismatches on finite values;
   - cancellation landing after a turn>0 response with no tool calls;
   - a text stop with no tools configured;
   - a pre-cancelled approval resume.
6. Read the changelog last, and skeptically, to classify each divergence as documented or undocumented.

Behaviors verified as matching upstream:

- **Ladder.** Rungs compare with `>=` and the strongest wins (stop > block > escalate > steer > observe). Defaults are observe 2, block 3, stop 6, with steer and escalate off. Text and server-tool verdicts are non-blockable (block falls through, stop still stops). Escalate is budget-gated in the monitor. Dead-rung, block-without-stop and escalate-without-mechanism warnings are present. `advisor:false` is not a mechanism. `maxEscalations` < 1 clamps to 2. (`doom_loop.go: ResolveLadderAction, ResolveDoomLoopOption, warnOnLadderHazards`)
- **Round-scoped streaks.** `beginDoomLoopRound` is called at the same three batch boundaries as upstream: `executeToolCallsForTurn` (the `executeToolRound` equivalent), `executeAutoApproveTools`, and `prepareResumeRequest` (the `processApprovalDecisions` equivalent). Duplicates reuse the cached round decision with no second hook emit. Restored streaks carry no round, so the first resumed record increments. Interleaved other tools do not reset a streak, and a different fingerprint resets it to 1.
- **Block and stop before execution.** The checkpoint runs before PreToolUse, on the model-issued (pre-mutation) arguments. Block and stop both refuse the call with the same blocked shape as a PreToolUse block, and PreToolUse does not fire. The output is `{"error": "<verdict message>"}`, the same as upstream.
- **Sealing.** Stop sites are the doom gate before stopWhen and approval, the post-round break, and the stopWhen final-response gate. The final-response gate skips the `tool_choice:none` request. Each seals unresolved calls with a `Run halted by doom-loop detection: …` error output. The no-tools early return and the approval-resume gate do not seal, which also matches upstream. A doom-stopped run also skips the empty-final retry.
- **First-turn ordering.** On the first response of a call, upstream runs `handleApprovalCheck` (with the auto-approve batch) and the all-manual `persistClientToolsPause` before the loop-top doom gate. On later turns the gate runs first. `pauseBeforeFirstTurnDoomStop` reproduces this asymmetry, including the PermissionRequest gate.
- **Steer.** Steer messages are deduplicated. They are flushed only at the in-loop point after the doom-stop check and before the follow-up request, and are never injected at an approval, HITL or client-tools pause. They persist as `pendingSteer` and are re-queued on restore. The final-response path does not flush.
- **Escalation.** The first verdict per window is latched and queues an "An escalated turn follows…" steer. Overrides apply to the next in-loop follow-up only (`turn > 0`; not resume continuations, the final-response request or the retry), as one-turn copies. A model override clears `Models`. The advisor override appends `openrouter:advisor` with `forward_transcript` and default instructions (merged under caller parameters), and sets `tool_choice` to `allowed_tools`/`required`. Budget is consumed at application time, `escalationsUsed` is persisted, and the pending escalation itself is not persisted.
- **Hook override.** The last non-empty `OverrideAction` wins. Block on a non-tool-fingerprint verdict becomes observe. Escalate without config or budget becomes observe. The payload carries `toolInput` only for tool verdicts with record arguments, and the per-emit session/toolName context is threaded through.
- **Stop verdict across resumes.** The stop verdict is restored only when `ApproveToolCalls`/`RejectToolCalls` is non-empty, and is cleared on a fresh turn while streaks are kept. A stop armed while executing approved calls halts with no unsent-results request.
- **State persistence.** State is persisted in `ConversationState.DoomLoop`. Decoding is lenient: corrupt blobs and entries are dropped with a warning instead of failing deserialization. With detection off, a stale blob is left untouched and is inert.
- **Server-tool detection.** Every output item that is not a message, reasoning or `function_call` is treated as a server tool, recorded as `server:<type>` with id/type/status/output/result/results/error excluded and items with no identity skipped. Detection is non-blocking and recorded at the current round.
- **Text detectors.** These are a line-by-line port: char-budget tail slice in UTF-16 units, tie breaks to the smallest period, and the within-response verdict wins ties. Whitespace normalization uses the ECMAScript `\s` set (U+FEFF counted, U+0085 not).
- **JCS and SHA-256.** Keys sort by UTF-16 code units, number formatting is ECMAScript `Number::toString` (differential-tested), there is no HTML escaping, and lone surrogates (as WTF-8 bytes) become `\udXXX`. Depth limit 64, cycle detection and rejection of non-finite or big numbers are present. The shared vector file is byte-identical to upstream (`cmp`), and every vector reproduces.
- **SessionEnd.** `SessionEnd` reports `doom_loop` for stop sites in non-resume runs. A paused run reports `complete`. `ModelResult.DoomLoopVerdict(ctx)` is the equivalent of `getDoomLoopVerdict()`.
- **Cancellation.**
  - A pre-cancelled ctx fails before any dispatch, with `context.Cause`.
  - Cancellation between requests (inside a tool, or in AfterTurn) stops at the turn boundary, before any of that response's tools run.
  - An in-flight request is aborted through `ctx`, by either the caller's cancel or `Cancel()`.
  - The cancellation check also runs before the final-response and retry dispatches.
  - The per-request SDK timeout still bounds a request while the run ctx is cancellable, because the Go SDK applies `context.WithTimeout` per `Send`.

## Findings

1. **warning — `stream_transformers.go: ExtractToolCallsFromResponse` / `model_result.go: runToolWithHooks`. Upstream's "repeated unparseable tool call" doom path is not ported.**
   - Upstream records the raw argument string as the call identity before returning the parse-error output, so a model stuck emitting the same invalid JSON trips the ladder. Reference: `model-result.ts` `runToolWithHooks`, the `typeof rawArgs === 'string'` branch with `enqueueDoomLoopEvaluation({ keyMaterial: rawArgs, fallbackKeyMaterial: null })`, and the test `doom-loop-integration.test.ts` "counts parse-error repeats and stops instead of bouncing off the parse error forever".
   - Go silently drops function calls whose arguments are not valid JSON, so the path is unreachable. The turn then ends `complete`, the dropped `function_call` is left in the history with no output, and there is no parse-error feedback turn.
   - This predates the delta but was undocumented before it. It is now recorded as a compatibility note, so it is not blocking. It is still a real load-bearing-loop gap that this delta's feature depends on.
   - Fix: surface invalid-JSON calls as parse-error `function_call_output`s, as upstream does, and route them through `evaluateDoomLoop` with the raw string as key material.

2. **warning — `model_result.go: run` (`maxTurns` default 5). The default stop rung cannot fire within one call.**
   - Upstream has no default turn cap: with no `stopWhen`, `shouldStopExecution()` returns false. So `doomLoop: true`, whose stop rung is at 6, can halt a single run.
   - Go caps a run at `MaxTurns` requests, default 5. `&DoomLoopConfig{}` therefore observes and blocks but never stops within one call, and `SessionEnd` reports `complete` rather than `doom_loop`.
   - The cap predates the delta. Its interaction with the delta is documented in the changelog and README.
   - Fix: none needed for this delta. Consider aligning the default cap with upstream, or documenting `MaxTurns` as the Go-specific bound in the contract.

3. **warning — `model_result.go: run` (the `len(calls) == 0` break placed before `runCanceledErr`). A cancellation that lands after a turn>0 response with no tool calls is ignored.**
   - Upstream calls `this.options.signal?.throwIfAborted()` at the top of every loop iteration. A signal aborted during `onTurnEnd`, or during a `DoomLoopDetected` handler, on a follow-up response therefore rejects the run with the abort reason even when that response has no tool calls. Reference: `executeToolsIfNeeded`, loop top.
   - In Go, `AfterTurn` cancelling on turn 2 of a tool→text run returns `text="final", err=nil` (verified by experiment). The turn-0 behavior matches upstream (an early return with no check).
   - The window is narrow, but this is a cancellation-semantics divergence that is not documented.
   - Fix: on `turn > 0`, check `runCanceledErr()` before the no-tool-calls break, or add a compatibility note.

4. **info — `model_result.go: pauseBeforeFirstTurnDoomStop` with no tools configured.**
   - A first-response text stop on a response that carries `function_call`s while `CallModelInput.Tools` is empty is sealed with halt outputs in Go.
   - Upstream takes the `!this.options.tools?.length` early return: `doom_loop`, no seal, dangling calls left in place.
   - This follows from a pre-existing divergence: Go treats calls with no tools configured as client tools. Go's result is the better-formed history. It is not documented.

5. **info — `model_result.go: run` turn 0. Cancellation is checked before the approval gate.**
   - Go checks `runCanceledErr()` right after the first response, before the approval/auto-approve batch.
   - Upstream's first `throwIfAborted` comes after `handleApprovalCheck`, so a signal aborted between the response and the approval check (for example inside a `DoomLoopDetected` handler) still runs the auto-approved calls and pauses upstream, but fails in Go.
   - The window is very narrow, and Go's ordering is arguably safer.

6. **info — `model_result.go: prepareResumeRequest` and the pre-cancelled decision resume.**
   - With a pre-cancelled ctx, Go executes the approved tool and then fails at dispatch without saving to the StateAccessor. The accessor still says `awaiting_approval`, so a retry re-executes the side-effecting tool.
   - Upstream's `processApprovalDecisions` calls `saveStateSafely` before `continueWithUnsentResults` throws.
   - This predates the delta and is covered by the existing documented per-turn checkpoint gap, but cancellation makes it easier to hit.

7. **info — `model_result.go: pauseBeforeFirstTurnDoomStop` and PermissionRequest denies.**
   - When a first-turn doom stop coincides with a PermissionRequest gate that denies every gated call, Go keeps the `Denied by PermissionRequest hook` output and excludes it from the seal.
   - Upstream records denies only in `hookDeniedCalls`, so its loop-top seal writes a halt output for those calls instead.
   - Only the message text of a synthetic output differs.

8. **info — `doom_loop.go` / `model_result.go: toolInputMap`. Minor identity edge cases.**
   - Non-object JSON arguments (an array or scalar) are fingerprinted as `{}` in Go; upstream uses the parsed value. Tools are typed structs, so this is practically unreachable.
   - `DoomLoopMonitor.Restore` drops entries with an empty fingerprint; upstream accepts any string.
   - Server-tool identities come from snake_case wire JSON, which is documented: fingerprints are stable within go-agent but not cross-port.
   - None of these is user-visible in normal traffic.

9. **info — documented divergences, confirmed acceptable.**
   - `signal` maps to `ctx` (contract divergence #1). Upstream's timeout composition is unnecessary because the Go SDK applies its timeout per `Send`.
   - `loopKey` returning `undefined` has no Go equivalent; returning an error falls back instead.
   - `DoomLoopThreshold` uses 0 for the default and a negative value for disabled.
   - Unpaired surrogates in decoded arguments become U+FFFD.
   - A `DoomLoopDetected` handler error is logged rather than thrown.
   - A doom stop on a decision resume records the executed outputs in `Messages`; upstream keeps them in `unsentToolResults`, which a fresh upstream turn never sends. Go's shape is the well-formed one.
   - Evaluation is sequential, with no promise chain, so evaluations record in emission order.

## Test parity

Upstream case → Go test (`DL` = `doom_loop_test.go`, `DLE` = `doom_loop_engine_test.go`, `RC` = `run_cancellation_test.go`):

**doom-loop.test.ts (unit)**
- canonicalize: key order, array order, dropped undefined, primitives per JCS → `DL TestCanonicalizeKeyMaterialJCSSemantics` (adds UTF-16 key-order, no-HTML-escape and U+2028 cases).
- rejects non-representable values, circular input and over-deep nesting → `DL TestCanonicalizeKeyMaterialRejectsUnrepresentable`.
- vectors (tool-call and bare), 64-char hex, tool name participates, non-ASCII → `DL TestDoomLoopFingerprintCrossPortVectors` (same vector file, byte-identical).
- resolveLoopKeyMaterial forms (absent, false, fields, `__proto__`, value, null, throw) → `DL TestResolveLoopKeyMaterialForms`. The `undefined` case is N/A and documented.
- detectTextRepetition: all 9 cases, including the multi-megabyte input and custom thresholds → `DL TestDetectTextRepetition`.
- resolveLadderAction (>=, non-blockable, disabled) → `DL TestResolveLadderAction`.
- resolveDoomLoopOption: null, defaults, partial, advisor false/true, text false, warnings → `DL TestResolveDoomLoopOption`.
- Monitor tool streaks (thresholds, same round, reset, interleave, key order, empty calls, bigint, server label, determinism) → `DL TestDoomLoopMonitorToolStreaks`.
- Monitor text (immediate stop, streak with block downgrade, whitespace, paraphrase miss, empty text, disabled) → `DL TestDoomLoopMonitorText` and `TestDoomLoopTextUsesJSWhitespace`.
- State round-trip, corrupt state, `__proto__` (both), snapshot → `DL TestDoomLoopMonitorStateRoundTrip`.

**doom-loop-integration.test.ts**
- observe→block→recover → `DLE TestDoomLoopObservesThenBlocksAndModelRecovers`.
- stop at threshold → `TestDoomLoopStopsRunAtStopThreshold`.
- SessionEnd `doom_loop` → `TestDoomLoopSessionEndReasonDoomLoop`.
- healthy run, and inert when off → `TestDoomLoopHealthyRunAndInertWhenOff`.
- empty `{}` calls → `TestDoomLoopRepeatedEmptyCallsBlock`.
- **parse-error repeats → no Go test. The path is unreachable (Finding 1).**
- degenerate repeated tokens → `TestDoomLoopTextRepetitionStopsNoToolsRun`.
- identical filler text across a tool loop → `TestDoomLoopTextStreakHaltsToolLoop`.
- loopKey cwd, null exemption, throwing key → `TestDoomLoopToolLoopKeyForms` (plus a MarkMcp loopKey subtest).
- hook de-escalate and escalate overrides → `TestDoomLoopHookOverrideAction` (adds a text-block downgrade via InlineHookConfig).
- persistence across resume, JSON round-trip → `TestDoomLoopStatePersistsAcrossResume`.

**doom-loop-remediation.test.ts**
- B3: 6-wide fan-out, and shared block decision → `TestDoomLoopParallelDuplicatesAreOneRound`.
- B1/D7: text stop with tool calls is sealed, resume is well-formed, no-tools reason → `TestDoomLoopTextStopSealsDanglingCalls` and `TestDoomLoopTextRepetitionStopsNoToolsRun`.
- D1: no final-response request after a stop → `TestDoomLoopStopSkipsFinalResponseRequest`.
- D2/D3: approval-resume stop, fresh-turn clear → `TestDoomLoopApprovalResumeStopAndCondemnation` (plus `TestDoomLoopCondemnedRunStaysHaltedOnDecisionResume`).
- D4: in-loop steer, and steer at a HITL pause persisted → `TestDoomLoopSteer`. Delivery on resume → `TestDoomLoopPendingSteerDeliveredAfterResume`.
- API1: field-array forms, `false` → `TestDoomLoopToolLoopKeyForms`. D5 (`undefined`) is N/A and documented.
- H1: nonce miss, and field list catches it → `TestDoomLoopNonceArgsEvadeDefaultIdentity`.
- H2: server streak stops, distinct queries do not → `TestDoomLoopServerToolRepetition`. The `__proto__` server field case is N/A in Go (no prototype setter).
- ON→OFF and ON→ON → `TestDoomLoopStatePersistsAcrossResume`.
- Go-only additions:
  - `TestDoomLoopCheckpointRunsBeforePreToolUseMutation` (upstream has the behavior, not a dedicated test);
  - `TestDoomLoopFirstTurnPauseTakesPrecedenceOverDoomStop` (pins the first-turn vs later-turn asymmetry);
  - `TestDoomLoopUnhashableLoopKeyFallsBack`.

**doom-loop-escalation.test.ts**
- Config resolution (4 cases) → `DL TestResolveDoomLoopOption`.
- Monitor escalate rung, budget exhaustion, `escalationsUsed` round-trip → `DL TestDoomLoopMonitorEscalationBudget`.
- Engine:
  - one-turn model swap;
  - advisor and pinned `toolChoice`, checked on the serialized request;
  - advisor merge;
  - steer notice;
  - per-applied budget;
  - hook-override escalate with and without config;
  - text escalation;
  - escalate once for two detectors

  → the 7 subtests of `DLE TestDoomLoopEscalationRecovery`.

**run-cancellation.test.ts / e2e/cancellation.test.ts**
- pre-aborted signal → `RC TestRunCancellationPreCanceledFailsBeforeDispatch`.
- abort between requests → `TestRunCancellationBetweenRequestsStopsLoop` (also asserts stream consumers see the cause).
- mid-request abort → `TestRunCancellationAbortsInFlightRequest` (cause and `Cancel()`).
- timeoutMs still bounds with a signal present → `TestRunCancellationPerRequestTimeoutComposesWithRunContext`.
- Go-only: `TestRunCancellationBeforeToolExecution`.
- "threads a composed signal into every dispatch" and "timeoutMs alone passes through" are N/A: they are TS SDK composition mechanics.
- **Gaps:**
  - No Go test that the per-request timeout is applied per dispatch across multiple turns (upstream: "the composed timeout is created per dispatch, not shared across turns").
  - No Go live e2e counterpart for `e2e/cancellation.test.ts`; `e2e_test.go` is unchanged.
  - No test for cancellation after a final no-call response (Finding 3).

The tests assert upstream-observable behavior: request counts and request contents, tool output payloads, hook payload sequences, SessionEnd reasons, persisted `ConversationState.DoomLoop` shape, well-formed history, and serialized request JSON for the advisor case. They are not pinned to internal structure. Concurrency-touching tests pass under `-race`.

## Documented divergences check

Divergences recorded in the contract or the changelog (acceptable):

- `ctx` for `signal` (contract #1).
- Explicit `(string, error)` from the canonicalize and fingerprint functions (contract #2).
- `DoomLoopThreshold` sentinel values.
- `LoopKeyFunc` nil/error semantics, with no `undefined` case.
- Invalid-JSON calls dropped, so the parse-error doom path does not arise (Finding 1).
- Unpaired surrogates become U+FFFD.
- Server-tool fingerprints from snake_case wire JSON.
- `DoomLoopDetected` handler errors are logged.
- A doom stop on a decision resume stores outputs in `Messages`.
- The `MaxTurns` default of 5 versus the stop rung (Finding 2).
- Sequential evaluation.

The new naming mappings are recorded in the changelog's mapping table: `DoomLoop`, `DoomLoopVerdict(ctx)`, `LoopKey*`, `MarkMcpOptions`, `OnDoomLoopDetected`, the monitor and primitive functions, the constants, and `signal` → ctx. They are not yet in the contract's Naming Map. Per the contract they should be reported for addition.

Undocumented divergences:

- Finding 3: cancellation ignored after a turn>0 no-call response (warning).
- Finding 4: a text stop with no tools configured seals halt outputs (info).
- Finding 5: turn-0 cancellation ordering versus the approval gate (info).
- Finding 7: PermissionRequest-deny text on a first-turn seal (info).
- Finding 8: non-object argument identity (info).

Version honesty: the changelog labels this work "ported from `@openrouter/agent` work after 0.8.0, not yet in an upstream release". That is accurate at the target commit (`package.json` is still 0.8.0). `verify.sh` correctly warns not to tag v0.8.0 from this tree. `.upstreamer/state.yaml` is unchanged.

## Repo-owned files check

- `git status --short`: modified files are `.upstreamer/coverage-floor.txt`, `.upstreamer/eval-report.md`, `README.md`, `async_params.go`, `conversation_state.go`, `hooks_manager.go`, `hooks_resolve.go`, `hooks_schemas.go`, `model_result.go`, `tool.go`, `tool_types.go` and `upstreamer-changelog.md`. New files are `doom_loop.go`, `doom_loop_engine_test.go`, `doom_loop_test.go`, `run_cancellation_test.go` and `testdata/`.
- `.github/`, `LICENSE`, `scripts/upstream`, `go.mod` and `go.sum` are unchanged (`git diff --stat` is empty for them).
- The module path is `github.com/OpenRouterTeam/go-agent`, and `go-sdk` is pinned at `v0.5.4`. The `service_tier` workaround is retained, per `verify.sh`.
- `.upstreamer/coverage-floor.txt` was raised from 72.0 to 77.0, which is allowed; measured coverage is 78.4%. `.upstreamer/state.yaml` is unchanged.

## Verification commands and results

- `go test -race -count=1 ./...` → `ok github.com/OpenRouterTeam/go-agent 1.927s` (exit 0).
- `.upstreamer/scripts/verify.sh .` → `=== PASS: 0 failures ===`:
  - gofmt, build, vet, test and race all pass;
  - all 24 required symbols are exported, and every required symbol has coverage;
  - coverage is 78.4% against the 77.0% floor;
  - go-sdk is pinned at v0.5.4, and the service_tier pin is retained;
  - no TS artifacts, and the repo-owned files are present.
  - Advisory notes: the tree is 3 commits ahead of the 0.8.0 tag, and the `shared-*.test.ts` cluster has no Go mention. Both predate this delta.
- `cmp testdata/doom-loop-fingerprints.json tmp/upstreamer/upstream/packages/agent/tests/vectors/doom-loop-fingerprints.json` → identical.
- Throwaway experiments in `tmp/upstreamer/opencode/evalcopy`, now deleted:
  - JCS number differential against Node `JSON.stringify` (3,008 values): 0 mismatches on finite values.
  - Cancel in AfterTurn after a turn-2 text response: Go returns `("final", nil)`, while upstream would reject (Finding 3).
  - Text stop with no tools and a `function_call`: Go `complete` with 1 halt output, while upstream is `complete` with no output (Finding 4).
  - Pre-cancelled approval resume: the tool ran once, the run returned `context canceled`, and the accessor was still `awaiting_approval` (Finding 6).

---

## Converter follow-up (after this eval; written by the converter, not the evaluator)

This section is not part of the independent evaluation above. It records what
the converter changed in response to the findings, so the next reader can
tell which findings are still open.

- **Fixed, warning 3 (cancellation on a later, text-only turn was ignored).**
  `model_result.go:run` now checks `runCanceledErr()` on every follow-up
  response (`turn > 0`) before the no-tool-calls break. This matches
  upstream's signal check at the top of the loop. Turn 0 skips the check, just
  as upstream's first-response no-tools path returns before its loop. The new
  test is `run_cancellation_test.go:TestRunCancellationOnTextOnlyFollowUpTurn`.
  I disabled the check and confirmed that the test then fails. The verifier
  was re-run and passes, with coverage at 78.4% (floor 77.0%).
- **Still open, warning 1 (invalid-JSON tool calls).** This predates the delta.
  `ExtractToolCallsFromResponse` drops calls whose arguments are invalid JSON.
  The gap is documented in `upstreamer-changelog.md` and needs a deliberate
  behavior change (parse-error outputs) outside this delta's scope.
  Recommended as a follow-up port item.
- **Still open, warning 2 (`MaxTurns` default 5 vs the default stop rung).**
  The turn cap predates the delta. The interaction is documented in the README
  and the changelog. Recommended contract decision: record `MaxTurns` as a
  Go-specific bound, or align its default with upstream's unbounded loop.
- **Naming-map additions** for the contract are listed in the final run report.
