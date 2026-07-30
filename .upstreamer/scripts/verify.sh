#!/usr/bin/env bash
# Mechanical verification for the Go port. Objective checks only —
# judgment-heavy parity review lives in .upstreamer/eval.md.
set -uo pipefail
cd "$(dirname "$0")/../.."

FAILURES=0
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1"; FAILURES=$((FAILURES + 1)); }

run() {
  local label="$1"; shift
  if "$@" >/tmp/verify-out 2>&1; then
    pass "$label"
  else
    fail "$label"
    sed 's/^/        /' /tmp/verify-out | tail -40
  fi
}

echo "=== Verification: go-agent ==="
echo

echo "-- Toolchain"
if command -v go >/dev/null 2>&1; then
  unformatted=$(gofmt -l . 2>/dev/null | grep -v '^tmp/' || true)
  [ -z "$unformatted" ] && pass "gofmt clean" || fail "gofmt: $unformatted"
  run "go build ./..." go build ./...
  run "go vet ./..."   go vet ./...
  # -count=1 defeats the test cache: a cached "ok" is not evidence that the
  # tests pass against the tree as it stands right now.
  run "go test ./..."  go test -count=1 ./...
  # The race detector is not optional for this package. Its whole job is
  # concurrent stream fan-out plus a hooks manager documented as
  # concurrency-safe, and a data race there is a real bug that a plain
  # `go test` reports as a pass.
  run "go test -race ./..." go test -race -count=1 ./...
else
  fail "go not installed (required to build and test this module)"
fi
echo

# Required public API. This is the parity floor from .upstreamer/upstreamer.md.
# Presence only — the eval judges behavior. Uses `go doc` so it sees the real
# exported surface rather than grepping for source text.
echo "-- Required exported API"
REQUIRED_SYMBOLS=(
  CallModel NewOpenRouter NewTool MustNewTool NewServerTool ModelResult
  CreateInitialState AppendToMessages UpdateState PartitionToolCalls
  StepCountIs HasToolCall MaxTokensUsed MaxCost FinishReasonIs
  ToClaudeMessage FromClaudeMessages ToChatMessage FromChatMessages
  ExtractUnsupportedContent HasUnsupportedContent GetUnsupportedContentSummary
  ToolContextStore ToolEventBroadcaster
)
if command -v go >/dev/null 2>&1; then
  if surface=$(go doc -all . 2>/dev/null); then
    missing=""
    for sym in "${REQUIRED_SYMBOLS[@]}"; do
      grep -qE "\b(func|type|var|const)\b.*\b${sym}\b" <<<"$surface" || missing="$missing $sym"
    done
    [ -z "${missing// /}" ] && pass "all ${#REQUIRED_SYMBOLS[@]} required symbols exported" \
                            || fail "missing exported symbols:$missing"
  else
    fail "go doc failed — cannot verify exported API"
  fi
fi
echo

# Test coverage. Two gates, both deliberately self-contained: like every other
# check here they read only this Go tree, never the upstream checkout, which is
# what lets ci.yaml run this script in a job that does a plain `actions/checkout`
# with no upstream clone. Upstream-vs-port test comparison is judgment work and
# lives in .upstreamer/eval.md, which does have the upstream tree.
echo "-- Test coverage"
COVERAGE_FLOOR_FILE=".upstreamer/coverage-floor.txt"
if command -v go >/dev/null 2>&1; then
  cover_profile=$(mktemp)
  if go test -count=1 -coverprofile="$cover_profile" ./... >/tmp/verify-cover 2>&1; then
    total=$(go tool cover -func="$cover_profile" | awk '/^total:/ {gsub("%","",$NF); print $NF}')

    # Gate A: ratchet. Coverage may not fall below the committed floor, and a
    # meaningful gain must be locked in by raising the floor, so improvements
    # can't silently erode later.
    floor=$(tr -d '[:space:]' <"$COVERAGE_FLOOR_FILE" 2>/dev/null || echo "")
    if [ -z "$floor" ]; then
      fail "missing or empty $COVERAGE_FLOOR_FILE (needed for the coverage ratchet)"
    elif awk -v t="$total" -v f="$floor" 'BEGIN{exit !(t+0 < f+0)}'; then
      fail "coverage ${total}% is below the floor ${floor}% — add tests, do not lower the floor"
    elif awk -v t="$total" -v f="$floor" 'BEGIN{exit !(t+0 >= f+0 + 1.5)}'; then
      fail "coverage rose to ${total}% (floor ${floor}%) — raise the floor in $COVERAGE_FLOOR_FILE to lock it in"
    else
      pass "coverage ${total}% >= floor ${floor}%"
    fi

    # Gate B: every required-API symbol must be *exercised*, not merely exported.
    # The presence check above cannot see an exported symbol that no test ever
    # calls -- which is exactly how FinishReasonIs sat at 0% while passing.
    #
    # Types are checked through their constructor, since `go tool cover` reports
    # functions. Names are matched across every same-named function (Execute,
    # Push and friends repeat across types), passing if ANY occurrence is
    # covered.
    uncovered=$(go tool cover -func="$cover_profile" | awk '
      { pct=$NF; gsub("%","",pct); if (pct+0 > best[$2]+0) best[$2]=pct+0 }
      END {
        n=split("CallModel NewOpenRouter NewTool MustNewTool NewServerTool \
CreateInitialState AppendToMessages UpdateState PartitionToolCalls \
StepCountIs HasToolCall MaxTokensUsed MaxCost FinishReasonIs \
ToClaudeMessage FromClaudeMessages ToChatMessage FromChatMessages \
ExtractUnsupportedContent HasUnsupportedContent GetUnsupportedContentSummary \
NewToolContextStore NewToolEventBroadcaster", req, " ")
        for (i=1;i<=n;i++) if (best[req[i]]+0 == 0) printf " %s", req[i]
      }')
    if [ -z "${uncovered// /}" ]; then
      pass "every required-API symbol has test coverage"
    else
      fail "required-API symbols exported but never exercised by a test:$uncovered"
    fi
  else
    fail "coverage run failed"
    sed 's/^/        /' /tmp/verify-cover | tail -20
  fi
  rm -f "$cover_profile"
fi
echo

# Hooks and versioned state are the 0.8.0 parity floor. The 0.8.0 port chose
# the upstream spelling `HooksManager` verbatim, so this check pins that exact
# name; the contract's required-API list is the source of truth. If a future
# port renames it, that is a contract change — update the contract's naming map
# and this check together, not just the code.
echo "-- 0.8.0 parity surface present"
grep -rqE '\bHooksManager\b' --include='*.go' . 2>/dev/null \
  && pass "hooks manager present" \
  || fail "no HooksManager equivalent found (upstream #7/#67 not ported)"
grep -rqiE 'conversation_?state_?version|SerializeConversationState' --include='*.go' . 2>/dev/null \
  && pass "versioned state serialization present" \
  || fail "no versioned conversation-state serialization found (upstream #66 not ported)"
echo

echo "-- go-sdk substrate pin"
pinned=$(grep -m1 'OpenRouterTeam/go-sdk' go.mod | awk '{print $2}')
[ -n "$pinned" ] && pass "go-sdk pinned at $pinned" || fail "go-sdk not found in go.mod"

echo "-- service_tier workaround retained"
grep -rq 'service_tier\|ServiceTier' --include='*.go' . 2>/dev/null \
  && pass "service_tier pin retained" \
  || fail "service_tier=auto workaround missing (breaks real request serialization)"
echo

echo "-- No leaked TypeScript artifacts"
leaked=$(find . -path ./tmp -prune -o -type f \( -name '*.ts' -o -name '*.js' \
  -o -name 'package.json' -o -name 'tsconfig*.json' -o -name 'pnpm-lock.yaml' \) -print 2>/dev/null)
[ -z "$leaked" ] && pass "no TS/JS artifacts" || fail "leaked upstream artifacts: $leaked"

echo "-- Repo-owned files present"
# CI and the coverage floor are included deliberately: a port run that deleted
# its own gates would otherwise pass verification while removing the checks that
# make the next run trustworthy.
for f in LICENSE README.md go.mod scripts/upstream \
         .github/workflows/ci.yaml .upstreamer/coverage-floor.txt; do
  [ -e "$f" ] && pass "$f present" || fail "$f missing (port must not delete repo-owned files)"
done
echo

if [ "$FAILURES" -eq 0 ]; then
  echo "=== PASS: 0 failures ==="
  exit 0
fi
echo "=== FAIL: $FAILURES failure(s) ==="
exit 1
