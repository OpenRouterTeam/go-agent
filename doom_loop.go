package agent

// Port of upstream src/lib/doom-loop.ts.
//
// Doom-loop detection for the tool-execution loop.
//
// A "doom loop" is a run that stops making progress while continuing to
// spend: the model re-issues the same tool call with identical arguments
// (including repeated empty calls and repeated unparseable calls), or emits
// the same text tokens over and over. This file provides the deterministic
// detection primitives; the engine wires them into the loop behind the
// doom-loop option.
//
// Design principles (identical to upstream):
//
//   - Deterministic. Every verdict is a pure function of the recorded
//     sequence of tool calls / assistant texts.
//   - Cross-port fingerprints. Key material is canonicalized per RFC 8785
//     (JCS) and hashed with SHA-256 over the UTF-8 bytes. The shared vector
//     file testdata/doom-loop-fingerprints.json is the conformance contract.
//     Unlike upstream (async WebCrypto), hashing here is synchronous
//     (crypto/sha256) and byte-identical.
//   - Tool-declared identity via [ToolLoopKey].
//   - Round-scoped streaks (see [DoomLoopMonitor.DeclareRound]).
//   - Graduated response: observe → steer → escalate → block → stop.
//   - Never the cause of failure: internal errors degrade to a fallback
//     identity or skip detection for that call.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// doomLoopWarnf is the warning sink (upstream: console.warn). A package
// variable so tests can capture warnings.
var doomLoopWarnf = log.Printf

//#region Public Types

// DoomLoopAction is an escalation action, weakest to strongest:
//
//   - observe  — emit the DoomLoopDetected hook only.
//   - steer    — inject a corrective user message before the next model turn;
//     the call still runs.
//   - escalate — swap to a stronger model and/or force an advisor consult on
//     the NEXT turn, per the escalation config. Bounded by MaxEscalations.
//   - block    — refuse the tool call and synthesize an error
//     function_call_output.
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

// DoomLoopDetectorKind names the detector that produced a verdict.
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
// fingerprint detectors the number of consecutive rounds in which that tool
// was called with the same fingerprint set (or the per-call count, whichever
// is larger); for text-repetition the number of consecutive repeats of a token
// block within one response; for text-streak the number of consecutive steps
// with identical assistant text.
type DoomLoopVerdict struct {
	Detector DoomLoopDetectorKind `json:"detector"`
	Action   DoomLoopAction       `json:"action"`
	Streak   int                  `json:"streak"`
	// Fingerprint is the deterministic fingerprint of the repeated unit.
	Fingerprint string `json:"fingerprint"`
	// ToolName is present for fingerprint verdicts.
	ToolName string `json:"toolName,omitempty"`
	// Message is a human/model-readable explanation, used verbatim for block
	// outputs and steer messages.
	Message string `json:"message"`
}

// DoomLoopCallRecord is the result of recording one tool call.
//
// DuplicateInRound is true when the same (toolName, fingerprint) was already
// recorded in this round — the streak did NOT increment and the caller should
// reuse the decision it applied for the first occurrence.
type DoomLoopCallRecord struct {
	Fingerprint      string
	Streak           int
	DuplicateInRound bool
	// Verdict is nil when no ladder rung was crossed.
	Verdict *DoomLoopVerdict
}

// DoomLoopThreshold is one ladder rung's streak threshold.
//
// In a [DoomLoopLadder] passed as configuration, the zero value means "use the
// default", [DoomLoopDisabled] disables the rung (upstream `false`), and any
// value >= 1 is the threshold. (Upstream treats other invalid numbers — e.g.
// negatives — as "use the default"; in Go every negative value disables.)
type DoomLoopThreshold int

// DoomLoopDisabled disables a ladder rung (upstream `false`).
const DoomLoopDisabled DoomLoopThreshold = -1

// Enabled reports whether the threshold denotes an active rung.
func (t DoomLoopThreshold) Enabled() bool { return t >= 1 }

// DoomLoopLadder holds streak thresholds for each action. When several rungs
// are crossed the strongest wins (stop > block > escalate > steer > observe).
// Thresholds compare with >=.
type DoomLoopLadder struct {
	Observe  DoomLoopThreshold `json:"observe,omitempty"`
	Steer    DoomLoopThreshold `json:"steer,omitempty"`
	Escalate DoomLoopThreshold `json:"escalate,omitempty"`
	Block    DoomLoopThreshold `json:"block,omitempty"`
	Stop     DoomLoopThreshold `json:"stop,omitempty"`
}

// DoomLoopEscalationConfig configures the escalate rung.
type DoomLoopEscalationConfig struct {
	// Model is the model slug to run the NEXT turn on ("" = unset).
	Model string
	// Advisor forces an openrouter:advisor consult on the next turn. nil or
	// false = unset; true = defaults; map[string]any = advisor parameters.
	Advisor any
	// MaxEscalations caps escalations per run. Values < 1 use
	// [DefaultMaxEscalations].
	MaxEscalations int
}

// DoomLoopTextOptions tunes within-response text-repetition detection.
// Zero-valued integer fields use the defaults.
type DoomLoopTextOptions struct {
	// Enabled is the master switch for both text detectors. nil = true.
	Enabled *bool
	// MaxPeriodTokens is the longest repeating token block searched. Default 16.
	MaxPeriodTokens int
	// MinRepeats is the minimum consecutive repeats of a block. Default 4.
	MinRepeats int
	// MinCoveredTokens is the minimum total tokens covered. Default 12.
	MinCoveredTokens int
	// MaxWindowTokens: only the trailing N tokens are scanned. Default 400.
	MaxWindowTokens int
}

// DoomLoopConfig configures doom-loop detection. The upstream `doomLoop: true`
// option corresponds to &DoomLoopConfig{} (all defaults); `false`/absent
// corresponds to a nil *DoomLoopConfig.
type DoomLoopConfig struct {
	Ladder DoomLoopLadder
	// Text tunes text-loop detection; nil = defaults (on). To disable
	// (upstream `text: false`) set Text.Enabled to false.
	Text *DoomLoopTextOptions
	// Escalation configures the escalate rung; nil = off.
	Escalation *DoomLoopEscalationConfig
}

// DoomLoopStreak is a consecutive-repetition counter for one identity.
type DoomLoopStreak struct {
	Fingerprint string `json:"fingerprint"`
	Streak      int    `json:"streak"`
	// RoundFingerprints is the full fingerprint set of the tool's last round,
	// present only when that round had more than one distinct call.
	RoundFingerprints []string `json:"roundFingerprints,omitempty"`
	// CallStreaks maps fingerprint → consecutive rounds that exact call has
	// been issued in (per-call detector evidence). Omitted when
	// reconstructible from Fingerprint/Streak/RoundFingerprints.
	CallStreaks map[string]int `json:"callStreaks,omitempty"`
}

// DoomLoopSerializedState is plain-JSON detector state, persisted inside the
// conversation state so loop memory survives serialize → resume.
type DoomLoopSerializedState struct {
	// Tools holds per-tool streaks keyed by tool name.
	Tools map[string]DoomLoopStreak `json:"tools"`
	// Text is the cross-step assistant-text streak.
	Text *DoomLoopStreak `json:"text,omitempty"`
	// StopVerdict is the verdict that condemned this run (engine-owned).
	StopVerdict *DoomLoopVerdict `json:"stopVerdict,omitempty"`
	// PendingSteer is steer guidance queued but not injected (engine-owned).
	PendingSteer []string `json:"pendingSteer,omitempty"`
	// EscalationsUsed counts consumed escalations.
	EscalationsUsed int `json:"escalationsUsed,omitempty"`
}

// LoopKeyResolutionKind discriminates [LoopKeyResolution].
type LoopKeyResolutionKind string

const (
	LoopKeyExempt   LoopKeyResolutionKind = "exempt"
	LoopKeyKey      LoopKeyResolutionKind = "key"
	LoopKeyFallback LoopKeyResolutionKind = "fallback"
)

// LoopKeyResolution is the result of resolving a tool's [ToolLoopKey]
// against one call's arguments. KeyMaterial is set for Key and Fallback;
// Warning is set for Fallback.
type LoopKeyResolution struct {
	Kind        LoopKeyResolutionKind
	KeyMaterial any
	Warning     string
}

//#endregion

//#region ToolLoopKey

// ToolLoopKey declares a tool's loop identity (upstream `loopKey`): a function
// over one call's validated arguments that returns the key material
// identifying the call, e.g.
//
//	func(args map[string]any) (any, error) {
//		return map[string]any{"command": args["command"], "cwd": args["cwd"]}, nil
//	}
//
// The module canonicalizes (RFC 8785) and hashes the returned material, so
// fingerprints are uniform across tools and ports.
//
//   - A nil ToolLoopKey (upstream: absent) uses the full arguments.
//   - Returning [ErrLoopKeyExempt] exempts THIS call (upstream: return null).
//     [ExemptToolLoopKey] exempts every call (upstream: `loopKey: false`).
//   - Returning (nil, nil) falls back to the full arguments with a warning
//     (upstream: return undefined — almost always a bug).
//   - Returning any other error, or panicking, falls back to the full
//     arguments with a warning (upstream: a throw). A [*LoopKeyFallbackError]
//     supplies the warning text verbatim.
//
// [LoopKeyFields] builds the field-list form (upstream `['command','cwd']`).
type ToolLoopKey func(args map[string]any) (keyMaterial any, err error)

// ErrLoopKeyExempt, returned from a [ToolLoopKey], exempts the call from
// doom-loop detection.
var ErrLoopKeyExempt = errors.New("doom loop: call exempt from loop detection")

// LoopKeyFallbackError, returned from a [ToolLoopKey], requests the
// full-arguments identity with Warning as the warning text.
type LoopKeyFallbackError struct{ Warning string }

func (e *LoopKeyFallbackError) Error() string { return e.Warning }

// ExemptToolLoopKey exempts every call of a tool (upstream `loopKey: false`).
func ExemptToolLoopKey(map[string]any) (any, error) { return nil, ErrLoopKeyExempt }

// LoopKeyFields returns the field-list loop key (upstream `loopKey: [...]`):
// identity is the subset of the named argument fields. An empty list, or a
// list whose fields are all absent from the arguments, falls back to the full
// arguments with a warning (it would collapse every call onto one identity).
func LoopKeyFields(fields ...string) ToolLoopKey {
	fields = append([]string(nil), fields...)
	return func(args map[string]any) (any, error) {
		if len(fields) == 0 {
			return nil, &LoopKeyFallbackError{Warning: "loopKey is an empty field list, which would give every call to this tool the same identity; falling back to full arguments. Use ExemptToolLoopKey to exempt the tool."}
		}
		subset := map[string]any{}
		for _, field := range fields {
			if v, ok := args[field]; ok {
				subset[field] = v
			}
		}
		if len(subset) == 0 {
			return nil, &LoopKeyFallbackError{Warning: fmt.Sprintf("loopKey fields [%s] are all absent from the arguments, which would give every call the same identity; falling back to full arguments.", strings.Join(fields, ", "))}
		}
		return subset, nil
	}
}

// ResolveLoopKeyMaterial resolves a tool's loop key against one call's
// arguments. It never fails: degenerate declarations fall back to the full
// arguments with a warning.
func ResolveLoopKeyMaterial(loopKey ToolLoopKey, args map[string]any) LoopKeyResolution {
	if loopKey == nil {
		return LoopKeyResolution{Kind: LoopKeyKey, KeyMaterial: args}
	}
	result, err := callLoopKey(loopKey, args)
	if err != nil {
		if errors.Is(err, ErrLoopKeyExempt) {
			return LoopKeyResolution{Kind: LoopKeyExempt}
		}
		var fb *LoopKeyFallbackError
		if errors.As(err, &fb) {
			return LoopKeyResolution{Kind: LoopKeyFallback, KeyMaterial: args, Warning: fb.Warning}
		}
		return LoopKeyResolution{
			Kind:        LoopKeyFallback,
			KeyMaterial: args,
			Warning:     fmt.Sprintf("loopKey threw (%s); falling back to full arguments", err.Error()),
		}
	}
	if result == nil {
		return LoopKeyResolution{
			Kind:        LoopKeyFallback,
			KeyMaterial: args,
			Warning:     "loopKey returned nil; falling back to full arguments. Return ErrLoopKeyExempt to exempt a call, or a value to use as identity.",
		}
	}
	return LoopKeyResolution{Kind: LoopKeyKey, KeyMaterial: result}
}

func callLoopKey(loopKey ToolLoopKey, args map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("%v", r)
		}
	}()
	return loopKey(args)
}

//#endregion

//#region Defaults & Config Resolution

// DefaultDoomLoopLadder is the default ladder: observe at 2 consecutive
// identical rounds, block at 3, stop at 6. Steer and escalate are off.
// Treat as read-only.
var DefaultDoomLoopLadder = DoomLoopLadder{
	Observe:  2,
	Steer:    DoomLoopDisabled,
	Escalate: DoomLoopDisabled,
	Block:    3,
	Stop:     6,
}

// defaultDoomLoopLadder is the immutable internal copy used for resolution.
var defaultDoomLoopLadder = DefaultDoomLoopLadder

// DefaultMaxEscalations is the escalation budget when unset.
const DefaultMaxEscalations = 2

// MaxCanonicalizeDepth is the maximum nesting depth accepted by
// [CanonicalizeKeyMaterial].
const MaxCanonicalizeDepth = 64

var defaultTextOptions = ResolvedDoomLoopTextOptions{
	Enabled:          true,
	MaxPeriodTokens:  16,
	MinRepeats:       4,
	MinCoveredTokens: 12,
	MaxWindowTokens:  400,
}

// ResolvedDoomLoopTextOptions is the fully-resolved text detector config.
type ResolvedDoomLoopTextOptions struct {
	Enabled          bool
	MaxPeriodTokens  int
	MinRepeats       int
	MinCoveredTokens int
	MaxWindowTokens  int
}

// ResolvedEscalationConfig is an escalation config with the budget resolved.
type ResolvedEscalationConfig struct {
	Model          string
	Advisor        any
	MaxEscalations int
}

// ResolvedDoomLoopConfig is the fully-resolved config used by the monitor.
// Every Ladder rung is either >= 1 or [DoomLoopDisabled].
type ResolvedDoomLoopConfig struct {
	Ladder     DoomLoopLadder
	Text       ResolvedDoomLoopTextOptions
	Escalation *ResolvedEscalationConfig
}

func sanitizeThreshold(value, fallback DoomLoopThreshold) DoomLoopThreshold {
	switch {
	case value < 0:
		return DoomLoopDisabled
	case value >= 1:
		return value
	default:
		return fallback
	}
}

func (l DoomLoopLadder) rungs() [5]struct {
	name      string
	threshold DoomLoopThreshold
} {
	return [5]struct {
		name      string
		threshold DoomLoopThreshold
	}{
		{"observe", l.Observe},
		{"steer", l.Steer},
		{"escalate", l.Escalate},
		{"block", l.Block},
		{"stop", l.Stop},
	}
}

func warnOnLadderHazards(ladder DoomLoopLadder) {
	if ladder.Block.Enabled() && !ladder.Stop.Enabled() {
		doomLoopWarnf("[DoomLoop] ladder has block enabled with stop disabled: a model that keeps " +
			"re-issuing a blocked call loops indefinitely (each blocked round still costs a " +
			"model request). Bound the run with StopWhen, or enable the stop rung.")
	}
	rungs := ladder.rungs()
	for weak := 0; weak < len(rungs); weak++ {
		if !rungs[weak].threshold.Enabled() {
			continue
		}
		for strong := weak + 1; strong < len(rungs); strong++ {
			if rungs[strong].threshold.Enabled() && rungs[weak].threshold >= rungs[strong].threshold {
				doomLoopWarnf("[DoomLoop] ladder rung %q (%d) can never fire: stronger rung %q (%d) already wins at that streak (strongest crossed rung is applied).",
					rungs[weak].name, rungs[weak].threshold, rungs[strong].name, rungs[strong].threshold)
				break
			}
		}
	}
}

func advisorEnabled(advisor any) bool {
	if advisor == nil {
		return false
	}
	if b, ok := advisor.(bool); ok && !b {
		return false
	}
	return true
}

// ResolveDoomLoopOption normalizes the doom-loop option. nil → nil
// (detection off). &DoomLoopConfig{} is upstream's `true` (all defaults).
func ResolveDoomLoopOption(option *DoomLoopConfig) *ResolvedDoomLoopConfig {
	if option == nil {
		return nil
	}
	in := option.Ladder
	ladder := DoomLoopLadder{
		Observe:  sanitizeThreshold(in.Observe, defaultDoomLoopLadder.Observe),
		Steer:    sanitizeThreshold(in.Steer, defaultDoomLoopLadder.Steer),
		Escalate: sanitizeThreshold(in.Escalate, defaultDoomLoopLadder.Escalate),
		Block:    sanitizeThreshold(in.Block, defaultDoomLoopLadder.Block),
		Stop:     sanitizeThreshold(in.Stop, defaultDoomLoopLadder.Stop),
	}
	warnOnLadderHazards(ladder)

	var escalation *ResolvedEscalationConfig
	esc := option.Escalation
	hasAdvisor := esc != nil && advisorEnabled(esc.Advisor)
	hasMechanism := esc != nil && (esc.Model != "" || hasAdvisor)
	if hasMechanism {
		escalation = &ResolvedEscalationConfig{Model: esc.Model, MaxEscalations: DefaultMaxEscalations}
		if hasAdvisor {
			escalation.Advisor = esc.Advisor
		}
		if esc.MaxEscalations >= 1 {
			escalation.MaxEscalations = esc.MaxEscalations
		}
		if !ladder.Escalate.Enabled() {
			doomLoopWarnf("[DoomLoop] escalation config provided but the escalate ladder rung is disabled; " +
				"set Ladder.Escalate to a streak threshold for recovery to trigger.")
		}
	} else if ladder.Escalate.Enabled() {
		doomLoopWarnf("[DoomLoop] Ladder.Escalate is enabled but no escalation config (model/advisor) was " +
			"provided; the rung is skipped and verdicts fall through to weaker rungs.")
	}

	return &ResolvedDoomLoopConfig{
		Ladder:     ladder,
		Escalation: escalation,
		Text:       resolveTextOptions(option.Text),
	}
}

func resolveTextOptions(in *DoomLoopTextOptions) ResolvedDoomLoopTextOptions {
	out := defaultTextOptions
	if in == nil {
		return out
	}
	if in.Enabled != nil {
		out.Enabled = *in.Enabled
	}
	if in.MaxPeriodTokens != 0 {
		out.MaxPeriodTokens = in.MaxPeriodTokens
	}
	if in.MinRepeats != 0 {
		out.MinRepeats = in.MinRepeats
	}
	if in.MinCoveredTokens != 0 {
		out.MinCoveredTokens = in.MinCoveredTokens
	}
	if in.MaxWindowTokens != 0 {
		out.MaxWindowTokens = in.MaxWindowTokens
	}
	return out
}

//#endregion

//#region Canonicalization & Fingerprinting

// CanonicalizeKeyMaterial returns RFC 8785 (JCS) canonical JSON for value:
// object keys sorted by UTF-16 code units (recursively), arrays in order,
// strings and finite numbers serialized per ECMAScript JSON.stringify (e.g.
// -0 → 0, 1e21 → 1e+21, lone surrogates → \udXXX escapes).
//
// Accepted inputs follow the JSON data model as produced by encoding/json
// decoding into any (map[string]any, []any, float64, string, bool, nil), plus
// json.Number, Go integer/float kinds, maps with string keys, slices/arrays
// and pointers. Other values (structs, json.Marshaler implementations) are
// converted through encoding/json first. Func/chan values are dropped from
// objects and serialize as null in arrays (JSON.stringify semantics).
//
// Strings are interpreted as UTF-8; WTF-8-encoded surrogates (how a Go string
// can carry a JS lone surrogate) are escaped as \udXXX like JSON.stringify,
// and other invalid bytes become U+FFFD. Note encoding/json itself decodes a
// lone "\ud800" escape to U+FFFD, so such inputs differ from upstream unless
// decoded with surrogate preservation.
//
// Returns an error for non-finite numbers, complex numbers, cyclic values and
// nesting deeper than [MaxCanonicalizeDepth].
func CanonicalizeKeyMaterial(value any) (string, error) {
	c := canonicalizer{seen: map[seenKey]struct{}{}}
	var b strings.Builder
	if err := c.canon(&b, value, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

type seenKey struct {
	ptr uintptr
	len int
	typ reflect.Type
}

type canonicalizer struct {
	seen map[seenKey]struct{}
}

var errDoomLoopDepth = fmt.Errorf("cannot canonicalize key material nested deeper than %d levels for doom-loop fingerprinting", MaxCanonicalizeDepth)

func (c *canonicalizer) canon(b *strings.Builder, v any, depth int) error {
	if depth > MaxCanonicalizeDepth {
		return errDoomLoopDepth
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return nil
	case string:
		writeJSString(b, x)
		return nil
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case float64:
		return writeJSNumber(b, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("cannot canonicalize json.Number %q for doom-loop fingerprinting: %w", string(x), err)
		}
		return writeJSNumber(b, f)
	case map[string]any:
		return c.canonObject(b, reflect.ValueOf(x), depth)
	case []any:
		return c.canonArray(b, reflect.ValueOf(x), depth)
	}
	return c.canonReflect(b, reflect.ValueOf(v), depth)
}

var jsonMarshalerType = reflect.TypeFor[json.Marshaler]()

func (c *canonicalizer) canonReflect(b *strings.Builder, rv reflect.Value, depth int) error {
	if !rv.IsValid() {
		b.WriteString("null")
		return nil
	}
	if rv.Type().Implements(jsonMarshalerType) && !(rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return c.canonViaJSON(b, rv.Interface(), depth)
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		if rv.Kind() == reflect.Pointer {
			key := seenKey{ptr: rv.Pointer(), typ: rv.Type()}
			if _, ok := c.seen[key]; ok {
				return errors.New("cannot canonicalize circular key material for doom-loop fingerprinting")
			}
			c.seen[key] = struct{}{}
			defer delete(c.seen, key)
		}
		return c.canon(b, rv.Elem().Interface(), depth)
	case reflect.String:
		writeJSString(b, rv.String())
		return nil
	case reflect.Bool:
		return c.canon(b, rv.Bool(), depth)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return writeJSNumber(b, float64(rv.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return writeJSNumber(b, float64(rv.Uint()))
	case reflect.Float32, reflect.Float64:
		return writeJSNumber(b, rv.Float())
	case reflect.Complex64, reflect.Complex128:
		return errors.New("cannot canonicalize complex number for doom-loop fingerprinting: RFC 8785 has no representation")
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return c.canonViaJSON(b, rv.Interface(), depth)
		}
		return c.canonObject(b, rv, depth)
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return c.canonViaJSON(b, rv.Interface(), depth) // base64 string, like encoding/json
		}
		return c.canonArray(b, rv, depth)
	case reflect.Array:
		return c.canonArray(b, rv, depth)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		b.WriteString("null")
		return nil
	default:
		return c.canonViaJSON(b, rv.Interface(), depth)
	}
}

func (c *canonicalizer) canonViaJSON(b *strings.Builder, v any, depth int) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cannot canonicalize key material for doom-loop fingerprinting: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("cannot canonicalize key material for doom-loop fingerprinting: %w", err)
	}
	return c.canon(b, decoded, depth)
}

func isDroppedKind(v reflect.Value) bool {
	for v.Kind() == reflect.Interface && !v.IsNil() {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return true
	}
	return false
}

func (c *canonicalizer) enter(rv reflect.Value) (seenKey, error) {
	key := seenKey{ptr: rv.Pointer(), typ: rv.Type()}
	if rv.Kind() == reflect.Slice {
		key.len = rv.Len()
	}
	if key.ptr == 0 {
		return key, nil
	}
	if _, ok := c.seen[key]; ok {
		return key, errors.New("cannot canonicalize circular key material for doom-loop fingerprinting")
	}
	c.seen[key] = struct{}{}
	return key, nil
}

func (c *canonicalizer) canonObject(b *strings.Builder, rv reflect.Value, depth int) error {
	key, err := c.enter(rv)
	if err != nil {
		return err
	}
	defer delete(c.seen, key)
	type kv struct {
		key   string
		units []uint16
		val   reflect.Value
	}
	entries := make([]kv, 0, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		val := iter.Value()
		if isDroppedKind(val) {
			continue
		}
		k := iter.Key().String()
		entries = append(entries, kv{key: k, units: jsUTF16Units(k), val: val})
	}
	sort.Slice(entries, func(i, j int) bool { return compareUnits(entries[i].units, entries[j].units) < 0 })
	b.WriteByte('{')
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSString(b, e.key)
		b.WriteByte(':')
		if err := c.canon(b, e.val.Interface(), depth+1); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

func (c *canonicalizer) canonArray(b *strings.Builder, rv reflect.Value, depth int) error {
	if rv.Kind() == reflect.Slice {
		key, err := c.enter(rv)
		if err != nil {
			return err
		}
		defer delete(c.seen, key)
	}
	b.WriteByte('[')
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := c.canon(b, rv.Index(i).Interface(), depth+1); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

func compareUnits(a, b []uint16) int {
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

// decodeWTF8 decodes one code point at s[i:]. Surrogate code points encoded
// in WTF-8 (ED A0..BF xx) are returned as-is with surrogate=true; any other
// invalid byte yields (utf8.RuneError, 1, false).
func decodeWTF8(s string, i int) (r rune, size int, surrogate bool) {
	r, size = utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && size == 1 && i+2 < len(s) && s[i] == 0xED &&
		s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		r = 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
		return r, 3, true
	}
	return r, size, false
}

// forEachJSCodePoint iterates s as a JS string would see it: WTF-8 surrogate
// pairs combine into one code point; lone surrogates are reported with
// lone=true.
func forEachJSCodePoint(s string, fn func(r rune, lone bool)) {
	for i := 0; i < len(s); {
		r, size, sur := decodeWTF8(s, i)
		if sur && r >= 0xD800 && r <= 0xDBFF && i+size < len(s) {
			if r2, size2, sur2 := decodeWTF8(s, i+size); sur2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
				fn(utf16.DecodeRune(r, r2), false)
				i += size + size2
				continue
			}
		}
		fn(r, sur)
		i += size
	}
}

func jsUTF16Units(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	forEachJSCodePoint(s, func(r rune, lone bool) {
		if lone || r < 0x10000 {
			units = append(units, uint16(r))
			return
		}
		r1, r2 := utf16.EncodeRune(r)
		units = append(units, uint16(r1), uint16(r2))
	})
	return units
}

// jsUTF16Len returns the JS string length (UTF-16 code units) of s.
func jsUTF16Len(s string) int {
	n := 0
	forEachJSCodePoint(s, func(r rune, lone bool) {
		if !lone && r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	})
	return n
}

// toWellFormedUTF8 mirrors TextEncoder: each lone surrogate (and each invalid
// byte) becomes U+FFFD.
func toWellFormedUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	forEachJSCodePoint(s, func(r rune, lone bool) {
		if lone {
			r = utf8.RuneError
		}
		b.WriteRune(r)
	})
	return b.String()
}

const lowerHex = "0123456789abcdef"

// writeJSString writes s as JSON.stringify would (well-formed variant).
func writeJSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	forEachJSCodePoint(s, func(r rune, lone bool) {
		if lone {
			b.WriteString(`\u`)
			b.WriteByte(lowerHex[r>>12&0xF])
			b.WriteByte(lowerHex[r>>8&0xF])
			b.WriteByte(lowerHex[r>>4&0xF])
			b.WriteByte(lowerHex[r&0xF])
			return
		}
		switch r {
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
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(lowerHex[r>>4])
				b.WriteByte(lowerHex[r&0xF])
				return
			}
			b.WriteRune(r)
		}
	})
	b.WriteByte('"')
}

// writeJSNumber writes f as ECMAScript Number::toString (the JCS number
// serialization): shortest round-trip digits, fixed notation for exponents
// in [-7, 20], otherwise d.ddde±n.
func writeJSNumber(b *strings.Builder, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("cannot canonicalize non-finite number (NaN/Infinity) for doom-loop fingerprinting: RFC 8785 has no representation")
	}
	b.WriteString(formatJSNumber(f))
	return nil
}

func formatJSNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	var sign string
	if f < 0 {
		sign = "-"
		f = -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.dddde±XX
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		es := "+"
		if n-1 < 0 {
			es = "-"
		}
		ev := strconv.Itoa(absInt(n - 1))
		if k == 1 {
			out = digits + "e" + es + ev
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + es + ev
		}
	}
	return sign + out
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(toWellFormedUTF8(s)))
	return hex.EncodeToString(sum[:])
}

// FingerprintKeyMaterial returns sha256(utf8(jcs(keyMaterial))) as lowercase
// hex (64 chars). Synchronous; upstream returns a Promise.
func FingerprintKeyMaterial(keyMaterial any) (string, error) {
	canonical, err := CanonicalizeKeyMaterial(keyMaterial)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

// FingerprintToolCall returns sha256(utf8(toolName + "\n" + jcs(keyMaterial)))
// as lowercase hex. The tool name participates so equal arguments to
// different tools never share a fingerprint.
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
	// Repeats is the consecutive repeats of the block (feeds the ladder).
	Repeats int
	// PeriodTokens is the block length in whitespace-delimited tokens.
	PeriodTokens int
	// CoveredTokens is Repeats × PeriodTokens.
	CoveredTokens int
	// Sample is the repeating block itself.
	Sample string
}

// isJSWhitespace reports whether r matches the ECMAScript \s class.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// jsTailUTF16 returns the last n UTF-16 code units of s (JS
// `s.slice(-n)`); a split surrogate pair leaves its low half as a WTF-8 lone
// surrogate, exactly as a JS string would.
func jsTailUTF16(s string, n int) string {
	type cp struct {
		start int
		units int
		r     rune
	}
	var cps []cp
	for i := 0; i < len(s); {
		start := i
		r, size, sur := decodeWTF8(s, i)
		units := 1
		if !sur && r >= 0x10000 {
			units = 2
		}
		// Combine WTF-8 pairs into one 2-unit code point.
		if sur && r >= 0xD800 && r <= 0xDBFF && i+size < len(s) {
			if r2, size2, sur2 := decodeWTF8(s, i+size); sur2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
				r = utf16.DecodeRune(r, r2)
				units = 2
				size += size2
			}
		}
		cps = append(cps, cp{start: start, units: units, r: r})
		i += size
	}
	count := 0
	for j := len(cps) - 1; j >= 0; j-- {
		if count+cps[j].units > n {
			if count < n { // split pair: keep the low surrogate
				_, lo := utf16.EncodeRune(cps[j].r)
				end := len(s)
				if j+1 < len(cps) {
					end = cps[j+1].start
				}
				return encodeWTF8Surrogate(lo) + s[end:]
			}
			if j+1 < len(cps) {
				return s[cps[j+1].start:]
			}
			return ""
		}
		count += cps[j].units
	}
	return s
}

func encodeWTF8Surrogate(r rune) string {
	return string([]byte{0xED, byte(0x80 | (r>>6)&0x3F), byte(0x80 | r&0x3F)})
}

// jsSplitWhitespace mirrors `s.trim().split(/\s+/).filter(Boolean)`.
func jsSplitWhitespace(s string) []string {
	return strings.FieldsFunc(s, isJSWhitespace)
}

// jsNormalizeWhitespace mirrors `s.replace(/\s+/g, ' ').trim()`.
func jsNormalizeWhitespace(s string) string {
	return strings.Join(jsSplitWhitespace(s), " ")
}

// DetectTextRepetition detects a period-p token block repeating at the tail
// of text — "I am stuck. I am stuck. I am stuck. …". Pure and deterministic.
//
// Only the trailing region is examined: 64 UTF-16 code units per window token
// are sliced off the tail before tokenization. Ties on covered tokens prefer
// the smallest period. Returns nil when no block meets MinRepeats and
// MinCoveredTokens within MaxPeriodTokens / MaxWindowTokens. Zero-valued
// option fields use the defaults (Enabled is ignored).
//
// KNOWN LIMIT: token blocks must repeat exactly.
func DetectTextRepetition(text string, options DoomLoopTextOptions) *TextRepetitionResult {
	opts := resolveTextOptions(&options)
	charBudget := opts.MaxWindowTokens * 64
	tail := text
	if jsUTF16Len(text) > charBudget {
		tail = jsTailUTF16(text, charBudget)
	}
	tokens := jsSplitWhitespace(tail)
	if len(tokens) > opts.MaxWindowTokens {
		tokens = tokens[len(tokens)-opts.MaxWindowTokens:]
	}
	if len(tokens) < max(opts.MinCoveredTokens, 2) {
		return nil
	}

	var best *TextRepetitionResult
	maxPeriod := min(opts.MaxPeriodTokens, len(tokens)/2)
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
				best = &TextRepetitionResult{
					Repeats:       repeats,
					PeriodTokens:  period,
					CoveredTokens: covered,
					Sample:        strings.Join(tokens[len(tokens)-period:], " "),
				}
			}
		}
	}
	return best
}

//#endregion

//#region Ladder Resolution

// LadderActionOptions controls [ResolveLadderAction].
type LadderActionOptions struct {
	// AllowBlock false is used for text and server-tool verdicts: a
	// block-level streak falls through to weaker rungs; stop still stops.
	AllowBlock bool
	// DisallowEscalate disables the escalate rung (upstream
	// `allowEscalate: false`; the zero value allows it, matching upstream's
	// default of true).
	DisallowEscalate bool
}

// ResolveLadderAction maps a streak onto the strongest crossed ladder rung.
// Returns "" when no rung is crossed (upstream: undefined).
func ResolveLadderAction(ladder DoomLoopLadder, streak int, options LadderActionOptions) DoomLoopAction {
	meets := func(t DoomLoopThreshold) bool { return t.Enabled() && streak >= int(t) }
	switch {
	case meets(ladder.Stop):
		return DoomLoopActionStop
	case options.AllowBlock && meets(ladder.Block):
		return DoomLoopActionBlock
	case !options.DisallowEscalate && meets(ladder.Escalate):
		return DoomLoopActionEscalate
	case meets(ladder.Steer):
		return DoomLoopActionSteer
	case meets(ladder.Observe):
		return DoomLoopActionObserve
	}
	return ""
}

//#endregion

//#region Monitor

// streakEntry is the in-memory streak entry: serialized shape plus run-local
// round bookkeeping (see upstream StreakEntry for the full rationale).
type streakEntry struct {
	fingerprint string
	streak      int
	// hasRound/round: round of the most recent record; never serialized.
	hasRound bool
	round    int
	// roundFingerprints: the round's complete (sorted) fingerprint set.
	roundFingerprints []string
	// seenThisRound: fingerprints already recorded this round.
	seenThisRound map[string]struct{}
	// priorRoundFingerprints/priorStreak: the previous round's set and streak.
	priorRoundFingerprints []string
	priorStreak            int
	// priorCallStreaks: previous round's per-call streaks.
	priorCallStreaks map[string]int
	// callStreaks: current round's per-call streaks.
	callStreaks map[string]int
}

func callStreaksReconstructible(e *streakEntry) bool {
	if e.callStreaks == nil {
		return true
	}
	set := e.roundFingerprints
	if set == nil {
		set = []string{e.fingerprint}
	}
	if len(e.callStreaks) != len(set) {
		return false
	}
	for key, count := range e.callStreaks {
		if !containsString(set, key) || count != e.streak {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func setsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DoomLoopRoundCall is one call declared to [DoomLoopMonitor.DeclareRound].
type DoomLoopRoundCall struct {
	ToolName    string
	KeyMaterial any
}

// RecordToolCallOptions tunes [DoomLoopMonitor.RecordToolCall].
type RecordToolCallOptions struct {
	// DisallowBlock marks post-execution records (server tools), for which
	// block is meaningless (upstream `allowBlock: false`).
	DisallowBlock bool
	// Detector labels the verdict; "" = tool-fingerprint.
	Detector DoomLoopDetectorKind
}

// DoomLoopMonitor is a pure state machine over the recorded transcript: feed
// it tool calls and assistant texts, get verdicts back. Engine concerns (hook
// emission, blocking, steering, stopping) live in the model loop.
//
// Safe for concurrent use: all state is guarded by a mutex.
type DoomLoopMonitor struct {
	mu              sync.Mutex
	config          ResolvedDoomLoopConfig
	tools           map[string]*streakEntry
	text            *streakEntry
	escalationsUsed int
	declaredRound   *declaredRound
}

type declaredRound struct {
	round        int
	fingerprints map[string][]string
}

// NewDoomLoopMonitor creates a monitor. config must be non-nil (use
// ResolveDoomLoopOption(&DoomLoopConfig{}) for defaults). initialState, when
// non-nil, is passed to [DoomLoopMonitor.Restore].
func NewDoomLoopMonitor(config *ResolvedDoomLoopConfig, initialState any) *DoomLoopMonitor {
	m := &DoomLoopMonitor{tools: map[string]*streakEntry{}}
	if config != nil {
		m.config = *config
	} else {
		m.config = *ResolveDoomLoopOption(&DoomLoopConfig{})
	}
	if initialState != nil {
		m.Restore(initialState)
	}
	return m
}

// DeclareRound declares the complete set of calls a round will make, before
// any of them is recorded. Idempotent per round and safe to skip (per-call
// detection needs no declaration). Unhashable key material is excluded with
// a warning. Never serialized.
func (m *DoomLoopMonitor) DeclareRound(round int, calls []DoomLoopRoundCall) {
	collected := map[string]map[string]struct{}{}
	for _, call := range calls {
		fp, err := FingerprintToolCall(call.ToolName, call.KeyMaterial)
		if err != nil {
			doomLoopWarnf("[DoomLoop] could not fingerprint a %q call while declaring round %d; excluding it from the round set: %v", call.ToolName, round, err)
			continue
		}
		set := collected[call.ToolName]
		if set == nil {
			set = map[string]struct{}{}
			collected[call.ToolName] = set
		}
		set[fp] = struct{}{}
	}
	fingerprints := make(map[string][]string, len(collected))
	for name, set := range collected {
		list := make([]string, 0, len(set))
		for fp := range set {
			list = append(list, fp)
		}
		sort.Strings(list)
		fingerprints[name] = list
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declaredRound = &declaredRound{round: round, fingerprints: fingerprints}
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

// ConsumeEscalation consumes one escalation from the budget. The engine calls
// this when it actually applies the recovery, not at verdict time.
func (m *DoomLoopMonitor) ConsumeEscalation() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.escalationsUsed++
}

// GetState snapshots the serializable detector state (deep copy). Round
// markers are dropped. StopVerdict/PendingSteer are engine-owned and left
// empty here.
func (m *DoomLoopMonitor) GetState() DoomLoopSerializedState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := DoomLoopSerializedState{Tools: make(map[string]DoomLoopStreak, len(m.tools))}
	for name, e := range m.tools {
		s := DoomLoopStreak{Fingerprint: e.fingerprint, Streak: e.streak}
		if len(e.roundFingerprints) > 1 {
			s.RoundFingerprints = append([]string(nil), e.roundFingerprints...)
		}
		if e.callStreaks != nil && !callStreaksReconstructible(e) {
			s.CallStreaks = make(map[string]int, len(e.callStreaks))
			for k, v := range e.callStreaks {
				s.CallStreaks[k] = v
			}
		}
		state.Tools[name] = s
	}
	if m.text != nil {
		state.Text = &DoomLoopStreak{Fingerprint: m.text.fingerprint, Streak: m.text.streak}
	}
	if m.escalationsUsed > 0 {
		state.EscalationsUsed = m.escalationsUsed
	}
	return state
}

// Restore replaces detector state from a persisted blob: a
// DoomLoopSerializedState (value or pointer), json.RawMessage, or the generic
// JSON shape (map[string]any). Invalid blobs are ignored with a warning, and
// malformed entries are dropped or clamped individually — a corrupt state
// must never take down a resumed run. Engine-owned fields are ignored.
func (m *DoomLoopMonitor) Restore(state any) {
	obj, ok := normalizePersistedState(state)
	if !ok {
		doomLoopWarnf("[DoomLoop] Ignoring invalid persisted doom-loop state")
		return
	}
	tools := map[string]*streakEntry{}
	if rawTools, ok := obj["tools"].(map[string]any); ok {
		for name, raw := range rawTools {
			if entry, ok := raw.(map[string]any); ok {
				if fp, streak, ok := validStreak(entry); ok {
					tools[name] = restoreStreakEntry(fp, streak, entry)
				}
			}
		}
	}
	var text *streakEntry
	if rawText, ok := obj["text"].(map[string]any); ok {
		if fp, streak, ok := validStreak(rawText); ok {
			// Upstream restores the text streak verbatim (no range clamping);
			// Go truncates to int, bounded only to keep the conversion defined.
			text = &streakEntry{fingerprint: fp, streak: int(math.Max(math.Min(streak, 1e9), -1e9))}
		}
	}
	escalations := 0
	if n, ok := obj["escalationsUsed"].(float64); ok && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0 {
		escalations = int(math.Floor(math.Min(n, 1e9)))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = tools
	m.text = text
	m.escalationsUsed = escalations
}

func normalizePersistedState(state any) (map[string]any, bool) {
	if state == nil {
		return nil, false
	}
	var raw []byte
	switch s := state.(type) {
	case map[string]any:
		return s, true
	case json.RawMessage:
		raw = s
	default:
		var err error
		raw, err = json.Marshal(state)
		if err != nil {
			return nil, false
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	obj, ok := decoded.(map[string]any)
	return obj, ok
}

func validStreak(entry map[string]any) (string, float64, bool) {
	fp, ok := entry["fingerprint"].(string)
	if !ok {
		return "", 0, false
	}
	streak, ok := toFiniteFloat(entry["streak"])
	if !ok {
		return "", 0, false
	}
	return fp, streak, true
}

func toFiniteFloat(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func restoreStreakEntry(fingerprint string, rawStreak float64, entry map[string]any) *streakEntry {
	set := []string{fingerprint}
	if list, ok := entry["roundFingerprints"].([]any); ok && len(list) > 1 {
		strs := make([]string, 0, len(list))
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				strs = nil
				break
			}
			strs = append(strs, s)
		}
		if strs != nil {
			sort.Strings(strs)
			set = strs
		}
	}
	persisted := map[string]int{}
	if counts, ok := entry["callStreaks"].(map[string]any); ok {
		for fp, raw := range counts {
			if n, ok := toFiniteFloat(raw); ok && n >= 1 {
				persisted[fp] = int(math.Floor(math.Min(n, 1e9)))
			}
		}
	}
	streak := int(math.Min(math.Max(math.Floor(rawStreak), 1), 1_000_000))
	if len(persisted) == 0 {
		for _, member := range set {
			persisted[member] = streak
		}
	}
	return &streakEntry{
		fingerprint:       fingerprint,
		streak:            streak,
		roundFingerprints: set,
		callStreaks:       persisted,
	}
}

// RecordToolCall records one tool call and returns the streak record plus
// any verdict.
//
//   - Per tool: interleaved calls to other tools do not reset a tool's streak.
//   - Per round: the same (tool, fingerprint) recorded again in the SAME round
//     is a duplicate (DuplicateInRound; streak does not increment).
//   - A round's identity is the SET of fingerprints the tool was called with
//     (declare it first via DeclareRound); a differing set resets to 1.
//   - A per-call streak (consecutive rounds this exact call was issued in)
//     runs alongside; the larger of the two decides.
//
// Returns the canonicalization error for unhashable key material; callers
// must fall back (see ResolveLoopKeyMaterial).
func (m *DoomLoopMonitor) RecordToolCall(toolName string, keyMaterial any, round int, options *RecordToolCallOptions) (DoomLoopCallRecord, error) {
	fingerprint, err := FingerprintToolCall(toolName, keyMaterial)
	if err != nil {
		return DoomLoopCallRecord{}, err
	}
	var opts RecordToolCallOptions
	if options != nil {
		opts = *options
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous := m.tools[toolName]
	isSameRound := previous != nil && previous.hasRound && previous.round == round

	var declared []string
	if m.declaredRound != nil && m.declaredRound.round == round {
		declared = m.declaredRound.fingerprints[toolName]
	}
	declaredMember := containsString(declared, fingerprint)

	var seen map[string]struct{}
	if isSameRound {
		seen = previous.seenThisRound
	}
	_, duplicateInRound := seen[fingerprint]

	var priorSet []string
	priorStreak := 0
	var priorCallStreaks map[string]int
	switch {
	case isSameRound:
		priorSet = previous.priorRoundFingerprints
		priorStreak = previous.priorStreak
		priorCallStreaks = previous.priorCallStreaks
	case previous != nil:
		priorSet = previous.roundFingerprints
		if priorSet == nil {
			priorSet = []string{previous.fingerprint}
		}
		priorStreak = previous.streak
		priorCallStreaks = previous.callStreaks
	}
	if priorCallStreaks == nil {
		priorCallStreaks = map[string]int{}
	}
	score := func(set []string) int {
		if priorSet != nil && setsMatch(priorSet, set) {
			return priorStreak + 1
		}
		return 1
	}
	callStreak := priorCallStreaks[fingerprint] + 1

	callSet := []string{fingerprint}
	if declaredMember {
		callSet = declared
	}
	streak := score(callSet)
	roundSet := declared
	if roundSet == nil {
		roundSet = []string{fingerprint}
	}

	entryFingerprint := fingerprint
	if declared != nil && !declaredMember && previous != nil {
		entryFingerprint = previous.fingerprint
	}
	if seen == nil {
		seen = map[string]struct{}{}
	}
	seen[fingerprint] = struct{}{}
	var callStreaks map[string]int
	if isSameRound && previous.callStreaks != nil {
		callStreaks = previous.callStreaks
	} else {
		callStreaks = map[string]int{}
	}
	callStreaks[fingerprint] = callStreak

	m.tools[toolName] = &streakEntry{
		fingerprint:            entryFingerprint,
		hasRound:               true,
		round:                  round,
		seenThisRound:          seen,
		roundFingerprints:      roundSet,
		streak:                 score(roundSet),
		priorRoundFingerprints: priorSet,
		priorStreak:            priorStreak,
		priorCallStreaks:       priorCallStreaks,
		callStreaks:            callStreaks,
	}

	effective := max(streak, callStreak)
	action := ResolveLadderAction(m.config.Ladder, effective, LadderActionOptions{
		AllowBlock:       !opts.DisallowBlock,
		DisallowEscalate: !m.canEscalateLocked(),
	})
	record := DoomLoopCallRecord{Fingerprint: fingerprint, Streak: effective, DuplicateInRound: duplicateInRound}
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
		Streak:      effective,
		Fingerprint: fingerprint,
		ToolName:    toolName,
		Message:     buildToolVerdictMessage(toolName, fingerprint, callSet, streak, callStreak, declaredMember),
	}
	return record, nil
}

// ResetTextStreak clears the cross-step text streak (called by the engine
// when a late async-tool result is injected: observable progress). Tool
// streaks are kept.
func (m *DoomLoopMonitor) ResetTextStreak() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.text = nil
}

// RecordAssistantText records one step's assistant text and returns the
// strongest verdict from the two text detectors (text-repetition within this
// response; text-streak across consecutive steps), or nil. Empty or
// whitespace-only text is a no-op.
func (m *DoomLoopMonitor) RecordAssistantText(text string) *DoomLoopVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.config.Text.Enabled {
		return nil
	}
	normalized := jsNormalizeWhitespace(text)
	if normalized == "" {
		return nil
	}
	t := m.config.Text
	var within *DoomLoopVerdict
	if rep := DetectTextRepetition(normalized, DoomLoopTextOptions{
		MaxPeriodTokens:  t.MaxPeriodTokens,
		MinRepeats:       t.MinRepeats,
		MinCoveredTokens: t.MinCoveredTokens,
		MaxWindowTokens:  t.MaxWindowTokens,
	}); rep != nil {
		action := ResolveLadderAction(m.config.Ladder, rep.Repeats, LadderActionOptions{DisallowEscalate: !m.canEscalateLocked()})
		if action != "" {
			within = &DoomLoopVerdict{
				Detector:    DoomLoopDetectorTextRepetition,
				Action:      action,
				Streak:      rep.Repeats,
				Fingerprint: sha256Hex(canonicalString(rep.Sample)),
				// Raw interpolation inside quotes, as upstream (not %q).
				Message: "Doom loop suspected: the response repeats \"" + rep.Sample + "\" " +
					strconv.Itoa(rep.Repeats) + " times in a row. Stop repeating and take a different approach.",
			}
		}
	}

	fingerprint := sha256Hex(canonicalString(normalized))
	streak := 1
	if m.text != nil && m.text.fingerprint == fingerprint {
		streak = m.text.streak + 1
	}
	m.text = &streakEntry{fingerprint: fingerprint, streak: streak}
	var cross *DoomLoopVerdict
	if action := ResolveLadderAction(m.config.Ladder, streak, LadderActionOptions{DisallowEscalate: !m.canEscalateLocked()}); action != "" {
		cross = &DoomLoopVerdict{
			Detector:    DoomLoopDetectorTextStreak,
			Action:      action,
			Streak:      streak,
			Fingerprint: fingerprint,
			Message: "Doom loop suspected: the assistant produced identical text for " + strconv.Itoa(streak) +
				" consecutive turns. Stop repeating and take a different approach.",
		}
	}
	return strongerVerdict(within, cross)
}

func canonicalString(s string) string {
	var b strings.Builder
	writeJSString(&b, s)
	return b.String()
}

var actionStrength = map[DoomLoopAction]int{
	DoomLoopActionObserve:  0,
	DoomLoopActionSteer:    1,
	DoomLoopActionEscalate: 2,
	DoomLoopActionBlock:    3,
	DoomLoopActionStop:     4,
}

func strongerVerdict(a, b *DoomLoopVerdict) *DoomLoopVerdict {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// Within-response wins ties: its message names the repeated block.
	if actionStrength[b.Action] > actionStrength[a.Action] {
		return b
	}
	return a
}

func summarizeRound(fingerprints []string) string {
	shown := make([]string, 0, 3)
	for i := 0; i < len(fingerprints) && i < 3; i++ {
		shown = append(shown, prefixString(fingerprints[i], 8))
	}
	if len(fingerprints) > 3 {
		return fmt.Sprintf("%s+%d more…", strings.Join(shown, "+"), len(fingerprints)-3)
	}
	return strings.Join(shown, "+") + "…"
}

func prefixString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func buildToolVerdictMessage(toolName, fingerprint string, callSet []string, roundStreak, callStreak int, roundDeclared bool) string {
	if callStreak > roundStreak || !roundDeclared {
		return "Doom loop suspected: this exact \"" + toolName + "\" call has been repeated across " +
			"consecutive rounds. Repeating it will not change the result. " +
			"Take a different approach, or explain why repetition is required."
	}
	if len(callSet) > 1 {
		return fmt.Sprintf("Doom loop suspected: tool \"%s\" was invoked in %d consecutive rounds "+
			"with the same set of %d parallel calls (round identity %s). Reissuing the same fan-out "+
			"will not change the results. Take a different approach, or explain why repetition is required.",
			toolName, roundStreak, len(callSet), summarizeRound(callSet))
	}
	return fmt.Sprintf("Doom loop suspected: tool \"%s\" was invoked in %d consecutive rounds "+
		"with identical arguments (fingerprint %s…). Repeating the call "+
		"will not change the result. Take a different approach, or explain why repetition is required.",
		toolName, roundStreak, prefixString(fingerprint, 16))
}

//#endregion
