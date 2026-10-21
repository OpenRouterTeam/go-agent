package agent

// Doom-loop detection for the tool-execution loop. Ported from upstream
// lib/doom-loop.ts.
//
// A "doom loop" is a run that stops making progress while continuing to
// spend: the model re-issues the same tool call with identical arguments
// (including repeated *empty* calls), repeats identical server-tool
// requests, or emits the same text tokens over and over. This file provides
// the deterministic detection primitives; ModelResult wires them into the
// loop behind CallModelInput.DoomLoop.
//
// Design principles (mirroring upstream):
//
//   - Deterministic. Every verdict is a pure function of the recorded
//     sequence of tool calls / assistant texts — no wall clocks, no
//     randomness.
//   - Cross-port fingerprints. Key material is canonicalized per RFC 8785
//     (JCS) and hashed with SHA-256 over the UTF-8 bytes. The upstream
//     conformance vectors (tests/vectors/doom-loop-fingerprints.json) are
//     reproduced byte-for-byte by this implementation; see
//     doom_loop_test.go.
//   - Tool-declared identity via ToolLoopKey (function, declarative field
//     list, or static exemption).
//   - Round-scoped streaks: N identical calls in ONE round count once.
//   - Graduated response: observe → steer → escalate → block → stop.
//   - Never the cause of failure: an internal detection error degrades to a
//     fallback identity or skips detection for that call.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

//#region Public Types

// DoomLoopAction is a ladder rung, weakest to strongest:
//
//   - observe  — emit the DoomLoopDetected hook only.
//   - steer    — inject a corrective user message before the next model turn
//     (same mechanism as the Stop hook's AppendPrompt); the call still runs.
//   - escalate — run the NEXT turn with more intelligence (a stronger model
//     and/or a forced openrouter:advisor consult), per the Escalation
//     config. One-turn overrides, budgeted by MaxEscalations; the call still
//     runs.
//   - block    — refuse the tool call and synthesize an error
//     function_call_output (same shape as a PreToolUse block).
//   - stop     — halt the run before the next model request
//     (SessionEnd reason "doom_loop").
type DoomLoopAction string

const (
	DoomLoopActionObserve  DoomLoopAction = "observe"
	DoomLoopActionSteer    DoomLoopAction = "steer"
	DoomLoopActionEscalate DoomLoopAction = "escalate"
	DoomLoopActionBlock    DoomLoopAction = "block"
	DoomLoopActionStop     DoomLoopAction = "stop"
)

// DoomLoopDetectorKind identifies which detector produced a verdict.
type DoomLoopDetectorKind string

const (
	DoomLoopDetectorToolFingerprint       DoomLoopDetectorKind = "tool-fingerprint"
	DoomLoopDetectorServerToolFingerprint DoomLoopDetectorKind = "server-tool-fingerprint"
	DoomLoopDetectorTextRepetition        DoomLoopDetectorKind = "text-repetition"
	DoomLoopDetectorTextStreak            DoomLoopDetectorKind = "text-streak"
)

// DoomLoopVerdict is a doom-loop detection event.
//
// Streak is the repetition count that crossed a ladder threshold: for
// fingerprint detectors the consecutive identical-fingerprint round count
// for that tool; for text-repetition the number of consecutive repeats of a
// token block within one response; for text-streak the number of
// consecutive steps with identical assistant text.
type DoomLoopVerdict struct {
	Detector DoomLoopDetectorKind `json:"detector"`
	Action   DoomLoopAction       `json:"action"`
	Streak   int                  `json:"streak"`
	// Fingerprint is the deterministic fingerprint of the repeated unit.
	Fingerprint string `json:"fingerprint"`
	// ToolName is set for fingerprint verdicts.
	ToolName string `json:"toolName,omitempty"`
	// Message is the human/model-readable explanation, used verbatim for
	// block outputs and steer messages.
	Message string `json:"message"`
}

// DoomLoopCallRecord is the result of recording one tool call with the
// monitor. DuplicateInRound is true when the same (toolName, fingerprint)
// was already recorded in this round — the streak did NOT increment and the
// caller should reuse the decision it applied for the first occurrence.
type DoomLoopCallRecord struct {
	Fingerprint      string
	Streak           int
	DuplicateInRound bool
	Verdict          *DoomLoopVerdict
}

// DoomLoopThreshold is one ladder rung's streak threshold. The zero value
// means "use the default for this rung"; a positive value is the threshold
// (compared with >=); DoomLoopRungDisabled (any negative value) disables the
// rung — the Go stand-in for upstream's `false`.
type DoomLoopThreshold int

// DoomLoopRungDisabled disables a ladder rung (upstream `false`).
const DoomLoopRungDisabled DoomLoopThreshold = -1

func (t DoomLoopThreshold) enabled() bool { return t > 0 }

// DoomLoopLadder holds the streak thresholds for each action. When several
// rungs are crossed the strongest wins (stop > block > escalate > steer >
// observe). Thresholds compare with >=, so a verdict fires on every record
// at or past a rung.
type DoomLoopLadder struct {
	Observe  DoomLoopThreshold
	Steer    DoomLoopThreshold
	Escalate DoomLoopThreshold
	Block    DoomLoopThreshold
	Stop     DoomLoopThreshold
}

// DefaultDoomLoopLadder returns the default ladder: observe at 2
// consecutive identical rounds, block at 3, stop at 6. Steer and escalate
// are off by default.
func DefaultDoomLoopLadder() DoomLoopLadder {
	return DoomLoopLadder{Observe: 2, Steer: DoomLoopRungDisabled, Escalate: DoomLoopRungDisabled, Block: 3, Stop: 6}
}

// DefaultMaxEscalations is the escalation budget when the rung is enabled
// without a cap.
const DefaultMaxEscalations = 2

// DoomLoopEscalationConfig configures the recovery applied by the escalate
// rung. At least one of Model / Advisor / AdvisorParameters must be set for
// the rung to do anything.
type DoomLoopEscalationConfig struct {
	// Model is the slug to run the NEXT turn on, replacing the request's
	// configured model (and clearing any Models fallback list) for one
	// request only.
	Model string
	// Advisor forces an openrouter:advisor consult on the next turn with
	// default parameters (transcript forwarded, loop-diagnosing
	// instructions). Upstream `advisor: true`.
	Advisor bool
	// AdvisorParameters, when non-nil, enables the advisor consult and is
	// passed through as the advisor tool's parameters; nil ForwardTranscript
	// / Instructions are filled with the defaults. Upstream's object form of
	// `advisor`.
	AdvisorParameters *components.AdvisorServerToolConfig
	// MaxEscalations caps recoveries per conversation. Values < 1 mean
	// DefaultMaxEscalations.
	MaxEscalations int
}

// DoomLoopTextOptions tunes within-response text-repetition detection.
// Zero-valued numeric fields use the defaults.
type DoomLoopTextOptions struct {
	// Disabled turns off both text detectors (upstream `text: false` /
	// `enabled: false`).
	Disabled bool
	// MaxPeriodTokens is the longest repeating token block to search for. Default 16.
	MaxPeriodTokens int
	// MinRepeats is the minimum consecutive repeats of a block. Default 4.
	MinRepeats int
	// MinCoveredTokens is the minimum total tokens covered. Default 12.
	MinCoveredTokens int
	// MaxWindowTokens: only the trailing N tokens of a response are scanned. Default 400.
	MaxWindowTokens int
}

// DoomLoopConfig configures doom-loop detection. A zero DoomLoopConfig
// (`&agent.DoomLoopConfig{}`) is upstream's `doomLoop: true`: every default.
type DoomLoopConfig struct {
	Ladder     DoomLoopLadder
	Text       DoomLoopTextOptions
	Escalation *DoomLoopEscalationConfig
}

// DoomLoopStreak is a consecutive-repetition counter for one identity.
type DoomLoopStreak struct {
	Fingerprint string `json:"fingerprint"`
	Streak      int    `json:"streak"`
}

// DoomLoopSerializedState is the plain-JSON detector state persisted inside
// ConversationState.DoomLoop so loop memory survives serialize → resume
// (provided the resuming call passes DoomLoop again).
//
// Decoding is deliberately lenient (see UnmarshalJSON): a corrupt detector
// blob never fails ConversationState deserialization; invalid entries are
// dropped, matching upstream's "a corrupt detector state must never take
// down a resumed run".
type DoomLoopSerializedState struct {
	// Tools holds per-tool streaks keyed by tool name.
	Tools map[string]DoomLoopStreak `json:"tools"`
	// Text is the cross-step assistant-text streak.
	Text *DoomLoopStreak `json:"text,omitempty"`
	// StopVerdict is the verdict that condemned this run, kept across
	// decision-only resumes and cleared by a fresh conversational turn.
	StopVerdict *DoomLoopVerdict `json:"stopVerdict,omitempty"`
	// PendingSteer is steer guidance queued but not yet injected when the
	// run paused; flushed into the conversation on resume.
	PendingSteer []string `json:"pendingSteer,omitempty"`
	// EscalationsUsed counts consumed escalations against MaxEscalations.
	EscalationsUsed int `json:"escalationsUsed,omitempty"`
}

// UnmarshalJSON decodes leniently: invalid shapes are ignored with a
// warning instead of returning an error.
func (s *DoomLoopSerializedState) UnmarshalJSON(data []byte) error {
	*s = DoomLoopSerializedState{Tools: map[string]DoomLoopStreak{}}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		log.Printf("[DoomLoop] Ignoring invalid persisted doom-loop state")
		return nil
	}
	if b, ok := raw["tools"]; ok {
		var tools map[string]json.RawMessage
		if json.Unmarshal(b, &tools) == nil {
			for name, entry := range tools {
				if streak, ok := decodeStreak(entry); ok {
					s.Tools[name] = streak
				}
			}
		}
	}
	if b, ok := raw["text"]; ok {
		if streak, ok := decodeStreak(b); ok {
			s.Text = &streak
		}
	}
	if b, ok := raw["stopVerdict"]; ok {
		var v DoomLoopVerdict
		if json.Unmarshal(b, &v) == nil && v.Action != "" {
			s.StopVerdict = &v
		}
	}
	if b, ok := raw["pendingSteer"]; ok {
		var items []any
		if json.Unmarshal(b, &items) == nil {
			for _, item := range items {
				if msg, ok := item.(string); ok {
					s.PendingSteer = append(s.PendingSteer, msg)
				}
			}
		}
	}
	if b, ok := raw["escalationsUsed"]; ok {
		var n float64
		if json.Unmarshal(b, &n) == nil && n >= 0 && !math.IsInf(n, 0) {
			s.EscalationsUsed = int(math.Floor(n))
		}
	}
	return nil
}

func decodeStreak(b json.RawMessage) (DoomLoopStreak, bool) {
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil || raw == nil {
		return DoomLoopStreak{}, false
	}
	fp, ok1 := raw["fingerprint"].(string)
	n, ok2 := raw["streak"].(float64)
	if !ok1 || !ok2 {
		return DoomLoopStreak{}, false
	}
	return DoomLoopStreak{Fingerprint: fp, Streak: int(n)}, true
}

// LoopKeyResolutionKind classifies a LoopKeyResolution.
type LoopKeyResolutionKind string

const (
	LoopKeyResolutionExempt   LoopKeyResolutionKind = "exempt"
	LoopKeyResolutionKey      LoopKeyResolutionKind = "key"
	LoopKeyResolutionFallback LoopKeyResolutionKind = "fallback"
)

// LoopKeyResolution is the result of resolving a tool's loop-key
// declaration against one call's arguments. See ResolveLoopKeyMaterial.
type LoopKeyResolution struct {
	Kind        LoopKeyResolutionKind
	KeyMaterial any
	// Warning is set for LoopKeyResolutionFallback.
	Warning string
}

//#endregion

//#region Config Resolution

var defaultDoomLoopTextOptions = ResolvedDoomLoopTextOptions{Enabled: true, MaxPeriodTokens: 16, MinRepeats: 4, MinCoveredTokens: 12, MaxWindowTokens: 400}

// ResolvedEscalationConfig is an escalation config with the budget resolved.
type ResolvedEscalationConfig struct {
	Model             string
	Advisor           bool
	AdvisorParameters *components.AdvisorServerToolConfig
	MaxEscalations    int
}

func (c *ResolvedEscalationConfig) hasAdvisor() bool {
	return c != nil && (c.Advisor || c.AdvisorParameters != nil)
}

// ResolvedDoomLoopTextOptions is DoomLoopTextOptions with defaults applied.
type ResolvedDoomLoopTextOptions struct {
	Enabled          bool
	MaxPeriodTokens  int
	MinRepeats       int
	MinCoveredTokens int
	MaxWindowTokens  int
}

// ResolvedDoomLoopConfig is the fully-resolved config used by the monitor.
// Every ladder rung is either a positive threshold or DoomLoopRungDisabled.
type ResolvedDoomLoopConfig struct {
	Ladder     DoomLoopLadder
	Text       ResolvedDoomLoopTextOptions
	Escalation *ResolvedEscalationConfig
}

func sanitizeThreshold(value, fallback DoomLoopThreshold) DoomLoopThreshold {
	switch {
	case value < 0:
		return DoomLoopRungDisabled
	case value > 0:
		return value
	default:
		return fallback
	}
}

// warnOnLadderHazards logs configurations that are accepted but probably
// not what the caller meant: block enabled with stop disabled (unbounded
// block/re-issue), and dead rungs that can never fire under strongest-wins.
func warnOnLadderHazards(ladder DoomLoopLadder) {
	if ladder.Block.enabled() && !ladder.Stop.enabled() {
		log.Printf("[DoomLoop] ladder has block enabled with stop disabled: a model that keeps " +
			"re-issuing a blocked call loops indefinitely (each blocked round still costs a " +
			"model request). Bound the run with StopWhen/MaxTurns, or enable the stop rung.")
	}
	names := []string{"observe", "steer", "escalate", "block", "stop"}
	values := []DoomLoopThreshold{ladder.Observe, ladder.Steer, ladder.Escalate, ladder.Block, ladder.Stop}
	for weak := range values {
		if !values[weak].enabled() {
			continue
		}
		for strong := weak + 1; strong < len(values); strong++ {
			if values[strong].enabled() && values[weak] >= values[strong] {
				log.Printf("[DoomLoop] ladder rung %q (%d) can never fire: stronger rung %q (%d) already wins at that streak (strongest crossed rung is applied).", names[weak], values[weak], names[strong], values[strong])
				break
			}
		}
	}
}

// ResolveDoomLoopOption normalizes CallModelInput.DoomLoop. nil → nil
// (detection off — the default posture).
func ResolveDoomLoopOption(option *DoomLoopConfig) *ResolvedDoomLoopConfig {
	if option == nil {
		return nil
	}
	defaults := DefaultDoomLoopLadder()
	ladder := DoomLoopLadder{
		Observe:  sanitizeThreshold(option.Ladder.Observe, defaults.Observe),
		Steer:    sanitizeThreshold(option.Ladder.Steer, defaults.Steer),
		Escalate: sanitizeThreshold(option.Ladder.Escalate, defaults.Escalate),
		Block:    sanitizeThreshold(option.Ladder.Block, defaults.Block),
		Stop:     sanitizeThreshold(option.Ladder.Stop, defaults.Stop),
	}
	warnOnLadderHazards(ladder)

	var escalation *ResolvedEscalationConfig
	in := option.Escalation
	hasAdvisor := in != nil && (in.Advisor || in.AdvisorParameters != nil)
	hasMechanism := in != nil && (in.Model != "" || hasAdvisor)
	if hasMechanism {
		maxEscalations := in.MaxEscalations
		if maxEscalations < 1 {
			maxEscalations = DefaultMaxEscalations
		}
		escalation = &ResolvedEscalationConfig{Model: in.Model, MaxEscalations: maxEscalations}
		if hasAdvisor {
			escalation.Advisor = true
			escalation.AdvisorParameters = in.AdvisorParameters
		}
		if !ladder.Escalate.enabled() {
			log.Printf("[DoomLoop] escalation config provided but the escalate ladder rung is disabled; " +
				"set Ladder.Escalate to a streak threshold for recovery to trigger.")
		}
	} else if ladder.Escalate.enabled() {
		log.Printf("[DoomLoop] Ladder.Escalate is enabled but no escalation config (Model/Advisor) was " +
			"provided; the rung is skipped and verdicts fall through to weaker rungs.")
	}

	return &ResolvedDoomLoopConfig{Ladder: ladder, Escalation: escalation, Text: resolveTextOptions(option.Text)}
}

func resolveTextOptions(in DoomLoopTextOptions) ResolvedDoomLoopTextOptions {
	out := defaultDoomLoopTextOptions
	out.Enabled = !in.Disabled
	if in.MaxPeriodTokens > 0 {
		out.MaxPeriodTokens = in.MaxPeriodTokens
	}
	if in.MinRepeats > 0 {
		out.MinRepeats = in.MinRepeats
	}
	if in.MinCoveredTokens > 0 {
		out.MinCoveredTokens = in.MinCoveredTokens
	}
	if in.MaxWindowTokens > 0 {
		out.MaxWindowTokens = in.MaxWindowTokens
	}
	return out
}

//#endregion

//#region loopKey

// ToolLoopKeyFn computes the key material identifying a call for doom-loop
// detection from the call's (decoded JSON) arguments. Return (nil, nil) to
// exempt THIS call (upstream `null`); return an error (or panic) to fall
// back to the full-arguments identity. Must be pure and deterministic.
type ToolLoopKeyFn func(args map[string]any) (any, error)

// ToolLoopKey is a tool's doom-loop identity declaration (upstream
// `loopKey`). A nil *ToolLoopKey means "absent": the full arguments object
// is the identity. Precedence: Exempt, then Fields (non-nil), then Func.
type ToolLoopKey struct {
	// Func computes key material per call.
	Func ToolLoopKeyFn
	// Fields is a declarative field subset of the arguments (data, not
	// code); missing fields are simply absent.
	Fields []string
	// Exempt statically exempts every call of the tool (upstream `false`).
	Exempt bool
}

// LoopKeyFields declares a field-subset loop key (upstream `['a', 'b']`).
func LoopKeyFields(fields ...string) *ToolLoopKey {
	return &ToolLoopKey{Fields: append([]string{}, fields...)}
}

// LoopKeyExempt statically exempts a tool from detection (upstream `false`).
func LoopKeyExempt() *ToolLoopKey { return &ToolLoopKey{Exempt: true} }

// LoopKeyFunc declares a typed loop-key function: the call's arguments are
// decoded into In before fn runs (a decode failure falls back to the full
// arguments). Return nil to exempt the call.
func LoopKeyFunc[In any](fn func(In) (any, error)) *ToolLoopKey {
	return &ToolLoopKey{Func: func(args map[string]any) (any, error) {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		var in In
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, err
		}
		return fn(in)
	}}
}

// loopKeyProvider is implemented by tools that carry a loop-key declaration
// (TypedTool via ToolConfig.LoopKey, and MarkMcp-wrapped tools).
type loopKeyProvider interface{ toolLoopKey() *ToolLoopKey }

func loopKeyOf(t Tool) *ToolLoopKey {
	if p, ok := t.(loopKeyProvider); ok {
		return p.toolLoopKey()
	}
	return nil
}

// ResolveLoopKeyMaterial resolves a loop-key declaration against one call's
// arguments:
//
//   - nil → the full arguments (LoopKeyResolutionKey).
//   - Exempt → LoopKeyResolutionExempt.
//   - Fields → the declared field subset (LoopKeyResolutionKey).
//   - Func → its result; nil exempts THIS call; an error or panic falls
//     back to the full arguments with a warning (detection must never take
//     down a run).
func ResolveLoopKeyMaterial(loopKey *ToolLoopKey, args map[string]any) (res LoopKeyResolution) {
	if loopKey == nil {
		return LoopKeyResolution{Kind: LoopKeyResolutionKey, KeyMaterial: args}
	}
	if loopKey.Exempt {
		return LoopKeyResolution{Kind: LoopKeyResolutionExempt}
	}
	if loopKey.Fields != nil {
		subset := map[string]any{}
		for _, field := range loopKey.Fields {
			if v, ok := args[field]; ok {
				subset[field] = v
			}
		}
		return LoopKeyResolution{Kind: LoopKeyResolutionKey, KeyMaterial: subset}
	}
	if loopKey.Func == nil {
		return LoopKeyResolution{Kind: LoopKeyResolutionFallback, KeyMaterial: args, Warning: "loopKey has unsupported shape (empty declaration); falling back to full arguments"}
	}
	defer func() {
		if r := recover(); r != nil {
			res = LoopKeyResolution{Kind: LoopKeyResolutionFallback, KeyMaterial: args, Warning: fmt.Sprintf("loopKey threw (%v); falling back to full arguments", r)}
		}
	}()
	key, err := loopKey.Func(args)
	if err != nil {
		return LoopKeyResolution{Kind: LoopKeyResolutionFallback, KeyMaterial: args, Warning: fmt.Sprintf("loopKey threw (%v); falling back to full arguments", err)}
	}
	if key == nil {
		return LoopKeyResolution{Kind: LoopKeyResolutionExempt}
	}
	return LoopKeyResolution{Kind: LoopKeyResolutionKey, KeyMaterial: key}
}

//#endregion

//#region Canonicalization & Fingerprinting

// MaxCanonicalizeDepth is the maximum nesting depth accepted by
// CanonicalizeKeyMaterial.
const MaxCanonicalizeDepth = 64

// ErrUncanonicalizable is wrapped by every CanonicalizeKeyMaterial error.
var ErrUncanonicalizable = errors.New("cannot canonicalize key material for doom-loop fingerprinting")

// CanonicalizeKeyMaterial renders value as RFC 8785 (JCS) canonical JSON:
// object keys sorted by UTF-16 code units (recursively), arrays in order,
// strings and numbers serialized exactly as ECMAScript JSON.stringify does
// (-0 → 0, 1e21 → 1e+21, lone surrogates → \udXXX). Byte-identical to the
// TypeScript and Python ports for the same key material.
//
// Values outside the JSON data model are mapped the way JSON.stringify maps
// them: func/chan entries are dropped from objects and serialize as null in
// arrays. Integer types are converted to float64 (ECMAScript numbers);
// structs and other typed values go through encoding/json first.
//
// Returns an error wrapping ErrUncanonicalizable for values RFC 8785 cannot
// represent — non-finite numbers, *big.Int / complex numbers (upstream's
// bigint), circular references, and nesting deeper than
// MaxCanonicalizeDepth. Real tool arguments (decoded JSON) never trigger
// these; the engine falls back to the full-arguments identity when a
// computed loop key does.
func CanonicalizeKeyMaterial(value any) (string, error) {
	c := &canonicalizer{seen: map[uintptr]bool{}}
	var b strings.Builder
	if err := c.write(&b, value, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

type canonicalizer struct{ seen map[uintptr]bool }

func (c *canonicalizer) write(b *strings.Builder, v any, depth int) error {
	if depth > MaxCanonicalizeDepth {
		return fmt.Errorf("%w: nested deeper than %d levels", ErrUncanonicalizable, MaxCanonicalizeDepth)
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return nil
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case string:
		writeJCSString(b, x)
		return nil
	case float64:
		return writeJCSNumber(b, x)
	case float32:
		return writeJCSNumber(b, float64(x))
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return fmt.Errorf("%w: invalid json.Number %q", ErrUncanonicalizable, string(x))
		}
		return writeJCSNumber(b, f)
	case *big.Int, big.Int, *big.Float, *big.Rat:
		return fmt.Errorf("%w: big numbers have no RFC 8785 representation (convert to string or number in the loop key)", ErrUncanonicalizable)
	case map[string]any:
		return c.writeMap(b, reflect.ValueOf(x), depth)
	case []any:
		return c.writeSlice(b, reflect.ValueOf(x), depth)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return writeJCSNumber(b, float64(rv.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return writeJCSNumber(b, float64(rv.Uint()))
	case reflect.Float32, reflect.Float64:
		return writeJCSNumber(b, rv.Float())
	case reflect.Bool:
		return c.write(b, rv.Bool(), depth)
	case reflect.String:
		writeJCSString(b, rv.String())
		return nil
	case reflect.Complex64, reflect.Complex128:
		return fmt.Errorf("%w: complex numbers have no RFC 8785 representation", ErrUncanonicalizable)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		b.WriteString("null")
		return nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if c.seen[ptr] {
				return fmt.Errorf("%w: circular reference", ErrUncanonicalizable)
			}
			c.seen[ptr] = true
			defer delete(c.seen, ptr)
		}
		return c.write(b, rv.Elem().Interface(), depth)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			return c.writeMap(b, rv, depth)
		}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			break // []byte: encoding/json renders base64, follow it
		}
		return c.writeSlice(b, rv, depth)
	}
	// Structs, non-string-keyed maps, []byte, json.Marshaler values: map
	// through encoding/json's data model, then canonicalize that.
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUncanonicalizable, err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("%w: %v", ErrUncanonicalizable, err)
	}
	return c.write(b, decoded, depth)
}

func (c *canonicalizer) enter(rv reflect.Value) (func(), error) {
	if rv.IsNil() || (rv.Kind() == reflect.Slice && rv.Len() == 0) {
		return func() {}, nil
	}
	ptr := rv.Pointer()
	if c.seen[ptr] {
		return nil, fmt.Errorf("%w: circular reference", ErrUncanonicalizable)
	}
	c.seen[ptr] = true
	return func() { delete(c.seen, ptr) }, nil
}

func (c *canonicalizer) writeMap(b *strings.Builder, rv reflect.Value, depth int) error {
	if rv.IsNil() {
		b.WriteString("null")
		return nil
	}
	leave, err := c.enter(rv)
	if err != nil {
		return err
	}
	defer leave()
	type entry struct {
		key   string
		units []uint16
		value any
	}
	entries := make([]entry, 0, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		val := iter.Value().Interface()
		if isDroppedJSONValue(val) {
			continue
		}
		key := iter.Key().String()
		entries = append(entries, entry{key: key, units: utf16.Encode([]rune(key)), value: val})
	}
	sort.Slice(entries, func(i, j int) bool { return compareUTF16(entries[i].units, entries[j].units) < 0 })
	b.WriteByte('{')
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJCSString(b, e.key)
		b.WriteByte(':')
		if err := c.write(b, e.value, depth+1); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

func (c *canonicalizer) writeSlice(b *strings.Builder, rv reflect.Value, depth int) error {
	if rv.Kind() == reflect.Slice {
		if rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		leave, err := c.enter(rv)
		if err != nil {
			return err
		}
		defer leave()
	}
	b.WriteByte('[')
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := c.write(b, rv.Index(i).Interface(), depth+1); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

// isDroppedJSONValue mirrors JSON.stringify dropping function/symbol object
// entries (Go: func and chan values).
func isDroppedJSONValue(v any) bool {
	if v == nil {
		return false
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return true
	}
	return false
}

func compareUTF16(a, b []uint16) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// writeJCSNumber serializes a finite float64 exactly as ECMAScript's
// Number::toString does (which is the JCS number serialization).
func writeJCSNumber(b *strings.Builder, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w: non-finite number (NaN/Infinity) has no RFC 8785 representation", ErrUncanonicalizable)
	}
	if f == 0 {
		b.WriteByte('0') // also collapses -0
		return nil
	}
	if f < 0 {
		b.WriteByte('-')
		f = -f
	}
	// Shortest round-trip digits and decimal exponent.
	sci := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±XX
	mantissa, expPart, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	k := len(digits)
	n := exp + 1
	switch {
	case k <= n && n <= 21:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		b.WriteString(digits[:n])
		b.WriteByte('.')
		b.WriteString(digits[n:])
	case -6 < n && n <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -n))
		b.WriteString(digits)
	default:
		e := n - 1
		b.WriteByte(digits[0])
		if k > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if e >= 0 {
			b.WriteByte('+')
		} else {
			b.WriteByte('-')
			e = -e
		}
		b.WriteString(strconv.Itoa(e))
	}
	return nil
}

// writeJCSString serializes s as JSON.stringify would: only `"`, `\`, and
// control characters are escaped (short forms for \b \f \n \r \t, \u00xx
// otherwise); everything else is emitted literally. Go strings are UTF-8,
// so an unpaired UTF-16 surrogate can only appear as its WTF-8 byte
// sequence (ED A0..BF xx); those are escaped as \udxxx exactly like
// JSON.stringify's well-formed output. Any other invalid byte becomes
// U+FFFD, matching encoding/json.
func writeJCSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					fmt.Fprintf(b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
				unit := 0xD000 | (uint16(s[i+1]&0x3F) << 6) | uint16(s[i+2]&0x3F)
				fmt.Fprintf(b, `\u%04x`, unit)
				i += 3
				continue
			}
			b.WriteString("\uFFFD")
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	b.WriteByte('"')
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// FingerprintKeyMaterial is sha256(utf8(jcs(keyMaterial))), lowercase hex.
func FingerprintKeyMaterial(keyMaterial any) (string, error) {
	canonical, err := CanonicalizeKeyMaterial(keyMaterial)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

// FingerprintToolCall is sha256(utf8(toolName + "\n" + jcs(keyMaterial))),
// lowercase hex — the cross-port tool-call fingerprint contract.
func FingerprintToolCall(toolName string, keyMaterial any) (string, error) {
	canonical, err := CanonicalizeKeyMaterial(keyMaterial)
	if err != nil {
		return "", err
	}
	return sha256Hex(toolName + "\n" + canonical), nil
}

//#endregion

//#region Text Repetition Detection

// TextRepetitionResult is a detected repeating token block at the tail of a
// response.
type TextRepetitionResult struct {
	// Repeats is the number of consecutive repeats (feeds the ladder).
	Repeats int
	// PeriodTokens is the block length in whitespace-delimited tokens.
	PeriodTokens int
	// CoveredTokens is Repeats × PeriodTokens.
	CoveredTokens int
	// Sample is the repeating block itself.
	Sample string
}

// DetectTextRepetition detects a period-p token block repeating at the
// tail of text ("I am stuck. I am stuck. …"). Pure and deterministic:
// whitespace tokenization, suffix comparison. Only the trailing
// MaxWindowTokens×64 characters (UTF-16 code units, as upstream) are
// examined. Ties on covered tokens prefer the smallest period. Returns nil
// when nothing meets MinRepeats and MinCoveredTokens. Zero-valued options
// use the defaults; Disabled is ignored here.
//
// KNOWN LIMIT: token blocks must repeat exactly; paraphrased loops do not
// trip this detector.
func DetectTextRepetition(text string, options DoomLoopTextOptions) *TextRepetitionResult {
	opts := resolveTextOptions(options)
	charBudget := opts.MaxWindowTokens * 64
	tail := tailUTF16Units(text, charBudget)
	tokens := strings.FieldsFunc(tail, isJSSpace)
	if len(tokens) > opts.MaxWindowTokens {
		tokens = tokens[len(tokens)-opts.MaxWindowTokens:]
	}
	minTokens := opts.MinCoveredTokens
	if minTokens < 2 {
		minTokens = 2
	}
	if len(tokens) < minTokens {
		return nil
	}
	var best *TextRepetitionResult
	maxPeriod := opts.MaxPeriodTokens
	if half := len(tokens) / 2; half < maxPeriod {
		maxPeriod = half
	}
	for period := 1; period <= maxPeriod; period++ {
		repeats := 1
	outer:
		for start := len(tokens) - 2*period; start >= 0; start -= period {
			for i := 0; i < period; i++ {
				if tokens[start+i] != tokens[len(tokens)-period+i] {
					break outer
				}
			}
			repeats++
		}
		covered := repeats * period
		if repeats >= opts.MinRepeats && covered >= opts.MinCoveredTokens {
			if best == nil || covered > best.CoveredTokens {
				best = &TextRepetitionResult{Repeats: repeats, PeriodTokens: period, CoveredTokens: covered, Sample: strings.Join(tokens[len(tokens)-period:], " ")}
			}
		}
	}
	return best
}

// isJSSpace reports whether r is in ECMAScript's `\s` class (WhiteSpace +
// LineTerminator), which upstream uses to tokenize and normalize text.
// It differs from unicode.IsSpace in two code points: U+FEFF counts as
// whitespace here, U+0085 (NEL) does not. Matching it keeps text
// fingerprints identical to the TypeScript port.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// tailUTF16Units returns the suffix of s spanning at most budget UTF-16 code
// units (upstream slices the JS string by code units).
func tailUTF16Units(s string, budget int) string {
	units := 0
	i := len(s)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		n := 1
		if r >= 0x10000 {
			n = 2
		}
		if units+n > budget {
			break
		}
		units += n
		i -= size
	}
	return s[i:]
}

//#endregion

//#region Ladder Resolution

// ResolveLadderOptions tunes ResolveLadderAction.
type ResolveLadderOptions struct {
	// NonBlocking makes block unavailable (text and server-tool verdicts:
	// the tokens are already emitted / the tool already ran); a block-level
	// streak falls through to escalate/steer/observe, while stop still stops.
	NonBlocking bool
	// NoEscalate disables the escalate rung for this resolution (no
	// mechanism configured or budget exhausted).
	NoEscalate bool
}

// ResolveLadderAction maps a streak onto the strongest crossed ladder rung,
// or "" when no rung is crossed.
func ResolveLadderAction(ladder DoomLoopLadder, streak int, opts ResolveLadderOptions) DoomLoopAction {
	meets := func(t DoomLoopThreshold) bool { return t.enabled() && streak >= int(t) }
	switch {
	case meets(ladder.Stop):
		return DoomLoopActionStop
	case !opts.NonBlocking && meets(ladder.Block):
		return DoomLoopActionBlock
	case !opts.NoEscalate && meets(ladder.Escalate):
		return DoomLoopActionEscalate
	case meets(ladder.Steer):
		return DoomLoopActionSteer
	case meets(ladder.Observe):
		return DoomLoopActionObserve
	}
	return ""
}

var doomLoopActionStrength = map[DoomLoopAction]int{
	DoomLoopActionObserve: 0, DoomLoopActionSteer: 1, DoomLoopActionEscalate: 2, DoomLoopActionBlock: 3, DoomLoopActionStop: 4,
}

func strongerVerdict(a, b *DoomLoopVerdict) *DoomLoopVerdict {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// Within-response wins ties: its message names the repeated block.
	if doomLoopActionStrength[b.Action] > doomLoopActionStrength[a.Action] {
		return b
	}
	return a
}

//#endregion

//#region Monitor

type streakEntry struct {
	DoomLoopStreak
	round    int
	hasRound bool
}

// RecordToolCallOptions tunes DoomLoopMonitor.RecordToolCall.
type RecordToolCallOptions struct {
	// NonBlocking is set for post-execution records (server tools): block
	// is meaningless.
	NonBlocking bool
	// Detector labels the verdict; default DoomLoopDetectorToolFingerprint.
	Detector DoomLoopDetectorKind
}

// DoomLoopMonitor is the pure state machine over the recorded transcript:
// feed it tool calls and assistant texts, get verdicts back. It holds no
// engine concerns (hook emission, blocking, steering, stopping live in
// ModelResult). State is bounded plain JSON and round-trips through
// ConversationState. Safe for concurrent use.
type DoomLoopMonitor struct {
	mu              sync.Mutex
	config          ResolvedDoomLoopConfig
	tools           map[string]streakEntry
	text            *DoomLoopStreak
	escalationsUsed int
}

// NewDoomLoopMonitor constructs a monitor, optionally restoring persisted state.
func NewDoomLoopMonitor(config ResolvedDoomLoopConfig, initial *DoomLoopSerializedState) *DoomLoopMonitor {
	m := &DoomLoopMonitor{config: config, tools: map[string]streakEntry{}}
	if initial != nil {
		m.Restore(initial)
	}
	return m
}

// CanEscalate reports whether the escalate rung can still fire: a mechanism
// is configured and the budget is not exhausted.
func (m *DoomLoopMonitor) CanEscalate() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canEscalateLocked()
}

func (m *DoomLoopMonitor) canEscalateLocked() bool {
	return m.config.Escalation != nil && m.escalationsUsed < m.config.Escalation.MaxEscalations
}

// ConsumeEscalation burns one unit of the escalation budget. The engine
// calls it when it actually APPLIES a recovery, not at verdict time.
func (m *DoomLoopMonitor) ConsumeEscalation() {
	m.mu.Lock()
	m.escalationsUsed++
	m.mu.Unlock()
}

// State snapshots the serializable detector state (deep copy). Round
// markers are dropped (rounds are meaningful only within one run);
// StopVerdict/PendingSteer are owned by the engine.
func (m *DoomLoopMonitor) State() DoomLoopSerializedState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := DoomLoopSerializedState{Tools: make(map[string]DoomLoopStreak, len(m.tools)), EscalationsUsed: m.escalationsUsed}
	for name, entry := range m.tools {
		out.Tools[name] = entry.DoomLoopStreak
	}
	if m.text != nil {
		text := *m.text
		out.Text = &text
	}
	return out
}

// Restore replaces the detector state from a persisted blob. Engine-owned
// fields (StopVerdict, PendingSteer) are ignored here. Entries with an
// empty fingerprint are dropped.
func (m *DoomLoopMonitor) Restore(state *DoomLoopSerializedState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = map[string]streakEntry{}
	m.text = nil
	m.escalationsUsed = 0
	if state == nil {
		return
	}
	for name, entry := range state.Tools {
		if entry.Fingerprint == "" {
			continue
		}
		// round intentionally absent: the first resumed record increments.
		m.tools[name] = streakEntry{DoomLoopStreak: entry}
	}
	if state.Text != nil && state.Text.Fingerprint != "" {
		text := *state.Text
		m.text = &text
	}
	if state.EscalationsUsed > 0 {
		m.escalationsUsed = state.EscalationsUsed
	}
}

// RecordToolCall records one tool call and returns the streak record plus
// any verdict.
//
//   - Per tool: interleaved calls to other tools do not reset a tool's streak.
//   - Per round: the same (tool, fingerprint) recorded again in the SAME
//     round is a duplicate (no increment, DuplicateInRound true).
//   - A different fingerprint for the same tool resets the streak to 1.
//
// Returns the canonicalization error for unhashable key material; the
// engine catches it and falls back.
func (m *DoomLoopMonitor) RecordToolCall(toolName string, keyMaterial any, round int, opts RecordToolCallOptions) (DoomLoopCallRecord, error) {
	fingerprint, err := FingerprintToolCall(toolName, keyMaterial)
	if err != nil {
		return DoomLoopCallRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, ok := m.tools[toolName]
	streak := 1
	duplicate := false
	if ok && previous.Fingerprint == fingerprint {
		if previous.hasRound && previous.round == round {
			streak = previous.Streak
			duplicate = true
		} else {
			streak = previous.Streak + 1
		}
	}
	m.tools[toolName] = streakEntry{DoomLoopStreak: DoomLoopStreak{Fingerprint: fingerprint, Streak: streak}, round: round, hasRound: true}

	action := ResolveLadderAction(m.config.Ladder, streak, ResolveLadderOptions{NonBlocking: opts.NonBlocking, NoEscalate: !m.canEscalateLocked()})
	record := DoomLoopCallRecord{Fingerprint: fingerprint, Streak: streak, DuplicateInRound: duplicate}
	if action == "" {
		return record, nil
	}
	detector := opts.Detector
	if detector == "" {
		detector = DoomLoopDetectorToolFingerprint
	}
	record.Verdict = &DoomLoopVerdict{
		Detector:    detector,
		Action:      action,
		Streak:      streak,
		Fingerprint: fingerprint,
		ToolName:    toolName,
		Message: fmt.Sprintf("Doom loop suspected: tool \"%s\" was invoked in %d consecutive rounds "+
			"with identical arguments (fingerprint %s…). Repeating the call "+
			"will not change the result. Take a different approach, or explain why repetition is required.", toolName, streak, fingerprint[:16]),
	}
	return record, nil
}

// RecordAssistantText records one step's assistant text and returns the
// strongest verdict from the two text detectors (within-response
// text-repetition and cross-step text-streak). Empty/whitespace-only text
// is a no-op: it neither counts nor resets the cross-step streak.
func (m *DoomLoopMonitor) RecordAssistantText(text string) (*DoomLoopVerdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.config.Text.Enabled {
		return nil, nil
	}
	normalized := strings.Join(strings.FieldsFunc(text, isJSSpace), " ")
	if normalized == "" {
		return nil, nil
	}
	textOpts := DoomLoopTextOptions{MaxPeriodTokens: m.config.Text.MaxPeriodTokens, MinRepeats: m.config.Text.MinRepeats, MinCoveredTokens: m.config.Text.MinCoveredTokens, MaxWindowTokens: m.config.Text.MaxWindowTokens}

	var within *DoomLoopVerdict
	if rep := DetectTextRepetition(normalized, textOpts); rep != nil {
		action := ResolveLadderAction(m.config.Ladder, rep.Repeats, ResolveLadderOptions{NonBlocking: true, NoEscalate: !m.canEscalateLocked()})
		if action != "" {
			fp, err := FingerprintKeyMaterial(rep.Sample)
			if err != nil {
				return nil, err
			}
			within = &DoomLoopVerdict{
				Detector:    DoomLoopDetectorTextRepetition,
				Action:      action,
				Streak:      rep.Repeats,
				Fingerprint: fp,
				Message:     fmt.Sprintf("Doom loop suspected: the response repeats \"%s\" %d times in a row. Stop repeating and take a different approach.", rep.Sample, rep.Repeats),
			}
		}
	}

	fingerprint, err := FingerprintKeyMaterial(normalized)
	if err != nil {
		return nil, err
	}
	streak := 1
	if m.text != nil && m.text.Fingerprint == fingerprint {
		streak = m.text.Streak + 1
	}
	m.text = &DoomLoopStreak{Fingerprint: fingerprint, Streak: streak}
	var cross *DoomLoopVerdict
	if action := ResolveLadderAction(m.config.Ladder, streak, ResolveLadderOptions{NonBlocking: true, NoEscalate: !m.canEscalateLocked()}); action != "" {
		cross = &DoomLoopVerdict{
			Detector:    DoomLoopDetectorTextStreak,
			Action:      action,
			Streak:      streak,
			Fingerprint: fingerprint,
			Message:     fmt.Sprintf("Doom loop suspected: the assistant produced identical text for %d consecutive turns. Stop repeating and take a different approach.", streak),
		}
	}
	return strongerVerdict(within, cross), nil
}

//#endregion
