# Porting

This package is a **port** of the OpenRouter TypeScript Agent SDK
(`@openrouter/agent`). TypeScript is the reference spec; this repo tracks it
automatically using [Upstreamer](https://github.com/mountgram/upstreamer) (MIT).

Behavioral divergence from the TypeScript reference is a **bug**, unless it is
listed in the Idiomatic Divergences section of `.upstreamer/upstreamer.md`.

## How it works

The port tracks upstream's **default-branch HEAD**, not the latest published npm
release.

```
   weekly cron  ·  publish dispatch  ·  manual dispatch
        │              (a nudge,          (optional
        │               not a ref)         explicit ref)
        ▼
.github/workflows/upstreamer-port.yaml
        │  resolve ref: explicit input, else upstream HEAD
        ▼
scripts/upstream
        │  1. fetch upstream, resolve target commit
        │  2. REFUSE if target is behind state.yaml (would revert work)
        │  3. compare against .upstreamer/state.yaml — skip if unchanged
        │  4. opencode runs the port against .upstreamer/upstreamer.md
        │  5. .upstreamer/scripts/verify.sh   (mechanical gate)
        │  6. .upstreamer/eval.md            (parity gate, fresh context)
        │  7. advance state.yaml — ONLY if both gates pass
        ▼
    Pull request, opened by the GitHub App so it gets real
    pull_request-event CI checks  (never a direct push to main)
```

### Why HEAD and not the latest release

Release tracking sounds more conservative and is worse in practice. Upstream can
sit for weeks with large unreleased work on `main` — doom-loop detection (#73) was
~7,500 lines, ~4,700 of it tests, and rewrote a big part of `model-result.ts`. A
release-tracking port stays blind to that, then absorbs the entire delta in one
automated run touching the most load-bearing module in the package. Tracking HEAD
keeps each delta small enough that a human can actually review it.

Two consequences follow, and both are handled rather than ignored:

**The port is routinely ahead of the latest release.** So a release ref is now
*dangerous*: it resolves to an ancestor of what is already ported, and the
converter would faithfully "port" the older tree, reverting landed work.
`scripts/upstream` refuses a target that is behind `state.yaml` (exit 3) unless
`--force` is given, and the workflow ignores the publish dispatch's
`client_payload.ref` for the same reason.

**A release tag cut from that tree would misrepresent it.** A Go module declares no
version, so there is no version field to drift — but the ported tree can sit ahead
of the release tag matching its surface, and `git tag v0.8.0` there would ship
unreleased upstream behavior under a released number. Module tags are immutable in
the proxy's cache, so the verifier reports how many commits ahead the tree is
before a tag gets cut. Tag from a commit level with a release tag.

The weekly cron is the primary trigger. The publish dispatch still fires on a new
npm release — useful as "something shipped, sync promptly" — but it syncs to HEAD
like everything else. `workflow_dispatch` allows a manual run against any ref.

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

Two more are needed in CI only, for the bot that opens port PRs:

| Name | Kind | What |
|------|------|------|
| `PORT_BOT_APP_ID` | repo **variable** | The GitHub App's App ID |
| `PORT_BOT_PRIVATE_KEY` | repo **secret** | The App's generated private key (full PEM, including the BEGIN/END lines) |

### Why the port PR needs a GitHub App

GitHub does not trigger workflows from events created with the native
`GITHUB_TOKEN` (its recursion guard), so a PR opened with that token gets no
`pull_request`-event checks at all.

Measured on python-agent PR #24, which runs this same pipeline: the commit carried
**14 check-runs while the PR's rollup showed 7** — a `workflow_dispatch` run of the
same workflow on the same commit was completely invisible to the PR. That is why
"just dispatch `ci.yaml` at the branch" does not substitute for it; the run exists
but gates nothing.

`main` is not branch-protected on this repo, so unlike python-agent nothing
mechanically blocks a merge — which makes this *more* important here, not less.
An automated port PR with no attached checks, sitting next to a green dispatched
run, is exactly the thing a reviewer merges on the assumption CI covered it.

An App installation token is not recursion-guarded, so the PR it opens gets real
checks. Preferred over a PAT: scoped to this repo, not tied to anyone's personal
account, and revocable on its own.

**Setup** — create a GitHub App (org Settings → Developer settings → GitHub Apps):

- Repository permissions: **Contents: Read and write**, **Pull requests: Read and
  write**. Nothing else.
- Install it on `OpenRouterTeam/go-agent`.
- Generate a private key, then add `PORT_BOT_APP_ID` (variable) and
  `PORT_BOT_PRIVATE_KEY` (secret).

Until both exist the pipeline still runs and still opens a PR, but emits a
`::warning::` saying the PR will receive no checks. That is deliberate — an
unconfigured bot should degrade loudly, not look healthy. Both halves are required
by the gate: the ID alone would make the mint step run and fail on the missing
key, breaking a pipeline that otherwise works.

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
