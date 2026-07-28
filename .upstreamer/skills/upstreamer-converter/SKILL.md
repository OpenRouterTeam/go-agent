---
name: upstreamer-converter
description: Port an upstream source repository into this repository following an upstreamer.md contract. Use when running scripts/upstream, syncing this port with upstream, or reconciling the ported public API surface against the upstream reference.
---

# Upstreamer Converter (source port)

Port an upstream repository into this repository following the contract at
`.upstreamer/upstreamer.md`. The contract is the source of truth; this skill
supplies execution discipline.

Adapted from `mountgram/upstreamer` (MIT). Key difference: upstream's converter
generates a fresh downstream tree from scratch each time. This one maintains a
**living port of a versioned SDK** — the repo already exists, has consumers, and
publishes to a package registry. Incremental correctness matters more than
regeneration.

## Step 0: Read the contract

Read `.upstreamer/upstreamer.md` completely before touching anything. Parse the
frontmatter (`upstream`, `model`). Treat every section as binding: scope,
required public API, naming maps, idiomatic divergences, substrate pins, output
shape, verification. If this skill conflicts with the contract, follow the
contract and note the conflict in the final report.

## Step 1: Establish the delta

1. The upstream checkout is already at the target commit. Do not re-clone or
   change its checkout.
2. Read `upstream_commit` from `.upstreamer/state.yaml`. This is the last commit
   successfully ported *and* verified *and* eval-passed.
3. If a last commit exists and force is 0:
   ```bash
   git -C tmp/upstreamer/upstream diff --name-status <last>..<target>
   git -C tmp/upstreamer/upstream log --oneline <last>..<target>
   ```
   Scope work to changed files plus their consequences in this repo. Do not
   refactor unrelated ported code — a large unreviewable diff is a failed port
   even if it is correct.
4. If no last commit exists, or force is 1, reconcile the **entire** required
   public API list in the contract against this repo. Report every gap found.
5. Read the changed upstream files in full. Diffs alone hide behavioral intent;
   the surrounding code and its tests carry it.

## Step 2: Port, don't transliterate

For each upstream change:

1. Apply the contract's naming map. Never invent a mapping that the contract
   does not specify — if a new upstream symbol has no mapping, derive one that
   follows the documented convention and **list it in the final report** so it
   can be added to the contract.
2. Honor the contract's idiomatic divergences. These are deliberate and
   permanent. Do not "fix" them toward the TypeScript shape.
3. Preserve *observable behavior*: call ordering, error surfaces, stream event
   sequence and boundaries, state-shape compatibility, pause/resume semantics.
   These are the port's actual contract with users. Type-level convenience is
   secondary and the contract says where it is allowed to be looser.
4. Where the upstream behavior cannot be reproduced faithfully in this language,
   do not silently approximate. Implement the closest honest equivalent and
   record it as a compatibility note in `upstreamer-changelog.md`.
5. Never port build tooling, package manifests from upstream, CI, changesets, or
   generated output. The contract's drop list is authoritative.

## Step 3: Never clobber repo-owned files

This repo is not disposable generated output. These are owned by the repo and
must not be rewritten by a port run unless the contract explicitly says to:

- `.github/` — CI and release workflows
- `LICENSE`
- `.upstreamer/` — except `state.yaml`, `eval-report.md`, and `logs/`
- `scripts/upstream`
- Release/publish configuration and package identity (name, module path)

Package **version** and dependency pins change only where the contract's
substrate-pin section directs it.

## Step 4: Tests

Ported behavior without a test proves nothing. For every behavioral change:

1. Add or update deterministic tests in this repo's existing test layout and style.
2. Cover the specific upstream behavior that changed, not just the happy path.
   Upstream fixes are usually edge cases — that edge case is the test.
3. Tests must pass without network access or paid credentials. Live/e2e tests
   must skip cleanly when credentials are absent.

## Step 5: Mechanical verification

Run the verifier and fix what it reports:

```bash
.upstreamer/scripts/verify.sh
```

It checks objective facts: toolchain build, lint, type check, tests, required
public API symbols present, no upstream-language artifacts leaked, declared
version consistent with the ported upstream version. A verifier failure is never
acceptable to hand off.

## Step 6: Qualitative parity eval

After mechanical verification passes, run `.upstreamer/eval.md` in a **fresh
subagent or separate review context**. This matters: the converter cannot
usefully grade its own port. The evaluator reads the contract, the upstream
reference, and this repo directly, and returns `PASS`, `PASS WITH WARNINGS`, or
`FAIL` with concrete findings. Write the result to `.upstreamer/eval-report.md`.

`FAIL` is a blocker. Fix, re-run mechanical verification, re-run the eval. Up to
three focused attempts.

## Step 7: State, or bankruptcy

If the verifier passed and the eval returned `PASS` or `PASS WITH WARNINGS`,
write the target commit to `.upstreamer/state.yaml`:

```yaml
upstream_commit: <target-sha>
```

Otherwise **declare bankruptcy**:

1. Do not touch `.upstreamer/state.yaml`.
2. Write the failed eval result, what you attempted, the remaining blocker, and
   the recommended human next action to `.upstreamer/eval-report.md`.
3. Say clearly in the final report that the eval failed.

A stale state file is the correct outcome for a failed port. It is what makes the
next run retry the same delta instead of skipping past it. Never advance state to
make a run look successful.

## Final report

Begin with `Run summary`:

1. **Upstream changes since last run** — meaningful commits/files/behavior
   inspected. For a full reconciliation, say so and summarize the snapshot.
2. **Changes made here** — modules, public API, tests touched.
3. **Why these changes** — tie back to the contract, especially judgment calls.
4. **Verification** — commands run and results.
5. **Parity eval** — result and `eval-report.md` path.

Then: previous and target upstream commit; incremental vs full; any new naming
mappings you had to derive (so they can be added to the contract); any parity gap
left open and why; any place the contract was ambiguous or wrong.

Also update `upstreamer-changelog.md` at the repo root with user-facing
release-note bullets. That file is for users of this package: behavior changes,
new API, compatibility notes. Keep commit hashes, `state.yaml`, and verifier
internals out of it.
