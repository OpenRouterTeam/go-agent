# go-agent Changelog

## Unreleased

### Doom-loop detection (ported from `@openrouter/agent` work after 0.8.0, not yet in an upstream release)

- Added opt-in doom-loop detection via `CallModelInput.DoomLoop`. It catches
  runs that stop making progress while still spending: the model re-issuing the
  same tool call with identical arguments in consecutive rounds (including
  repeated empty `{}` calls), repeating identical server-tool requests (for
  example `web_search_call`, detected after the response lands), or emitting the
  same text over and over, either within one response or across turns.
  `&agent.DoomLoopConfig{}` turns on the defaults. `nil` (the default) leaves it off.
- Detection is deterministic and goes through a graduated ladder (`DoomLoopLadder`):
  `observe` emits the new `DoomLoopDetected` hook, `steer` injects corrective
  guidance before the next request, `escalate` runs the next turn with more help,
  `block` refuses the call before it executes and returns an explanatory tool
  error, and `stop` halts before any further model request. The defaults are
  observe at 2, block at 3, and stop at 6. When a stop happens, unresolved calls
  in the final turn get synthesized halt-error outputs so the persisted history
  stays well-formed, `SessionEnd` reports the new `SessionEndReasonDoomLoop`
  (`"doom_loop"`), and `ModelResult.DoomLoopVerdict(ctx)` returns the verdict that
  stopped the run.
- `escalate` rung: set `DoomLoopConfig.Escalation` to run the next turn on a
  stronger `Model` (a one-turn override that reverts automatically), to force an
  `openrouter:advisor` consult (`Advisor: true`, or `AdvisorParameters` for custom
  advisor settings), or both. The advisor tool is appended with
  `forward_transcript` and loop-diagnosing instructions, and `tool_choice` is
  pinned to it. A notice naming the loop goes with the escalated turn. Escalations
  are capped by `MaxEscalations` (default 2). Budget is consumed only when an
  escalation is actually applied, the count persists across resumes, and two
  detectors firing in the same window escalate only once. A spent or
  unconfigured escalation falls through to the weaker rungs.
- `DoomLoopDetected` handlers can override the action for each event with
  `DoomLoopDetectedResult.OverrideAction` (the last override wins). A `block`
  override on a text or server-tool verdict downgrades to `observe`, and an
  `escalate` override without config or remaining budget also downgrades to
  `observe`. The hook can be registered with `HooksManager.OnDoomLoopDetected` or
  `InlineHookConfig.DoomLoopDetected`.
- Tools can declare what makes two calls "the same" with `ToolConfig.LoopKey`:
  `agent.LoopKeyFields("command", "cwd")` (a declarative field subset),
  `agent.LoopKeyFunc(func(in Args) (any, error) {...})` (computed key material;
  return `nil` to exempt that call), or `agent.LoopKeyExempt()` (the tool is never
  checked). Without a declaration, the full arguments object is the identity.
  `MarkMcp` takes an optional `MarkMcpOptions{LoopKey: ...}` for MCP-wrapped
  tools. A loop key that errors or panics falls back to the full arguments, so
  detection never fails a run.
- Streaks are round-scoped: identical calls fanned out in parallel within one
  turn count once and share one decision. Detector state persists in the new
  `ConversationState.DoomLoop` field, so streaks survive serialize and resume
  (pass `DoomLoop` again when resuming). The field is additive within
  `ConversationStateVersion` 1. A stop verdict survives approve/reject-only
  resumes and is cleared by a fresh conversational turn. Steer guidance queued
  at a pause is delivered after the resume. A corrupt detector blob is ignored
  instead of failing deserialization. Resolving a config logs a warning for dead
  rungs, for `block` without `stop`, and for an `escalate` rung without a
  mechanism (or a mechanism without the rung).
- Fingerprints follow the cross-port contract: RFC 8785 (JCS) canonicalization
  plus SHA-256 (`CanonicalizeKeyMaterial`, `FingerprintToolCall`,
  `FingerprintKeyMaterial`). They match upstream's conformance vectors
  byte-for-byte (see the compatibility note on unpaired surrogates). The primitives (`DoomLoopMonitor`, `DetectTextRepetition`,
  `ResolveLadderAction`, `ResolveDoomLoopOption`, `ResolveLoopKeyMaterial`) are
  exported for direct use.
- On the first response of a call, an approval pause (including running the
  turn's auto-approved calls) or an all-client-tools pause takes precedence over
  a doom stop, as upstream. The armed verdict is persisted, so the following
  approve/reject resume stays halted. On later turns, the stop seals the
  turn's calls first.
- Documented limits, as upstream: loops whose arguments vary each time (for
  example a fresh nonce) evade the default identity unless the tool declares a
  `LoopKey`. Paraphrased text repetition is not detected. Manual or
  client-executed tool calls are not recorded.

### Run cancellation

- Cancelling the `ctx` passed to `CallModel`, or calling `result.Cancel()`, now
  reliably stops the run at the next turn boundary *and* fails before any
  further dispatch. A pre-cancelled ctx fails without touching the network,
  and every consumer returns the cancellation cause (`context.Cause`, so a
  `context.WithCancelCause` reason comes through). Before this change, a
  cancelled ctx was only noticed by the HTTP transport.
- An in-flight request aborted by cancellation now fails with the cancellation
  cause (wrapped together with the transport error) instead of only a
  transport-specific error.

### Compatibility notes

- Upstream's new `signal` option maps to the `ctx` argument (context
  cancellation instead of `AbortSignal`). Upstream also composes
  `RequestOptions.timeoutMs` with the signal to work around a TypeScript SDK
  quirk. The Go SDK already derives a fresh per-request timeout from the run ctx
  (`SDKOptions.Timeout`), so it needs no composition step and no new option.
- Go has no `undefined`. A `LoopKeyFunc` that returns `nil` exempts the call
  (upstream `null`). Upstream's "returned `undefined` → warn and fall back" case
  has no Go equivalent. Return an error to fall back instead.
- Ladder thresholds use `DoomLoopThreshold`: `0` means use the default, a
  positive value is the threshold, and `agent.DoomLoopRungDisabled` (any negative
  value) is upstream's `false`.
- Upstream's "repeated invalid-JSON tool call" scenario does not arise in this
  port. Function calls whose arguments are not valid JSON are dropped when tool
  calls are extracted (this is long-standing go-agent behavior), so such a turn
  ends the run instead of looping on parse errors.
- Go strings are UTF-8, so `encoding/json` decodes an unpaired UTF-16
  surrogate escape (for example `"\ud800"`) in tool arguments to U+FFFD.
  Arguments containing one therefore fingerprint differently from the
  TypeScript port. `CanonicalizeKeyMaterial` does escape a surrogate carried
  as WTF-8 bytes exactly as upstream would, which is what upstream's
  conformance vector for this case checks. Every other upstream vector matches
  as-is. Text normalization uses ECMAScript's `\s` whitespace set (not
  `unicode.IsSpace`), so text fingerprints match upstream.
- Server-tool fingerprints are computed from the output item's wire JSON fields
  (snake_case). They are stable within go-agent but not byte-identical to the
  TypeScript port's server-tool fingerprints, which upstream's cross-port vectors
  do not cover. Client tool-call and text fingerprints are identical across
  ports.
- An error from a `DoomLoopDetected` handler under `ThrowOnHandlerError` is
  logged and the resolved ladder action still applies. Detection never fails a
  run.
- A doom stop on an approve/reject resume records the executed outputs in the
  conversation messages (well-formed `function_call` → `function_call_output`)
  and sends no follow-up request. The next fresh-input call continues from
  there.
- go-agent caps a run at `CallModelInput.MaxTurns` model requests, which
  defaults to 5. Upstream's loop has no default cap. With the default cap, the
  default `stop` rung (6 consecutive identical rounds) cannot fire inside one
  call; `observe` and `block` still do. Raise `MaxTurns`, or lower
  `Ladder.Stop`, if you want the stop rung to bound long runs.
- Tool execution in this port is sequential, so doom-loop evaluations record in
  the order the model emitted the calls, with no extra serialization step.

### Maintenance

- Fixed a possible nil-pointer dereference in the `AllowFinalResponse` test path
  and lower-cased the `stop hook: …` wrapped error string to follow Go
  conventions (it was `Stop hook: …`). If you match on that error's text rather
  than using `errors.Is`/`errors.As`, update the comparison.
- Apart from the doom-loop detection and cancellation work above, the rest of
  this release is test and CI hardening: the race detector, a coverage ratchet,
  and per-required-symbol coverage are now enforced gates, and `ToolEventBroadcaster` plus the
  `stream_guards.go` predicates are now used by the streaming loop instead of
  sitting alongside duplicate inline logic.

## Lifecycle Hooks, Versioned State, And 0.8.0 Parity (ported from `@openrouter/agent@0.8.0`)

- Added `HooksManager` (`NewHooksManager`), a typed lifecycle-hook system with the nine built-in hooks — `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `UserPromptSubmit`, `Stop`, `PermissionRequest`, `SessionStart`, `SessionEnd`, and `PostModelCall` — plus fully custom hooks via the generic `On`/`Emit` functions. Register built-ins with the typed `OnXxx`/`EmitXxx` methods (`manager.OnPreToolUse(...)`, etc.).
- `CallModelInput.Hooks` accepts a `*HooksManager` or an `InlineHookConfig` (via `ResolveHooks`) to install hooks on a run.
- `PreToolUse`/`PostToolUse`/`PostToolUseFailure` now fire around every client-tool execution path (auto-approve, manual approval, approved-on-resume) — a `PreToolUse` block synthesizes a rejected tool result without running the tool; a mutated input is used for the actual call.
- `PermissionRequest` now fires for every approval-gated tool call before the human-approval pause: `allow` promotes the call past the gate and executes it this round, `deny` synthesizes a rejected output without pausing or executing, and `ask_user` (the default, and the behavior with no hooks installed) falls through to the existing approval pause. Risk level (`low`/`medium`/`high`) is derived from the approval gate's shape, matching upstream.
- `Stop` now fires whenever a `StopWhen` condition halts the loop mid-tool-call: a handler's `ForceResume` keeps the run going instead of taking the forced-final-response turn, and `AppendPrompt` injects a user message (independently of `ForceResume`) into both the conversation state and the next request. Consecutive overrides without tool progress are capped (mirroring upstream's `MAX_FORCE_RESUME_OVERRIDES`) so a misbehaving handler can't spin the loop forever.
- `UserPromptSubmit` now fires once per run against the latest user-role text message in the accumulated input: a handler can mutate that message's content before the first request is sent, or reject the prompt outright (surfaced as a `CallModel` error before any model call is made).
- `PostModelCall` fires once per materialized model response (initial, resume, tool-round, final, and retry turns — including the `resume` tag for the model call issued right after processing an approval/HITL resume) with per-call usage (`ModelCallUsage`); `SessionStart`/`SessionEnd` fire once per *non-resuming* `CallModel` call, with `SessionEnd` carrying aggregated `SessionUsageTotals` across that call's model calls. An approval/HITL resume call (state `awaiting_approval`/`awaiting_hitl` plus `ApproveToolCalls`/`RejectToolCalls`) is treated as a continuation of the same logical session and does **not** get its own `SessionStart`/`SessionEnd` pair — only `PreToolUse`/`PostToolUse`/`PostModelCall`/`Stop`/`PermissionRequest` fire during it, matching upstream. Session identity is threaded per-emit (`EmitOptions.SessionID`), so one `HooksManager` is safe to share across concurrent `CallModel` runs.
- Hooks are drained (`HooksManager.Drain`) on every `CallModel` exit path, including a no-tools stream error before any tool ever runs, so fire-and-forget handler work set up via `AsyncResult`/`AsyncOutput` is never silently dropped.
- Added a versioned `ConversationState` serialization contract: `SerializeConversationState` / `DeserializeConversationState`, `ConversationStateVersion`, `*UnsupportedStateVersionError` for a future/incompatible version, and `*InvalidStateError` for malformed or incomplete state JSON. Version-less legacy blobs are accepted and normalized to version 1.
- Unresolved manual (client-executed) tool calls now pause a run with the distinct `ConversationStatusAwaitingClientTools` status (previously reported as the generic `interrupted` status), so callers can tell "you must execute this tool call yourself" apart from an arbitrary interruption or a HITL pause.
- `AllowFinalResponse` is now **default-on**: omitting it behaves like bare `true`. When a stop condition halts the loop mid-tool-call, go-agent executes the pending tool calls and issues one more request with `tool_choice: "none"` — keeping `tools` in the request (rather than stripping them) so the prompt-cache prefix survives — and appends `DefaultFinalResponseDirective` as a final user message so models that emit tool-call syntax as text don't leak an unparsed call into the answer. A non-empty string still overrides the wording; `""` forbids further tool calls without appending a message; `false` still disables the forced final turn entirely.
- Added `CallModelInput.StrictFinalResponse`. A completed run that ends with a genuinely empty response after at least one tool round is retried once (mirroring the model needing a one-turn nudge); `StrictFinalResponse: true` skips that retry.
- Added MCP-source discrimination: `MarkMcp(tool)` / `IsMcpTool(tool)`, and a `Source` field (`ToolSourceClient` / `ToolSourceMCP`) on `ToolExecutionResult` and `ToolStreamEvent`/`ToolResultEvent`, so a tool wrapped from a remote MCP server can be identified without changing its execution or wire shape.
- Added `ReusableStream[T].IsComplete()`, mirroring the source-exhausted flag on the underlying fan-out stream.
- Fixed a `CallModelInput.StateAccessor` persistence bug: an approval/HITL resume that itself left calls pending (not every decision resolved the round, so the resume re-paused) never saved the updated state back to the accessor. A caller relying solely on `StateAccessor.Load`/`Save` (rather than manually re-threading `ModelResult.State()`) could reload stale state on the next `CallModel` call and re-execute an already-approved tool. The resume path now saves before returning on that pause, matching the save that already happened at the end of a fully-resolved run.

## Initial Go Port

- Ported the OpenRouter agent toolkit to a standalone Go module with package name `agent` and module path `github.com/OpenRouterTeam/go-agent`.
- Added the core Responses API call path on top of the official OpenRouter Go SDK, including the `OpenRouter` wrapper and middleware-based request/response interception.
- Added regular, generator, manual, HITL, and server tool shapes with JSON Schema generation, schema sanitization, typed input decoding, and tool-event streaming.
- Added `ModelResult` consumers for final text, full response, text/reasoning streams, tool streams, tool-call streams, new-message streams, state, and cancellation.
- Added conversation state helpers, approval partitioning, stop conditions, tool context storage, reusable fan-out streams, and tool event broadcasting.
- Added approval/HITL resume support: pending calls can be approved or rejected, original `function_call` items are replayed before matching outputs, and `previous_response_id` is carried forward.
- Follow-up requests now preserve accumulated input/history, including the original user input, response output items, tool outputs, and carried `previous_response_id`.
- Added `AllowFinalResponse` behavior: when a stop condition halts on a tool-call turn, go-agent can execute pending tools and issue a final no-tools request for a closing assistant message.
- Added Claude and Chat compatibility helpers with tool-use/tool-result handling, image carriers, metadata, and unsupported-content carriers using `original_type`, `data`, and `reason`.

## Mixed-turn orchestration and reliability

- Mixed approval turns now match upstream ordering: when one model turn contains both auto-executable and approval-required tool calls, the auto-executable tools run and their outputs are recorded **before** the conversation pauses for approval. Those recorded outputs are replayed alongside your approve/reject decisions on resume.
- Mixed regular + HITL turns no longer lose work: when a regular tool runs and a later human-in-the-loop tool pauses the same turn, the already-produced regular-tool output is persisted to conversation state and replayed (in correct `function_call` → `function_call_output` order) when the conversation resumes.
- SDK stream and transport errors now surface through every `ModelResult` consumer (`Text`, `TextStream`, `ToolStream`, `FullResponsesStream`, …) instead of a consumer hanging or ending silently.
- `ToClaudeMessage` / `FromClaudeMessages` / `FromChatMessages` are now fallible (`(T, error)`): a real encoding failure is returned/wrapped instead of being degraded into a placeholder value. `ToChatMessage` remains a direct value conversion.
- `ToClaudeMessage` now produces the full Claude assistant-message shape: a Claude `usage` block (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`), a mapped `stop_reason` (`tool_use` / `end_turn` / `max_tokens`), `stop_sequence`, reasoning rendered as `thinking` blocks, and a top-level `unsupported_content` array for content Claude cannot represent. A `ToClaudeMessage` → `FromClaudeMessages` round trip preserves metadata, reasoning, tool use, and unsupported content.
- The Responses request now pins `service_tier` to `auto`, working around a marshalling bug in the current Go SDK that emitted an invalid `service_tier` token and broke real request serialization.

## TypeScript To Go API Mapping

| TypeScript | Go |
| --- | --- |
| `callModel(...)` | `CallModel(ctx, client, CallModelInput)` |
| `new OpenRouter(...)` wrapper | `NewOpenRouter(OpenRouterOptions)` |
| `tool(...)` | `NewTool[T](ToolConfig[T])` / `MustNewTool[T]` |
| `serverTool(...)` | `NewServerTool(ServerToolConfig)` |
| `result.getText()` | `result.Text(ctx)` |
| `result.getResponse()` | `result.Response(ctx)` |
| `result.getTextStream()` | `result.TextStream(ctx)` |
| `result.getReasoningStream()` | `result.ReasoningStream(ctx)` |
| `result.getToolStream()` | `result.ToolStream(ctx)` |
| `result.getToolCallsStream()` | `result.ToolCallsStream(ctx)` |
| `result.getToolCalls()` | `result.ToolCalls(ctx)` |
| `result.getFullResponsesStream()` | `result.FullResponsesStream(ctx)` |
| `result.getNewMessagesStream()` | `result.NewMessagesStream(ctx)` |
| `createInitialState()` | `CreateInitialState()` |
| `appendToMessages(...)` | `AppendToMessages(...)` |
| `updateState(...)` | `UpdateState(...)` |
| `partitionToolCalls(...)` | `PartitionToolCalls(...)` |
| `stepCountIs`, `hasToolCall`, `maxTokensUsed`, `maxCost`, `finishReasonIs` | `StepCountIs`, `HasToolCall`, `MaxTokensUsed`, `MaxCost`, `FinishReasonIs` |
| `toClaudeMessage` / `fromClaudeMessages` | `ToClaudeMessage` (`(ClaudeMessage, error)`) / `FromClaudeMessages` (`([]…, error)`) |
| `toChatMessage` / `fromChatMessages` | `ToChatMessage` / `FromChatMessages` (`([]…, error)`) |
| `extractUnsupportedContent(message, type)` | `ExtractUnsupportedContent(ClaudeMessage, originalType)` |
| `hasUnsupportedContent(message)` | `HasUnsupportedContent(ClaudeMessage)` |
| `getUnsupportedContentSummary(message)` | `GetUnsupportedContentSummary(ClaudeMessage) map[string]int` |
| `approveToolCalls` / `rejectToolCalls` | `ApproveToolCalls` / `RejectToolCalls` on `CallModelInput` |
| `allowFinalResponse` | `AllowFinalResponse` on `CallModelInput` |
| HITL `onResponseReceived` | `OnResponseReceived` on `ToolConfig[T]` |
| `new HooksManager(custom, opts)` | `NewHooksManager(opts...)` (custom hooks: use `On[P, R](manager, name, entry)`, no upfront registry needed) |
| `manager.on(hookName, entry)` / `manager.off(hookName, handler)` | `manager.OnXxx(entry)` (built-ins) / `On[P, R](manager, name, entry)` (custom); `Off[P, R](manager, name, handler)` |
| `manager.emit(hookName, payload, ctx)` | `manager.EmitXxx(payload, opts)` (built-ins) / `Emit[P, R](manager, name, payload, opts)` (custom) |
| `manager.setSessionId(id)` / `manager.hasHandlers(name)` | `manager.SetSessionID(id)` / `manager.HasHandlers(name)` |
| `manager.drain()` / `manager.abortInflight()` | `manager.Drain()` / `manager.AbortInflight()` |
| `resolveHooks(hooks)` | `ResolveHooks(hooks any) *HooksManager` (accepts `*HooksManager` or `InlineHookConfig`) |
| `isAsyncOutput(value)` | not ported as a standalone predicate — `HookHandlerResult[R].Async` is the typed signal (see `AsyncResult`/`VoidResult`/`SyncResult`) |
| `matchesTool(matcher, toolName)` | `MatchesTool(matcher ToolMatcher, toolName string) bool` |
| `CONVERSATION_STATE_VERSION` | `ConversationStateVersion` |
| `serializeConversationState` / `deserializeConversationState` | `SerializeConversationState(state) (string, error)` / `DeserializeConversationState(json) (ConversationState, error)` |
| `UnsupportedStateVersionError` / `InvalidStateError` | `*UnsupportedStateVersionError` / `*InvalidStateError` |
| `strictFinalResponse` | `StrictFinalResponse` on `CallModelInput` |
| `DEFAULT_FINAL_RESPONSE_DIRECTIVE` | `DefaultFinalResponseDirective` |
| `markMcp(tool)` / `isMcpTool(tool)` | `MarkMcp(tool) Tool` / `IsMcpTool(tool) bool` |
| tool result / stream event `source` | `Source ToolSource` (`ToolSourceClient` / `ToolSourceMCP`) on `ToolExecutionResult`, `ToolStreamEvent`, `ToolResultEvent` |
| `awaiting_client_tools` | `ConversationStatusAwaitingClientTools` |
| `doomLoop` option (`true` / config) | `DoomLoop *DoomLoopConfig` on `CallModelInput` (`&DoomLoopConfig{}` / config) |
| `result.getDoomLoopVerdict()` | `result.DoomLoopVerdict(ctx)` |
| tool `loopKey` (function / field list / `false`) | `ToolConfig.LoopKey` (`LoopKeyFunc` / `LoopKeyFields` / `LoopKeyExempt`) |
| `markMcp(tool, { loopKey })` | `MarkMcp(tool, MarkMcpOptions{LoopKey: ...})` |
| `DoomLoopDetected` hook | `HooksManager.OnDoomLoopDetected` / `InlineHookConfig.DoomLoopDetected` |
| `DoomLoopMonitor`, `detectTextRepetition`, `resolveLadderAction`, `resolveDoomLoopOption`, `resolveLoopKeyMaterial` | `DoomLoopMonitor` (`NewDoomLoopMonitor`), `DetectTextRepetition`, `ResolveLadderAction`, `ResolveDoomLoopOption`, `ResolveLoopKeyMaterial` |
| `canonicalizeKeyMaterial`, `fingerprintKeyMaterial`, `fingerprintToolCall` | `CanonicalizeKeyMaterial`, `FingerprintKeyMaterial`, `FingerprintToolCall` (`(string, error)`) |
| `DEFAULT_DOOM_LOOP_LADDER`, `DEFAULT_MAX_ESCALATIONS`, `MAX_CANONICALIZE_DEPTH` | `DefaultDoomLoopLadder()`, `DefaultMaxEscalations`, `MaxCanonicalizeDepth` |
| `signal` option | the `ctx` argument to `CallModel` |

## Compatibility Notes

- The Go SDK does not expose the generated internal hooks package, so `SDKHooks` is adapted to public middleware on `OpenRouterOptions` and installed with `openrouter.WithClient`.
- Request, response, item, stream, usage, and tool union types are aliases to the official Go SDK component types where possible.
- JSON Schema support validates common object/string/number/array/required cases and relies on `invopop/jsonschema` for schema generation; highly specialized draft features remain at the dynamic JSON boundary.
- JavaScript package infrastructure, TypeScript source, Vitest setup, and monorepo release files are intentionally omitted from this Go module.
- `service_tier` is set to `auto` on every request to work around an upstream Go SDK serialization defect; callers may override it on the request as usual.
- The unsupported-content helpers (`HasUnsupportedContent`, `ExtractUnsupportedContent`, `GetUnsupportedContentSummary`) operate on a `ClaudeMessage`, matching the upstream surface. A separate best-effort helper, `ScanUnsupportedContent(any)`, recursively finds carriers in arbitrary values (e.g. converted input-item slices) and is not part of the upstream API.
- Hooks have no runtime schema layer: upstream validates hook payloads/results against Zod schemas at runtime; go-agent's hook payload/result shapes are plain compile-time-typed Go structs (the type system is the validation layer), consistent with the existing "struct tags replace Zod" divergence for tool schemas. A registered handler whose declared type doesn't match the hook it's registered under is skipped with a logged warning rather than causing a runtime schema-validation error.
- `PreToolUseResult.Block`/`BlockReason` and `UserPromptSubmitResult.Reject`/`RejectReason` split upstream's `boolean | string` union field into two Go fields; either one set triggers the same block/reject short-circuit as upstream (`=== true` or a non-empty string).
- A handler's upstream fire-and-forget `AsyncOutput.work?: Promise<unknown>` becomes `AsyncOutput.Work <-chan error` in Go (channels are this port's stream/async primitive, per the existing "channels for streams" divergence); a handler kicks off a goroutine and closes the channel (optionally sending an error first) when its detached work finishes.
- `HooksManager.AbortInflight` cancels each in-flight emit's `context.Context` (threaded through `LifecycleHookContext.Ctx`) rather than firing an `AbortSignal`, per the existing "`context.Context` for cancellation" divergence.
- `HooksManager.Off` is a best-effort match on the handler function's code pointer via `reflect`, since Go function values are not comparable the way upstream compares handler references; the unsubscribe function returned by `On`/`OnXxx` is the precise removal path.
- go-sdk's `OpenResponsesResult` carries a separate `OutputText` convenience field alongside `Output` items (upstream's `output` is the sole content carrier), so go-agent does not port upstream's hard "empty final response is invalid" error — an empty `Output` array is not on its own a reliable invalidity signal in the Go SDK's response shape. go-agent still retries once when a completed run's final response has neither `Output` items nor extractable text after at least one tool round (`StrictFinalResponse` opts out of the retry), but it never turns that into a hard error the way upstream's `validateFinalResponse` does.
- Go's `NewTool[T]` already carries a single input-type parameter without upstream's separate context-schema type parameter; `ToolExecuteContext.Context`/`.Shared` are `map[string]any` by design (idiomatic divergence #5, generics where TypeScript used conditional types), so upstream's contextSchema-type-inference fix (upstream #65, a TypeScript-conditional-type-only change with no runtime behavior) has no Go analog to port.
- `CallModelInput.StateAccessor` persistence is guaranteed at every documented pause boundary (`awaiting_approval`, `awaiting_hitl`, `awaiting_client_tools`, and a resume that itself re-pauses) and at normal completion, matching upstream's `saveStateSafely` call sites for those cases. It is **not** yet checkpointed after every individual turn/tool-round the way upstream's `saveResponseToState`/`saveToolResultsToState` are: a genuine mid-run turn-level error (e.g. a transport failure) after an earlier turn in the *same* run already executed a tool does not persist that turn's work to the accessor before returning the error. This predates the 0.8.0 delta. Callers who need per-turn durability today should re-thread `ModelResult.State()` themselves after each call rather than relying solely on `StateAccessor`; per-turn incremental accessor saves are tracked as a follow-up.
