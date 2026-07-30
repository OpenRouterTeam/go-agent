# Porting

This package is a **port** of the OpenRouter TypeScript Agent SDK
(`@openrouter/agent`). TypeScript is the reference spec; this repo tracks it
automatically using [Upstreamer](https://github.com/mountgram/upstreamer) (MIT).

Behavioral divergence from the TypeScript reference is a **bug**, unless it is
listed in the Idiomatic Divergences section of `.upstreamer/upstreamer.md`.

## How it works

```
typescript-agent publishes @openrouter/agent to npm
        │
        │  repository_dispatch: openrouter-agent-published
        ▼
.github/workflows/upstreamer-port.yaml
        │
        ▼
scripts/upstream
        │  1. fetch upstream, resolve target commit
        │  2. compare against .upstreamer/state.yaml — skip if unchanged
        │  3. opencode runs the port against .upstreamer/upstreamer.md
        │  4. .upstreamer/scripts/verify.sh   (mechanical gate)
        │  5. .upstreamer/eval.md            (parity gate, fresh context)
        │  6. advance state.yaml — ONLY if both gates pass
        ▼
    Pull request  (never a direct push to main)
```

A weekly cron backs up the dispatch in case one is missed, and
`workflow_dispatch` allows a manual run against any ref.

## The contract is the product

`.upstreamer/upstreamer.md` is the durable artifact — it defines scope, the
required public API, naming maps, permanent idiomatic divergences, and the
substrate pin. The ported source is an *output* of that contract.

So when the port gets something wrong, **fix the contract**, not just the
generated code. A code-only fix gets re-broken on the next sync; a contract fix
holds.

## Files

| Path | What |
|------|------|
| `.upstreamer/upstreamer.md` | The rewrite contract. Binding. |
| `.upstreamer/state.yaml` | Last commit ported *and* verified *and* eval-passed. |
| `.upstreamer/scripts/verify.sh` | Mechanical gate: build, lint, types, tests, race, coverage, required API. |
| `.upstreamer/coverage-floor.txt` | Coverage ratchet. Raise it, never lower it. |
| `.upstreamer/eval.md` | Parity gate: fresh-context behavioral review. |
| `.upstreamer/eval-report.md` | Latest eval result, or a bankruptcy report. |
| `.upstreamer/skills/upstreamer-converter/` | Execution discipline for the porting agent. |
| `.upstreamer/skills/porting-tests/` | How to write parity tests for a ported behavior. |
| `.upstreamer/port.env` | Local secrets. **Gitignored.** |
| `scripts/upstream` | The wrapper. |

## Two gates, and why state matters

**Mechanical** (`verify.sh`) — objective: does it build, lint, type-check, pass
tests (including under `-race`), hold the coverage floor, exercise every
required-API symbol with at least one test, and export every symbol the contract
requires.

Note what the two coverage gates buy. A presence check cannot see an exported
symbol that no test ever calls — that is how `FinishReasonIs` sat at 0% coverage
while passing verification. The per-symbol gate closes that hole; the ratchet
stops coverage from eroding one sync at a time.

**Parity** (`eval.md`) — judgment, run in a fresh context that reads the upstream
reference directly: does it actually *behave* like upstream. This is the gate that
catches a port which compiles cleanly while sitting a version behind on real
behavior.

If either gate fails the run **declares bankruptcy**: `state.yaml` is left
untouched and `.upstreamer/eval-report.md` explains why. A stale state file is the
correct outcome for a failed port — it makes the next run retry the same delta
instead of skipping past the gap. The workflow labels such a PR `eval-failed` and
marks the title `do not merge`.

Never hand-edit `state.yaml` to make a run look successful.

## Running it locally

```bash
# one-time
bun install -g opencode-ai          # or: npm install -g opencode-ai
cp .upstreamer/port.env.example .upstreamer/port.env
# then edit .upstreamer/port.env and fill in OPENROUTER_API_KEY + OPENCODE_MODEL

./scripts/upstream                  # sync if upstream changed
./scripts/upstream --force           # re-run after editing the contract
./scripts/upstream --ref v0.8.0      # port a specific upstream ref
./scripts/upstream -- --print-logs   # pass args through to opencode
```

`--force` is the escape hatch for a changed contract with unchanged upstream.
Expect to use it often while the contract is still settling.

## Credentials

Two values, same names locally and in CI:

| Name | Where | What |
|------|-------|------|
| `OPENROUTER_API_KEY` | local: `.upstreamer/port.env` · CI: repo **secret** | `sk-or-…` key opencode uses for inference |
| `OPENCODE_MODEL` | local: `.upstreamer/port.env` · CI: repo **variable** | e.g. `openrouter/~anthropic/claude-opus-latest` |

The wrapper writes the key into `~/.local/share/opencode/auth.json` so headless
runs work without the interactive `opencode /connect` flow.

This is a load-bearing SDK port behind a strict parity eval — use a strong coding
model. `OPENCODE_MODEL` overrides the `model:` field in the contract, so you can
change models without a code change.

## Reviewing a port PR

Review it as a *port*, not a normal diff:

1. Check `.upstreamer/eval-report.md` first. If state did not advance, stop.
2. Read the upstream delta yourself for anything load-bearing — the tool loop,
   state serialization, approval/HITL ordering, hooks, streaming.
3. Confirm new tests assert *upstream behavior*, not merely the port's own shape.
   Read the upstream test file for the changed behavior and check the port covers
   the same cases. Ignore test counts — Go subtests bundle cases, so the ratio to
   upstream's `it` blocks is meaningless.
4. Any new naming mapping the run derived should be promoted into the contract's
   naming table.
5. Check the coverage floor moved up, or stayed put — never down. A lowered floor
   is the same class of error as hand-editing `state.yaml`.
