FAIL

# Port Parity Eval: go-agent vs `@openrouter/agent` @ e309ebf

- Reviewer: independent, fresh context. I read the code directly and did not rely on the converter's summary or the earlier report.
- Delta: `680bceb` (0.8.0) → `e309ebf` (0.11.0 plus 18 unreleased commits, including #110, #118, #125 and the #11/#41 fix).
- Rubric: `.upstreamer/eval.md`. Contract: `.upstreamer/upstreamer.md`.

## Mechanical results

- `go test -race -count=1 ./...`: **ok**.
- `.upstreamer/scripts/verify.sh`: **PASS, 0 failures**.
  - The coverage floor went up from 72.0 to 74.0, which the ratchet allows.
  - Advisory NOTE: there are no Go tests for these upstream test clusters: `doom-*`, `steering-*`, `startup-*`, `shared-*`.
  - Advisory NOTE: the tree is 18 commits ahead of the 0.11.0 tag, so no `v0.11.0` tag may be cut from it.
- Repo-owned files are intact: `.github/`, `LICENSE`, `scripts/upstream`, and the coverage floor are all present.

## Verdict rationale

The subset that was ported is careful and well tested (see below). The verdict is FAIL anyway, because the rubric's FAIL rule is triggered.

The rule: "a behavioral parity gap exists in the load-bearing loop / state / approval ordering / hooks … or a behavior that changed in this delta landed with no test covering it."

This delta's headline 0.9.0 features are absent from the port. Each one changes the loop, the state, or the hooks:

| Unported upstream behavior | Surface it touches | Severity |
| --- | --- | --- |
| Doom-loop detection (#73, #89): `doomLoop` option, `DoomLoopMonitor`, ladder observe/steer/escalate/block/stop, `ModelResult.getDoomLoopVerdict()` | **Loop**: pre-execution block/stop. **Hooks**: new built-in `DoomLoopDetected` hook, `SessionEnd.reason: 'doom_loop'`. **State**: `ConversationState.doomLoop` (streaks, `roundFingerprints`, `callStreaks`, `escalationsUsed`). There is a cross-port conformance contract (`tests/vectors/doom-loop-fingerprints.json`) that names the Go port explicitly. | Blocking |
| Async tools (#90): `run` + `lifecycle` (`background`/`deferred`), universal `task` tool, steering (`sendToTask`/`queueUserMessage`), `tool.agent()` subagents, `timeoutMs`/`toolTimeoutMs`, `toolConcurrency`, `resumeToolResults`, `tool.async_started`/`tool.async_settled` events, reserved tool name `task` | **State**: new `ConversationStatus` `awaiting_async_tools`, plus `pendingAsyncTools`/`settledAsyncCallIds` state fields. **Loop**: placeholder outputs, injected `tool_task_result` messages, run-end drain/detach/cancel. **Public API**: new `ModelResult` methods. | Blocking |
| `createToolSet` (#31) | Helper subpath. Only the `ActiveTools` filter it feeds was ported. | Warning |
| `streamReplay: 'active-consumers'` (#109, #116) | Opt-in stream retention. The Go port keeps full replay, which matches upstream's default. | Warning; documented |
| MCP under `packages/agent/src/mcp` (#102, plus `loopKey` for MCP #74) | Upstream moved it into the ported package. The contract's Scope section excludes MCP and says adding it "is a deliberate contract change". Leaving it out follows the contract's intent, but the contract should be updated to name the new location. | Warning; documented |

`upstreamer-changelog.md` is honest. It says "Partial sync toward 0.11.0 … must not be described as 0.11.0 parity" and lists every item above under "Not yet ported". There is **no textual version overclaim**.

Disclosure does not waive the FAIL rule, though. There is also a structural problem:

- Doom-loop (`78c562e`) is the **first** commit in the delta, and async tools (`e8d7d6d`) comes right after it.
- So `state.yaml` cannot advance to any point in this range without recording those features as ported.
- Advancing to `e309ebf` would make the next sync start after them. That is exactly the "silently falls a version behind" failure the rubric exists to prevent.

To unblock, do one of the following:

- Port doom-loop detection and the async-tool lifecycle, with tests ported from `doom-loop*.test.ts`, `async-tool-*.test.ts`, `steering.test.ts`, `task-tool-*.test.ts`, `tool-cancellation.test.ts`, and the fingerprint vectors.
- Or make a deliberate contract change that explicitly defers these subsystems, the way MCP is deferred. It should record a tracked backlog outside `state.yaml` so the next run cannot skip them silently.

## Ported and verified behaviors

For each item I compared the Go code with the upstream source and upstream tests.

| Upstream change | Go location | Faithful? | Go test coverage |
| --- | --- | --- | --- |
| #125 no tool execution from a `max_output_tokens`-truncated response (batch rule); `ToolCallsStream` still reports emitted calls | `stream_transformers.go` `isTruncatedAtMaxOutputTokens`, `ResponseHasToolCalls`, `ExtractToolCallsFromResponse`; `model_result.go` `run` (uses `extractEmittedToolCalls` for the stream) | Yes. It matches `status==incomplete && incompleteDetails.reason==max_output_tokens`. | `max_output_tokens_truncation_test.go`: covers all 3 upstream cases, plus streaming `response.incomplete` as terminal and `response.failed` as an error |
| `response.incomplete` is a terminal event; `response.failed` throws (stream-transformers) | `consumeCreateResponse` | Yes | Yes, a streaming test built with SDK `Create*` constructors |
| #94 predicates see normalized args; tool-level fails closed on invalid args; call-level runs unconditionally with raw args on parse failure | `conversation_state.go` `ToolRequiresApproval`, `argumentNormalizer` | Yes | `TestApprovalPredicateSeesDecodedDefaults`, `…FailsClosedOnSchemaInvalidArguments`, `TestCallLevelCheckHonoredWithRawArgs…` |
| #94 PreToolUse runs before execution for gate survivors; a mutated input is re-validated and re-gated; `PreToolUseApplied` is persisted; the hook is not re-applied on resume; `PostToolUseFailure` fires on a prepared block, a mutated-input denial, or a user rejection of a prepared call | `gateToolCalls`, `prepareAfterInitialApproval`, `mutatedInputRequiresApproval`, `blockPreparedToolCall`, `emitPreparedFailure`, `prepareResumeRequest` | Yes. The phases match upstream `resolveApprovalPhases`/`prepareAfterInitialApproval`. | `TestPreToolUseMutationIsReGatedAndNotReappliedOnResume` (includes a serialize round trip), `TestPreToolUseInvalidMutationIsBlocked` |
| #94 the `allowFinalResponse` path enforces the approval gate; PermissionRequest `deny` works on that path | `run`, stop-condition branch → `gateToolCalls` | Yes | `TestAllowFinalResponsePathEnforcesApprovalGate`, `TestPermissionRequestDenyOnAllowFinalResponsePath` |
| #94 each call occurrence is gated once; duplicate ids are isolated | The verdict lives on the `ParsedToolCall` value itself (`tool_types.go` `preparedBlockReason`, unexported and never serialized). It is consumed in `runToolWithHooks` (`model_result.go:1456`). | Yes. The Go port is stricter than upstream: upstream keys `preparedToolCalls` by `toolCall.id`. | `TestPreparedBlockTargetsTheRightDuplicateOccurrence`, `TestPreparedBlockDoesNotLeakAcrossResponsesWithSameCallID` |
| #100 a forced `toolChoice` is relaxed after it produces a tool call; persists across approval/HITL/client resume; resets on completion | `applyForcedToolChoicePolicy`, `commitToolChoiceDispatch`, `syncToolChoiceState`, `ConsumedForcedToolChoiceKey` | Yes | `tool_choice_relaxation_test.go` (10 cases) |
| #114 `nextTurnParams.toolChoice`, with composition across tools and calls | `next_turn_params.go` `applyToolNextTurnParams` | Yes | `next_turn_params_test.go` (6 cases) |
| #99 `strict` passthrough, default `null` | `tool.go` | Yes. The wire-default change is disclosed. | `TestFunctionToolStrictPassThrough` |
| #118 manual-tool `wireInputSchema` (copied and sanitized) | `tool.go` `WireInputSchema` | Yes. The `ErrWireInputSchemaNotManual` runtime error stands in for upstream's compile-time `never`, which is reasonable. | `tool_wire_schema_test.go` (4 upstream cases, plus rejection) |
| #97 `getUsage()` | `ModelResult.Usage` | Yes (never fails; totals agree with `SessionEnd`) | `get_usage_test.go` (6 cases, including streaming and an approval pause) |
| #31 `activeTools` filter | `async_params.go` / `model_result.go` | Yes (server tools kept; empty filter omits `tools`) | `call_model_active_tools_test.go` (5 cases) |
| #110 generator yields are not accumulated | `tool.go`, `ToolExecutionResult.Events` is always empty | Yes | `tool_test.go` updated |
| #11/#41 `fromChatMessages` emits `function_call` items; an empty assistant message is dropped only when tool calls replace it; `status: completed`; the id is used for both `id` and `call_id`; args are not re-stringified | `chat_compat.go` `FromChatMessages` | Yes | `chat_compat_test.go` (5 cases, mirroring upstream `chat-compat.test.ts`) |
| #91 the executed tool call is threaded into the hook/execute context; `conversationId` | `tool_executor.go` `ExecuteTool`, `ToolExecuteContext.ConversationID` | Yes | `tool_call_context_test.go` (4 cases) |
| user-agent identification (`ddab365`) | `agent.go` (already present; divergence #7) | Yes | `middleware_test.go` |
| #45 clearer `validateFinalResponse` messages | n/a: the existing documented `StrictFinalResponse` divergence | Documented | n/a |
| #111, #112, `HasClientTool` type refactor, Item-union typing | TS-only (import graph, subpath export, types) | No Go analogue needed | n/a |

## Throwaway probes

I wrote these in `zz_probe_test.go`, ran them under `-race`, and deleted the file afterwards. All passed.

1. **Duplicate call ids in one response, blocked call first.** The response was `[dup:bad, dup:good]`. Only `good` executed. The outputs came back in call order: `{"error":"bad query"}` then `ok:good`. Together with the repo's own test for the opposite order, the verdict is isolated per occurrence.
2. **A later response reuses the id, blocked then allowed.** Turn 1 was `dup:bad` (blocked), turn 2 was `dup:good`. `good` executed and there were 3 requests. The block did not leak forward.
3. **A later response reuses the id, allowed then blocked.** Turn 1 was `dup:good`, turn 2 was `dup:bad`. Turn 2 was blocked, and the last request carried the rejection. An earlier "ready" state did not leak forward.
4. **Gate runs once when a stop condition fires on the first iteration** (upstream "gates the initial response only once …"). The tool had `RequireApproval`, PermissionRequest returned `allow`, and `StopWhen: StepCountIs(1)` was set. Result: PermissionRequest ×1, PreToolUse ×1, execute ×1, final text produced.
5. **Mixed turn** with a PreToolUse-blocked call, an auto call, and an approval-required call. The run paused with `awaiting_approval`: 1 pending, 2 unsent (the blocked rejection and the auto output), and only the auto tool had executed. On resume with approval, the order was `call:a call:b call:g out:a out:b out:g`, i.e. `function_call` before `function_call_output`, in call order.
6. **Invalid JSON arguments with a mutating PreToolUse hook.** The hook did not fire and the tool did not run, so the port keeps upstream's fail-closed parse behavior.

## Other findings

1. **Test gaps against upstream `approval-gate-regressions.test.ts`** (low).
   - These upstream cases have no Go counterpart:
     - "gates the initial response only once when the stop condition fires on the first iteration" (probe 4 shows the behavior holds, but nothing in the suite locks it in)
     - "persists unconditional approvals with post-hook arguments …" (static `RequireApproval: true` + ask_user)
     - "executes a type-changing hook mutation from raw input exactly once"
     - "does not abort approval bookkeeping for circular hook mutations"
     - "applies inputSchema coercions …"
     - "still fails closed on schema-invalid arguments for manual tools"
   - Missing from `hooks-post-model-call.test.ts`: "emits when the source close arrives after the terminal event". Go's synchronous `consumeCreateResponse` returns only after `stream.Next()` ends, so the risk is low, but no Go test covers it.
2. **`FromChatMessages` user/system/developer non-string content** (low, pre-existing, undocumented).
   - Upstream runs every role through `contentToString`, so array content becomes a JSON-stringified string.
   - Go passes non-string user/system content as structured `EasyInputMessageContentUnion2Any`.
   - Both before and after this delta, this divergence is not recorded as a compatibility note.
3. **Contract Scope text is stale** (process). `packages/mcp` is now a facade over `packages/agent/src/mcp`, and the Scope section should name that path explicitly.
4. **Naming map additions to report.** These new mappings are not yet in the contract table:
   - `getUsage()` → `Usage(ctx)`
   - `activeTools` → `ActiveTools`
   - `wireInputSchema` → `WireInputSchema`
   - `strict` → `Strict`
   - `preToolUseApplied` → `PreToolUseApplied`
   - `responseHasToolCalls` → `ResponseHasToolCalls`
   - `conversationId` → `ToolExecuteContext.ConversationID`

## Load-bearing loop, HITL, hooks, and state: regression check

Within the ported scope I found no regressions:

- `previous_response_id` is still carried forward (`model_result.go:497`).
- Mixed-turn ordering holds: auto outputs are recorded before the pause (probe 5).
- `SessionStart`/`SessionEnd` and drain are handled in a deferred teardown (`run`).
- Session id is threaded per emit via `hookEmitOptions`.
- Stream errors reach every consumer through the deferred `Complete(m.err)`.
- State stays at version 1. The additive fields `PreToolUseApplied` and `ConsumedForcedToolChoiceKey` are `omitempty` and round-trip.

## Summary

- **Blocking:**
  - Doom-loop detection is unported. It adds a new hook, a new `SessionEnd` reason, state fields, and loop block/stop behavior, and it has a cross-port vector contract.
  - The async-tool lifecycle is unported. It adds the `awaiting_async_tools` status, new state fields, the `task` tool, steering, timeouts, and concurrency.
  - Both are in the delta, both are loop/state/hooks parity gaps with no tests, and advancing `state.yaml` to `e309ebf` would skip them.
- **Warnings:** tool-set helper and stream-replay compaction are unported (documented); MCP is excluded (documented, and the contract wording needs an update); a few approval-gate regression cases have no Go test; the chat-compat non-string content divergence is undocumented.
- **Ported subset:** faithful, tested, and verified by probes, including the per-occurrence isolation of PreToolUse block verdicts.

---

# Converter bankruptcy declaration (appended by the converter after the eval)

**Outcome: the port FAILED the parity eval. `.upstreamer/state.yaml` was NOT updated** and still reads
`upstream_commit: 680bceb4598f228d3e2ec58e2416e4335cdff059`, so the next run retries this same delta
(680bceb..e309ebf).

## Eval history (three fresh-context runs)

1. **FAIL.** B1: doom-loop detection, async tools, createToolSet, replay compaction, and MCP were not
   ported. B2: the `preparedBlocked` map was never cleared, so a PreToolUse block leaked onto a later
   response that reused the same call id.
2. **FAIL.** The cross-response leak was fixed, but the verdict was still keyed by call id, so a block
   landed on the wrong occurrence when one response held duplicate ids. B2 (unported subsystems) remained.
3. **FAIL (this report).** Every ported behavior is verified faithful, including occurrence-isolated block
   verdicts. The remaining blockers are doom-loop detection (#73/#89) and async tools (#90), both unported.

## Fix attempts

- Attempt 1: cleared the prepared-block verdicts per response and on resume. Consumed them on use. Made
  the forced tool-choice key canonical (sorted-key) JSON. Added tests for:
  - a PermissionRequest deny on the final-response path;
  - a fresh request after a completed run;
  - canonical keys;
  - `nextTurnParams` with non-object arguments or invalid keys;
  - streaming `Usage`.
- Attempt 2: moved the block verdict off the id-keyed map and onto the call occurrence itself
  (`ParsedToolCall.preparedBlockReason`, never serialized). Added the duplicate-occurrence test. Relabeled
  a test that cited the wrong upstream case. Added compatibility notes for replay compaction and
  state/key encoding.
- Attempt 3: re-evaluated. The only blocker left is the unported doom-loop and async-tool subsystems.
  This is not a focused fix. It means porting about 1.7k lines of doom-loop plus the async-tool,
  task-tool, steering, subagent and concurrency machinery (most of upstream's +5.4k-line
  `model-result.ts` delta), plus their tests and state fields. That cannot be done coherently in this
  run.

## What this run did land (kept in the working tree, verified, and disclosed)

- The `max_output_tokens` truncation guard, with `response.incomplete` and `response.failed` handled as
  terminal stream events.
- Approval-gate hardening (#94): normalized predicate args, fail-closed checks, PreToolUse before
  gating, mutated-input re-gate, the `PreToolUseApplied` marker, prepared-failure hooks, and
  occurrence-isolated verdicts.
- Forced tool-choice relaxation (#100).
- `NextTurnParams` wired into the loop, including `toolChoice` (#114).
- `ModelResult.Usage` (#97), `ActiveTools` (#31), `Strict` (#99), `WireInputSchema` (#118), and dropped
  generator-yield accumulation (#110).
- The `FromChatMessages` fix (#11/#41), `Turn.ToolCall` threading (#91), and
  `ToolExecuteContext.ConversationID`.

`verify.sh` passes: 0 failures, coverage 74.5% against a floor raised to 74.0.

## Recommended human action

1. **Decide the scope in the contract.** Either:
   - (a) keep doom-loop detection and async tools in scope and schedule a dedicated port run for each,
     porting upstream's doom-loop-* / async-tool-* / task-tool-* / steering / tool-cancellation tests
     and the `tests/vectors/doom-loop-fingerprints.json` conformance vectors; or
   - (b) amend `.upstreamer/upstreamer.md` to defer them explicitly, the way MCP is deferred, and track
     them in a backlog outside `state.yaml`.
2. **Amend the Scope section for the MCP move.** Upstream moved MCP into `packages/agent/src/mcp`, and
   the contract only names `packages/mcp/`.
3. **Add the new naming mappings to the contract table:**
   - `getUsage` → `Usage(ctx)`
   - `activeTools` → `ActiveTools`
   - `wireInputSchema` → `WireInputSchema`
   - `strict` → `Strict`
   - `preToolUseApplied` → `PreToolUseApplied`
   - `consumedForcedToolChoiceKey` → `ConsumedForcedToolChoiceKey`
   - `responseHasToolCalls` → `ResponseHasToolCalls`
   - `nextTurnParams` → `NextTurnParams` on `ToolConfig`
4. **Review and commit the landed partial work** (it is behavior-correct and tested), but do not advance
   `state.yaml` by hand. Also avoid a large bulk port: if the delta is split, it would be safer to port
   the doom-loop range (78c562e/75271c3) first, then async tools (e8d7d6d).
