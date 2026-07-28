---
upstream: OpenRouterTeam/typescript-agent
downstream: OpenRouterTeam/go-agent
model: openrouter/~anthropic/claude-sonnet-latest
---

# Go Port Contract — `@openrouter/agent` → `github.com/OpenRouterTeam/go-agent`

This repository is an idiomatic Go port of the OpenRouter TypeScript Agent SDK
(`@openrouter/agent`, in `packages/agent/` upstream). TypeScript is the
**reference spec**. Behavioral divergence is a bug unless it appears in the
Idiomatic Divergences section below.

## Scope

Port **only** `packages/agent/` from upstream.

Explicitly out of scope:

- `packages/mcp/` (`@openrouter/mcp`). Not ported. Do not begin porting it as a
  side effect of a sync. Adding it is a deliberate contract change.
- Upstream JS/TS infrastructure: `package.json`, `pnpm-lock.yaml`, `tsconfig*`,
  `turbo.json`, `biome.json`, vitest config, `.changeset/`, `.github/`, `esm/`
  build output, `node_modules/`.
- Upstream README/docs prose. This repo's `README.md` is its own product surface.

## Substrate Pin

The port sits on the official OpenRouter Go SDK and must not reimplement HTTP,
auth, retries, or generated model types.

- Target: `github.com/OpenRouterTeam/go-sdk v0.5.4` (current `go.mod` pin).
- `CallModel` sends through `client.Beta.Responses.Send` — the Responses API.
- Request, response, item, stream, usage, and tool union types alias the
  generated SDK's component types where possible.

Do **not** bump `go-sdk` on your own initiative. If an upstream change *requires*
a newer `go-sdk`, stop and report it as a blocker.

Known workaround to preserve: the Responses request pins `service_tier` to
`auto`, working around a marshalling bug in the current Go SDK that emitted an
invalid `service_tier` token. Do not remove this until the SDK pin moves past the
fix.

## Module Version

Go modules version by git tag, not by a manifest field. Do not invent a version
field. Record the ported `@openrouter/agent` version in
`upstreamer-changelog.md` and let release tagging happen separately.

## Required Public API

Every symbol below must be exported and behaviorally faithful. This list is the
parity floor — the verifier enforces presence, the eval enforces behavior.

Entry point and client:
- `CallModel(ctx, client, CallModelInput)`, `NewOpenRouter(OpenRouterOptions)`

Tools:
- `NewTool[T](ToolConfig[T])`, `MustNewTool[T]`, `NewServerTool(ServerToolConfig)`
- Regular, generator, manual, HITL, and server tool shapes; JSON Schema
  generation and sanitization; typed input decoding; tool-event streaming

Result consumption (`ModelResult`):
- `Text(ctx)`, `Response(ctx)`, `TextStream(ctx)`, `ReasoningStream(ctx)`,
  `ToolStream(ctx)`, `ToolCallsStream(ctx)`, `ToolCalls(ctx)`,
  `FullResponsesStream(ctx)`, `NewMessagesStream(ctx)`
- Turn boundary events, state access, cancellation
- SDK stream and transport errors must surface through **every** consumer, never
  hang or end silently

State:
- `CreateInitialState`, `AppendToMessages`, `UpdateState`, `PartitionToolCalls`
- **Versioned serialization contract** (upstream #66): serialize/deserialize
  conversation state, a `CONVERSATION_STATE_VERSION` equivalent, and Go error
  values for invalid and version-mismatched state. Round-trip stable; mismatch
  returns an error rather than silently accepting.

Stop conditions:
- `StepCountIs`, `HasToolCall`, `MaxTokensUsed`, `MaxCost`, `FinishReasonIs`

Lifecycle hooks (upstream #7, #67 — the 0.8.0 headline):
- A `HooksManager` equivalent with options, hook name constants, hook
  definition/entry/handler/registry types, tool matchers, `PostModelCall`
  telemetry, `SessionEnd` usage totals
- Session-id threading per emit so a shared manager is concurrency-safe
- `SessionEnd` / drain guaranteed on no-tools stream error paths

Approval / HITL:
- `ApproveToolCalls` / `RejectToolCalls` on `CallModelInput`; `OnResponseReceived`
  on `ToolConfig[T]`
- Mixed-turn ordering: when one model turn contains both auto-executable and
  approval-required calls, auto-executable tools run and their outputs are
  recorded **before** the pause; those outputs replay alongside the decisions
- Mixed regular + HITL turns must not lose work: an already-produced regular-tool
  output is persisted and replayed in correct `function_call` →
  `function_call_output` order on resume
- `previous_response_id` carried forward
- Unresolved manual tool calls persist as an `awaiting_client_tools` equivalent
  (upstream #64)

Final-turn control:
- `AllowFinalResponse` on `CallModelInput`, plus the default final-answer
  directive for bare `true` (upstream #68)

Compatibility:
- `ToClaudeMessage` → `(ClaudeMessage, error)`; `FromClaudeMessages` and
  `FromChatMessages` → `([]…, error)`; `ToChatMessage` is a direct value
  conversion
- `ToClaudeMessage` emits the full Claude assistant shape: `usage` block
  (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`), mapped `stop_reason`
  (`tool_use`/`end_turn`/`max_tokens`), `stop_sequence`, reasoning as `thinking`
  blocks, top-level `unsupported_content`. A `ToClaudeMessage` →
  `FromClaudeMessages` round trip preserves metadata, reasoning, tool use, and
  unsupported content.
- `ExtractUnsupportedContent(ClaudeMessage, originalType)`,
  `HasUnsupportedContent(ClaudeMessage)`,
  `GetUnsupportedContentSummary(ClaudeMessage) map[string]int`

Support:
- Tool context storage, tool event broadcasting, reusable fan-out streams,
  next-turn params, async params, turn context, stream guards, middleware-based
  request/response interception

## Naming Map

TypeScript `camelCase` → Go `PascalCase` for exported identifiers.

Established mappings — do not re-derive:

| TypeScript | Go |
| --- | --- |
| `callModel(...)` | `CallModel(ctx, client, CallModelInput)` |
| `new OpenRouter(...)` | `NewOpenRouter(OpenRouterOptions)` |
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
| `toClaudeMessage` / `fromClaudeMessages` | `ToClaudeMessage` `(ClaudeMessage, error)` / `FromClaudeMessages` `([]…, error)` |
| `toChatMessage` / `fromChatMessages` | `ToChatMessage` / `FromChatMessages` `([]…, error)` |
| `extractUnsupportedContent(message, type)` | `ExtractUnsupportedContent(ClaudeMessage, originalType)` |
| `hasUnsupportedContent(message)` | `HasUnsupportedContent(ClaudeMessage)` |
| `getUnsupportedContentSummary(message)` | `GetUnsupportedContentSummary(ClaudeMessage) map[string]int` |
| `approveToolCalls` / `rejectToolCalls` | `ApproveToolCalls` / `RejectToolCalls` on `CallModelInput` |
| `allowFinalResponse` | `AllowFinalResponse` on `CallModelInput` |
| HITL `onResponseReceived` | `OnResponseReceived` on `ToolConfig[T]` |

If upstream adds a symbol with no mapping here, follow the convention and
**report the new mapping** so it can be added to this table.

## Idiomatic Divergences

Deliberate and permanent. Do not converge these toward TypeScript.

1. **`context.Context` for cancellation**, not `AbortSignal`. Every blocking
   consumer takes a `ctx`.
2. **Explicit errors, not exceptions.** Operations that can genuinely fail return
   `(T, error)`. A real encoding failure is returned or wrapped — never degraded
   into a placeholder value.
3. **Struct tags + `invopop/jsonschema`** replace Zod. Schema support validates
   common object/string/number/array/required cases; highly specialized draft
   features stay at the dynamic JSON boundary.
4. **`SDKHooks` becomes public middleware** on `OpenRouterOptions`, installed via
   `openrouter.WithClient`, because the Go SDK does not expose its generated
   internal hooks package.
5. **Generics where TypeScript used conditional types.** `NewTool[T]` carries the
   input type. Go cannot express upstream's full per-tool narrowing; runtime
   parity wins.
6. **Channels for streams.** Fan-out uses the reusable-stream helper; a stream
   error must reach every consumer.

## Output Shape

This repo IS the downstream. Flat package at the repo root, `package agent`:

```text
*.go                      # port, one file per upstream lib module
*_test.go                 # tests alongside
go.mod / go.sum           # substrate pin
README.md                 # this package's own docs
upstreamer-changelog.md   # user-facing port notes
```

File naming follows the existing layout: upstream `lib/tool-executor.ts` →
`tool_executor.go`. Keep that correspondence for new files so the port stays
navigable against the reference. Do not introduce nested packages without a
contract change — the flat single-package shape is intentional.

## Verification

`.upstreamer/scripts/verify.sh` must pass: `gofmt` check, `go build ./...`,
`go vet ./...`, `go test ./...`, plus the required-public-API presence check and
no-TS-artifact check.

Then `.upstreamer/eval.md` must return PASS or PASS WITH WARNINGS from a fresh
review context before state advances.

## Final Report

Include: upstream delta, files and exported API touched, tests added, any new
naming mappings derived, any parity gap deliberately left open with reasoning,
verifier result, eval result and report path, and whether the `go-sdk` substrate
pin blocked anything.
