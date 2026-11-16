# go-agent Changelog

## Unreleased

- Fixed a possible nil-pointer dereference in the `AllowFinalResponse` test path
  and lower-cased the `stop hook: …` wrapped error string to follow Go
  conventions (it was `Stop hook: …`). If you match on that error's text rather
  than using `errors.Is`/`errors.As`, update the comparison.
- No behavior changes to the public API. The rest of this release is test and CI
  hardening: the race detector, a coverage ratchet, and per-required-symbol
  coverage are now enforced gates, and `ToolEventBroadcaster` plus the
  `stream_guards.go` predicates are now used by the streaming loop instead of
  sitting alongside duplicate inline logic.

## Sync Toward `@openrouter/agent@1.2.1` (partial — see "Not yet ported")

This release brings go-agent's tool loop, approval gate, and state handling in
line with `@openrouter/agent` 1.2.1 (plus the unreleased fixes after it), and
adds doom-loop detection, tool sets, and aggregate usage. Several large upstream
features are **not yet ported**; they are listed at the end so the version
claim stays honest.

### New API

- `ModelResult.Usage(ctx)` returns aggregate token/cost totals across every model call in a run (initial, tool rounds, final turn, retry, resume). It works without hooks, never returns an error, and matches `SessionEnd.TotalUsage`.
- Doom-loop detection: `CallModelInput.DoomLoop` (`nil` disables; `&DoomLoopConfig{}` uses defaults). Repeated identical tool calls, same-tool fan-outs, server-tool repeats, and repeated text get a graduated response: observe, steer (guidance injected as a user message), escalate (a one-turn model swap and/or advisor tool), block (the call gets an error output instead of running), or stop (the run halts and dangling calls are sealed with error outputs).
  - New `DoomLoopDetected` hook whose `OverrideAction` can change the verdict.
  - `ToolConfig.LoopKey` / `McpMarkOptions.LoopKey` set a tool's loop identity.
  - `ModelResult.DoomLoopVerdict` returns the verdict that stopped a run, and `SessionEndReasonDoomLoop` is reported.
  - Detector state persists on `ConversationState.DoomLoop`.
  - Fingerprints are byte-identical to the TypeScript SDK.
- Tool sets: `NewToolSet` with `Activate` / `Deactivate` / `ActivateWhen` / `DeactivateWhen` / `DefineSituations` / `Resolve`. Pass the resolved snapshot as `CallModelInput.Tools` and `CallModelInput.ActiveTools`.
  - `FilterToolsByIDs` and `ToolID` are exported.
  - Server tools get an ID (`ServerToolConfig.ID`, default `server:<type>`).
- `ToolConfig.NextTurnParams` now actually runs. After a call succeeds, its functions can set `model`, `models`, `temperature`, `maxOutputTokens`, `topP`, `topK`, `instructions`, `input`, or `toolChoice` for the next request.
  - They run only for calls whose tool body succeeded; blocked, denied, invalid, or failed calls never change the next request.
  - A `toolChoice` set this way becomes the policy for later turns.
- `ToolConfig.ToModelOutput` maps a successful result to content parts sent to the model, via `ContentModelOutput(...)`. It receives the decoded input and applies on every result path: the normal round, auto-approved siblings at an approval pause, and calls approved on resume.
- `ToolConfig.Strict` (`*bool`) is serialized as the function tool's `strict`.
- `ToolConfig.WireInputSchema` (manual tools) and `McpMarkOptions.WireInputSchema` (via `MarkMcp(tool, opts)`) send a caller-owned JSON Schema verbatim.
- `ToolConfig.Timeout` and `CallModelInput.ToolTimeout` bound tool execution. On expiry the model gets a `*ToolTimeoutError` output (code `tool_timeout`), the loop continues, and the abandoned body's context is cancelled.
- `ToolExecuteContext.CallID` and `ConversationID` are available to tool bodies.
- `ModelResult.QueueUserMessage` injects a user message at the next safe turn boundary.
- `CallModelInput.StreamReplay` (`StreamReplayFull` default, `StreamReplayActiveConsumers`) controls how much stream history is retained for late subscribers. Also available: `NewReusableStreamWithReplay`, `NewToolEventBroadcasterWithReplay`, and `ReusableStream.Cancel`.
- `ResponseHasToolCalls`, `IsContentArray`, `AssertUniqueClientToolNames`, `FindDuplicateClientToolName`, `ConversationPausedError`, and `ErrResponseNotAvailable` are exported.

### Behavior changes

- `CallModel` returns an error when two client tools share a name. Calls are dispatched by name, so the first tool would otherwise receive calls meant for the other.
- Approval decisions are hardened:
  - `ApproveToolCalls` and `RejectToolCalls` are de-duplicated.
  - An ID in both lists makes `CallModel` return an error before anything runs.
  - Resuming an `awaiting_approval` conversation without decisions stays paused: no request is sent, and supplied input is neither sent nor saved.
  - `awaiting_hitl` resumes only when `Input` carries a `function_call_output` for a pending call (or with decisions).
  - While such a call stays paused, `Response`, `Text`, `ToolCalls`, `ItemsStream`, and every stream's error function return `*ConversationPausedError`, which names the pause and lists up to 10 pending IDs.
  - Approvals are processed in the order given, then rejections.
  - Approving only some calls of a multi-call pause leaves the conversation `awaiting_approval` while any call still owes a decision, so a later `Input`-only resume stays paused instead of sending a request with a dangling call.
- Approval gate:
  - `ToolConfig.Approval` predicates and the call-level `CallModelInput.Approval` receive arguments normalized exactly as the tool body will decode them.
  - Tool-level predicates fail closed on invalid arguments.
  - The call-level check also applies to unknown tool names.
  - The `PermissionRequest` risk level comes from the gate's shape and never calls a predicate.
- `PreToolUse` now runs once per call in a preparation phase before execution, for calls that passed the approval gate.
  - If its mutation now requires approval, the call goes through a second gate. Such a pending call carries `PreToolUseApplied`, so a resume doesn't run the hook again.
  - A mutation that produces schema-invalid input is blocked.
  - A prepared call that is later rejected or denied fires `PostToolUseFailure`.
- The initial response now goes through the approval gate before stop conditions and the `Stop` hook are consulted. An approval-gated call pauses without firing `Stop` or injecting its `AppendPrompt`.
- When a stop condition is met on the first response with `AllowFinalResponse: false` and a call needs approval, the run pauses, but the auto-approved sibling calls are not executed. They are recorded as "Tool execution skipped: stop condition met".
- On the `AllowFinalResponse` final turn, calls that cannot run (manual or unknown tools) are paired with a "Tool execution skipped: step limit reached." output instead of pausing the run.
- A response truncated at `max_output_tokens` (`status: incomplete`) executes none of its tool calls, and the run finalizes on that response. `ToolCallsStream` still reports the emitted calls.
  - Streamed `response.incomplete` is now accepted as a terminal event.
  - `response.failed` returns a `response failed: …` error.
- A forced `tool_choice` (`required`, a specific tool, or `allowed_tools` in required mode) relaxes to `auto` on follow-up turns once it has produced a tool call; `allowed_tools` keeps its tool set. The consumption survives a pause and resume (`ConversationState.ConsumedForcedToolChoiceKey`) and resets when the run completes.
- The caller's input for a call is now saved to `ConversationState.Messages` together with the first response. Previously only model output and tool results were saved, so a resumed conversation lost its user turns.
- Tool calls whose arguments are not valid JSON are no longer silently dropped. They get an error output, so the follow-up request has no dangling `function_call`.
- `TurnContext.ToolCall` is now set inside tool bodies (`ToolExecuteContext.Turn.ToolCall`) in the streaming loop and in `OnToolCalled`.
- Struct-derived tool schemas are now inline object schemas instead of a `$ref`/`$defs` document. Validation follows local `$ref`s, so required fields are now actually enforced.
- `FromChatMessages`:
  - An assistant message with no content is omitted when tool calls replace it.
  - Tool calls become `function_call` items with `status: completed`.
- `RequiresApproval` reports `true` for every pause that waits on the caller (`awaiting_approval`, `awaiting_hitl`, `awaiting_client_tools`) or whenever pending calls exist.
- Cancelling a run (`ModelResult.Cancel` or the context) stops the loop at the next turn boundary with the context error, and cancels in-flight tool bodies.
- `tool.call_output` stream events are now also emitted for hook-blocked, denied and doom-loop-blocked calls.
- An initial response whose calls are all manual (or unknown) pauses with `awaiting_client_tools` even when a stop condition is already met, as upstream does.
- The calls in a tool round now run concurrently, as upstream does. Outputs, `tool.result` events and state updates still follow call order. Tool bodies that share state must synchronize it.
- A HITL pause no longer cuts the round short. Every call in the round runs; regular outputs are saved and every paused HITL call becomes pending. Before, calls after the first paused HITL call never ran and were left without outputs, which the API rejects on resume.
- With a `StateAccessor`, state is now saved after every response and every tool round, not only at a pause or at the end of the run. A failure on a later turn no longer loses earlier work.
- A HITL resume through `Input` now continues the tool loop normally. Before, the loaded `awaiting_hitl` status stopped the run after its first tool round.
- Resuming from `awaiting_client_tools` clears the resolved pending calls once the resumed request succeeds.
- A response stream is consumed up to its terminal event (`response.completed` / `incomplete` / `failed`), so a connection that stays open afterwards no longer stalls the run.

### Compatibility notes

- **Breaking:**
  - `ToolExecutionResult.Events` is removed: generator yields are still broadcast live as `tool.preliminary_result` events but no longer accumulated.
  - The unused `ToModelOutputFunction` / `ToModelOutputResult` placeholder types are redefined around content parts.
  - Function tools no longer send `strict: true` by default; set `ToolConfig.Strict` to opt in.
  - Tool stream event types are renamed to the upstream names: `tool.preliminary` is now `tool.preliminary_result`, and `tool.call.output` is now `tool.call_output`. The `IsTool*Event` predicates follow.
- `ExecuteNextTurnParamsFunctions` / `ApplyNextTurnParamsToRequest` apply keys in sorted order, because Go maps are unordered; TypeScript uses insertion order. A `nil` value clears the parameter, and it is left off the request.
- Doom-loop: a lone UTF-16 surrogate that reaches Go through `encoding/json` decodes to U+FFFD, so such arguments fingerprint differently from TypeScript. All other inputs match byte-for-byte.
- Round and per-tool concurrency caps (`toolConcurrency`, `maxConcurrency`) are not available; a round's calls are always unbounded.
- Tool context has no schema validation in Go, so upstream's context-setter parsing change has no Go equivalent.

### Known differences still open

- Runs stop after 5 model requests unless `MaxTurns` is set; upstream has no implicit cap.
- `StopWhen` conditions count the current response as a step, so `StepCountIs(n)` stops one tool round earlier than upstream's `stepCountIs(n)`.
- `SessionEnd` reports `complete` rather than `max_turns` when a stop condition halts the loop.

### Not yet ported

- Unified `run()` tools with `background` / `deferred` lifecycles: the `awaiting_async_tools` status, `resumeToolResults`, the task check-in tool, `sendToTask` steering, `tool.agent()` subagents, and round/per-tool concurrency caps.
- The MCP integration (`@openrouter/agent/mcp`). It is out of scope for this module's contract.

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
