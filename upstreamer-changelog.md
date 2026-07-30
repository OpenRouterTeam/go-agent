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
