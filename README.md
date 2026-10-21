# go-agent

go-agent is an idiomatic Go port of `@openrouter/agent`: a small orchestration layer on top of the official OpenRouter Go SDK for tool execution, streaming response consumption, multi-turn state, approval gates, tool context, stop conditions, and Claude/Chat format compatibility.

> **This package is a port.** `@openrouter/agent` (TypeScript) is the reference
> spec; this repo is kept in sync automatically. See [PORTING.md](PORTING.md).

## Install

```bash
go get github.com/OpenRouterTeam/go-agent
```

The package depends on `github.com/OpenRouterTeam/go-sdk` and calls the Responses API through `client.Beta.Responses.Send`; it does not reimplement OpenRouter HTTP, auth, retry, or generated model types.

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    agent "github.com/OpenRouterTeam/go-agent"
)

type WeatherInput struct {
    Location string `json:"location" jsonschema:"required"`
}

func main() {
    ctx := context.Background()
    client := agent.NewOpenRouter(agent.OpenRouterOptions{})

    weather := agent.MustNewTool(agent.ToolConfig[WeatherInput]{
        Name: "get_weather",
        Description: "Get the current weather for a location",
        Execute: func(ctx context.Context, in WeatherInput, tc agent.ToolExecuteContext) (any, error) {
            return map[string]any{"temperature": 72, "condition": "sunny", "location": in.Location}, nil
        },
    })

    result, err := agent.CallModel(ctx, client, agent.CallModelInput{
        Model: "openai/gpt-4o-mini",
        Input: "What is the weather in San Francisco?",
        Tools: []agent.Tool{weather},
    })
    if err != nil { panic(err) }

    text, err := result.Text(ctx)
    if err != nil { panic(err) }
    fmt.Println(text)
}
```

## Streaming

`ModelResult` exposes concurrent-safe consumers. Each stream replays prior data to late subscribers.

```go
textCh, done := result.TextStream(ctx)
for delta := range textCh {
    fmt.Print(delta)
}
if err := done(); err != nil { panic(err) }
```

Additional consumers include `Response`, `FullResponsesStream`, `ReasoningStream`, `ToolStream`, `ToolCallsStream`, `ToolCalls`, `NewMessagesStream`, `ItemsStream`, `State`, and `PendingToolCalls`.

## Tool Variants

- Regular tools use `Execute` and return a final output.
- Generator tools use `Generate` and emit preliminary events through a yield callback before returning the final output.
- Manual tools set `Manual: true` or omit executable callbacks; they are surfaced as pending calls instead of auto-executed.
- HITL tools use `OnToolCalled`; returning `proceed=false` pauses the agent with pending calls. `OnResponseReceived` rewrites fresh human-supplied tool outputs before the next Responses request.
- Server tools wrap SDK `components.ResponsesRequestToolUnion` values and are passed through to OpenRouter.

Tool input schemas are generated from Go structs with `invopop/jsonschema`, sanitized to remove upstream-internal `~` keys, and checked before execution. Dynamic `map[string]any` remains available at JSON boundaries.

## State, Approval, And Context

Use `CreateInitialState`, `AppendToMessages`, `UpdateState`, `PartitionToolCalls`, and `StateAccessor` to persist multi-turn conversations. Approval checks can be configured per tool or per call; resume by passing `ApproveToolCalls` or `RejectToolCalls` with the saved state. go-agent replays the original `function_call` before its `function_call_output` and carries `previous_response_id`, matching the TypeScript resume shape. `ToolContextStore` provides concurrency-safe per-tool and shared context with snapshot, get, set, merge, and subscribe operations. Unresolved manual (client-executed) tool calls pause the run with `ConversationStatusAwaitingClientTools`, distinct from `ConversationStatusAwaitingHITL`.

Use `SerializeConversationState` / `DeserializeConversationState` for a versioned, durable-storage-friendly encoding of `ConversationState` (`ConversationStateVersion`). A version mismatch returns `*UnsupportedStateVersionError`; a malformed blob returns `*InvalidStateError` — callers get an explicit error instead of a silently misinterpreted state.

## Stop Conditions

Use `StepCountIs`, `HasToolCall`, `MaxTokensUsed`, `MaxCost`, `FinishReasonIs`, and `IsStopConditionMet`. Multiple stop conditions are ORed, matching the TypeScript package. `MaxTokensUsed` compares cumulative `total_tokens` only.

`AllowFinalResponse` is **default-on**: when a stop condition halts the loop mid-tool-call, go-agent executes the pending tool calls and issues one more request with `tool_choice: "none"` (tools stay in the request so the prompt-cache prefix survives) so the run ends with a natural-language answer. Omitting the option, or setting it to `true`, appends `agent.DefaultFinalResponseDirective` as a final user message; a non-empty string overrides that wording; `""` forbids tool calls without appending any message; `false` disables the forced final turn entirely.

## Lifecycle Hooks

`HooksManager` (`agent.NewHooksManager`) supports the ten built-in lifecycle hooks — `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `UserPromptSubmit`, `Stop`, `PermissionRequest`, `SessionStart`, `SessionEnd`, `PostModelCall`, and `DoomLoopDetected` — plus fully custom hooks via the generic `agent.On`/`agent.Emit`. Register handlers with the typed `OnXxx` methods (e.g. `manager.OnPreToolUse(...)`) and pass the manager on `CallModelInput.Hooks`:

```go
hooks := agent.NewHooksManager()
hooks.OnPreToolUse(agent.HookEntry[agent.PreToolUsePayload, agent.PreToolUseResult]{
    Handler: func(payload agent.PreToolUsePayload, hctx agent.LifecycleHookContext) (agent.HookHandlerResult[agent.PreToolUseResult], error) {
        return agent.VoidResult[agent.PreToolUseResult](), nil
    },
})

result, err := agent.CallModel(ctx, client, agent.CallModelInput{
    Model: "openai/gpt-4o-mini",
    Input: "...",
    Hooks: hooks,
})
```

`SessionStart`/`SessionEnd` fire once per non-resuming run (`SessionEnd` carries aggregated token usage across that run's model calls); an approval/HITL resume call is a continuation of the same session and does not get its own pair. `PostModelCall` fires once per model response, tagged `initial`/`resume`/`tool_round`/`final`/`retry`. `PreToolUse`/`PostToolUse`/`PostToolUseFailure` fire around every client-tool execution path, including during a resume. `PermissionRequest` fires before the human-approval pause and can `allow`/`deny`/`ask_user` (default) a gated call. `Stop` fires whenever a stop condition halts the loop mid-tool-call and can force a resume and/or inject a prompt. `UserPromptSubmit` fires once per non-resuming run against the initial user input and can mutate or reject it. Session identity is threaded per emit, so one `HooksManager` is safe to share across concurrent `CallModel` runs. Call `manager.Drain()` to await fire-and-forget handler work; go-agent always drains on every exit path, including no-tools error paths.

## Doom-Loop Detection

Set `CallModelInput.DoomLoop` to catch runs that stop making progress while continuing to spend: the same tool call with identical arguments in consecutive rounds (including repeated empty `{}` calls), repeated identical server-tool requests, or the same text repeated within or across responses. `&agent.DoomLoopConfig{}` enables the defaults (observe at 2 consecutive identical rounds, block at 3, stop at 6); `nil` (the default) leaves detection off. A run is also capped at `MaxTurns` requests (default 5), so raise `MaxTurns` or lower `Ladder.Stop` if you want the stop rung to bound the run.

```go
result, err := agent.CallModel(ctx, client, agent.CallModelInput{
    Model:    "openai/gpt-4o-mini",
    Input:    "...",
    Tools:    []agent.Tool{searchTool},
    DoomLoop: &agent.DoomLoopConfig{Ladder: agent.DoomLoopLadder{Steer: 3, Block: 4, Stop: 8}},
})
verdict, err := result.DoomLoopVerdict(ctx) // non-nil when detection stopped the run
```

The ladder escalates `observe` (emit the `DoomLoopDetected` hook) → `steer` (inject corrective guidance before the next request) → `escalate` (run the next turn on `Escalation.Model` and/or force an `openrouter:advisor` consult, budgeted by `MaxEscalations`) → `block` (refuse the call with an explanatory tool error) → `stop` (halt before any further model request; `SessionEnd` reason `doom_loop`). A zero threshold uses the default, `agent.DoomLoopRungDisabled` turns a rung off. `DoomLoopDetected` handlers can override the action per event. Streaks are round-scoped (parallel identical calls in one turn count once) and persist in `ConversationState.DoomLoop`, so pass `DoomLoop` again when resuming. Tools declare call identity with `ToolConfig.LoopKey`: `agent.LoopKeyFields("command", "cwd")`, `agent.LoopKeyFunc(func(in Args) (any, error) {...})` (return `nil` to exempt a call), or `agent.LoopKeyExempt()`; `agent.MarkMcp(tool, agent.MarkMcpOptions{LoopKey: ...})` attaches one to an MCP-wrapped tool. Fingerprints are RFC 8785 + SHA-256 and match the TypeScript and Python ports' conformance vectors (arguments carrying unpaired UTF-16 surrogates are the one documented exception, since Go strings are UTF-8).

## Cancellation

The `ctx` passed to `CallModel` cancels the whole run: a canceled ctx (or `result.Cancel()`) stops the loop at the next turn boundary and aborts any in-flight request, and every consumer returns the cancellation cause (`context.Cause`, so `context.WithCancelCause` reasons propagate). A pre-canceled ctx fails before any network dispatch. `SDKOptions.Timeout` bounds each request independently.

## Format Compatibility

`ToClaudeMessage` / `FromClaudeMessages` and `ToChatMessage` / `FromChatMessages` convert between OpenRouter Responses output and Claude or Chat-style messages. Unsupported content is carried with the structured `original_type`, `data`, and `reason` shape so it can round-trip without being silently lost.

## Notes

The TypeScript package re-exports `SDKHooks`; the Go SDK keeps hooks in an internal package, so go-agent adapts the same intent with `OpenRouterOptions` middleware installed through `openrouter.WithClient`. This keeps request and response interception working for Responses calls without importing internal SDK packages.
