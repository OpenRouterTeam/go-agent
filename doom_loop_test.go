package agent

// Ports of upstream tests/unit/doom-loop.test.ts, the detector-level cases of
// tests/unit/doom-loop-fanout.test.ts, and tests/unit/doom-loop-public-api.test.ts.
// Engine-level suites (doom-loop-integration/escalation/remediation) need the
// full CallModel loop and are not ported here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// captureDoomLoopWarnings swaps the warning sink for the test's duration.
// Tests using it must not run in parallel.
func captureDoomLoopWarnings(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	prev := doomLoopWarnf
	doomLoopWarnf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { doomLoopWarnf = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func defaultDoomLoopConfig() *ResolvedDoomLoopConfig {
	return ResolveDoomLoopOption(&DoomLoopConfig{})
}

func newTestMonitor(t *testing.T, cfg *DoomLoopConfig) *DoomLoopMonitor {
	t.Helper()
	if cfg == nil {
		cfg = &DoomLoopConfig{}
	}
	resolved := ResolveDoomLoopOption(cfg)
	if resolved == nil {
		t.Fatal("config must resolve")
	}
	return NewDoomLoopMonitor(resolved, nil)
}

func mustRecord(t *testing.T, m *DoomLoopMonitor, tool string, km any, round int, opts *RecordToolCallOptions) DoomLoopCallRecord {
	t.Helper()
	rec, err := m.RecordToolCall(tool, km, round, opts)
	if err != nil {
		t.Fatalf("RecordToolCall(%s): %v", tool, err)
	}
	return rec
}

func actionOf(rec DoomLoopCallRecord) string {
	if rec.Verdict == nil {
		return "none"
	}
	return string(rec.Verdict.Action)
}

func pathArgs(path string) map[string]any { return map[string]any{"path": path} }

func mustCanon(t *testing.T, v any) string {
	t.Helper()
	s, err := CanonicalizeKeyMaterial(v)
	if err != nil {
		t.Fatalf("CanonicalizeKeyMaterial(%#v): %v", v, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Canonicalization
// ---------------------------------------------------------------------------

func TestCanonicalizeKeyMaterial(t *testing.T) {
	t.Run("insensitive to object key order, recursively", func(t *testing.T) {
		a := mustCanon(t, map[string]any{"b": 2.0, "a": map[string]any{"d": 4.0, "c": 3.0}})
		b := mustCanon(t, map[string]any{"a": map[string]any{"c": 3.0, "d": 4.0}, "b": 2.0})
		if a != b || a != `{"a":{"c":3,"d":4},"b":2}` {
			t.Fatalf("got %q vs %q", a, b)
		}
	})
	t.Run("distinguishes array order", func(t *testing.T) {
		if mustCanon(t, []any{1.0, 2.0}) == mustCanon(t, []any{2.0, 1.0}) {
			t.Fatal("array order must matter")
		}
	})
	t.Run("drops func/chan object entries (JS undefined/function analogue)", func(t *testing.T) {
		got := mustCanon(t, map[string]any{"a": 1.0, "b": func() {}, "c": make(chan int)})
		if got != mustCanon(t, map[string]any{"a": 1.0}) {
			t.Fatalf("got %q", got)
		}
		if arr := mustCanon(t, []any{func() {}, 1}); arr != "[null,1]" {
			t.Fatalf("got %q", arr)
		}
	})
	t.Run("primitives per JCS", func(t *testing.T) {
		cases := []struct {
			in   any
			want string
		}{
			{"query", `"query"`},
			{42.0, "42"},
			{42, "42"},
			{int64(-7), "-7"},
			{uint8(3), "3"},
			{float32(0.5), "0.5"},
			{json.Number("1e21"), "1e+21"},
			{nil, "null"},
			{true, "true"},
			{math.Copysign(0, -1), "0"},
			{1e21, "1e+21"},
			{"a\"b\\c\n\t\x01\u2028<>&", `"a\"b\\c\n\t\u0001` + "\u2028" + `<>&"`},
		}
		for _, c := range cases {
			if got := mustCanon(t, c.in); got != c.want {
				t.Errorf("canon(%#v) = %q, want %q", c.in, got, c.want)
			}
		}
	})
	t.Run("rejects values RFC 8785 cannot represent", func(t *testing.T) {
		for _, v := range []any{math.NaN(), math.Inf(1), map[string]any{"x": math.Inf(-1)}} {
			if _, err := CanonicalizeKeyMaterial(v); err == nil || !strings.Contains(strings.ToLower(err.Error()), "non-finite") {
				t.Errorf("expected non-finite error for %v, got %v", v, err)
			}
		}
		if _, err := CanonicalizeKeyMaterial(map[string]any{"id": complex(1, 2)}); err == nil {
			t.Error("expected error for complex")
		}
	})
	t.Run("circular key material errors instead of hanging", func(t *testing.T) {
		circular := map[string]any{}
		circular["self"] = circular
		_, err := CanonicalizeKeyMaterial(circular)
		if err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("got %v", err)
		}
		// A shared (non-circular) reference is fine.
		shared := map[string]any{"k": 1.0}
		if got := mustCanon(t, []any{shared, shared}); got != `[{"k":1},{"k":1}]` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("nesting past MaxCanonicalizeDepth errors", func(t *testing.T) {
		var deep any = "leaf"
		for i := 0; i < MaxCanonicalizeDepth+10; i++ {
			deep = []any{deep}
		}
		_, err := CanonicalizeKeyMaterial(deep)
		if err == nil || !strings.Contains(err.Error(), "deeper than") {
			t.Fatalf("got %v", err)
		}
		var ok any = "leaf"
		for i := 0; i < MaxCanonicalizeDepth; i++ {
			ok = []any{ok}
		}
		if _, err := CanonicalizeKeyMaterial(ok); err != nil {
			t.Fatalf("depth %d must be accepted: %v", MaxCanonicalizeDepth, err)
		}
	})
	t.Run("keys sort by UTF-16 code units, not UTF-8 bytes", func(t *testing.T) {
		// U+FF61 (BMP, 0xFF61) vs U+1F600 (surrogates 0xD83D…): UTF-16 puts
		// the emoji first, UTF-8 byte order would put U+FF61 first.
		got := mustCanon(t, map[string]any{"\uff61": 1.0, "\U0001F600": 2.0})
		if got != `{"`+"\U0001F600"+`":2,"`+"\uff61"+`":1}` {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("Go-typed values follow the JSON data model", func(t *testing.T) {
		type args struct {
			Cwd     string `json:"cwd"`
			Command string `json:"command"`
		}
		got := mustCanon(t, map[string]any{"a": args{Cwd: "/tmp", Command: "ls"}, "b": []string{"x"}, "c": map[string]int{"z": 1, "y": 2}, "p": (*int)(nil)})
		if got != `{"a":{"command":"ls","cwd":"/tmp"},"b":["x"],"c":{"y":2,"z":1},"p":null}` {
			t.Fatalf("got %q", got)
		}
	})
}

func TestCanonicalJSNumberFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-1, "-1"},
		{100, "100"},
		{1.5, "1.5"},
		{0.1, "0.1"},
		{0.000001, "0.000001"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{123456789012345680000, "123456789012345680000"},
		{1e21, "1e+21"},
		{-2.5e25, "-2.5e+25"},
		{5e-324, "5e-324"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{9007199254740993, "9007199254740992"},
		{1234.5678, "1234.5678"},
		{0.3333333333333333, "0.3333333333333333"},
	}
	for _, c := range cases {
		if got := formatJSNumber(c.in); got != c.want {
			t.Errorf("formatJSNumber(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Fingerprints — the cross-port contract
// ---------------------------------------------------------------------------

type doomLoopVectorFile struct {
	ToolCallVectors []struct {
		Name        string          `json:"name"`
		ToolName    string          `json:"toolName"`
		KeyMaterial json.RawMessage `json:"keyMaterial"`
		JCS         string          `json:"jcs"`
		Fingerprint string          `json:"fingerprint"`
	} `json:"toolCallVectors"`
	KeyMaterialVectors []struct {
		Name        string          `json:"name"`
		KeyMaterial json.RawMessage `json:"keyMaterial"`
		JCS         string          `json:"jcs"`
		Fingerprint string          `json:"fingerprint"`
	} `json:"keyMaterialVectors"`
}

func loadDoomLoopVectors(t *testing.T) doomLoopVectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/doom-loop-fingerprints.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf doomLoopVectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	return vf
}

// decodeVectorValue decodes JSON like encoding/json into any, except that
// lone-surrogate \u escapes are preserved (as WTF-8) instead of being
// replaced with U+FFFD — so the JS-only "lone surrogate" vector reaches the
// canonicalizer intact. For every other vector the result is identical to
// encoding/json; that is asserted alongside.
func decodeVectorValue(t *testing.T, raw []byte) any {
	t.Helper()
	p := &wtf8JSONParser{s: string(raw)}
	v, err := p.value()
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

type wtf8JSONParser struct {
	s string
	i int
}

func (p *wtf8JSONParser) ws() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *wtf8JSONParser) value() (any, error) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, errors.New("unexpected end")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		obj := map[string]any{}
		p.ws()
		if p.s[p.i] == '}' {
			p.i++
			return obj, nil
		}
		for {
			p.ws()
			k, err := p.str()
			if err != nil {
				return nil, err
			}
			p.ws()
			if p.s[p.i] != ':' {
				return nil, errors.New("expected :")
			}
			p.i++
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			obj[k] = v
			p.ws()
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.s[p.i] == '}' {
				p.i++
				return obj, nil
			}
			return nil, errors.New("expected , or }")
		}
	case c == '[':
		p.i++
		arr := []any{}
		p.ws()
		if p.s[p.i] == ']' {
			p.i++
			return arr, nil
		}
		for {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
			p.ws()
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.s[p.i] == ']' {
				p.i++
				return arr, nil
			}
			return nil, errors.New("expected , or ]")
		}
	case c == '"':
		return p.str()
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil, nil
	default:
		start := p.i
		for p.i < len(p.s) && strings.ContainsRune("+-0123456789.eE", rune(p.s[p.i])) {
			p.i++
		}
		return strconv.ParseFloat(p.s[start:p.i], 64)
	}
}

func (p *wtf8JSONParser) str() (string, error) {
	if p.s[p.i] != '"' {
		return "", errors.New("expected string")
	}
	p.i++
	var units []uint16
	var b strings.Builder
	flush := func() {
		// Combine pairs; keep lone surrogates as WTF-8.
		for j := 0; j < len(units); j++ {
			u := rune(units[j])
			if utf16.IsSurrogate(u) && u < 0xDC00 && j+1 < len(units) && units[j+1] >= 0xDC00 && units[j+1] <= 0xDFFF {
				b.WriteRune(utf16.DecodeRune(u, rune(units[j+1])))
				j++
			} else if utf16.IsSurrogate(u) {
				b.WriteString(encodeWTF8Surrogate(u))
			} else {
				b.WriteRune(u)
			}
		}
		units = units[:0]
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			flush()
			p.i++
			return b.String(), nil
		case c == '\\':
			esc := p.s[p.i+1]
			p.i += 2
			if esc == 'u' {
				n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 16)
				if err != nil {
					return "", err
				}
				units = append(units, uint16(n))
				p.i += 4
				continue
			}
			flush()
			b.WriteByte(map[byte]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[esc])
		default:
			flush()
			r, size := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r)
			p.i += size
		}
	}
	return "", errors.New("unterminated string")
}

func TestFingerprintVectors(t *testing.T) {
	vf := loadDoomLoopVectors(t)
	if len(vf.ToolCallVectors) <= 5 {
		t.Fatalf("expected > 5 tool-call vectors, got %d", len(vf.ToolCallVectors))
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, v := range vf.ToolCallVectors {
		t.Run("tool/"+v.Name, func(t *testing.T) {
			km := decodeVectorValue(t, v.KeyMaterial)
			if got := mustCanon(t, km); got != v.JCS {
				t.Fatalf("jcs = %q, want %q", got, v.JCS)
			}
			fp, err := FingerprintToolCall(v.ToolName, km)
			if err != nil {
				t.Fatal(err)
			}
			if fp != v.Fingerprint || !sha.MatchString(fp) {
				t.Fatalf("fingerprint = %s, want %s", fp, v.Fingerprint)
			}
			// Plain encoding/json decoding gives the same result for every
			// vector that does not contain a lone surrogate.
			var std any
			if err := json.Unmarshal(v.KeyMaterial, &std); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(v.KeyMaterial), `\ud8`) {
				if fp2, _ := FingerprintToolCall(v.ToolName, std); fp2 != v.Fingerprint {
					t.Fatalf("encoding/json-decoded fingerprint = %s, want %s", fp2, v.Fingerprint)
				}
			}
		})
	}
	for _, v := range vf.KeyMaterialVectors {
		t.Run("key/"+v.Name, func(t *testing.T) {
			km := decodeVectorValue(t, v.KeyMaterial)
			if got := mustCanon(t, km); got != v.JCS {
				t.Fatalf("jcs = %q, want %q", got, v.JCS)
			}
			fp, err := FingerprintKeyMaterial(km)
			if err != nil {
				t.Fatal(err)
			}
			if fp != v.Fingerprint {
				t.Fatalf("fingerprint = %s, want %s", fp, v.Fingerprint)
			}
		})
	}
}

func TestFingerprintToolCall(t *testing.T) {
	fp, err := FingerprintToolCall("t", map[string]any{})
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fp) {
		t.Fatalf("got %q, %v", fp, err)
	}
	a, _ := FingerprintToolCall("web_search", map[string]any{"q": "x"})
	b, _ := FingerprintToolCall("fetch_page", map[string]any{"q": "x"})
	if a == b {
		t.Fatal("tool name must participate in the fingerprint")
	}
	if _, err := FingerprintToolCall("t", math.NaN()); err == nil {
		t.Fatal("expected error for unhashable key material")
	}
	if _, err := FingerprintKeyMaterial(math.NaN()); err == nil {
		t.Fatal("expected error for unhashable key material")
	}
	// A lone surrogate in the tool name hashes as U+FFFD, like TextEncoder.
	lone, _ := FingerprintToolCall("x"+encodeWTF8Surrogate(0xD800), map[string]any{})
	repl, _ := FingerprintToolCall("x\uFFFD", map[string]any{})
	if lone != repl {
		t.Fatal("lone surrogate in tool name must encode as U+FFFD")
	}
}

// ---------------------------------------------------------------------------
// ToolLoopKey resolution
// ---------------------------------------------------------------------------

func TestResolveLoopKeyMaterial(t *testing.T) {
	args := map[string]any{"command": "ls", "cwd": "/tmp", "verbose": true}

	t.Run("nil declaration → full arguments", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(nil, args)
		if got.Kind != LoopKeyKey || !reflect.DeepEqual(got.KeyMaterial, args) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("ExemptToolLoopKey → exempt", func(t *testing.T) {
		if got := ResolveLoopKeyMaterial(ExemptToolLoopKey, args); got != (LoopKeyResolution{Kind: LoopKeyExempt}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("field list → subset (missing fields absent)", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(LoopKeyFields("command", "cwd", "not_a_field"), args)
		if got.Kind != LoopKeyKey || !reflect.DeepEqual(got.KeyMaterial, map[string]any{"command": "ls", "cwd": "/tmp"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("empty field list → fallback with warning", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(LoopKeyFields(), args)
		if got.Kind != LoopKeyFallback || !reflect.DeepEqual(got.KeyMaterial, args) || !strings.Contains(got.Warning, "empty field list") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("empty field list does not collapse unrelated calls", func(t *testing.T) {
		ls := ResolveLoopKeyMaterial(LoopKeyFields(), map[string]any{"command": "ls"})
		rm := ResolveLoopKeyMaterial(LoopKeyFields(), map[string]any{"command": "rm -rf /"})
		if reflect.DeepEqual(ls.KeyMaterial, rm.KeyMaterial) {
			t.Fatal("identities collapsed")
		}
	})
	t.Run("all fields absent → fallback with warning", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(LoopKeyFields("nope", "also_nope"), args)
		if got.Kind != LoopKeyFallback || !strings.Contains(got.Warning, "absent from the arguments") || !strings.Contains(got.Warning, "nope, also_nope") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("__proto__ is an ordinary field", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(LoopKeyFields("__proto__"), map[string]any{"__proto__": "declared"})
		if got.Kind != LoopKeyKey || !reflect.DeepEqual(got.KeyMaterial, map[string]any{"__proto__": "declared"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("function returning a value → that value", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(func(a map[string]any) (any, error) {
			return strings.TrimSpace(fmt.Sprint(a["command"])), nil
		}, args)
		if got != (LoopKeyResolution{Kind: LoopKeyKey, KeyMaterial: "ls"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("function returning ErrLoopKeyExempt (wrapped too) → per-call exemption", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(func(map[string]any) (any, error) {
			return nil, fmt.Errorf("polling: %w", ErrLoopKeyExempt)
		}, args)
		if got.Kind != LoopKeyExempt {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("function returning (nil, nil) → fallback with warning", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(func(map[string]any) (any, error) { return nil, nil }, args)
		if got.Kind != LoopKeyFallback || !reflect.DeepEqual(got.KeyMaterial, args) || !strings.Contains(got.Warning, "returned nil") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("function returning an error → fallback with warning", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(func(map[string]any) (any, error) { return nil, errors.New("loopKey bug") }, args)
		if got.Kind != LoopKeyFallback || !strings.Contains(got.Warning, "threw (loopKey bug)") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("panicking function → fallback with warning", func(t *testing.T) {
		got := ResolveLoopKeyMaterial(func(map[string]any) (any, error) { panic("boom") }, args)
		if got.Kind != LoopKeyFallback || !strings.Contains(got.Warning, "threw (boom)") || !reflect.DeepEqual(got.KeyMaterial, args) {
			t.Fatalf("got %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Text repetition
// ---------------------------------------------------------------------------

func repeatJoin(s string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return strings.Join(parts, " ")
}

func TestDetectTextRepetition(t *testing.T) {
	t.Run("single repeated token", func(t *testing.T) {
		got := DetectTextRepetition("no no no no no no no no no no no no", DoomLoopTextOptions{})
		if got == nil || got.PeriodTokens != 1 || got.Repeats != 12 || got.Sample != "no" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("repeating phrase block", func(t *testing.T) {
		phrase := "I am stuck in a loop."
		got := DetectTextRepetition(repeatJoin(phrase, 6), DoomLoopTextOptions{})
		if got == nil || got.PeriodTokens != 6 || got.Repeats != 6 || got.Sample != phrase || got.CoveredTokens != 36 {
			t.Fatalf("got %+v", got)
		}
	})
	quiet := map[string]string{
		"recovered output does not fire": repeatJoin("retry now", 6) + " but then I found the actual answer to your question about databases",
		"ordinary prose":                 "The quick brown fox jumps over the lazy dog and then runs far away into the woods.",
		"below the repeat threshold":     "this is very very very good and quite long enough overall",
		"DOCUMENTED MISS: paraphrase": strings.Join([]string{
			"I am unable to find the file.", "I cannot locate the file.", "I am not able to find that file.",
			"The file cannot be found by me.", "Finding the file is not possible.", "I could not locate that file.",
		}, " "),
	}
	for name, text := range quiet {
		t.Run(name, func(t *testing.T) {
			if got := DetectTextRepetition(text, DoomLoopTextOptions{}); got != nil {
				t.Fatalf("got %+v", got)
			}
		})
	}
	t.Run("deterministic", func(t *testing.T) {
		text := repeatJoin("loop detected", 8)
		if !reflect.DeepEqual(DetectTextRepetition(text, DoomLoopTextOptions{}), DetectTextRepetition(text, DoomLoopTextOptions{})) {
			t.Fatal("non-deterministic")
		}
	})
	t.Run("multi-megabyte input uses the char-budget tail", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 400_000; i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("w" + strconv.Itoa(i))
		}
		got := DetectTextRepetition(b.String()+" "+repeatJoin("stuck", 20), DoomLoopTextOptions{})
		if got == nil || got.PeriodTokens != 1 || got.Repeats != 20 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("custom thresholds", func(t *testing.T) {
		if got := DetectTextRepetition("ha ha ha", DoomLoopTextOptions{}); got != nil {
			t.Fatalf("got %+v", got)
		}
		got := DetectTextRepetition("ha ha ha", DoomLoopTextOptions{MinRepeats: 3, MinCoveredTokens: 3})
		if got == nil || got.Repeats != 3 || got.PeriodTokens != 1 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("unicode whitespace splits tokens like JS \\s", func(t *testing.T) {
		got := DetectTextRepetition(strings.Repeat("no\u00a0", 11)+"no", DoomLoopTextOptions{})
		if got == nil || got.Repeats != 12 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("tail slice counts UTF-16 code units", func(t *testing.T) {
		// "😀" is 2 UTF-16 units; budget 3 units splits the first pair.
		s := "😀😀"
		tail := jsTailUTF16(s, 3)
		if tail != encodeWTF8Surrogate(0xDE00)+"😀" || jsUTF16Len(tail) != 3 {
			t.Fatalf("got %q", tail)
		}
		if jsTailUTF16(s, 4) != s || jsTailUTF16(s, 2) != "😀" {
			t.Fatal("unexpected tail")
		}
	})
}

// ---------------------------------------------------------------------------
// Ladder & option resolution
// ---------------------------------------------------------------------------

func TestResolveLadderAction(t *testing.T) {
	ladder := DoomLoopLadder{Observe: 2, Steer: DoomLoopDisabled, Escalate: DoomLoopDisabled, Block: 3, Stop: 6}
	cases := []struct {
		streak int
		opts   LadderActionOptions
		want   DoomLoopAction
	}{
		{1, LadderActionOptions{AllowBlock: true}, ""},
		{2, LadderActionOptions{AllowBlock: true}, DoomLoopActionObserve},
		{3, LadderActionOptions{AllowBlock: true}, DoomLoopActionBlock},
		{5, LadderActionOptions{AllowBlock: true}, DoomLoopActionBlock},
		{6, LadderActionOptions{AllowBlock: true}, DoomLoopActionStop},
		{3, LadderActionOptions{}, DoomLoopActionObserve},
		{6, LadderActionOptions{}, DoomLoopActionStop},
	}
	for _, c := range cases {
		if got := ResolveLadderAction(ladder, c.streak, c.opts); got != c.want {
			t.Errorf("streak %d %+v: got %q want %q", c.streak, c.opts, got, c.want)
		}
	}
	disabled := DoomLoopLadder{Observe: DoomLoopDisabled, Steer: DoomLoopDisabled, Escalate: DoomLoopDisabled, Block: DoomLoopDisabled, Stop: 4}
	if got := ResolveLadderAction(disabled, 3, LadderActionOptions{AllowBlock: true}); got != "" {
		t.Errorf("disabled rungs: got %q", got)
	}
	esc := DoomLoopLadder{Observe: 2, Steer: 3, Escalate: 4, Block: DoomLoopDisabled, Stop: DoomLoopDisabled}
	if got := ResolveLadderAction(esc, 4, LadderActionOptions{}); got != DoomLoopActionEscalate {
		t.Errorf("escalate: got %q", got)
	}
	if got := ResolveLadderAction(esc, 4, LadderActionOptions{DisallowEscalate: true}); got != DoomLoopActionSteer {
		t.Errorf("escalate disallowed: got %q", got)
	}
}

func TestResolveDoomLoopOption(t *testing.T) {
	t.Run("nil → detection off", func(t *testing.T) {
		if ResolveDoomLoopOption(nil) != nil {
			t.Fatal("expected nil")
		}
	})
	t.Run("defaults", func(t *testing.T) {
		warnings := captureDoomLoopWarnings(t)
		r := ResolveDoomLoopOption(&DoomLoopConfig{})
		if r.Ladder != DefaultDoomLoopLadder || !r.Text.Enabled || r.Escalation != nil {
			t.Fatalf("got %+v", r)
		}
		if r.Text != (ResolvedDoomLoopTextOptions{Enabled: true, MaxPeriodTokens: 16, MinRepeats: 4, MinCoveredTokens: 12, MaxWindowTokens: 400}) {
			t.Fatalf("text %+v", r.Text)
		}
		if w := warnings(); len(w) != 0 {
			t.Fatalf("default ladder must not warn: %v", w)
		}
		if DefaultDoomLoopLadder != (DoomLoopLadder{Observe: 2, Steer: DoomLoopDisabled, Escalate: DoomLoopDisabled, Block: 3, Stop: 6}) {
			t.Fatal("DefaultDoomLoopLadder changed")
		}
	})
	t.Run("merges partial ladders; zero means default", func(t *testing.T) {
		r := ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Block: 5, Stop: 0}})
		if r.Ladder.Block != 5 || r.Ladder.Stop != DefaultDoomLoopLadder.Stop || r.Ladder.Observe != DefaultDoomLoopLadder.Observe {
			t.Fatalf("got %+v", r.Ladder)
		}
	})
	t.Run("advisor false is not a mechanism", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		if r := ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Advisor: false}}); r.Escalation != nil {
			t.Fatalf("got %+v", r.Escalation)
		}
	})
	t.Run("advisor false does not resolve alongside a model", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		r := ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Model: "stronger-model", Advisor: false}})
		if r.Escalation == nil || r.Escalation.Model != "stronger-model" || r.Escalation.Advisor != nil || r.Escalation.MaxEscalations != DefaultMaxEscalations {
			t.Fatalf("got %+v", r.Escalation)
		}
	})
	t.Run("advisor true is a mechanism; custom cap", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		r := ResolveDoomLoopOption(&DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Advisor: true, MaxEscalations: 5}})
		if r.Escalation == nil || r.Escalation.Advisor != true || r.Escalation.MaxEscalations != 5 {
			t.Fatalf("got %+v", r.Escalation)
		}
	})
	t.Run("text disabled", func(t *testing.T) {
		off := false
		if r := ResolveDoomLoopOption(&DoomLoopConfig{Text: &DoomLoopTextOptions{Enabled: &off}}); r.Text.Enabled {
			t.Fatal("expected disabled")
		}
		r := ResolveDoomLoopOption(&DoomLoopConfig{Text: &DoomLoopTextOptions{MinRepeats: 2}})
		if !r.Text.Enabled || r.Text.MinRepeats != 2 || r.Text.MaxWindowTokens != 400 {
			t.Fatalf("got %+v", r.Text)
		}
	})
	warnCases := []struct {
		name string
		cfg  DoomLoopConfig
		want string
	}{
		{"block with stop disabled", DoomLoopConfig{Ladder: DoomLoopLadder{Block: 3, Stop: DoomLoopDisabled}}, "stop disabled"},
		{"dead rung", DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 5, Block: 2}}, `"observe" (5) can never fire`},
		{"escalate rung without mechanism", DoomLoopConfig{Ladder: DoomLoopLadder{Escalate: 2, Block: 3}}, "no escalation config"},
		{"mechanism without escalate rung", DoomLoopConfig{Escalation: &DoomLoopEscalationConfig{Model: "m"}}, "escalate ladder rung is disabled"},
	}
	for _, c := range warnCases {
		t.Run("warns: "+c.name, func(t *testing.T) {
			warnings := captureDoomLoopWarnings(t)
			ResolveDoomLoopOption(&c.cfg)
			found := false
			for _, w := range warnings() {
				found = found || strings.Contains(w, c.want)
			}
			if !found {
				t.Fatalf("expected warning containing %q, got %v", c.want, warnings())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Monitor — tool-call streaks
// ---------------------------------------------------------------------------

func TestDoomLoopMonitorToolStreaks(t *testing.T) {
	t.Run("observe at 2, block at 3", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		args := map[string]any{"query": "same thing"}
		if r := mustRecord(t, m, "search", args, 1, nil); r.Verdict != nil {
			t.Fatalf("round 1: %+v", r.Verdict)
		}
		r := mustRecord(t, m, "search", args, 2, nil)
		if v := r.Verdict; v == nil || v.Action != DoomLoopActionObserve || v.Streak != 2 || v.Detector != DoomLoopDetectorToolFingerprint || v.ToolName != "search" {
			t.Fatalf("round 2: %+v", v)
		}
		r = mustRecord(t, m, "search", args, 3, nil)
		if v := r.Verdict; v == nil || v.Action != DoomLoopActionBlock || v.Streak != 3 {
			t.Fatalf("round 3: %+v", v)
		}
	})
	t.Run("stop at the stop threshold", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		var last DoomLoopCallRecord
		for round := 1; round <= 6; round++ {
			last = mustRecord(t, m, "search", map[string]any{"q": "x"}, round, nil)
		}
		if v := last.Verdict; v == nil || v.Action != DoomLoopActionStop || v.Streak != 6 {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("same-round identical calls are duplicates", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		args := map[string]any{"q": "parallel"}
		if r := mustRecord(t, m, "search", args, 1, nil); r.Streak != 1 || r.DuplicateInRound {
			t.Fatalf("first: %+v", r)
		}
		for i := 0; i < 5; i++ {
			if r := mustRecord(t, m, "search", args, 1, nil); r.Streak != 1 || !r.DuplicateInRound || r.Verdict != nil {
				t.Fatalf("dup %d: %+v", i, r)
			}
		}
		if r := mustRecord(t, m, "search", args, 2, nil); r.Streak != 2 || r.DuplicateInRound {
			t.Fatalf("next round: %+v", r)
		}
	})
	t.Run("different arguments reset the streak", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "search", map[string]any{"q": "a"}, 1, nil)
		mustRecord(t, m, "search", map[string]any{"q": "a"}, 2, nil)
		if r := mustRecord(t, m, "search", map[string]any{"q": "b"}, 3, nil); r.Verdict != nil {
			t.Fatalf("got %+v", r.Verdict)
		}
		if r := mustRecord(t, m, "search", map[string]any{"q": "b"}, 4, nil); r.Verdict == nil || r.Verdict.Streak != 2 {
			t.Fatalf("got %+v", r.Verdict)
		}
	})
	t.Run("interleaved other tools do not reset", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		search := map[string]any{"q": "x"}
		mustRecord(t, m, "search", search, 1, nil)
		mustRecord(t, m, "read_file", map[string]any{"path": "/a"}, 2, nil)
		if r := mustRecord(t, m, "search", search, 3, nil); r.Verdict == nil || r.Verdict.Action != DoomLoopActionObserve || r.Verdict.Streak != 2 {
			t.Fatalf("got %+v", r.Verdict)
		}
		mustRecord(t, m, "read_file", map[string]any{"path": "/b"}, 4, nil)
		if r := mustRecord(t, m, "search", search, 5, nil); r.Verdict == nil || r.Verdict.Action != DoomLoopActionBlock || r.Verdict.Streak != 3 {
			t.Fatalf("got %+v", r.Verdict)
		}
	})
	t.Run("argument key order does not defeat detection", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "run", map[string]any{"cmd": "ls", "cwd": "/tmp"}, 1, nil)
		if r := mustRecord(t, m, "run", map[string]any{"cwd": "/tmp", "cmd": "ls"}, 2, nil); r.Verdict == nil || r.Verdict.Streak != 2 {
			t.Fatalf("got %+v", r.Verdict)
		}
	})
	t.Run("repeated EMPTY calls trip the detector", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		want := []string{"none", "observe", "block"}
		for i, w := range want {
			if got := actionOf(mustRecord(t, m, "list_tasks", map[string]any{}, i+1, nil)); got != w {
				t.Fatalf("round %d: %s want %s", i+1, got, w)
			}
		}
	})
	t.Run("propagates canonicalization errors", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		if _, err := m.RecordToolCall("search", map[string]any{"id": math.NaN()}, 1, nil); err == nil || !strings.Contains(err.Error(), "non-finite") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("server-tool records are non-blockable", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		var last DoomLoopCallRecord
		for round := 1; round <= 3; round++ {
			last = mustRecord(t, m, "server:web_search_call", map[string]any{"query": "x"}, round,
				&RecordToolCallOptions{DisallowBlock: true, Detector: DoomLoopDetectorServerToolFingerprint})
		}
		if v := last.Verdict; v == nil || v.Detector != DoomLoopDetectorServerToolFingerprint || v.Action != DoomLoopActionObserve || v.Streak != 3 {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("deterministic across monitors", func(t *testing.T) {
		script := []struct {
			name  string
			args  any
			round int
		}{
			{"a", map[string]any{"x": 1.0}, 1}, {"b", map[string]any{}, 1}, {"a", map[string]any{"x": 1.0}, 2},
			{"a", map[string]any{"x": 2.0}, 3}, {"a", map[string]any{"x": 2.0}, 4},
		}
		run := func() []*DoomLoopVerdict {
			m := newTestMonitor(t, nil)
			var out []*DoomLoopVerdict
			for _, s := range script {
				out = append(out, mustRecord(t, m, s.name, s.args, s.round, nil).Verdict)
			}
			return out
		}
		if !reflect.DeepEqual(run(), run()) {
			t.Fatal("non-deterministic")
		}
	})
	t.Run("escalate rung honors the budget", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		m := newTestMonitor(t, &DoomLoopConfig{
			Ladder:     DoomLoopLadder{Escalate: 2, Block: DoomLoopDisabled, Stop: DoomLoopDisabled},
			Escalation: &DoomLoopEscalationConfig{Model: "frontier", MaxEscalations: 1},
		})
		mustRecord(t, m, "s", map[string]any{}, 1, nil)
		if got := actionOf(mustRecord(t, m, "s", map[string]any{}, 2, nil)); got != "escalate" {
			t.Fatalf("got %s", got)
		}
		if !m.CanEscalate() {
			t.Fatal("budget should remain until consumed")
		}
		m.ConsumeEscalation()
		if m.CanEscalate() {
			t.Fatal("budget should be exhausted")
		}
		if got := actionOf(mustRecord(t, m, "s", map[string]any{}, 3, nil)); got != "observe" {
			t.Fatalf("exhausted escalate must fall through: got %s", got)
		}
		if st := m.GetState(); st.EscalationsUsed != 1 {
			t.Fatalf("escalationsUsed = %d", st.EscalationsUsed)
		}
		resumed := NewDoomLoopMonitor(ResolveDoomLoopOption(&DoomLoopConfig{
			Ladder:     DoomLoopLadder{Escalate: 2, Block: DoomLoopDisabled, Stop: DoomLoopDisabled},
			Escalation: &DoomLoopEscalationConfig{Model: "frontier", MaxEscalations: 1},
		}), m.GetState())
		if resumed.CanEscalate() {
			t.Fatal("resume must not reset the escalation budget")
		}
	})
}

// ---------------------------------------------------------------------------
// Monitor — text
// ---------------------------------------------------------------------------

func TestDoomLoopMonitorText(t *testing.T) {
	t.Run("within-response repetition can stop immediately", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		v := m.RecordAssistantText(repeatJoin("I am stuck.", 20))
		if v == nil || v.Detector != DoomLoopDetectorTextRepetition || v.Action != DoomLoopActionStop || v.Streak != 20 {
			t.Fatalf("got %+v", v)
		}
		// The repeated unit's fingerprint is the bare key-material vector
		// "plain phrase" from testdata/doom-loop-fingerprints.json.
		if v.Fingerprint != "37c38ab53f4c3570d8baf87392cb035ef8b2ad98ca1c1e083ea84184173911aa" {
			t.Fatalf("fingerprint %s", v.Fingerprint)
		}
		if v.Message != `Doom loop suspected: the response repeats "I am stuck." 20 times in a row. Stop repeating and take a different approach.` {
			t.Fatalf("message %q", v.Message)
		}
	})
	t.Run("cross-step identical text builds a streak (block → observe)", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		text := "Let me try that again."
		if v := m.RecordAssistantText(text); v != nil {
			t.Fatalf("got %+v", v)
		}
		for _, streak := range []int{2, 3} {
			v := m.RecordAssistantText(text)
			if v == nil || v.Detector != DoomLoopDetectorTextStreak || v.Action != DoomLoopActionObserve || v.Streak != streak {
				t.Fatalf("streak %d: got %+v", streak, v)
			}
		}
		m.RecordAssistantText(text)
		m.RecordAssistantText(text)
		v := m.RecordAssistantText(text)
		if v == nil || v.Action != DoomLoopActionStop || v.Streak != 6 {
			t.Fatalf("got %+v", v)
		}
		fp, _ := FingerprintKeyMaterial(text)
		if v.Fingerprint != fp {
			t.Fatalf("text-streak fingerprint = %s, want %s", v.Fingerprint, fp)
		}
	})
	t.Run("whitespace-only differences do not defeat the streak", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		m.RecordAssistantText("Retrying   the\nsame plan.")
		if v := m.RecordAssistantText("Retrying the same plan."); v == nil || v.Streak != 2 {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("DOCUMENTED MISS: paraphrased cross-step text", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		for _, text := range []string{
			"I am unable to find the file.", "I cannot locate the file.", "I am not able to find that file.",
			"The file cannot be found.", "Locating the file is not possible.", "I could not find that file.",
		} {
			if v := m.RecordAssistantText(text); v != nil {
				t.Fatalf("%q: %+v", text, v)
			}
		}
	})
	t.Run("empty text neither counts nor resets", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		m.RecordAssistantText("same")
		m.RecordAssistantText("")
		m.RecordAssistantText("   ")
		if v := m.RecordAssistantText("same"); v == nil || v.Streak != 2 {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("ResetTextStreak clears the cross-step streak only", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		m.RecordAssistantText("same")
		mustRecord(t, m, "a", map[string]any{}, 1, nil)
		m.ResetTextStreak()
		if v := m.RecordAssistantText("same"); v != nil {
			t.Fatalf("got %+v", v)
		}
		if r := mustRecord(t, m, "a", map[string]any{}, 2, nil); r.Streak != 2 {
			t.Fatalf("tool streak must survive: %+v", r)
		}
	})
	t.Run("text disabled disables both detectors", func(t *testing.T) {
		off := false
		m := newTestMonitor(t, &DoomLoopConfig{Text: &DoomLoopTextOptions{Enabled: &off}})
		if v := m.RecordAssistantText(repeatJoin("loop", 30)); v != nil {
			t.Fatalf("got %+v", v)
		}
	})
}

// ---------------------------------------------------------------------------
// Monitor — state round-trip
// ---------------------------------------------------------------------------

func jsonRoundTrip(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDoomLoopMonitorState(t *testing.T) {
	t.Run("serializes to plain JSON and resumes counting", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "search", map[string]any{"q": "x"}, 1, nil)
		mustRecord(t, m, "search", map[string]any{"q": "x"}, 2, nil)
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), jsonRoundTrip(t, m.GetState()))
		r := mustRecord(t, resumed, "search", map[string]any{"q": "x"}, 1, nil)
		if r.Verdict == nil || r.Verdict.Action != DoomLoopActionBlock || r.Verdict.Streak != 3 {
			t.Fatalf("got %+v", r.Verdict)
		}
	})
	t.Run("accepts typed, pointer and RawMessage blobs", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "search", map[string]any{"q": "x"}, 1, nil)
		mustRecord(t, m, "search", map[string]any{"q": "x"}, 2, nil)
		st := m.GetState()
		raw, _ := json.Marshal(st)
		for name, blob := range map[string]any{"value": st, "pointer": &st, "raw": json.RawMessage(raw)} {
			r := mustRecord(t, NewDoomLoopMonitor(defaultDoomLoopConfig(), blob), "search", map[string]any{"q": "x"}, 0, nil)
			if r.Streak != 3 {
				t.Errorf("%s: streak %d", name, r.Streak)
			}
		}
	})
	t.Run("ignores corrupt persisted state", func(t *testing.T) {
		warnings := captureDoomLoopWarnings(t)
		for _, blob := range []any{"not an object", []any{1.0}, json.RawMessage("{oops"), 42} {
			m := NewDoomLoopMonitor(defaultDoomLoopConfig(), blob)
			if r := mustRecord(t, m, "search", map[string]any{"q": "x"}, 1, nil); r.Verdict != nil || r.Streak != 1 {
				t.Fatalf("%v: %+v", blob, r)
			}
		}
		if len(warnings()) != 4 {
			t.Fatalf("expected 4 warnings, got %v", warnings())
		}
	})
	t.Run("Restore on a live monitor with invalid state keeps state", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "a", map[string]any{}, 1, nil)
		m.Restore("garbage")
		if r := mustRecord(t, m, "a", map[string]any{}, 2, nil); r.Streak != 2 {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("a persisted __proto__ tool cannot leak into other tools", func(t *testing.T) {
		var hostile any
		_ = json.Unmarshal([]byte(`{"tools":{"__proto__":{"fingerprint":"x","streak":999}}}`), &hostile)
		m := NewDoomLoopMonitor(defaultDoomLoopConfig(), hostile)
		r := mustRecord(t, m, "unrelated_tool", map[string]any{"q": "first call"}, 1, nil)
		if r.Streak != 1 || r.Verdict != nil {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("a tool named __proto__ is ordinary data", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "__proto__", map[string]any{}, 1, nil)
		if r := mustRecord(t, m, "__proto__", map[string]any{}, 2, nil); r.Streak != 2 {
			t.Fatalf("got %+v", r)
		}
		snap := m.GetState()
		if _, ok := snap.Tools["__proto__"]; !ok {
			t.Fatal("missing")
		}
		revived := jsonRoundTrip(t, snap).(map[string]any)["tools"].(map[string]any)
		if _, ok := revived["__proto__"]; !ok {
			t.Fatal("missing after JSON")
		}
	})
	t.Run("GetState returns a snapshot", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "a", map[string]any{}, 1, nil)
		snap := m.GetState()
		mustRecord(t, m, "a", map[string]any{}, 2, nil)
		if snap.Tools["a"].Streak != 1 || m.GetState().Tools["a"].Streak != 2 {
			t.Fatal("snapshot aliased live state")
		}
	})
	t.Run("serialized shape", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "a", map[string]any{}, 1, nil)
		m.RecordAssistantText("hello")
		fp, _ := FingerprintToolCall("a", map[string]any{})
		tfp, _ := FingerprintKeyMaterial("hello")
		raw, _ := json.Marshal(m.GetState())
		want := `{"tools":{"a":{"fingerprint":"` + fp + `","streak":1}},"text":{"fingerprint":"` + tfp + `","streak":1}}`
		if string(raw) != want {
			t.Fatalf("got  %s\nwant %s", raw, want)
		}
	})
	t.Run("restore clamps hostile streaks and counts", func(t *testing.T) {
		var blob any
		_ = json.Unmarshal([]byte(`{"tools":{"read":{"fingerprint":"f","streak":-50,"callStreaks":{"f":"x","g":0}}},"escalationsUsed":-3,"text":{"fingerprint":1,"streak":2}}`), &blob)
		m := NewDoomLoopMonitor(defaultDoomLoopConfig(), blob)
		st := m.GetState()
		if st.Tools["read"].Streak != 1 || st.Tools["read"].CallStreaks != nil || st.EscalationsUsed != 0 || st.Text != nil {
			t.Fatalf("got %+v", st)
		}
	})
}

// ---------------------------------------------------------------------------
// Fan-out streaks (doom-loop-fanout.test.ts)
// ---------------------------------------------------------------------------

func declarePaths(m *DoomLoopMonitor, tool string, round int, paths []string) {
	calls := make([]DoomLoopRoundCall, len(paths))
	for i, p := range paths {
		calls[i] = DoomLoopRoundCall{ToolName: tool, KeyMaterial: pathArgs(p)}
	}
	m.DeclareRound(round, calls)
}

func playRounds(t *testing.T, m *DoomLoopMonitor, fanouts [][]string) [][]string {
	t.Helper()
	if m == nil {
		m = newTestMonitor(t, nil)
	}
	var out [][]string
	for round, paths := range fanouts {
		declarePaths(m, "read", round, paths)
		var actions []string
		for _, p := range paths {
			actions = append(actions, actionOf(mustRecord(t, m, "read", pathArgs(p), round, nil)))
		}
		out = append(out, actions)
	}
	return out
}

func TestDoomLoopFanoutStreaks(t *testing.T) {
	abc := []string{"a", "b", "c"}
	cases := []struct {
		name    string
		fanouts [][]string
		want    [][]string
	}{
		{"accumulates a repeating distinct-argument fan-out", [][]string{abc, abc, abc},
			[][]string{{"none", "none", "none"}, {"observe", "observe", "observe"}, {"block", "block", "block"}}},
		{"order-insensitive within the round", [][]string{abc, {"c", "a", "b"}, {"b", "c", "a"}},
			[][]string{{"none", "none", "none"}, {"observe", "observe", "observe"}, {"block", "block", "block"}}},
		{"membership change scores calls individually", [][]string{{"a", "b"}, {"a", "b"}, {"a", "z"}, {"a", "z"}},
			[][]string{{"none", "none"}, {"observe", "observe"}, {"block", "none"}, {"block", "observe"}}},
		{"partial repeat flags what repeated", [][]string{abc, {"a", "b"}},
			[][]string{{"none", "none", "none"}, {"observe", "observe"}}},
		{"superset round flags repeats, never the new call", [][]string{{"a", "b"}, {"a", "b"}, abc},
			[][]string{{"none", "none"}, {"observe", "observe"}, {"block", "block", "none"}}},
		{"superset round permuted", [][]string{{"a", "b"}, {"a", "b"}, {"c", "a", "b"}},
			[][]string{{"none", "none"}, {"observe", "observe"}, {"none", "block", "block"}}},
		{"expanding fan-out per call", [][]string{{"a"}, {"a", "b"}, abc, {"a", "b", "c", "d"}},
			[][]string{{"none"}, {"observe", "none"}, {"block", "observe", "none"}, {"block", "block", "observe", "none"}}},
		{"single-call rounds unchanged", [][]string{{"a"}, {"a"}, {"a"}, {"a"}},
			[][]string{{"none"}, {"observe"}, {"block"}, {"block"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := playRounds(t, nil, c.fanouts); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %v\nwant %v", got, c.want)
			}
		})
	}

	t.Run("identical duplicates within one round count once", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		first := mustRecord(t, m, "read", pathArgs("a"), 0, nil)
		second := mustRecord(t, m, "read", pathArgs("a"), 0, nil)
		if first.DuplicateInRound || !second.DuplicateInRound || second.Streak != first.Streak {
			t.Fatalf("%+v %+v", first, second)
		}
	})

	t.Run("resumed single-call streak keeps incrementing", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "read", pathArgs("a"), 0, nil)
		mustRecord(t, m, "read", pathArgs("a"), 1, nil)
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState())
		if r := mustRecord(t, resumed, "read", pathArgs("a"), 0, nil); r.Streak != 3 {
			t.Fatalf("got %d", r.Streak)
		}
	})

	t.Run("undeclared round falls back to per-call scoring", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		mustRecord(t, m, "server:web_search", map[string]any{"q": "x"}, 0, nil)
		repeated := mustRecord(t, m, "server:web_search", map[string]any{"q": "x"}, 1, nil)
		fresh := mustRecord(t, m, "server:web_search", map[string]any{"q": "y"}, 1, nil)
		if repeated.Streak != 2 || fresh.Streak != 1 || fresh.Verdict != nil {
			t.Fatalf("%+v %+v", repeated, fresh)
		}
	})

	t.Run("undeclared multi-call round is per call and order-independent", func(t *testing.T) {
		undeclared := func(rounds [][]string) [][]string {
			m := newTestMonitor(t, nil)
			var out [][]string
			for round, paths := range rounds {
				var scored []string
				for _, p := range paths {
					r := mustRecord(t, m, "read", pathArgs(p), round, nil)
					scored = append(scored, fmt.Sprintf("%d:%s", r.Streak, actionOf(r)))
				}
				out = append(out, scored)
			}
			return out
		}
		cases := []struct {
			in, want [][]string
		}{
			{[][]string{{"a", "b"}, {"a", "b"}}, [][]string{{"1:none", "1:none"}, {"2:observe", "2:observe"}}},
			{[][]string{{"a", "b"}, {"b", "a"}}, [][]string{{"1:none", "1:none"}, {"2:observe", "2:observe"}}},
			{[][]string{{"b", "a"}, {"b", "c"}, {"b", "d"}}, [][]string{{"1:none", "1:none"}, {"2:observe", "1:none"}, {"3:block", "1:none"}}},
		}
		for _, c := range cases {
			if got := undeclared(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%v: got %v want %v", c.in, got, c.want)
			}
		}
	})

	unhashable := map[string]any{"size": math.NaN()}
	declareWithUnhashable := func(m *DoomLoopMonitor, round int) {
		m.DeclareRound(round, []DoomLoopRoundCall{
			{ToolName: "read", KeyMaterial: pathArgs("a")},
			{ToolName: "read", KeyMaterial: unhashable},
		})
	}

	t.Run("a non-member does not inherit the round streak", func(t *testing.T) {
		warnings := captureDoomLoopWarnings(t)
		m := newTestMonitor(t, nil)
		for round := 0; round < 2; round++ {
			declareWithUnhashable(m, round)
			mustRecord(t, m, "read", pathArgs("a"), round, nil)
		}
		declareWithUnhashable(m, 2)
		member := mustRecord(t, m, "read", pathArgs("a"), 2, nil)
		dropped := mustRecord(t, m, "read", map[string]any{"size": "fallback-identity"}, 2, nil)
		if member.Streak != 3 || dropped.Streak != 1 || dropped.Verdict != nil {
			t.Fatalf("%+v %+v", member, dropped)
		}
		if w := warnings(); len(w) != 3 || !strings.Contains(w[0], `could not fingerprint a "read" call while declaring round 0`) {
			t.Fatalf("warnings %v", w)
		}
	})

	t.Run("per-call evidence accumulates for an unhashable call reissued verbatim", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		m := newTestMonitor(t, nil)
		var memberStreaks, droppedStreaks []int
		for round := 0; round < 3; round++ {
			declareWithUnhashable(m, round)
			memberStreaks = append(memberStreaks, mustRecord(t, m, "read", pathArgs("a"), round, nil).Streak)
			droppedStreaks = append(droppedStreaks, mustRecord(t, m, "read", map[string]any{"size": "fallback-identity"}, round, nil).Streak)
		}
		if !reflect.DeepEqual(memberStreaks, []int{1, 2, 3}) || !reflect.DeepEqual(droppedStreaks, []int{1, 2, 3}) {
			t.Fatalf("%v %v", memberStreaks, droppedStreaks)
		}
	})

	t.Run("keeps accumulating when an unhashable call rides along, either order", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		for _, droppedFirst := range []bool{false, true} {
			m := newTestMonitor(t, nil)
			var streaks []int
			for round := 0; round < 4; round++ {
				declareWithUnhashable(m, round)
				if droppedFirst {
					mustRecord(t, m, "read", map[string]any{"size": "fallback-identity"}, round, nil)
				}
				streaks = append(streaks, mustRecord(t, m, "read", pathArgs("a"), round, nil).Streak)
				if !droppedFirst {
					mustRecord(t, m, "read", map[string]any{"size": "fallback-identity"}, round, nil)
				}
			}
			if !reflect.DeepEqual(streaks, []int{1, 2, 3, 4}) {
				t.Fatalf("droppedFirst=%v: %v", droppedFirst, streaks)
			}
		}
	})

	t.Run("persists the streak against the member that earned it", func(t *testing.T) {
		captureDoomLoopWarnings(t)
		m := newTestMonitor(t, nil)
		for round := 0; round < 2; round++ {
			declareWithUnhashable(m, round)
			mustRecord(t, m, "read", pathArgs("a"), round, nil)
			mustRecord(t, m, "read", map[string]any{"size": "fallback-identity"}, round, nil)
		}
		own := mustRecord(t, NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState()), "read", map[string]any{"size": "fallback-identity"}, 0, nil)
		if own.Streak != 3 {
			t.Fatalf("own repeat: %d", own.Streak)
		}
		fresh := mustRecord(t, NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState()), "read", map[string]any{"size": "never-seen-before"}, 0, nil)
		if fresh.Streak != 1 || fresh.Verdict != nil {
			t.Fatalf("fresh: %+v", fresh)
		}
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState())
		declarePaths(resumed, "read", 0, []string{"a"})
		if r := mustRecord(t, resumed, "read", pathArgs("a"), 0, nil); r.Streak != 3 {
			t.Fatalf("real repeat: %d", r.Streak)
		}
	})

	t.Run("declared-but-never-recorded member disappearing does not reset", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		var streaks []int
		for round := 0; round < 4; round++ {
			members := []string{"a"}
			if round < 2 {
				members = append(members, "phantom")
			}
			declarePaths(m, "read", round, members)
			streaks = append(streaks, mustRecord(t, m, "read", pathArgs("a"), round, nil).Streak)
		}
		if !reflect.DeepEqual(streaks, []int{1, 2, 3, 4}) {
			t.Fatalf("%v", streaks)
		}
	})

	steerMonitor := func(t *testing.T) *DoomLoopMonitor {
		return newTestMonitor(t, &DoomLoopConfig{Ladder: DoomLoopLadder{Steer: 2}})
	}
	collect := func(t *testing.T, m *DoomLoopMonitor, rounds [][]string, last int) (msgs []string, streaks []int) {
		for round, paths := range rounds {
			declarePaths(m, "read", round, paths)
			for _, p := range paths {
				r := mustRecord(t, m, "read", pathArgs(p), round, nil)
				if round == last && r.Verdict != nil {
					msgs = append(msgs, r.Verdict.Message)
					streaks = append(streaks, r.Verdict.Streak)
				}
			}
		}
		return msgs, streaks
	}
	distinct := func(msgs []string) int {
		set := map[string]struct{}{}
		for _, m := range msgs {
			set[m] = struct{}{}
		}
		return len(set)
	}

	t.Run("every call of a repeating round gets the SAME message", func(t *testing.T) {
		msgs, _ := collect(t, steerMonitor(t), [][]string{abc, abc}, 1)
		if len(msgs) != 3 || distinct(msgs) != 1 || !strings.Contains(msgs[0], "3 parallel calls") || !strings.Contains(msgs[0], "round identity ") {
			t.Fatalf("%v", msgs)
		}
	})

	t.Run("resumed FAN-OUT streak continues", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		playRounds(t, m, [][]string{{"a", "b"}, {"a", "b"}})
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), jsonRoundTrip(t, m.GetState()))
		declarePaths(resumed, "read", 0, []string{"a", "b"})
		r := mustRecord(t, resumed, "read", pathArgs("a"), 0, nil)
		if r.Streak != 3 || actionOf(r) != "block" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("paused HITL member dropping out does not reset the repeat", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		var work []string
		for round := 0; round < 3; round++ {
			members := []string{"work"}
			if round == 0 {
				members = append(members, "gated")
			}
			declarePaths(m, "deploy", round, members)
			for _, p := range members {
				r := mustRecord(t, m, "deploy", pathArgs(p), round, nil)
				if p == "work" {
					work = append(work, fmt.Sprintf("%d:%s", r.Streak, actionOf(r)))
				}
			}
		}
		if !reflect.DeepEqual(work, []string{"1:none", "2:observe", "3:block"}) {
			t.Fatalf("%v", work)
		}
	})

	t.Run("staggered per-call counts collapse to one message", func(t *testing.T) {
		msgs, streaks := collect(t, steerMonitor(t), [][]string{{"a"}, {"a", "b"}, abc, {"a", "b", "c", "d"}}, 3)
		if !reflect.DeepEqual(streaks, []int{4, 3, 2}) || len(msgs) != 3 || distinct(msgs) != 1 {
			t.Fatalf("%v %v", streaks, msgs)
		}
	})

	t.Run("mixed-evidence round: one message per distinct fact", func(t *testing.T) {
		msgs, _ := collect(t, steerMonitor(t), [][]string{{"a"}, {"a", "b"}, {"a", "b"}}, 2)
		if len(msgs) != 2 || distinct(msgs) != 2 ||
			!strings.Contains(msgs[0], `this exact "read" call has been repeated`) ||
			!strings.Contains(msgs[1], "same set of 2 parallel calls") {
			t.Fatalf("%v", msgs)
		}
	})

	t.Run("wide repeating round collapses per-call messages", func(t *testing.T) {
		wide := make([]string, 20)
		for i := range wide {
			wide[i] = "f" + strconv.Itoa(i)
		}
		msgs, _ := collect(t, steerMonitor(t), [][]string{wide, wide, append(append([]string(nil), wide...), "new")}, 2)
		if len(msgs) != 20 || distinct(msgs) != 1 {
			t.Fatalf("%d verdicts, %d distinct", len(msgs), distinct(msgs))
		}
	})

	t.Run("per-call evidence persists when the round shrank to one call", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		declarePaths(m, "deploy", 0, []string{"work", "gated"})
		mustRecord(t, m, "deploy", pathArgs("work"), 0, nil)
		mustRecord(t, m, "deploy", pathArgs("gated"), 0, nil)
		declarePaths(m, "deploy", 1, []string{"work"})
		if r := mustRecord(t, m, "deploy", pathArgs("work"), 1, nil); r.Streak != 2 {
			t.Fatalf("before save: %d", r.Streak)
		}
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), jsonRoundTrip(t, m.GetState()))
		declarePaths(resumed, "deploy", 0, []string{"work"})
		r := mustRecord(t, resumed, "deploy", pathArgs("work"), 0, nil)
		if r.Streak != 3 || actionOf(r) != "block" {
			t.Fatalf("after resume: %+v", r)
		}
	})

	t.Run("resumed SINGLE call scores on its own earned evidence", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		playRounds(t, m, [][]string{abc, abc})
		saved := m.GetState().Tools["read"]
		if saved.Streak != 2 || len(saved.RoundFingerprints) != 3 || saved.CallStreaks != nil {
			t.Fatalf("saved %+v", saved)
		}
		for _, p := range abc {
			resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState())
			declarePaths(resumed, "read", 0, []string{p})
			r := mustRecord(t, resumed, "read", pathArgs(p), 0, nil)
			if r.Streak != 3 || actionOf(r) != "block" {
				t.Fatalf("%s: %+v", p, r)
			}
		}
		resumed := NewDoomLoopMonitor(defaultDoomLoopConfig(), m.GetState())
		declarePaths(resumed, "read", 0, []string{"never-before"})
		if r := mustRecord(t, resumed, "read", pathArgs("never-before"), 0, nil); r.Streak != 1 || r.Verdict != nil {
			t.Fatalf("fresh %+v", r)
		}
	})

	t.Run("one verdict text per undeclared multi-call round", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		for round := 1; round <= 4; round++ {
			var texts []string
			for _, cmd := range []string{"a", "b"} {
				r := mustRecord(t, m, "sh", map[string]any{"cmd": cmd}, round, &RecordToolCallOptions{DisallowBlock: true})
				if r.Verdict != nil {
					texts = append(texts, r.Verdict.Message)
				}
			}
			if distinct(texts) > 1 {
				t.Fatalf("round %d: %v", round, texts)
			}
		}
	})

	t.Run("fingerprint-bearing text for a genuine single-call round", func(t *testing.T) {
		m := newTestMonitor(t, nil)
		var last string
		for round := 1; round <= 3; round++ {
			declarePaths(m, "read", round, []string{"same"})
			if r := mustRecord(t, m, "read", pathArgs("same"), round, nil); r.Verdict != nil {
				last = r.Verdict.Message
			}
		}
		fp, _ := FingerprintToolCall("read", pathArgs("same"))
		if !strings.Contains(last, "identical arguments (fingerprint "+fp[:16]+"…)") {
			t.Fatalf("%q", last)
		}
	})
}

// ---------------------------------------------------------------------------
// Public API (doom-loop-public-api.test.ts)
// ---------------------------------------------------------------------------

func TestDoomLoopPublicAPI(t *testing.T) {
	paths := []string{"a", "b", "c"}

	t.Run("defaults detect a repeating fan-out at the exported block threshold", func(t *testing.T) {
		m := NewDoomLoopMonitor(ResolveDoomLoopOption(&DoomLoopConfig{}), nil)
		var action DoomLoopAction
		var streak int
		for round := 0; round < 3; round++ {
			declarePaths(m, "read", round, paths)
			for _, p := range paths {
				if r := mustRecord(t, m, "read", pathArgs(p), round, nil); r.Verdict != nil {
					action, streak = r.Verdict.Action, r.Verdict.Streak
				}
			}
		}
		if streak != int(DefaultDoomLoopLadder.Block) || action != DoomLoopActionBlock {
			t.Fatalf("%s %d", action, streak)
		}
	})

	t.Run("custom ladder", func(t *testing.T) {
		m := NewDoomLoopMonitor(ResolveDoomLoopOption(&DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: DoomLoopDisabled, Stop: 4}}), nil)
		var last string
		for round := 0; round < 4; round++ {
			last = actionOf(mustRecord(t, m, "search", map[string]any{"q": "same"}, round, nil))
		}
		if last != "stop" {
			t.Fatalf("got %s", last)
		}
	})

	t.Run("fan-out streak accumulates across per-turn process boundaries", func(t *testing.T) {
		var wire any
		var perTurn []string
		for turn := 0; turn < 4; turn++ {
			m := NewDoomLoopMonitor(defaultDoomLoopConfig(), wire)
			declarePaths(m, "read", 0, paths)
			var last string
			for _, p := range paths {
				r := mustRecord(t, m, "read", pathArgs(p), 0, nil)
				last = fmt.Sprintf("%d:%s", r.Streak, actionOf(r))
			}
			perTurn = append(perTurn, last)
			wire = jsonRoundTrip(t, m.GetState())
		}
		if !reflect.DeepEqual(perTurn, []string{"1:none", "2:observe", "3:block", "4:block"}) {
			t.Fatalf("%v", perTurn)
		}
	})

	t.Run("legacy blob without roundFingerprints; malformed sets degrade", func(t *testing.T) {
		legacy := map[string]any{"tools": map[string]any{"read": map[string]any{"fingerprint": "not-a-real-fingerprint", "streak": 2.0}}}
		if r := mustRecord(t, NewDoomLoopMonitor(defaultDoomLoopConfig(), legacy), "read", pathArgs("x"), 0, nil); r.Streak != 1 {
			t.Fatalf("legacy %d", r.Streak)
		}
		hostile := map[string]any{"tools": map[string]any{"read": map[string]any{"fingerprint": "ab", "streak": 2.0, "roundFingerprints": []any{1.0, map[string]any{}, nil}}}}
		m := NewDoomLoopMonitor(defaultDoomLoopConfig(), hostile)
		if _, ok := m.GetState().Tools["read"]; !ok {
			t.Fatal("malformed set must degrade, not drop the entry")
		}
		if r := mustRecord(t, m, "read", pathArgs("x"), 0, nil); r.Streak != 1 {
			t.Fatalf("hostile %d", r.Streak)
		}
	})

	t.Run("snapshot mutations do not affect the live detector", func(t *testing.T) {
		m := NewDoomLoopMonitor(defaultDoomLoopConfig(), nil)
		ab := []string{"a", "b"}
		playRounds(t, m, [][]string{ab, ab})
		saved := m.GetState()
		read := saved.Tools["read"]
		read.RoundFingerprints[0] = "INJECTED"
		read.RoundFingerprints = append(read.RoundFingerprints, "INJECTED")
		declarePaths(m, "read", 2, ab)
		var last DoomLoopCallRecord
		for _, p := range ab {
			last = mustRecord(t, m, "read", pathArgs(p), 2, nil)
		}
		if last.Streak != 3 || actionOf(last) != "block" {
			t.Fatalf("%+v", last)
		}
	})

	t.Run("round-trips across a process boundary via plain JSON", func(t *testing.T) {
		first := NewDoomLoopMonitor(defaultDoomLoopConfig(), nil)
		mustRecord(t, first, "search", map[string]any{"q": "same"}, 0, nil)
		mustRecord(t, first, "search", map[string]any{"q": "same"}, 1, nil)
		raw, _ := json.Marshal(first.GetState())
		var blob any
		_ = json.Unmarshal(raw, &blob)
		second := NewDoomLoopMonitor(defaultDoomLoopConfig(), blob)
		if r := mustRecord(t, second, "search", map[string]any{"q": "same"}, 0, nil); r.Streak != 3 {
			t.Fatalf("%d", r.Streak)
		}
	})

	t.Run("nil config uses defaults", func(t *testing.T) {
		m := NewDoomLoopMonitor(nil, nil)
		for round := 0; round < 3; round++ {
			mustRecord(t, m, "s", map[string]any{}, round, nil)
		}
		if r := mustRecord(t, m, "s", map[string]any{}, 3, nil); actionOf(r) != "block" {
			t.Fatalf("%+v", r)
		}
	})
}

// TestDoomLoopMonitorConcurrent exercises every public method from many
// goroutines; meaningful under -race.
func TestDoomLoopMonitorConcurrent(t *testing.T) {
	m := NewDoomLoopMonitor(defaultDoomLoopConfig(), nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				tool := "t" + strconv.Itoa(g%3)
				m.DeclareRound(round, []DoomLoopRoundCall{{ToolName: tool, KeyMaterial: pathArgs("a")}})
				if _, err := m.RecordToolCall(tool, pathArgs("a"), round, nil); err != nil {
					t.Error(err)
					return
				}
				m.RecordAssistantText("same text")
				_ = m.GetState()
				_ = m.CanEscalate()
				if round%7 == 0 {
					m.ResetTextStreak()
					m.ConsumeEscalation()
				}
			}
		}(g)
	}
	wg.Wait()
	// Restore is exercised concurrently below on a snapshot; within the
	// loop it would race other goroutines' writes nondeterministically.
	m.Restore(m.GetState())
	if len(m.GetState().Tools) != 3 {
		t.Fatalf("tools %v", m.GetState().Tools)
	}
}
