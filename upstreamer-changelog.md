# go-agent Changelog

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

## Compatibility Notes

- The Go SDK does not expose the generated internal hooks package, so `SDKHooks` is adapted to public middleware on `OpenRouterOptions` and installed with `openrouter.WithClient`.
- Request, response, item, stream, usage, and tool union types are aliases to the official Go SDK component types where possible.
- JSON Schema support validates common object/string/number/array/required cases and relies on `invopop/jsonschema` for schema generation; highly specialized draft features remain at the dynamic JSON boundary.
- JavaScript package infrastructure, TypeScript source, Vitest setup, and monorepo release files are intentionally omitted from this Go module.
- `service_tier` is set to `auto` on every request to work around an upstream Go SDK serialization defect; callers may override it on the request as usual.
- The unsupported-content helpers (`HasUnsupportedContent`, `ExtractUnsupportedContent`, `GetUnsupportedContentSummary`) operate on a `ClaudeMessage`, matching the upstream surface. A separate best-effort helper, `ScanUnsupportedContent(any)`, recursively finds carriers in arbitrary values (e.g. converted input-item slices) and is not part of the upstream API.
