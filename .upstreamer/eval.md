# Port Parity Eval — Go

Run after mechanical verification passes, in a **fresh review context**. You are
grading a port you did not write. Do not rely on the converter's reasoning,
summary, or changelog — read the code.

## Goal

Decide whether this repository is a faithful, usable Go port of
`@openrouter/agent` at the target upstream commit. The question is not "does it
build" — the verifier already answered that. The question is **"would a user of
this package get the same behavior a TypeScript user gets?"**

This eval exists because a port can compile, pass its own tests, and still be a
version behind on real behavior. That failure mode is the one to catch.

## Inputs

- Contract: `.upstreamer/upstreamer.md`
- Upstream reference: `tmp/upstreamer/upstream/packages/agent/`
- This repo: `*.go` at the repo root
- Last ported commit: `.upstreamer/state.yaml`
- Converter's run log: `.upstreamer/logs/` (read last, and skeptically)

## Method

1. Read the contract's Required Public API list.
2. For each entry, find it in this repo **and** find its upstream counterpart.
   Compare behavior, not names.
3. Read the upstream delta yourself:
   ```bash
   git -C tmp/upstreamer/upstream diff <last>..<target> -- packages/agent/src
   ```
   For each behavioral change, verify the port reflects it. A missing edge case
   is a real finding.
4. Run the test suite and read what it actually asserts. Tests that assert the
   port's own shape rather than upstream behavior are not parity coverage.
5. Prefer reading upstream tests: they encode the behavior contract most
   precisely. Check the port covers the same cases.

## Required Qualities

**Public API completeness.** Every symbol in the contract's Required Public API
list is present, exported, and reachable from the package's public entry point.

**Version honesty.** The port's declared/recorded upstream version matches what
was actually ported. A port claiming 0.8.0 while missing `HooksManager` is a FAIL,
not a warning.

**The load-bearing loop.** Tool execution, multi-turn continuation, and stop
conditions behave as upstream: correct request sequence, accumulated input and
history preserved across turns, `previous_response_id` carried forward.

**Streaming.** Event order and turn boundaries match. Every consumer sees stream
and transport errors — no consumer hangs or ends silently. Multi-consumer fan-out
works.

**State.** Serialization round-trips. The version constant exists and a version
mismatch raises/returns an error rather than silently accepting a foreign blob.
State survives a pause/resume cycle mid-approval.

**Approval / HITL ordering.** Mixed turns are the classic bug: when one turn has
both auto-executable and approval-required calls, auto-executable outputs are
recorded *before* the pause and replayed with the decisions. Regular-tool output
produced before a HITL pause is not lost. Resume order is `function_call` then
`function_call_output`.

**Hooks.** Lifecycle hooks fire at the right points. Session id is threaded
per-emit so a shared manager is concurrency-safe. `SessionEnd` and drain happen
even on no-tools stream error paths.

**Compatibility helpers.** Claude/Chat conversion round-trips preserve metadata,
reasoning, tool use, and unsupported content.

**Test parity.** You have the upstream tree; the mechanical verifier does not, so
this comparison is yours to make and it is the one gate that can catch an
untested behavior change.

```bash
ls tmp/upstreamer/upstream/packages/agent/tests/unit/
git -C tmp/upstreamer/upstream diff --stat <last>..<target> -- '*.test.ts'
```

For every behavior that changed in the delta, find upstream's test for it and
confirm this repo covers the same case. Report upstream cases with no Go
counterpart, and treat a changed behavior with no test as a finding — the port
compiled and passed its own suite, which is exactly the state a version-behind
port is in.

Do **not** grade on test counts. Upstream has ~46 test files and ~619 `it` blocks
against this repo's ~70 `Test` funcs, but Go table-driven subtests bundle many
cases into one function, so the ratio means nothing. Compare *cases covered*,
per behavior.

Also check the tests assert upstream-observable behavior — ordering, error
surfaces, stream boundaries, state shape — rather than the port's internal shape.
A test that merely pins current structure passes just as happily when the port is
wrong.

**Divergences are the documented ones.** Every difference from upstream is either
in the contract's Idiomatic Divergences section or recorded as a compatibility
note. An undocumented divergence is a finding.

**Repo-owned files intact.** CI, license, release config, and package identity
were not rewritten by the port.

## Verdict

Return `PASS`, `PASS WITH WARNINGS`, or `FAIL` with concrete findings — file,
symbol, and what specifically differs from upstream.

- `FAIL` — a required API symbol is missing, a behavioral parity gap exists in the
  load-bearing loop / state / approval ordering / hooks, the declared version
  overstates what was ported, or a behavior that changed in this delta landed
  with no test covering it.
- `PASS WITH WARNINGS` — parity holds on behavior; gaps are cosmetic, type-level,
  or already documented as divergences.
- `PASS` — no findings.

Be specific and be willing to fail. A false PASS is worse than no eval: it
advances `.upstreamer/state.yaml` and the next run skips past the gap, which is
exactly how a port silently falls a version behind.
