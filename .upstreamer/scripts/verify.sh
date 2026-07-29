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
  run "go test ./..."  go test ./...
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
for f in LICENSE README.md go.mod scripts/upstream; do
  [ -e "$f" ] && pass "$f present" || fail "$f missing (port must not delete repo-owned files)"
done
echo

if [ "$FAILURES" -eq 0 ]; then
  echo "=== PASS: 0 failures ==="
  exit 0
fi
echo "=== FAIL: $FAILURES failure(s) ==="
exit 1
