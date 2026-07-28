PASS WITH WARNINGS

# Eval Report — go-agent, @openrouter/agent 0.7.2 → 0.8.0 (Round 4)

Fresh review context. No prior reasoning trusted — every claim below was
independently verified by reading upstream source, reading the port's code,
and writing/running throwaway probes against `StateAccessor` (not the raw
`State` pointer). Probes were deleted before finishing; `git status` before
and after this review is identical except for this report file.

## Headline

The Round-3 finding (an approval/HITL resume that itself re-pauses never
called `StateAccessor.Save`, so a `StateAccessor`-only caller could reload
stale state and re-execute an already-approved tool) **is genuinely fixed**,
and — going beyond just re-checking that one line — I traced every pause
path in `run()` and independently confirmed via fresh probes that the two
sibling pause boundaries the task specifically flagged as at-risk
(`awaiting_hitl` and `awaiting_client_tools`), plus the mixed-approval pause,
**all correctly persist via `StateAccessor.Save`** through a different,
independent code path (a `break` out of the main loop that falls through to
an unconditional save at the end of `run()`, rather than a `return`).

While hunting for "the next place a similar bug could hide" (as the task
explicitly asked), I found one real, reproducible, but narrower-scope gap:
**go-agent does not persist state incrementally per model turn the way
upstream does** (upstream's `saveResponseToState`/`saveToolResultsToState`
save after every turn; go-agent only saves at the very end of `run()` or at
an explicit pause). A `StateAccessor`-only caller who hits a genuine
mid-run turn-level error (network failure, rate limit, etc.) on turn N+1
loses visibility into turn N's already-executed tool work in the accessor,
even though that tool's side effect already happened. This is real and
worth fixing, but — unlike the Round-3 bug — it is (a) triggered by an
exceptional/error condition outside the documented pause/resume contract,
not the normal, expected, celebrated approval-pause path; (b) pre-existing
since the very first port (confirmed present in upstream's 0.7.2-era source
too, so it predates this whole 0.8.0 delta, not just this round); and (c)
not a violation of eval.md's actual enumerated "State" requirement, which is
scoped to "State survives a pause/resume cycle mid-approval" — a bullet this
round's fix (plus my own probes of the sibling boundaries) now fully
satisfies. I'm flagging it as a warning/follow-up rather than a blocker.

Verdict: **PASS WITH WARNINGS.**

---

## 1. Upstream reference re-confirmed from source

`tmp/upstreamer/upstream/packages/agent/src/lib/model-result.ts`:

- `saveStateSafely` (2428-2445): the single choke point that calls
  `stateAccessor.save`. It is unconditional — no caller checks the resulting
  status before deciding whether to save.
- `processApprovalDecisions` (2735-2881): computes `nextStatus` (which may be
  `awaiting_approval`, `awaiting_hitl`, or `in_progress`) and calls
  `await this.saveStateSafely(stateUpdates)` at line 2859 **before** the
  `if (nextStatus !== 'in_progress') { return; }` check at 2875 — i.e. the
  save happens regardless of whether the resume resolves or re-pauses. This
  is the exact behavior the Round-3/4 fix needed to match.
- `persistHitlPause` (1711-1727) and `persistClientToolsPause` (1753-1769)
  each call `saveStateSafely(stateUpdates)` unconditionally when a HITL or
  manual-tool pause happens on the **initial** (non-resume) path — confirming
  the initial pause paths, not just resumes, must persist.
- `handleApprovalCheck`'s mixed-approval-turn save (1690) is likewise
  unconditional before pausing with `awaiting_approval`.
- `saveResponseToState` (721-758) and `saveToolResultsToState` (776+) are
  called after **every** turn's response and after **every** tool-round's
  results (call sites at lines 3003, 3155, 3200, 3246, 3286, 3304) — i.e.
  upstream persists incrementally per turn, not just at pause/completion
  boundaries. This is the source of the warning-level finding in §4.

## 2. Go port control flow re-traced end to end (`model_result.go` `run()`)

Traced every `return` and every `break` in `run()` (lines 182-498):

- **Resume-repause branch** (227-251, this round's fix): `paused, err :=
  m.prepareResumeRequest(&req); if paused { ...StateAccessor.Save...; return
  }`. Confirmed present, calls `Save` before `return`, mirrors upstream's
  unconditional `saveStateSafely` in `processApprovalDecisions`. This is a
  faithful, minimal, correctly-placed fix.
- **Main loop pause exits** (`ConversationStatusAwaitingApproval` at 380-381
  and 437-439 for the mixed-approval case; `isPausedStatus` break at 391 and
  448-449 for HITL/client-tools/interrupted) all use `break`, not `return` —
  which drops out of the `for turn` loop and falls through to the
  **unconditional** `StateAccessor.Save` at lines 493-497. Since none of
  these pause paths early-`return`, they all reach that save. This is a
  structurally different (but equally correct) mechanism than the
  resume-repause fix, and it already covered these cases correctly — I did
  not find a second instance of the Round-3 bug class on the *initial* pause
  paths.
- `executeToolCallsForTurn` (763-822) sets
  `ConversationStatusAwaitingHITL`/`ConversationStatusAwaitingClientTools`
  directly on `m.state` and returns to the caller, which then `break`s — so
  the same unconditional end-of-`run()` save covers both.
- Genuine mid-loop **errors** (`m.err = err; return`, e.g. `SendResponse`
  failure, stream-consume failure, marshal failure) skip the end-of-`run()`
  save entirely, same as upstream would skip a `saveStateSafely` call it
  never reaches because the enclosing `async` function threw first — *except*
  upstream also has the extra `saveResponseToState`/`saveToolResultsToState`
  calls sprinkled through the loop that persist progress from **earlier,
  already-completed** turns before a **later** turn's request throws. Go-agent
  has no equivalent per-turn checkpoint; see §4.

## 3. Fresh probes (written, run, deleted — not the port's own tests)

Three throwaway tests in `probe_pause_stateaccessor_test.go`, using
`StateAccessor` end-to-end (a `memoryStateAccessor` test double, not
`ModelResult.State()`) and a **third** `CallModel` call reloading from the
same accessor to check for duplicate tool execution, exactly like the
port's own `TestStateAccessorPersistsWhenResumeRepauses`:

- `TestProbeStateAccessorSavedOnHITLPause`: an auto tool + a HITL tool
  (`OnToolCalled` returning `false`) pause the run with `awaiting_hitl`.
  `accessor.saves > 0`, `accessor.state.Status == awaiting_hitl`, the
  regular tool's output was already in `accessor.state.Messages`. A second
  `CallModel` call reloading from the same accessor and approving the HITL
  call did **not** re-run the regular tool (`regularRan == 1`).
- `TestProbeStateAccessorSavedOnAwaitingClientToolsPause`: an auto tool + an
  unresolved manual tool pause with `awaiting_client_tools`. Accessor saved,
  status and pending call correctly reflected, regular tool's output
  persisted before the pause.
- `TestProbeStateAccessorSavedOnMixedApprovalPause`: an auto tool + an
  approval-required tool pause with `awaiting_approval`. Accessor saved, the
  auto tool's unsent result was persisted to the accessor **before** the
  pause. A second `CallModel` call reloading from the same accessor and
  approving the pending call did not re-run the auto tool, ran the danger
  tool exactly once, and both outputs reached the resume request as
  `function_call_output` items in the correct order.

All three passed, including under `-race`. (My first draft of these probes
had a synchronization bug — checking counters immediately after `CallModel`
returns without blocking on `.State(ctx)`/`.Text(ctx)` first, since
`CallModel` starts `run()` in a goroutine and returns immediately. Fixed by
blocking before asserting; this was a bug in my probe, not the port.)

A fourth, throwaway probe (`probe_midrun_error_test.go`,
`TestProbeStateAccessorSavedAfterEarlierTurnWhenLaterTurnErrors`) is the
source of the §4 warning: a tool executes successfully on turn 1, a second
call errors on turn 2 (simulated transport failure), and
`accessor.saves == 0` — turn 1's already-executed tool work never reached
the accessor.

All probe files were deleted before finishing. `git status --porcelain`
before and after probing is identical (only `.upstreamer/eval-report.md` is
new/modified across the whole session).

## 4. Warning: no per-turn incremental `StateAccessor` persistence

Confirmed via probe (above) and via reading
`tmp/upstreamer/upstream/packages/agent/src/lib/model-result.ts` at both the
0.7.2 baseline (`adc7939`) and the 0.8.0 target (`680bceb`): upstream calls
`saveResponseToState`/`saveToolResultsToState` after **every** model
response and **every** tool-execution round in the main loop, not just at
pause/completion boundaries. `go-agent`'s `run()` only reaches
`StateAccessor.Save` at the very end (line 493-497, after the `for` loop
exits via `break` or falls through normally) or at the Round-4-fixed
resume-repause early return. Any hard error inside the loop (`m.err = err;
return`) skips persistence of everything accumulated in prior, already-
completed turns of the *same* run.

Impact: a caller who relies **solely** on `StateAccessor` (never manually
re-threading `ModelResult.State()`, which the contract explicitly allows as
an alternative) and who experiences a genuine mid-run turn-level failure
after at least one earlier turn already executed a tool with a real side
effect, has no durable record of that side effect. A naive retry from the
stale accessor state could re-execute that tool a second time — the same
failure mode as the Round-3 bug, but triggered by an exceptional error
condition on an ordinary multi-turn loop, rather than by the normal,
documented, partial-approval-resume pattern the Round-3/4 fix targets.

This predates the 0.8.0 delta (confirmed present in upstream's own 0.7.2-era
source, so it is not something this round — or even the initial port round —
was tasked with introducing or fixing as part of *this* sync), is not a
violation of eval.md's actual "State" bullet (scoped specifically to
"a pause/resume cycle mid-approval," which is satisfied — see §2-3), and does
not affect the correctness of `ModelResult.State()`/`.Text()`/etc. for a
caller who does use those directly. I'm recording it here as an actionable,
honestly-disclosed finding for a follow-up round rather than a blocker for
this one. Recommend: either add per-turn `saveResponseToState`/
`saveToolResultsToState`-equivalent checkpoints inside the loop, or add an
explicit compatibility note documenting that `StateAccessor`-only callers
should treat a `CallModel` error as "state as of the last successful pause,"
not "state as of the last successful turn."

## 5. Full test suite

`go test ./... -v`: 60 tests (57 port + 3 of my probes, later deleted), 1
skip (`TestE2ESimpleResponsesCall`, requires a live API key), 0 failures.
Read the assertions directly rather than trusting names:
- `TestStateAccessorPersistsWhenResumeRepauses` (the port's own Round-4 test):
  pauses on two approval-required calls, resumes approving only one (which
  re-pauses), asserts `accessor.saves` increased across that re-pause, then
  makes a **third** `CallModel` call from the same accessor and asserts the
  first tool did not re-execute. This is real parity coverage, not a
  shape-only test — it matches my own independent probes' methodology.
- `TestAwaitingClientToolsStatusForUnresolvedManualTool`,
  `TestMixedRegularHITLPreservesRegularOutput`,
  `TestMixedApprovalExecutesAutoToolsBeforePausing`,
  `TestMixedApprovalRejectStillSendsAutoOutput`: cover the mixed-turn
  ordering and manual/HITL pause requirements directly, asserting
  `function_call` → `function_call_output` ordering and pre-pause
  persistence, not just status strings.
- `TestHooksSessionStartEndDoNotRefireOnApprovalResume`,
  `TestPostModelCallTurnTypeDistinguishesResumeFromInitial`: cover the
  Round-2/Round-3 fixes; still pass, confirming no regression.
- `TestSDKStreamErrorPropagatesToConsumers`: confirms `Text`,
  `FullResponsesStream`, and `ToolStream` consumers all surface a stream
  error rather than hanging — required quality "Streaming" satisfied.
- `TestClaudeToFromRoundTripLossless`,
  `TestFormatCompatibilityCarriesMetadataAndUnsupportedContent`: confirm the
  Claude/Chat round-trip preserves metadata, reasoning, tool use, and
  unsupported content — required quality "Compatibility helpers" satisfied.

## 6. Verifier

`.upstreamer/scripts/verify.sh`: **PASS, 0 failures** — gofmt clean, `go
build`/`go vet`/`go test` all clean, all 24 required exported symbols
present, hooks manager + versioned state serialization present, go-sdk
pinned at v0.5.4, `service_tier: auto` workaround retained, no leaked
TS/JS artifacts, LICENSE/README/go.mod/scripts/upstream all present.

## 7. Required Qualities re-checked from scratch (not just state/approval)

- **Public API completeness**: every symbol in the contract's Required
  Public API list (`CallModel`, `NewOpenRouter`, `NewTool`/`MustNewTool`/
  `NewServerTool`, all nine `ModelResult` consumers, `CreateInitialState`/
  `AppendToMessages`/`UpdateState`/`PartitionToolCalls`, all five stop
  conditions, `ToClaudeMessage`/`FromClaudeMessages`/`FromChatMessages`/
  `ToChatMessage`, `ExtractUnsupportedContent`/`HasUnsupportedContent`/
  `GetUnsupportedContentSummary`) confirmed present and exported via direct
  `grep` against each file (not just the verifier's own list) —
  `model_result.go`, `agent.go`, `tool.go`, `conversation_state.go`,
  `stop_conditions.go`, `anthropic_compat.go`, `chat_compat.go`,
  `stream_transformers.go`.
- **Version honesty**: upstream target commit `680bceb4598f228d3e2ec58e2416e4335cdff059`
  has `packages/agent/package.json` version `0.8.0`, matching the
  changelog's claim. `HooksManager` and the full nine-hook surface
  (`PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `UserPromptSubmit`,
  `Stop`, `PermissionRequest`, `SessionStart`, `SessionEnd`, `PostModelCall`)
  are present with typed `OnXxx` registration methods in `hooks_manager.go` —
  not a hollow claim.
- **The load-bearing loop**: `TestFollowUpRequestPreservesAccumulatedInputHistory`
  and `TestApprovalResumeReplaysFunctionCallBeforeOutput` confirm
  `previous_response_id` carry-forward and correct accumulated-history
  ordering across turns.
- **Streaming**: confirmed above (§5).
- **Approval/HITL ordering**: confirmed above (§3, §5) — mixed-turn
  auto-before-pause ordering and `function_call`→`function_call_output`
  replay ordering both hold, independently re-verified with my own probes,
  not just by trusting the port's test names.
- **Hooks**: all nine present; `TestHooksStopForceResumeAndAppendPrompt`,
  `TestHooksPermissionRequestAllowBypassesApprovalGate`/`Deny.../AskUser...`,
  `TestHooksUserPromptSubmitMutatesInitialInput`/`Rejection...` each assert
  the specific documented behavior, not just that a handler fired.
- **Compatibility helpers**: confirmed above (§5).
- **Divergences documented**: the six Idiomatic Divergences in
  `upstreamer.md` are unaffected by this round. The Round-4-specific
  changelog entry and README updates (`git diff README.md`,
  `git diff upstreamer-changelog.md`) accurately describe the fix without
  overclaiming — the changelog explicitly scopes the fix to "an approval/HITL
  resume that itself left calls pending" and does not claim broader
  incremental-persistence parity, which is honest given the §4 finding.
  The §4 finding itself is **not yet documented** anywhere in
  `upstreamer-changelog.md`'s Compatibility Notes — recommend adding it in a
  follow-up.
- **Repo-owned files intact**: `git diff --stat HEAD -- .github LICENSE
  go.mod` is empty — no changes to CI, license, or the substrate pin.

## 8. Changelog honesty

The new changelog bullet ("Fixed a `CallModelInput.StateAccessor`
persistence bug: an approval/HITL resume that itself left calls pending...")
accurately and narrowly describes exactly what was fixed, matches what I
verified in the code, and does not overclaim broader persistence guarantees
it doesn't provide (consistent with the §4 finding still being open). No
overclaiming found.

---

## Verdict: PASS WITH WARNINGS

The Round-4 targeted fix is real, correctly implemented, and I independently
verified — via fresh code tracing and fresh probes exercising
`StateAccessor` end-to-end (not `ModelResult.State()`) — that the same fix
class already correctly covers the two sibling pause boundaries the task
flagged as most at-risk (`awaiting_hitl`, `awaiting_client_tools`) plus the
mixed-approval pause. All Required Qualities re-checked from scratch pass.
`go test ./... -v` and `.upstreamer/scripts/verify.sh` both pass cleanly.

One real, reproducible, but out-of-this-round's-scope finding remains open
(§4: no per-turn incremental `StateAccessor` persistence, unlike upstream) —
recorded here as an honest warning and a recommended follow-up, not a
blocker, because it predates this delta, does not violate eval.md's actual
"pause/resume cycle mid-approval" requirement (which is now fully satisfied
across all pause types), and does not affect the correctness of the
primary `ModelResult` consumer API.
