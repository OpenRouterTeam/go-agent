package agent

// Unit tests for the doom-loop detection primitives (doom_loop.go), ported
// from upstream tests/unit/doom-loop.test.ts and the config/monitor halves
// of tests/unit/doom-loop-escalation.test.ts. Everything here is pure and
// deterministic: JCS canonicalization, SHA-256 fingerprints (asserted
// against upstream's cross-port vector file), text-repetition detection,
// ladder resolution, loop-key resolution, and the monitor state machine.

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"math"
	"math/big"
	"os"
	"regexp"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of fn and
// returns what was logged (the Go stand-in for upstream's console.warn spy).
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	}()
	fn()
	return buf.String()
}

func mustCanon(t *testing.T, v any) string {
	t.Helper()
	s, err := CanonicalizeKeyMaterial(v)
	if err != nil {
		t.Fatalf("canonicalize %#v: %v", v, err)
	}
	return s
}

func TestCanonicalizeKeyMaterialJCSSemantics(t *testing.T) {
	a := mustCanon(t, map[string]any{"b": 2.0, "a": map[string]any{"d": 4.0, "c": 3.0}})
	b := mustCanon(t, map[string]any{"a": map[string]any{"c": 3.0, "d": 4.0}, "b": 2.0})
	if a != b || a != `{"a":{"c":3,"d":4},"b":2}` {
		t.Fatalf("key order must not matter, recursively: %s vs %s", a, b)
	}
	if mustCanon(t, []any{1.0, 2.0}) == mustCanon(t, []any{2.0, 1.0}) {
		t.Fatal("array order is data")
	}
	// func entries are dropped from objects (JSON.stringify drops functions).
	if got := mustCanon(t, map[string]any{"a": 1, "f": func() {}}); got != `{"a":1}` {
		t.Fatalf("func entries must be dropped: %s", got)
	}
	cases := map[string]any{
		`"query"`:               "query",
		"42":                    42,
		"null":                  nil,
		"0":                     math.Copysign(0, -1),
		"1e+21":                 1e21,
		"1e-7":                  1e-7,
		"0.000001":              1e-6,
		"1.5":                   1.5,
		"-12.25":                -12.25,
		"123456789012345680000": 123456789012345678901.0,
		"true":                  true,
		`"a\"b\\c\n\u0001"`:     "a\"b\\c\n\x01",
		`"<&>"`:                 "<&>",    // no HTML escaping, unlike encoding/json
		"\"\u2028\"":            "\u2028", // JSON.stringify leaves U+2028 literal
	}
	for want, in := range cases {
		if got := mustCanon(t, in); got != want {
			t.Errorf("canonicalize(%#v) = %s, want %s", in, got, want)
		}
	}
	// Keys sort by UTF-16 code units: U+FF61 (one unit, 0xFF61) sorts AFTER
	// U+1F600 (surrogate pair starting 0xD83D), the reverse of UTF-8 byte order.
	if got := mustCanon(t, map[string]any{"\uff61": 1, "\U0001F600": 2}); got != "{\"\U0001F600\":2,\"\uff61\":1}" {
		t.Fatalf("keys must sort by UTF-16 code units: %s", got)
	}
}

func TestCanonicalizeKeyMaterialRejectsUnrepresentable(t *testing.T) {
	for name, v := range map[string]any{
		"NaN":      math.NaN(),
		"Infinity": math.Inf(1),
		"bigint":   map[string]any{"id": big.NewInt(10)},
	} {
		if _, err := CanonicalizeKeyMaterial(v); !errors.Is(err, ErrUncanonicalizable) {
			t.Errorf("%s: expected ErrUncanonicalizable, got %v", name, err)
		}
	}
	if _, err := CanonicalizeKeyMaterial(math.NaN()); err == nil || !strings.Contains(err.Error(), "non-finite") {
		t.Errorf("NaN error should name non-finite: %v", err)
	}
	circular := map[string]any{}
	circular["self"] = circular
	if _, err := CanonicalizeKeyMaterial(circular); err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("circular key material must error, got %v", err)
	}
	var deep any = "leaf"
	for i := 0; i < MaxCanonicalizeDepth+10; i++ {
		deep = []any{deep}
	}
	if _, err := CanonicalizeKeyMaterial(deep); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("over-deep key material must error, got %v", err)
	}
}

type vectorFile struct {
	ToolCallVectors []struct {
		Name        string `json:"name"`
		ToolName    string `json:"toolName"`
		KeyMaterial any    `json:"keyMaterial"`
		JCS         string `json:"jcs"`
		Fingerprint string `json:"fingerprint"`
	} `json:"toolCallVectors"`
	KeyMaterialVectors []struct {
		Name        string `json:"name"`
		KeyMaterial any    `json:"keyMaterial"`
		JCS         string `json:"jcs"`
		Fingerprint string `json:"fingerprint"`
	} `json:"keyMaterialVectors"`
}

// loneSurrogateWTF8 is U+D800 as WTF-8 bytes. encoding/json decodes the
// vector's "\ud800" escape to U+FFFD (Go strings are UTF-8), so the vector's
// lone-surrogate key material is rebuilt with the only representation a Go
// string can carry for it.
const loneSurrogateWTF8 = "\xed\xa0\x80"

func TestDoomLoopFingerprintCrossPortVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/doom-loop-fingerprints.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors vectorFile
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.ToolCallVectors) <= 5 {
		t.Fatalf("expected the upstream vector set, got %d", len(vectors.ToolCallVectors))
	}
	for _, v := range vectors.ToolCallVectors {
		key := v.KeyMaterial
		if strings.Contains(v.Name, "lone surrogate") {
			key = map[string]any{"s": loneSurrogateWTF8}
		}
		if got := mustCanon(t, key); got != v.JCS {
			t.Errorf("%s: jcs = %s, want %s", v.Name, got, v.JCS)
		}
		fp, err := FingerprintToolCall(v.ToolName, key)
		if err != nil || fp != v.Fingerprint {
			t.Errorf("%s: fingerprint = %s (%v), want %s", v.Name, fp, err, v.Fingerprint)
		}
	}
	for _, v := range vectors.KeyMaterialVectors {
		fp, err := FingerprintKeyMaterial(v.KeyMaterial)
		if err != nil || fp != v.Fingerprint {
			t.Errorf("%s: fingerprint = %s (%v), want %s", v.Name, fp, err, v.Fingerprint)
		}
	}
	fp, _ := FingerprintToolCall("t", map[string]any{})
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fp) {
		t.Fatalf("fingerprint must be 64-char lowercase hex: %s", fp)
	}
	a, _ := FingerprintToolCall("web_search", map[string]any{"q": "x"})
	b, _ := FingerprintToolCall("fetch_page", map[string]any{"q": "x"})
	if a == b {
		t.Fatal("tool name must participate in the fingerprint")
	}
	// The -0 vector's JSON can only spell 0; assert the -0 collapse directly.
	negZero, _ := FingerprintToolCall("calc", map[string]any{"value": math.Copysign(0, -1)})
	zero, _ := FingerprintToolCall("calc", map[string]any{"value": 0})
	if negZero != zero || zero != "2c0292f2f8e9099f2879c189b8bd0d61fdc24dea98f06ec1d96f17f8ccb86f7d" {
		t.Fatalf("-0 must fingerprint like 0: %s %s", negZero, zero)
	}
}

func TestResolveLoopKeyMaterialForms(t *testing.T) {
	args := map[string]any{"command": "ls", "cwd": "/tmp", "verbose": true}
	if r := ResolveLoopKeyMaterial(nil, args); r.Kind != LoopKeyResolutionKey || mustCanon(t, r.KeyMaterial) != mustCanon(t, args) {
		t.Fatalf("absent → full args, got %#v", r)
	}
	if r := ResolveLoopKeyMaterial(LoopKeyExempt(), args); r.Kind != LoopKeyResolutionExempt {
		t.Fatalf("exempt → exempt, got %#v", r)
	}
	r := ResolveLoopKeyMaterial(LoopKeyFields("command", "cwd", "not_a_field"), args)
	if r.Kind != LoopKeyResolutionKey || mustCanon(t, r.KeyMaterial) != `{"command":"ls","cwd":"/tmp"}` {
		t.Fatalf("field list → subset, got %#v", r)
	}
	var protoArgs map[string]any
	_ = json.Unmarshal([]byte(`{"__proto__":"declared"}`), &protoArgs)
	if r := ResolveLoopKeyMaterial(LoopKeyFields("__proto__"), protoArgs); mustCanon(t, r.KeyMaterial) != `{"__proto__":"declared"}` {
		t.Fatalf("__proto__ is ordinary data, got %#v", r)
	}
	type bashArgs struct {
		Command string `json:"command"`
	}
	r = ResolveLoopKeyMaterial(LoopKeyFunc(func(in bashArgs) (any, error) { return strings.TrimSpace(in.Command), nil }), args)
	if r.Kind != LoopKeyResolutionKey || r.KeyMaterial != "ls" {
		t.Fatalf("function value → key, got %#v", r)
	}
	if r := ResolveLoopKeyMaterial(&ToolLoopKey{Func: func(map[string]any) (any, error) { return nil, nil }}, args); r.Kind != LoopKeyResolutionExempt {
		t.Fatalf("function nil → per-call exemption, got %#v", r)
	}
	r = ResolveLoopKeyMaterial(&ToolLoopKey{Func: func(map[string]any) (any, error) { return nil, errors.New("loopKey bug") }}, args)
	if r.Kind != LoopKeyResolutionFallback || !strings.Contains(r.Warning, "threw (loopKey bug)") || mustCanon(t, r.KeyMaterial) != mustCanon(t, args) {
		t.Fatalf("erroring function → fallback to full args, got %#v", r)
	}
	r = ResolveLoopKeyMaterial(&ToolLoopKey{Func: func(map[string]any) (any, error) { panic("boom") }}, args)
	if r.Kind != LoopKeyResolutionFallback || !strings.Contains(r.Warning, "threw") {
		t.Fatalf("panicking function → fallback, got %#v", r)
	}
	if r := ResolveLoopKeyMaterial(&ToolLoopKey{}, args); r.Kind != LoopKeyResolutionFallback {
		t.Fatalf("empty declaration → fallback, got %#v", r)
	}
}

func TestDetectTextRepetition(t *testing.T) {
	r := DetectTextRepetition("no no no no no no no no no no no no", DoomLoopTextOptions{})
	if r == nil || r.PeriodTokens != 1 || r.Repeats != 12 || r.Sample != "no" {
		t.Fatalf("single repeated token: %#v", r)
	}
	phrase := "I am stuck in a loop."
	r = DetectTextRepetition(strings.TrimSpace(strings.Repeat(phrase+" ", 6)), DoomLoopTextOptions{})
	if r == nil || r.PeriodTokens != 6 || r.Repeats != 6 || r.Sample != phrase {
		t.Fatalf("repeating phrase: %#v", r)
	}
	looping := strings.TrimSpace(strings.Repeat("retry now ", 6))
	if r := DetectTextRepetition(looping+" but then I found the actual answer to your question about databases", DoomLoopTextOptions{}); r != nil {
		t.Fatalf("recovered output must not fire: %#v", r)
	}
	if r := DetectTextRepetition("The quick brown fox jumps over the lazy dog and then runs far away into the woods.", DoomLoopTextOptions{}); r != nil {
		t.Fatalf("prose must not fire: %#v", r)
	}
	if r := DetectTextRepetition("this is very very very good and quite long enough overall", DoomLoopTextOptions{}); r != nil {
		t.Fatalf("below threshold must not fire: %#v", r)
	}
	paraphrased := "I am unable to find the file. I cannot locate the file. I am not able to find that file. The file cannot be found by me. Finding the file is not possible. I could not locate that file."
	if r := DetectTextRepetition(paraphrased, DoomLoopTextOptions{}); r != nil {
		t.Fatalf("DOCUMENTED MISS: paraphrase must not fire: %#v", r)
	}
	text := strings.TrimSpace(strings.Repeat("loop detected ", 8))
	if *DetectTextRepetition(text, DoomLoopTextOptions{}) != *DetectTextRepetition(text, DoomLoopTextOptions{}) {
		t.Fatal("must be deterministic")
	}
	var filler strings.Builder
	for i := 0; i < 400_000; i++ {
		filler.WriteString("w")
		filler.WriteString(strings.Repeat("x", i%3))
		filler.WriteString(" ")
	}
	r = DetectTextRepetition(filler.String()+strings.TrimSpace(strings.Repeat("stuck ", 20)), DoomLoopTextOptions{})
	if r == nil || r.PeriodTokens != 1 || r.Repeats != 20 {
		t.Fatalf("multi-megabyte input tail: %#v", r)
	}
	if r := DetectTextRepetition("ha ha ha", DoomLoopTextOptions{}); r != nil {
		t.Fatalf("defaults must not fire on 3 repeats: %#v", r)
	}
	if r := DetectTextRepetition("ha ha ha", DoomLoopTextOptions{MinRepeats: 3, MinCoveredTokens: 3}); r == nil || r.Repeats != 3 || r.PeriodTokens != 1 {
		t.Fatalf("custom thresholds: %#v", r)
	}
}

func TestResolveLadderAction(t *testing.T) {
	ladder := DoomLoopLadder{Observe: 2, Steer: DoomLoopRungDisabled, Escalate: DoomLoopRungDisabled, Block: 3, Stop: 6}
	want := map[int]DoomLoopAction{1: "", 2: DoomLoopActionObserve, 3: DoomLoopActionBlock, 5: DoomLoopActionBlock, 6: DoomLoopActionStop}
	for streak, action := range want {
		if got := ResolveLadderAction(ladder, streak, ResolveLadderOptions{}); got != action {
			t.Errorf("streak %d = %q, want %q", streak, got, action)
		}
	}
	if got := ResolveLadderAction(ladder, 3, ResolveLadderOptions{NonBlocking: true}); got != DoomLoopActionObserve {
		t.Errorf("non-blockable streak 3 = %q", got)
	}
	if got := ResolveLadderAction(ladder, 6, ResolveLadderOptions{NonBlocking: true}); got != DoomLoopActionStop {
		t.Errorf("non-blockable streak 6 = %q", got)
	}
	disabled := DoomLoopLadder{Observe: DoomLoopRungDisabled, Steer: DoomLoopRungDisabled, Escalate: DoomLoopRungDisabled, Block: DoomLoopRungDisabled, Stop: 4}
	if got := ResolveLadderAction(disabled, 3, ResolveLadderOptions{}); got != "" {
		t.Errorf("disabled rungs = %q", got)
	}
	esc := DoomLoopLadder{Observe: 1, Steer: DoomLoopRungDisabled, Escalate: 2, Block: 5, Stop: 8}
	if got := ResolveLadderAction(esc, 2, ResolveLadderOptions{}); got != DoomLoopActionEscalate {
		t.Errorf("escalate rung = %q", got)
	}
	if got := ResolveLadderAction(esc, 2, ResolveLadderOptions{NoEscalate: true}); got != DoomLoopActionObserve {
		t.Errorf("escalate unavailable falls through = %q", got)
	}
}

func TestResolveDoomLoopOption(t *testing.T) {
	if ResolveDoomLoopOption(nil) != nil {
		t.Fatal("nil option = detection off")
	}
	var resolved *ResolvedDoomLoopConfig
	logs := captureLog(t, func() { resolved = ResolveDoomLoopOption(&DoomLoopConfig{}) })
	if resolved.Ladder != DefaultDoomLoopLadder() || !resolved.Text.Enabled || resolved.Escalation != nil {
		t.Fatalf("zero config = defaults, got %#v", resolved)
	}
	if logs != "" {
		t.Fatalf("default ladder must not warn: %q", logs)
	}
	resolved = ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Block: 5, Stop: 0}})
	if resolved.Ladder.Block != 5 || resolved.Ladder.Stop != 6 || resolved.Ladder.Observe != 2 {
		t.Fatalf("partial ladder merge: %#v", resolved.Ladder)
	}
	if ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{}}).Escalation != nil {
		t.Fatal("an escalation config with no mechanism (upstream advisor:false) is not a mechanism")
	}
	r := ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Model: "stronger-model"}})
	if r.Escalation == nil || r.Escalation.Model != "stronger-model" || r.Escalation.hasAdvisor() || r.Escalation.MaxEscalations != DefaultMaxEscalations {
		t.Fatalf("model-only mechanism: %#v", r.Escalation)
	}
	if r := ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Advisor: true}}); r.Escalation == nil || !r.Escalation.Advisor {
		t.Fatal("advisor true IS a mechanism")
	}
	if ResolveDoomLoopOption(&DoomLoopConfig{Text: DoomLoopTextOptions{Disabled: true}}).Text.Enabled {
		t.Fatal("text disabled")
	}
	logs = captureLog(t, func() {
		ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Block: 3, Stop: DoomLoopRungDisabled}})
	})
	if !strings.Contains(logs, "stop disabled") {
		t.Fatalf("block without stop must warn: %q", logs)
	}
	logs = captureLog(t, func() { ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 5, Block: 2}}) })
	if !strings.Contains(logs, `"observe" (5) can never fire`) {
		t.Fatalf("dead rung must warn: %q", logs)
	}
	// Escalation config resolution (doom-loop-escalation.test.ts).
	r = ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Escalate: 3}, Escalation: &DoomLoopEscalationConfig{Model: "openai/gpt-5"}})
	if r.Escalation.MaxEscalations != 2 || r.Ladder.Escalate != 3 {
		t.Fatalf("default budget: %#v", r)
	}
	logs = captureLog(t, func() { r = ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Escalate: 3}}) })
	if r.Escalation != nil || !strings.Contains(logs, "no escalation config") {
		t.Fatalf("rung without mechanism must warn and resolve nil: %q", logs)
	}
	logs = captureLog(t, func() {
		r = ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Model: "openai/gpt-5"}})
	})
	if r.Escalation == nil || r.Ladder.Escalate != DoomLoopRungDisabled || !strings.Contains(logs, "escalate ladder rung is disabled") {
		t.Fatalf("mechanism without rung must warn and keep the mechanism: %q %#v", logs, r)
	}
	r = ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Escalate: 2}, Escalation: &DoomLoopEscalationConfig{Model: "openai/gpt-5", MaxEscalations: 0}})
	if r.Escalation.MaxEscalations != 2 {
		t.Fatalf("invalid budget clamps to default: %d", r.Escalation.MaxEscalations)
	}
}

func makeTestMonitor(t *testing.T, cfg *DoomLoopConfig) *DoomLoopMonitor {
	t.Helper()
	if cfg == nil {
		cfg = &DoomLoopConfig{}
	}
	return NewDoomLoopMonitor(*ResolveDoomLoopOption(cfg), nil)
}

func record(t *testing.T, m *DoomLoopMonitor, tool string, key any, round int) DoomLoopCallRecord {
	t.Helper()
	r, err := m.RecordToolCall(tool, key, round, RecordToolCallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDoomLoopMonitorToolStreaks(t *testing.T) {
	m := makeTestMonitor(t, nil)
	args := map[string]any{"query": "same thing"}
	if v := record(t, m, "search", args, 1).Verdict; v != nil {
		t.Fatalf("streak 1 has no verdict: %#v", v)
	}
	v := record(t, m, "search", args, 2).Verdict
	if v == nil || v.Action != DoomLoopActionObserve || v.Streak != 2 || v.Detector != DoomLoopDetectorToolFingerprint || v.ToolName != "search" {
		t.Fatalf("streak 2: %#v", v)
	}
	if v := record(t, m, "search", args, 3).Verdict; v == nil || v.Action != DoomLoopActionBlock || v.Streak != 3 {
		t.Fatalf("streak 3: %#v", v)
	}
	if !strings.Contains(v.Message, `tool "search" was invoked in 2 consecutive rounds`) {
		t.Fatalf("message: %s", v.Message)
	}

	m = makeTestMonitor(t, nil)
	var last DoomLoopCallRecord
	for round := 1; round <= 6; round++ {
		last = record(t, m, "search", map[string]any{"q": "x"}, round)
	}
	if last.Verdict == nil || last.Verdict.Action != DoomLoopActionStop || last.Verdict.Streak != 6 {
		t.Fatalf("stop at 6: %#v", last.Verdict)
	}

	// Same-round duplicates: one increment, no verdicts.
	m = makeTestMonitor(t, nil)
	if r := record(t, m, "search", map[string]any{"q": "parallel"}, 1); r.Streak != 1 || r.DuplicateInRound {
		t.Fatalf("first: %#v", r)
	}
	for i := 0; i < 5; i++ {
		if r := record(t, m, "search", map[string]any{"q": "parallel"}, 1); r.Streak != 1 || !r.DuplicateInRound || r.Verdict != nil {
			t.Fatalf("dup %d: %#v", i, r)
		}
	}
	if r := record(t, m, "search", map[string]any{"q": "parallel"}, 2); r.Streak != 2 || r.DuplicateInRound {
		t.Fatalf("next round: %#v", r)
	}

	// Different args reset; interleaved other tools do not.
	m = makeTestMonitor(t, nil)
	record(t, m, "search", map[string]any{"q": "a"}, 1)
	if r := record(t, m, "search", map[string]any{"q": "b"}, 2); r.Streak != 1 {
		t.Fatalf("different args must reset: %#v", r)
	}
	m = makeTestMonitor(t, nil)
	search := map[string]any{"q": "x"}
	record(t, m, "search", search, 1)
	record(t, m, "read", map[string]any{"path": "a"}, 2)
	record(t, m, "search", search, 3)
	record(t, m, "read", map[string]any{"path": "b"}, 4)
	if v := record(t, m, "search", search, 5).Verdict; v == nil || v.Action != DoomLoopActionBlock || v.Streak != 3 {
		t.Fatalf("interleaved other tools must not reset: %#v", v)
	}

	// Key order and empty calls.
	m = makeTestMonitor(t, nil)
	record(t, m, "search", map[string]any{"a": 1, "b": 2}, 1)
	if r := record(t, m, "search", map[string]any{"b": 2, "a": 1}, 2); r.Streak != 2 {
		t.Fatalf("key order must not defeat detection: %#v", r)
	}
	m = makeTestMonitor(t, nil)
	record(t, m, "list_tasks", map[string]any{}, 1)
	record(t, m, "list_tasks", map[string]any{}, 2)
	if v := record(t, m, "list_tasks", map[string]any{}, 3).Verdict; v == nil || v.Action != DoomLoopActionBlock {
		t.Fatalf("repeated empty calls must trip: %#v", v)
	}

	// Unhashable key material propagates for the engine fallback.
	if _, err := m.RecordToolCall("search", map[string]any{"id": big.NewInt(1)}, 9, RecordToolCallOptions{}); !errors.Is(err, ErrUncanonicalizable) {
		t.Fatalf("bigint must propagate: %v", err)
	}

	// Server-tool records: non-blockable, server detector label.
	m = makeTestMonitor(t, nil)
	var rec DoomLoopCallRecord
	for round := 1; round <= 3; round++ {
		rec, _ = m.RecordToolCall("server:web_search_call", map[string]any{"q": "x"}, round, RecordToolCallOptions{NonBlocking: true, Detector: DoomLoopDetectorServerToolFingerprint})
	}
	if rec.Verdict == nil || rec.Verdict.Action != DoomLoopActionObserve || rec.Verdict.Detector != DoomLoopDetectorServerToolFingerprint {
		t.Fatalf("server-tool verdict: %#v", rec.Verdict)
	}

	// Determinism: two monitors fed the same sequence agree.
	run := func() []DoomLoopCallRecord {
		mon := makeTestMonitor(t, nil)
		var out []DoomLoopCallRecord
		for i, k := range []string{"a", "a", "b", "a", "a", "a"} {
			out = append(out, record(t, mon, "t", map[string]any{"k": k}, i+1))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i].Fingerprint != b[i].Fingerprint || a[i].Streak != b[i].Streak || (a[i].Verdict == nil) != (b[i].Verdict == nil) {
			t.Fatalf("nondeterministic at %d: %#v vs %#v", i, a[i], b[i])
		}
	}
}

func TestDoomLoopMonitorText(t *testing.T) {
	m := makeTestMonitor(t, nil)
	v, _ := m.RecordAssistantText(strings.TrimSpace(strings.Repeat("I am stuck. ", 20)))
	if v == nil || v.Detector != DoomLoopDetectorTextRepetition || v.Action != DoomLoopActionStop || v.Streak != 20 {
		t.Fatalf("within-response stop: %#v", v)
	}

	m = makeTestMonitor(t, nil)
	say := func(s string) *DoomLoopVerdict {
		v, err := m.RecordAssistantText(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := say("Let me try that again."); v != nil {
		t.Fatalf("first text: %#v", v)
	}
	if v := say("Let me try that again."); v == nil || v.Detector != DoomLoopDetectorTextStreak || v.Action != DoomLoopActionObserve || v.Streak != 2 {
		t.Fatalf("streak 2: %#v", v)
	}
	if v := say("Let me try that again."); v == nil || v.Action != DoomLoopActionObserve || v.Streak != 3 {
		t.Fatalf("block downgrades to observe for text: %#v", v)
	}
	say("Let me try that again.")
	say("Let me try that again.")
	if v := say("Let me try that again."); v == nil || v.Action != DoomLoopActionStop || v.Streak != 6 {
		t.Fatalf("text stop at 6: %#v", v)
	}

	m = makeTestMonitor(t, nil)
	say("Retrying   the\nsame plan.")
	if v := say("Retrying the same plan."); v == nil || v.Streak != 2 {
		t.Fatalf("whitespace-only differences: %#v", v)
	}

	m = makeTestMonitor(t, nil)
	for _, s := range []string{"I will try again.", "Let me attempt that once more.", "Trying one more time.", "Once more, then."} {
		if v := say(s); v != nil {
			t.Fatalf("DOCUMENTED MISS: paraphrase must not streak: %#v", v)
		}
	}

	m = makeTestMonitor(t, nil)
	say("same")
	say("")
	say("   ")
	if v := say("same"); v == nil || v.Streak != 2 {
		t.Fatalf("empty text neither counts nor resets: %#v", v)
	}

	m = makeTestMonitor(t, &DoomLoopConfig{Text: DoomLoopTextOptions{Disabled: true}})
	if v := say(strings.Repeat("loop ", 30)); v != nil {
		t.Fatalf("text disabled: %#v", v)
	}
}

func TestDoomLoopMonitorStateRoundTrip(t *testing.T) {
	m := makeTestMonitor(t, nil)
	record(t, m, "search", map[string]any{"q": "x"}, 1)
	record(t, m, "search", map[string]any{"q": "x"}, 2)
	blob, err := json.Marshal(m.State())
	if err != nil {
		t.Fatal(err)
	}
	var restored DoomLoopSerializedState
	if err := json.Unmarshal(blob, &restored); err != nil {
		t.Fatal(err)
	}
	resumed := NewDoomLoopMonitor(*ResolveDoomLoopOption(&DoomLoopConfig{}), &restored)
	// Rounds restart at 1 after a resume; the first resumed record still increments.
	if v := record(t, resumed, "search", map[string]any{"q": "x"}, 1).Verdict; v == nil || v.Action != DoomLoopActionBlock || v.Streak != 3 {
		t.Fatalf("resumed streak: %#v", v)
	}

	// Corrupt persisted state is ignored with a warning, never an error.
	var corrupt DoomLoopSerializedState
	logs := captureLog(t, func() {
		if err := json.Unmarshal([]byte(`"not an object"`), &corrupt); err != nil {
			t.Fatalf("corrupt blob must not error: %v", err)
		}
	})
	if !strings.Contains(logs, "Ignoring invalid persisted doom-loop state") {
		t.Fatalf("corrupt blob must warn: %q", logs)
	}
	fresh := NewDoomLoopMonitor(*ResolveDoomLoopOption(&DoomLoopConfig{}), &corrupt)
	if v := record(t, fresh, "search", map[string]any{"q": "x"}, 1).Verdict; v != nil {
		t.Fatalf("fresh state after corrupt blob: %#v", v)
	}
	var partial DoomLoopSerializedState
	_ = json.Unmarshal([]byte(`{"tools":{"ok":{"fingerprint":"f","streak":2},"bad":{"streak":"x"},"worse":7},"text":5,"pendingSteer":["a",3],"escalationsUsed":-1}`), &partial)
	if len(partial.Tools) != 1 || partial.Text != nil || len(partial.PendingSteer) != 1 || partial.EscalationsUsed != 0 {
		t.Fatalf("invalid entries must be dropped individually: %#v", partial)
	}

	// A tool named "__proto__" is ordinary data and cannot pollute other tools.
	var hostile DoomLoopSerializedState
	_ = json.Unmarshal([]byte(`{"tools":{"__proto__":{"fingerprint":"seeded","streak":5}}}`), &hostile)
	h := NewDoomLoopMonitor(*ResolveDoomLoopOption(&DoomLoopConfig{}), &hostile)
	if r := record(t, h, "unseen", map[string]any{}, 1); r.Streak != 1 || r.Verdict != nil {
		t.Fatalf("hostile name must not leak into other tools: %#v", r)
	}
	p := makeTestMonitor(t, nil)
	record(t, p, "__proto__", map[string]any{}, 1)
	if r := record(t, p, "__proto__", map[string]any{}, 2); r.Streak != 2 {
		t.Fatalf("__proto__ tool tracked as data: %#v", r)
	}
	if _, ok := p.State().Tools["__proto__"]; !ok {
		t.Fatal("__proto__ must appear in the snapshot")
	}

	// State returns a snapshot, not a live reference.
	s := makeTestMonitor(t, nil)
	record(t, s, "a", map[string]any{}, 1)
	snap := s.State()
	record(t, s, "a", map[string]any{}, 2)
	if snap.Tools["a"].Streak != 1 || s.State().Tools["a"].Streak != 2 {
		t.Fatal("State must be a snapshot")
	}
}

func TestDoomLoopMonitorEscalationBudget(t *testing.T) {
	cfg := func(maxEscalations int) *DoomLoopConfig {
		return &DoomLoopConfig{
			Ladder:     DoomLoopLadder{Observe: 1, Steer: DoomLoopRungDisabled, Escalate: 2, Block: 5, Stop: 8},
			Escalation: &DoomLoopEscalationConfig{Model: "openai/gpt-5", MaxEscalations: maxEscalations},
		}
	}
	m := makeTestMonitor(t, cfg(2))
	args := map[string]any{"q": "same"}
	if v := record(t, m, "search", args, 1).Verdict; v == nil || v.Action != DoomLoopActionObserve || v.Streak != 1 {
		t.Fatalf("streak 1: %#v", v)
	}
	if v := record(t, m, "search", args, 2).Verdict; v == nil || v.Action != DoomLoopActionEscalate {
		t.Fatalf("streak 2 escalates: %#v", v)
	}
	if v := record(t, m, "search", args, 3).Verdict; v == nil || v.Action != DoomLoopActionEscalate {
		t.Fatalf("budget unconsumed keeps escalating: %#v", v)
	}

	m = makeTestMonitor(t, cfg(1))
	record(t, m, "search", args, 1)
	record(t, m, "search", args, 2)
	m.ConsumeEscalation()
	if m.CanEscalate() {
		t.Fatal("budget of 1 must be exhausted")
	}
	if v := record(t, m, "search", args, 3).Verdict; v == nil || v.Action != DoomLoopActionObserve {
		t.Fatalf("exhausted budget falls through: %#v", v)
	}
	record(t, m, "search", args, 4)
	if v := record(t, m, "search", args, 5).Verdict; v == nil || v.Action != DoomLoopActionBlock {
		t.Fatalf("block unaffected by budget: %#v", v)
	}

	m = makeTestMonitor(t, cfg(2))
	m.ConsumeEscalation()
	blob, _ := json.Marshal(m.State())
	if !strings.Contains(string(blob), `"escalationsUsed":1`) {
		t.Fatalf("escalationsUsed must serialize: %s", blob)
	}
	var restored DoomLoopSerializedState
	_ = json.Unmarshal(blob, &restored)
	resumed := NewDoomLoopMonitor(*ResolveDoomLoopOption(cfg(2)), &restored)
	if !resumed.CanEscalate() {
		t.Fatal("one of two used")
	}
	resumed.ConsumeEscalation()
	if resumed.CanEscalate() {
		t.Fatal("budget must persist across resume")
	}
}

// Text normalization uses ECMAScript's \s set (upstream's regex), not
// unicode.IsSpace: U+FEFF separates tokens, U+0085 does not.
func TestDoomLoopTextUsesJSWhitespace(t *testing.T) {
	m := makeTestMonitor(t, &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 1}})
	v, err := m.RecordAssistantText("a\ufeffb\u3000 c")
	if err != nil || v == nil {
		t.Fatalf("observe@1 must fire: %v %v", v, err)
	}
	if want, _ := FingerprintKeyMaterial("a b c"); v.Fingerprint != want {
		t.Fatal("U+FEFF and U+3000 must normalize to single spaces like JS \\s")
	}
	v, _ = m.RecordAssistantText("a\u0085b")
	if want, _ := FingerprintKeyMaterial("a\u0085b"); v.Fingerprint != want {
		t.Fatal("U+0085 is not JS whitespace and must be kept")
	}
	if r := DetectTextRepetition(strings.Repeat("no\ufeff", 12), DoomLoopTextOptions{}); r == nil || r.Repeats != 12 {
		t.Fatalf("U+FEFF must split tokens: %#v", r)
	}
}
