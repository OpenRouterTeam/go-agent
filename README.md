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

Use `CreateInitialState`, `AppendToMessages`, `UpdateState`, `PartitionToolCalls`, and `StateAccessor` to persist multi-turn conversations. Approval checks can be configured per tool or per call; resume by passing `ApproveToolCalls` or `RejectToolCalls` with the saved state. go-agent replays the original `function_call` before its `function_call_output` and carries `previous_response_id`, matching the TypeScript resume shape. `ToolContextStore` provides concurrency-safe per-tool and shared context with snapshot, get, set, merge, and subscribe operations.

## Stop Conditions

Use `StepCountIs`, `HasToolCall`, `MaxTokensUsed`, `MaxCost`, `FinishReasonIs`, and `IsStopConditionMet`. Multiple stop conditions are ORed, matching the TypeScript package. `MaxTokensUsed` compares cumulative `total_tokens` only. Set `AllowFinalResponse` to execute pending tools at the stop boundary and make one final no-tools request for a closing assistant message.

## Format Compatibility

`ToClaudeMessage` / `FromClaudeMessages` and `ToChatMessage` / `FromChatMessages` convert between OpenRouter Responses output and Claude or Chat-style messages. Unsupported content is carried with the structured `original_type`, `data`, and `reason` shape so it can round-trip without being silently lost.

## Notes

The TypeScript package re-exports `SDKHooks`; the Go SDK keeps hooks in an internal package, so go-agent adapts the same intent with `OpenRouterOptions` middleware installed through `openrouter.WithClient`. This keeps request and response interception working for Responses calls without importing internal SDK packages.
