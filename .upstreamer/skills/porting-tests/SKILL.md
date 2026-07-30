---
name: porting-tests
description: Write parity tests for a ported behavior in this Go port of @openrouter/agent. Use when a port run changes behavior, adds a required-API symbol, touches streaming or the tool loop, or when verify.sh reports a coverage-gate failure.
---

# Porting tests

Companion to `upstreamer-converter`. That skill covers *how to port code*; this
one covers *how to prove the port is right*. The contract's Test Quality section
is the binding rule — this is the execution detail.

## The failure this prevents

A port can be wrong in a way that every mechanical check misses:

- The symbol is exported → the verifier's presence check passes.
- The code compiles → build passes.
- No test calls it → nothing detects that it is wrong.

`FinishReasonIs` lived in exactly that state: listed in the contract's Required
Public API, exported, **0% covered**. It is now gated, but the shape of the
mistake recurs. Assume your next ported symbol is in that state until a test
fails when you break it.

## Start from upstream's test, not from the code

Upstream's tests are the most precise statement of the behavior contract that
exists. Porting the code without porting its test means re-deriving intent from
an implementation.

```bash
# What upstream tests cover this area?
ls tmp/upstreamer/upstream/packages/agent/tests/unit/
# Which tests changed in this delta? These are the behaviors that moved.
git -C tmp/upstreamer/upstream diff --stat <last>..<target> -- '*.test.ts'
# Read the case, then port it.
git -C tmp/upstreamer/upstream diff <last>..<target> -- '*hooks*.test.ts'
```

A changed `*.test.ts` with no corresponding change here is the single strongest
signal of a parity gap. Upstream also keeps `*-adversarial.test.ts` files —
those are edge-case suites, and edge cases are where ports break.

## Reuse the existing fakes

Do not invent a new fake. Parallel fakes drift apart from production and from
each other, and a fake that no longer resembles real traffic hides the bugs it
was built to catch.

| Need | Use | Where |
|---|---|---|
| One or more canned non-streaming responses | `fakeSender` | `model_result_test.go` |
| Two-turn tool round | `twoTurnSender` | `model_result_hooks_test.go` |
| A real multi-delta SSE stream | `streamingResponse` / `eventStreamFrom` | `stream_fake_test.go` |
| A stream that errors mid-frame | `failingEventStream` | `orchestration_test.go` |
| HTTP-level middleware | `fakeHTTPClient` | `middleware_test.go` |
| A completed response body | `completedResponse` | `stream_fake_test.go` |
| A paused (awaiting-approval) run | `pausedResult` | `result_accessors_test.go` |
| Force a tool call in a live e2e test | `requiredToolChoice` | `e2e_test.go` |

## Streaming tests

Most of this package's interesting behavior only happens on a stream. A
non-streaming fake response exercises a *fallback* path — it does not test
streaming at all. Before the `stream_fake_test.go` helpers existed, the suite had
26 non-streaming fakes and one error-only stream, which left
`consumeCreateResponse`'s success loop at 42% and `ReasoningStream` structurally
unreachable.

Three SDK details, each of which silently produces a fake that looks fine and
tests nothing:

1. **Build events with the SDK's `Create*` constructors.** They set the union's
   `type` discriminator *and* its member pointer together, as `UnmarshalJSON`
   does for real traffic. A hand-built struct literal can leave `type` empty,
   which still satisfies a pointer nil-check while failing every `Type`-based
   predicate — so the fake disagrees with production.
2. **Marshal the typed value, never `map[string]any`.** `StreamEvents` has a
   custom `MarshalJSON` that flattens the active union member; a map loses it and
   the event decodes back as `Type: "UNKNOWN"` with **no error**.
3. **The SDK re-wraps the SSE `data:` payload as `{"data": <event>}`** before the
   decoder sees it. So the frame body is the bare event, and the decoder
   unmarshals the *envelope* (`ResponsesStreamingResponse`). Get this backwards
   and every event decodes as `UNKNOWN`, again with no error.

Because all three fail silently, `stream_fake_test.go` has
`TestSanityFakeStreamDecodesThroughSDK`, which asserts every event round-trips
with both its pointer and its `Type` set. **Keep that test.** If it fails, every
other streaming test is asserting against a fake that carries no events.

Also note some SDK values must be valid to survive a wire round-trip: an empty
`ToolChoice` union will not marshal, and an empty `Object` will not unmarshal.
`completedResponse` sets both.

## Anti-patterns

**Asserting the port's own shape.** A test that pins current internal structure
passes when the port is wrong and fails when a correct refactor lands. Assert
what a *user* observes: request sequence, ordering, error surfaces, stream event
order and turn boundaries, serialized state shape, pause/resume semantics.

**Happy path only.** Upstream fixes are edge cases. Test the error branch, the
empty input, the mixed turn, the resume — the reason the upstream commit exists.

**Unsynchronized shared state.** The worked example is real: this suite's
`TestHooksManagerAsyncDrain` wrote a `bool` from a detached goroutine and read it
from the test goroutine. Production was fine; the *test* raced, and it failed the
moment `-race` became a gate. If a handler or goroutine writes a variable the
test later reads, guard it with a mutex or synchronize on a channel.

**A test that passes without `-race`.** Always run `go test -race`. For this
package that is the primary correctness signal, not a nicety.

**Coverage theater.** Do not test dead code to move the number. If nothing calls
a function, either wire production to use it or propose deleting it — testing it
raises coverage while adding maintenance surface. And if a function looks like the
real thing but is a simplified copy of it, testing it *endorses a footgun*:
`tool_orchestrator.go:ExecuteToolLoop` resembles the tool loop but passes a zero
`TurnContext` and nil emitter, silently dropping hooks, approval, and generator
streaming. The real loop is `executeToolCallsForTurn` in `model_result.go`.

## Prove the test has teeth

A test that cannot fail is worse than no test: it reports safety that does not
exist. Break the production code on purpose and confirm the test catches it.

```bash
# 1. Make the behavior wrong (guard the branch with `if false`, or return early).
# 2. The new test MUST fail, and name the actual problem:
go test -run TestYourNewTest -count=1 .
# 3. Restore, and confirm the tree is clean:
git diff --stat
```

If it still passes, the test is asserting something other than what you meant.

## Before handing off

```bash
gofmt -l . | grep -v '^tmp/'          # must be empty
go test -race -shuffle=on -count=1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
.upstreamer/scripts/verify.sh          # includes both coverage gates
env -u OPENROUTER_API_KEY go test -run TestE2E -v .   # must SKIP, not fail
```

`-count=1` matters: without it a cached `ok` reports that some earlier tree
passed. `-shuffle=on` catches order dependence.

If the coverage gate fails:

- **Below the floor** → add tests. Never lower the floor; that is the same class
  of error as hand-editing `state.yaml`.
- **Above the floor by >1.5 points** → raise the floor in
  `.upstreamer/coverage-floor.txt` to lock the gain in.
- **A required-API symbol is never exercised** → the gate names the symbol. Write
  a test that would fail if that symbol were broken.
