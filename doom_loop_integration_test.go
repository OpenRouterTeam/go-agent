package agent

// Loop-level doom-loop tests: ports of the engine scenarios in upstream
// tests/unit/doom-loop-integration.test.ts, doom-loop-escalation.test.ts and
// doom-loop-remediation.test.ts. The scripted fakeSender plays the "stuck
// LLM"; assertions read back what the engine sent and persisted.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

type dlQueryInput struct {
	Query string `json:"query"`
}

// dlSearchTool mirrors upstream's searchTool: identity is the normalized
// query (loopKey: ({query}) => query.trim().toLowerCase()).
func dlSearchTool(rec *phCounter) *TypedTool[dlQueryInput] {
	return MustNewTool(ToolConfig[dlQueryInput]{
		Name:        "web_search",
		InputSchema: phObjectSchema("query"),
		LoopKey: func(args map[string]any) (any, error) {
			q, _ := args["query"].(string)
			return strings.ToLower(strings.TrimSpace(q)), nil
		},
		Execute: func(_ context.Context, in dlQueryInput, _ ToolExecuteContext) (any, error) {
			if rec != nil {
				rec.add(in.Query)
			}
			return map[string]any{"results": []string{"result for " + in.Query}}, nil
		},
	})
}

// dlScript builds a fakeSender whose turn i calls `name` with args[i]
// (call ids call_1, call_2, ...), followed by the given trailing responses.
func dlCallTurn(n int, name, args string) components.OpenResponsesResult {
	id := "resp_" + itoa(n)
	return phResp(id, phFnCall("call_"+itoa(n), name, args))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// dlDetections registers a DoomLoopDetected recorder (and optional
// override) on a fresh manager. Guarded: handlers run on the run goroutine.
type dlDetections struct {
	mu       sync.Mutex
	payloads []DoomLoopDetectedPayload
}

func (d *dlDetections) add(p DoomLoopDetectedPayload) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.payloads = append(d.payloads, p)
}

func (d *dlDetections) get() []DoomLoopDetectedPayload {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DoomLoopDetectedPayload(nil), d.payloads...)
}

func (d *dlDetections) summary() []string {
	var out []string
	for _, p := range d.get() {
		out = append(out, string(p.Action)+"@"+itoa(p.Streak))
	}
	return out
}

func dlHooks(d *dlDetections, override func(DoomLoopDetectedPayload) DoomLoopAction) *HooksManager {
	manager := NewHooksManager()
	manager.OnDoomLoopDetected(HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]{
		Handler: func(p DoomLoopDetectedPayload, _ LifecycleHookContext) (HookHandlerResult[DoomLoopDetectedResult], error) {
			d.add(p)
			if override != nil {
				if a := override(p); a != "" {
					return SyncResult(DoomLoopDetectedResult{OverrideAction: a}), nil
				}
			}
			return VoidResult[DoomLoopDetectedResult](), nil
		},
	})
	return manager
}

// dlLastOutput returns the last function_call_output string of request i.
func dlLastOutput(t *testing.T, s *fakeSender, i int) string {
	t.Helper()
	outs := phOutputs(t, s, i)
	if len(outs) == 0 {
		t.Fatalf("request %d carries no function_call_output", i)
	}
	return phOutputString(outs[len(outs)-1])
}

// Upstream: "observes at 2, blocks at 3 with an explanatory tool error, and
// lets the model recover".
func TestDoomLoopObservesThenBlocksAndModelRecovers(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"openrouter sdk"}`),
		dlCallTurn(2, "web_search", `{"query":"OpenRouter SDK"}`),
		dlCallTurn(3, "web_search", `{"query":"  openrouter sdk  "}`),
		phTextResp("resp_4", "Giving up on repeating; here is my answer."),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Find the SDK.", Tools: []Tool{dlSearchTool(rec)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Giving up on repeating; here is my answer." {
		t.Fatalf("text = %q", text)
	}
	if rec.count() != 2 {
		t.Fatalf("calls 1 and 2 execute, call 3 is blocked before execution; executed %d", rec.count())
	}
	if got := det.summary(); !phEqualStrings(got, []string{"observe@2", "block@3"}) {
		t.Fatalf("detections = %v, want [observe@2 block@3]", got)
	}
	first := det.get()[0]
	if first.Detector != DoomLoopDetectorToolFingerprint || first.ToolName != "web_search" {
		t.Fatalf("first detection = %+v", first)
	}
	if first.ToolInput["query"] != "OpenRouter SDK" || first.Fingerprint == "" || first.Message == "" {
		t.Fatalf("payload must carry the repeated call's input, fingerprint and message: %+v", first)
	}
	if len(sender.requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(sender.requests))
	}
	outs := phOutputs(t, sender, 3)
	blocked := outs[len(outs)-1]
	if blocked.CallID != "call_3" {
		t.Fatalf("last output call id = %q", blocked.CallID)
	}
	msg := phOutputError(blocked)
	for _, want := range []string{"Doom loop suspected", "web_search", "3 consecutive rounds"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("blocked output %q must contain %q", phOutputString(blocked), want)
		}
	}
	if result.DoomLoopVerdict(context.Background()) != nil {
		t.Fatal("a block is not a stop: DoomLoopVerdict must be nil")
	}
}

// Upstream: "stops the run at the stop threshold instead of spending
// forever" + "emits SessionEnd with reason doom_loop when stopped".
func TestDoomLoopStopHaltsRunAndSealsDanglingCall(t *testing.T) {
	var resps []components.OpenResponsesResult
	for i := 1; i <= 10; i++ {
		resps = append(resps, dlCallTurn(i, "web_search", `{"query":"same query"}`))
	}
	sender := phSender(resps...)
	manager := NewHooksManager()
	var mu sync.Mutex
	var reasons []SessionEndReason
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{
		Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			mu.Lock()
			defer mu.Unlock()
			reasons = append(reasons, p.Reason)
			return VoidResult[EmptyHookResult](), nil
		},
	})
	rec := &phCounter{}
	acc := &phMemState{}
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Find it.", Tools: []Tool{dlSearchTool(rec)},
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 4}},
		Hooks:    manager, MaxTurns: 20, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 4 {
		t.Fatalf("the loop halts before requesting a 5th turn; requests = %d", len(sender.requests))
	}
	if resp.ID != "resp_4" {
		t.Fatalf("the run surfaces the last response; got %q", resp.ID)
	}
	if rec.count() != 2 {
		t.Fatalf("executed %d calls, want 2 (3 blocked, 4 stopped)", rec.count())
	}
	v := result.DoomLoopVerdict(context.Background())
	if v == nil || v.Action != DoomLoopActionStop || v.Detector != DoomLoopDetectorToolFingerprint || v.Streak != 4 || v.ToolName != "web_search" {
		t.Fatalf("verdict = %+v", v)
	}
	mu.Lock()
	gotReasons := append([]SessionEndReason(nil), reasons...)
	mu.Unlock()
	if len(gotReasons) != 1 || gotReasons[0] != SessionEndReasonDoomLoop {
		t.Fatalf("SessionEnd reasons = %v, want [doom_loop]", gotReasons)
	}

	// The condemned call is blocked with the verdict message (stop blocks the
	// call that tripped it) and persisted, so state has no dangling call.
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	for _, item := range state.Messages {
		if item.FunctionCallOutputItem != nil && item.FunctionCallOutputItem.CallID == "call_4" {
			sealed = phOutputError(*item.FunctionCallOutputItem)
		}
	}
	if sealed != v.Message {
		t.Fatalf("call_4 must carry the stop verdict's message as its error, got %q", sealed)
	}
	persisted := acc.get()
	if persisted == nil || persisted.DoomLoop == nil || persisted.DoomLoop.StopVerdict == nil || persisted.DoomLoop.StopVerdict.Streak != 4 {
		t.Fatalf("persisted doom-loop state must carry the stop verdict: %+v", persisted)
	}
}

// Upstream: "does not interfere with a healthy run".
func TestDoomLoopHealthyRunNoDetections(t *testing.T) {
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"first"}`),
		dlCallTurn(2, "web_search", `{"query":"second"}`),
		dlCallTurn(3, "web_search", `{"query":"third"}`),
		phTextResp("resp_4", "All three found."),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Search three things.", Tools: []Tool{dlSearchTool(nil)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "All three found." || len(det.get()) != 0 || len(sender.requests) != 4 {
		t.Fatalf("text=%q detections=%v requests=%d", text, det.summary(), len(sender.requests))
	}
}

// Upstream: "is fully inert when doomLoop is not configured" plus the Go
// contract that nil DoomLoop writes no DoomLoop state and emits no hook.
func TestDoomLoopNilConfigIsInert(t *testing.T) {
	det := &dlDetections{}
	rec := &phCounter{}
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"same"}`),
		dlCallTurn(2, "web_search", `{"query":"same"}`),
		dlCallTurn(3, "web_search", `{"query":"same"}`),
		dlCallTurn(4, "web_search", `{"query":"same"}`),
		phTextResp("resp_5", "done"),
	)
	acc := &phMemState{}
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(rec)},
		Hooks: dlHooks(det, nil), MaxTurns: 10, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" || rec.count() != 4 {
		t.Fatalf("text=%q executed=%d", text, rec.count())
	}
	for i := 1; i <= 4; i++ {
		if out := dlLastOutput(t, sender, i); strings.Contains(out, "Doom loop") {
			t.Fatalf("request %d output mentions doom loop: %s", i, out)
		}
	}
	if len(det.get()) != 0 {
		t.Fatalf("no DoomLoopDetected may fire when detection is off: %v", det.summary())
	}
	if s := acc.get(); s == nil || s.DoomLoop != nil {
		t.Fatalf("detection off must not persist DoomLoop state: %+v", s)
	}
	if result.DoomLoopVerdict(context.Background()) != nil {
		t.Fatal("DoomLoopVerdict must be nil when detection is off")
	}
}

// Upstream: "treats repeated {} calls as a streak and blocks at the threshold".
func TestDoomLoopRepeatedEmptyCallsBlock(t *testing.T) {
	det := &dlDetections{}
	listTool := MustNewTool(ToolConfig[struct{}]{
		Name: "list_tasks", InputSchema: map[string]any{"type": "object"},
		Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
			return map[string]any{"tasks": []string{"task-1"}}, nil
		},
	})
	sender := phSender(
		dlCallTurn(1, "list_tasks", `{}`),
		dlCallTurn(2, "list_tasks", `{}`),
		dlCallTurn(3, "list_tasks", `{}`),
		phTextResp("resp_4", "The list has not changed; stopping."),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "What tasks exist?", Tools: []Tool{listTool},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "The list has not changed; stopping." {
		t.Fatalf("text = %q", text)
	}
	if got := det.summary(); !phEqualStrings(got, []string{"observe@2", "block@3"}) {
		t.Fatalf("detections = %v", got)
	}
	if out := dlLastOutput(t, sender, 3); !strings.Contains(out, "Doom loop suspected") {
		t.Fatalf("blocked output = %s", out)
	}
}

// Upstream: "counts parse-error repeats and stops instead of bouncing off the
// parse error forever". Go's call extraction drops unparseable arguments
// before the run loop (emittedToolCalls), so the reachable analog is
// non-object arguments: their raw value is the identity.
func TestDoomLoopRepeatedNonObjectArgumentsStop(t *testing.T) {
	det := &dlDetections{}
	var resps []components.OpenResponsesResult
	for i := 1; i <= 4; i++ {
		resps = append(resps, dlCallTurn(i, "web_search", `"{\"query\": broken"`))
	}
	sender := phSender(resps...)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Search.", Tools: []Tool{dlSearchTool(nil)},
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}},
		Hooks:    dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := det.summary(); !phEqualStrings(got, []string{"observe@2", "stop@3"}) {
		t.Fatalf("detections = %v, want [observe@2 stop@3]", got)
	}
	if len(sender.requests) != 3 {
		t.Fatalf("halted after the 3rd turn; requests = %d", len(sender.requests))
	}
	if v := result.DoomLoopVerdict(context.Background()); v == nil || v.Action != DoomLoopActionStop || v.Streak != 3 {
		t.Fatalf("verdict = %+v", v)
	}
}

// Upstream: "stops a run whose response is one degenerate repeated-token loop".
func TestDoomLoopTextRepetitionStops(t *testing.T) {
	det := &dlDetections{}
	doomText := strings.TrimSpace(strings.Repeat("I need to check the file again. ", 40))
	sender := phSender(phTextResp("resp_1", doomText))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Summarize.", DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != doomText {
		t.Fatalf("the already-emitted text is surfaced as-is; got %q", text)
	}
	got := det.get()
	if len(got) != 1 || got[0].Detector != DoomLoopDetectorTextRepetition || got[0].Action != DoomLoopActionStop || got[0].Streak != 40 {
		t.Fatalf("detections = %+v", got)
	}
	if got[0].ToolInput != nil {
		t.Fatalf("text verdicts carry no tool input: %+v", got[0])
	}
}

// Upstream: "halts a tool loop when every turn repeats identical filler text".
func TestDoomLoopTextStreakHaltsToolLoop(t *testing.T) {
	filler := "Hmm, let me look somewhere else."
	var resps []components.OpenResponsesResult
	for i := 1; i <= 5; i++ {
		resps = append(resps, phResp("resp_"+itoa(i), phMessage(itoa(i), filler), phFnCall("call_"+itoa(i), "web_search", `{"query":"q`+itoa(i)+`"}`)))
	}
	sender := phSender(resps...)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Find it.", Tools: []Tool{dlSearchTool(nil)},
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}},
		MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(sender.requests))
	}
	v := result.DoomLoopVerdict(context.Background())
	if v == nil || v.Detector != DoomLoopDetectorTextStreak || v.Action != DoomLoopActionStop || v.Streak != 3 {
		t.Fatalf("verdict = %+v", v)
	}
}

type dlBashInput struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Verbose bool   `json:"verbose,omitempty"`
}

func dlBashTool(rec *phCounter, key ToolLoopKey) *TypedTool[dlBashInput] {
	return MustNewTool(ToolConfig[dlBashInput]{
		Name: "bash", InputSchema: phObjectSchema("command", "cwd"), LoopKey: key,
		Execute: func(_ context.Context, in dlBashInput, _ ToolExecuteContext) (any, error) {
			rec.add(in.Cwd + ":" + in.Command)
			return map[string]any{"stdout": "ran " + in.Command}, nil
		},
	})
}

// Upstream: "bash-style loopKey: same command in a different cwd is NOT a
// loop" and remediation API1 "subset loopKey ... different cwd".
func TestDoomLoopLoopKeyDistinguishesCwd(t *testing.T) {
	for name, key := range map[string]ToolLoopKey{
		"function": func(args map[string]any) (any, error) {
			cmd, _ := args["command"].(string)
			return map[string]any{"command": strings.TrimSpace(cmd), "cwd": args["cwd"]}, nil
		},
		"fields": LoopKeyFields("command", "cwd"),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &phCounter{}
			sender := phSender(
				dlCallTurn(1, "bash", `{"command":"ls","cwd":"/a"}`),
				dlCallTurn(2, "bash", `{"command":"ls","cwd":"/b"}`),
				dlCallTurn(3, "bash", `{"command":"ls","cwd":"/c"}`),
				phTextResp("resp_4", "Directory listings acquired."),
			)
			result, err := CallModel(context.Background(), sender, CallModelInput{
				Model: "openai/test", Input: "List.", Tools: []Tool{dlBashTool(rec, key)},
				DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Block: 2}}, MaxTurns: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, v := range rec.values() {
				got = append(got, v.(string))
			}
			if text != "Directory listings acquired." || !phEqualStrings(got, []string{"/a:ls", "/b:ls", "/c:ls"}) {
				t.Fatalf("text=%q runs=%v", text, got)
			}
		})
	}
}

// Upstream API1: "function loopKey collapses excluded-field variation" — a
// field-subset key ignores non-identity fields, so varying them still loops.
func TestDoomLoopFieldSubsetKeyIgnoresOtherFields(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "bash", `{"command":"ls","cwd":"/a","verbose":true}`),
		dlCallTurn(2, "bash", `{"command":"ls","cwd":"/a","verbose":false}`),
		phTextResp("resp_3", "done"),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlBashTool(rec, LoopKeyFields("command", "cwd"))},
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Block: 2}}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 || !phEqualStrings(det.summary(), []string{"block@2"}) {
		t.Fatalf("executed=%d detections=%v; the verbose flag is not identity", rec.count(), det.summary())
	}
	if out := dlLastOutput(t, sender, 2); !strings.Contains(out, "Doom loop suspected") {
		t.Fatalf("second call must be blocked: %s", out)
	}
}

// Upstream: "loopKey returning null exempts a legitimately-repetitive tool"
// and remediation "loopKey: false statically exempts a tool".
func TestDoomLoopExemptLoopKey(t *testing.T) {
	for name, key := range map[string]ToolLoopKey{
		"per-call": func(map[string]any) (any, error) { return nil, ErrLoopKeyExempt },
		"static":   ExemptToolLoopKey,
	} {
		t.Run(name, func(t *testing.T) {
			det := &dlDetections{}
			rec := &phCounter{}
			poll := MustNewTool(ToolConfig[phTargetInput]{
				Name: "check_status", InputSchema: phObjectSchema("target"), LoopKey: key,
				Execute: func(_ context.Context, in phTargetInput, _ ToolExecuteContext) (any, error) {
					rec.add(in)
					return map[string]any{"done": false}, nil
				},
			})
			var resps []components.OpenResponsesResult
			for i := 1; i <= 5; i++ {
				resps = append(resps, dlCallTurn(i, "check_status", `{"target":"job-1"}`))
			}
			resps = append(resps, phTextResp("resp_6", "Job still running after 5 polls."))
			sender := phSender(resps...)
			result, err := CallModel(context.Background(), sender, CallModelInput{
				Model: "openai/test", Input: "Wait.", Tools: []Tool{poll},
				DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if text != "Job still running after 5 polls." || rec.count() != 5 || len(det.get()) != 0 {
				t.Fatalf("text=%q executed=%d detections=%v", text, rec.count(), det.summary())
			}
		})
	}
}

// Upstream: "a throwing loopKey falls back to full-arguments identity
// instead of failing the run" (Go: an error or a panic).
func TestDoomLoopFailingLoopKeyFallsBack(t *testing.T) {
	for name, key := range map[string]ToolLoopKey{
		"error": func(map[string]any) (any, error) { return nil, errLoopKeyBug },
		"panic": func(map[string]any) (any, error) { panic("loopKey bug") },
	} {
		t.Run(name, func(t *testing.T) {
			det := &dlDetections{}
			sender := phSender(
				dlCallTurn(1, "flaky", `{"target":"same"}`),
				dlCallTurn(2, "flaky", `{"target":"same"}`),
				dlCallTurn(3, "flaky", `{"target":"same"}`),
				phTextResp("resp_4", "done"),
			)
			result, err := CallModel(context.Background(), sender, CallModelInput{
				Model: "openai/test", Input: "go", Tools: []Tool{phOKTool("flaky", &phCounter{}, func(c *ToolConfig[phTargetInput]) { c.LoopKey = key })},
				DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			text, err := result.Text(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if text != "done" || !phEqualStrings(det.summary(), []string{"observe@2", "block@3"}) {
				t.Fatalf("text=%q detections=%v", text, det.summary())
			}
		})
	}
}

var errLoopKeyBug = &LoopKeyFallbackError{Warning: "loopKey bug"}

// Upstream: "invokes a tool-supplied loopKey exactly once per call" and
// "never invokes loopKey for a manual call the detector will not check".
func TestDoomLoopLoopKeyInvocationCounts(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	countingKey := func(tool string) ToolLoopKey {
		return func(args map[string]any) (any, error) {
			mu.Lock()
			counts[tool]++
			mu.Unlock()
			return args["target"], nil
		}
	}
	search := phOKTool("web_search", &phCounter{}, func(c *ToolConfig[phTargetInput]) { c.LoopKey = countingKey("web_search") })
	manual := MustNewTool(ToolConfig[phTargetInput]{Name: "ask_human", InputSchema: phObjectSchema("target"), LoopKey: countingKey("ask_human")})

	sender := phSender(
		dlCallTurn(1, "web_search", `{"target":"first"}`),
		dlCallTurn(2, "web_search", `{"target":"second"}`),
		phResp("resp_3", phFnCall("call_3", "web_search", `{"target":"go"}`), phFnCall("call_4", "ask_human", `{"target":"what now?"}`)),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{search, manual}, DoomLoop: &DoomLoopConfig{}, MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["web_search"] != 3 {
		t.Fatalf("three executable calls -> exactly three loopKey invocations, got %d", counts["web_search"])
	}
	if counts["ask_human"] != 0 {
		t.Fatalf("a manual call is never checked, so its loopKey must not run; ran %d", counts["ask_human"])
	}
}

// Upstream: "a handler can de-escalate a block to observe (call still
// executes)".
func TestDoomLoopHookDeEscalatesBlock(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "noisy", `{"target":"x"}`),
		dlCallTurn(2, "noisy", `{"target":"x"}`),
		dlCallTurn(3, "noisy", `{"target":"x"}`),
		phTextResp("resp_4", "done"),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{phOKTool("noisy", rec, nil)},
		DoomLoop: &DoomLoopConfig{}, MaxTurns: 10,
		Hooks: dlHooks(det, func(DoomLoopDetectedPayload) DoomLoopAction { return DoomLoopActionObserve }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if rec.count() != 3 {
		t.Fatalf("de-escalation lets all 3 execute; executed %d", rec.count())
	}
	// The payload reports the engine's resolved action, before the override.
	if !phEqualStrings(det.summary(), []string{"observe@2", "block@3"}) {
		t.Fatalf("detections = %v", det.summary())
	}
	if out := dlLastOutput(t, sender, 3); strings.Contains(out, "Doom loop") {
		t.Fatalf("de-escalated call must run normally: %s", out)
	}
}

// Upstream: "a handler can escalate an observe to stop".
func TestDoomLoopHookEscalatesToStop(t *testing.T) {
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"x"}`),
		dlCallTurn(2, "web_search", `{"query":"x"}`),
		dlCallTurn(3, "web_search", `{"query":"x"}`),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, MaxTurns: 10,
		Hooks: dlHooks(det, func(DoomLoopDetectedPayload) DoomLoopAction { return DoomLoopActionStop }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(sender.requests))
	}
	v := result.DoomLoopVerdict(context.Background())
	if v == nil || v.Streak != 2 {
		t.Fatalf("verdict = %+v", v)
	}
}

// Text verdicts cannot block (the text was already emitted): a block — from
// the ladder or a hook override — downgrades to observe and the run goes on.
func TestDoomLoopBlockOverrideOnTextVerdictObserves(t *testing.T) {
	det := &dlDetections{}
	filler := "Same filler text every turn."
	rec := &phCounter{}
	sender := phSender(
		phResp("resp_1", phMessage("1", filler), phFnCall("call_1", "web_search", `{"query":"q1"}`)),
		phResp("resp_2", phMessage("2", filler), phFnCall("call_2", "web_search", `{"query":"q2"}`)),
		phTextResp("resp_3", "final"),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(rec)}, MaxTurns: 10,
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2}},
		Hooks: dlHooks(det, func(p DoomLoopDetectedPayload) DoomLoopAction {
			if p.Detector == DoomLoopDetectorTextStreak {
				return DoomLoopActionBlock
			}
			return ""
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "final" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	got := det.get()
	if len(got) != 1 || got[0].Detector != DoomLoopDetectorTextStreak || got[0].Streak != 2 {
		t.Fatalf("detections = %+v", got)
	}
	if rec.count() != 2 || len(sender.requests) != 3 || result.DoomLoopVerdict(context.Background()) != nil {
		t.Fatalf("executed=%d requests=%d: a block on a text verdict is observe", rec.count(), len(sender.requests))
	}
}

// dlSessionEnds records SessionEnd reasons.
func dlSessionEnds(manager *HooksManager) func() []SessionEndReason {
	var mu sync.Mutex
	var reasons []SessionEndReason
	manager.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{
		Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
			mu.Lock()
			defer mu.Unlock()
			reasons = append(reasons, p.Reason)
			return VoidResult[EmptyHookResult](), nil
		},
	})
	return func() []SessionEndReason {
		mu.Lock()
		defer mu.Unlock()
		return append([]SessionEndReason(nil), reasons...)
	}
}

// dlAssertPaired fails when items carry a function_call without a matching
// function_call_output (or vice versa) — upstream assertWellFormedHistory.
func dlAssertPaired(t *testing.T, items []components.InputsUnion1) {
	t.Helper()
	calls, outs := map[string]int{}, map[string]int{}
	for _, item := range items {
		m := phJSONMap(t, item)
		id, _ := m["call_id"].(string)
		switch m["type"] {
		case "function_call":
			calls[id]++
		case "function_call_output":
			outs[id]++
		}
	}
	for id, n := range calls {
		if outs[id] != n {
			t.Fatalf("function_call %q has %d outputs (want %d): kinds %v", id, outs[id], n, phItemKinds(t, items))
		}
	}
	for id := range outs {
		if calls[id] == 0 {
			t.Fatalf("function_call_output %q has no function_call", id)
		}
	}
}

// Upstream B3: "a 6-wide identical fan-out executes fully with zero
// detections".
func TestDoomLoopIdenticalFanOutIsOneRound(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	var items []components.OutputItems
	for i := 1; i <= 6; i++ {
		items = append(items, phFnCall("call_"+itoa(i), "web_search", `{"target":"same"}`))
	}
	sender := phSender(phResp("resp_1", items...), phTextResp("resp_2", "All six ran."))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Fan out.", Tools: []Tool{phOKTool("web_search", rec, nil)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "All six ran." {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if rec.count() != 6 || len(det.get()) != 0 {
		t.Fatalf("executed=%d detections=%v", rec.count(), det.summary())
	}
}

// Upstream B3: "duplicates of a call blocked this round share the block
// decision".
func TestDoomLoopDuplicatesShareBlockDecision(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	sender := phSender(
		dlCallTurn(1, "noisy", `{"target":"x"}`),
		dlCallTurn(2, "noisy", `{"target":"x"}`),
		phResp("resp_3", phFnCall("call_3", "noisy", `{"target":"x"}`), phFnCall("call_4", "noisy", `{"target":"x"}`), phFnCall("call_5", "noisy", `{"target":"x"}`)),
		phTextResp("resp_4", "done"),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{phOKTool("noisy", rec, nil)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if rec.count() != 2 || !phEqualStrings(det.summary(), []string{"observe@2", "block@3"}) {
		t.Fatalf("executed=%d detections=%v", rec.count(), det.summary())
	}
	outs := phOutputs(t, sender, 3)
	var blocked []string
	for _, o := range outs {
		if strings.Contains(phOutputError(o), "Doom loop suspected") {
			blocked = append(blocked, o.CallID)
		}
	}
	if !phEqualStrings(blocked, []string{"call_3", "call_4", "call_5"}) {
		t.Fatalf("all three duplicates share the block; blocked = %v", blocked)
	}
}

// Upstream B1/D7: "text stop on a response that ALSO carries tool calls
// leaves no dangling function_call" and "a resumed conversation after a
// sealed text stop sends well-formed input".
func TestDoomLoopTextStopSealsDanglingCallsAndResumes(t *testing.T) {
	acc := &phMemState{}
	manager := NewHooksManager()
	ends := dlSessionEnds(manager)
	rec := &phCounter{}
	doomText := strings.TrimSpace(strings.Repeat("I am stuck. ", 20))
	sender := phSender(phResp("resp_1", phMessage("1", doomText), phFnCall("call_1", "web_search", `{"query":"never runs"}`)))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(rec)},
		DoomLoop: &DoomLoopConfig{}, StateAccessor: acc, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 || rec.count() != 0 {
		t.Fatalf("requests=%d executed=%d: the loop halts before executing", len(sender.requests), rec.count())
	}
	if got := ends(); len(got) != 1 || got[0] != SessionEndReasonDoomLoop {
		t.Fatalf("SessionEnd reasons = %v", got)
	}
	v := result.DoomLoopVerdict(context.Background())
	if v == nil || v.Detector != DoomLoopDetectorTextRepetition || v.Action != DoomLoopActionStop {
		t.Fatalf("verdict = %+v", v)
	}
	state := acc.get()
	if state == nil {
		t.Fatal("state must be persisted")
	}
	dlAssertPaired(t, state.Messages)
	var outputs []string
	for _, item := range state.Messages {
		if item.FunctionCallOutputItem != nil {
			outputs = append(outputs, phOutputError(*item.FunctionCallOutputItem))
		}
	}
	if len(outputs) != 1 || outputs[0] != "Run halted by doom-loop detection: "+v.Message {
		t.Fatalf("synthesized outputs = %q", outputs)
	}

	resume := phSender(phTextResp("resp_2", "resumed fine"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{
		Model: "openai/test", Input: "continue please", Tools: []Tool{dlSearchTool(rec)},
		DoomLoop: &DoomLoopConfig{}, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := resumed.Text(context.Background()); err != nil || text != "resumed fine" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	dlAssertPaired(t, requestInputItems(resume.requests[0].Input))
}

// Upstream B1/D7: "text-only no-tools stop reports SessionEnd reason
// doom_loop (not complete)".
func TestDoomLoopTextOnlyStopReportsDoomLoopReason(t *testing.T) {
	manager := NewHooksManager()
	ends := dlSessionEnds(manager)
	doomText := strings.TrimSpace(strings.Repeat("I am stuck. ", 20))
	result, err := CallModel(context.Background(), phSender(phTextResp("resp_1", doomText)), CallModelInput{
		Model: "openai/test", Input: "Summarize.", DoomLoop: &DoomLoopConfig{}, Hooks: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != doomText {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if got := ends(); len(got) != 1 || got[0] != SessionEndReasonDoomLoop {
		t.Fatalf("SessionEnd reasons = %v", got)
	}
	if v := result.DoomLoopVerdict(context.Background()); v == nil || v.Detector != DoomLoopDetectorTextRepetition || v.Action != DoomLoopActionStop {
		t.Fatalf("verdict = %+v", v)
	}
}

// Upstream D1: "stopWhen + doom-stop during the halted turn skips
// makeFinalResponseRequest".
func TestDoomLoopStopSkipsFinalResponseRequest(t *testing.T) {
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"same"}`),
		dlCallTurn(2, "web_search", `{"query":"same"}`),
		phTextResp("resp_3", "should never be fetched"),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(nil)},
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 2, Stop: 2}},
		StopWhen: []StopCondition{StepCountIs(2)}, AllowFinalResponse: true, MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("the final text-coercion request must be skipped; requests = %d", len(sender.requests))
	}
	if v := result.DoomLoopVerdict(context.Background()); v == nil || v.Action != DoomLoopActionStop || v.Streak != 2 {
		t.Fatalf("verdict = %+v", v)
	}
}

// Upstream D4: "injects corrective guidance before the next model request
// (in-loop)".
func TestDoomLoopSteerInjectedAsUserMessage(t *testing.T) {
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"same"}`),
		dlCallTurn(2, "web_search", `{"query":"same"}`),
		phTextResp("resp_3", "adjusted course"),
	)
	rec := &phCounter{}
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(rec)}, MaxTurns: 10,
		DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Steer: 2, Block: DoomLoopDisabled, Stop: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "adjusted course" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if rec.count() != 2 {
		t.Fatalf("steer does not block: executed %d", rec.count())
	}
	steers := func(i int) int {
		n := 0
		for _, s := range phUserTexts(requestInputItems(sender.requests[i].Input)) {
			if strings.Contains(s, "Doom loop suspected") {
				n++
			}
		}
		return n
	}
	if steers(1) != 0 || steers(2) != 1 {
		t.Fatalf("steer fires on round 2, so only request 3 carries it once; got req2=%d req3=%d", steers(1), steers(2))
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.DoomLoop == nil || len(state.DoomLoop.PendingSteer) != 0 {
		t.Fatalf("a delivered steer is no longer pending: %+v", state.DoomLoop)
	}
}

// Upstream D4: "steer queued at a HITL pause persists in
// doomLoop.pendingSteer and delivers on resume" — here at an approval
// pause, with delivery checked on the resumed run.
func TestDoomLoopSteerPersistsAtApprovalPauseAndDeliversOnResume(t *testing.T) {
	acc := &phMemState{}
	cfg := &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Steer: 2, Block: DoomLoopDisabled, Stop: 6}}
	tools := func() []Tool {
		return []Tool{
			phOKTool("lookup", &phCounter{}, nil),
			phOKTool("risky", &phCounter{}, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true }),
		}
	}
	sender := phSender(
		dlCallTurn(1, "lookup", `{"target":"same"}`),
		phResp("resp_2", phFnCall("call_2", "lookup", `{"target":"same"}`), phFnCall("call_3", "risky", `{"target":"t"}`)),
	)
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: tools(), DoomLoop: cfg, StateAccessor: acc, MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	paused := acc.get()
	if paused == nil || paused.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected an approval pause: %+v", paused)
	}
	if paused.DoomLoop == nil || len(paused.DoomLoop.PendingSteer) != 1 || !strings.Contains(paused.DoomLoop.PendingSteer[0], "Doom loop suspected") {
		t.Fatalf("steer must be persisted as pending, not injected mid-pause: %+v", paused.DoomLoop)
	}
	for _, s := range phUserTexts(paused.Messages) {
		if strings.Contains(s, "Doom loop suspected") {
			t.Fatal("steer must not be injected between a dangling call and its output")
		}
	}
	if paused.DoomLoop.Tools["lookup"].Streak != 2 {
		t.Fatalf("persisted lookup streak = %+v", paused.DoomLoop.Tools)
	}

	// Resume: the queued guidance is delivered at the next safe flush point
	// (after the next fully-paired round), exactly once.
	resume := phSender(dlCallTurn(4, "lookup", `{"target":"other"}`), phTextResp("resp_5", "done"))
	resumed, err := CallModel(context.Background(), resume, CallModelInput{
		Model: "openai/test", Tools: tools(), DoomLoop: cfg, StateAccessor: acc, MaxTurns: 10,
		ApproveToolCalls: []string{"call_3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := resumed.Text(context.Background()); err != nil || text != "done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	last := resume.requests[len(resume.requests)-1]
	n := 0
	for _, s := range phUserTexts(requestInputItems(last.Input)) {
		if strings.Contains(s, "Doom loop suspected") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("restored steer must be delivered exactly once on resume; got %d", n)
	}
	if s := acc.get(); s == nil || s.DoomLoop == nil || len(s.DoomLoop.PendingSteer) != 0 {
		t.Fatalf("delivered steer must be cleared from state: %+v", s.DoomLoop)
	}
}

// dlEscalateAt2 is upstream's ESCALATE_AT_2 ladder.
var dlEscalateAt2 = DoomLoopLadder{Observe: 1, Steer: DoomLoopDisabled, Escalate: 2, Block: 5, Stop: 8}

func dlReqField(t *testing.T, s *fakeSender, i int, field string) any {
	t.Helper()
	if i >= len(s.requests) {
		t.Fatalf("request %d was never sent (only %d)", i, len(s.requests))
	}
	return phJSONMap(t, s.requests[i])[field]
}

// dlAdvisorParams returns the openrouter:advisor tool's parameters on
// request i (nil when absent).
func dlAdvisorParams(t *testing.T, s *fakeSender, i int) map[string]any {
	t.Helper()
	tools, _ := dlReqField(t, s, i, "tools").([]any)
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok && tool["type"] == "openrouter:advisor" {
			params, _ := tool["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			return params
		}
	}
	return nil
}

// Upstream: "swaps the model for exactly ONE turn, then reverts" (plus the
// Go contract that the Models fallback list is cleared on the swapped turn
// only) and "a steer notice accompanies the escalated turn".
func TestDoomLoopEscalationSwapsModelForOneDispatch(t *testing.T) {
	sender := phSender(
		dlCallTurn(1, "web_search", `{"query":"same"}`),
		dlCallTurn(2, "web_search", `{"query":"same"}`),
		dlCallTurn(3, "web_search", `{"query":"different now"}`),
		phTextResp("resp_4", "recovered"),
	)
	acc := &phMemState{}
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10, StateAccessor: acc,
		Request:  components.ResponsesRequest{Models: []string{"fallback-a", "fallback-b"}},
		DoomLoop: &DoomLoopConfig{Ladder: dlEscalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "recovered" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	wantModels := []string{"cheap-model", "cheap-model", "frontier-model", "cheap-model"}
	for i, want := range wantModels {
		if got := dlReqField(t, sender, i, "model"); got != want {
			t.Fatalf("request %d model = %v, want %s", i, got, want)
		}
		models := sender.requests[i].Models
		if want == "frontier-model" && len(models) != 0 {
			t.Fatalf("escalated dispatch must clear the Models fallback list, got %v", models)
		}
		if want == "cheap-model" && len(models) != 2 {
			t.Fatalf("request %d must keep the caller's Models list, got %v", i, models)
		}
	}
	notices := 0
	for _, s := range phUserTexts(requestInputItems(sender.requests[2].Input)) {
		if strings.Contains(s, "escalated turn follows") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("escalated turn must carry exactly one steer notice, got %d", notices)
	}
	if s := acc.get(); s == nil || s.DoomLoop == nil || s.DoomLoop.EscalationsUsed != 1 {
		t.Fatalf("one escalation consumed: %+v", s.DoomLoop)
	}
}

// Upstream: "injects the advisor tool with pinned toolChoice for the
// escalated turn only" and "advisor object config merges over the defaults".
func TestDoomLoopEscalationAdvisorTool(t *testing.T) {
	run := func(t *testing.T, advisor any) *fakeSender {
		sender := phSender(
			dlCallTurn(1, "web_search", `{"query":"same"}`),
			dlCallTurn(2, "web_search", `{"query":"same"}`),
			dlCallTurn(3, "web_search", `{"query":"unblocked"}`),
			phTextResp("resp_4", "done"),
		)
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10,
			DoomLoop: &DoomLoopConfig{Ladder: dlEscalateAt2, Escalation: &DoomLoopEscalationConfig{Advisor: advisor}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(context.Background()); err != nil {
			t.Fatal(err)
		}
		return sender
	}
	t.Run("defaults", func(t *testing.T) {
		sender := run(t, true)
		params := dlAdvisorParams(t, sender, 2)
		if params == nil {
			t.Fatal("escalated dispatch must append the advisor tool")
		}
		if params["forward_transcript"] != true || !strings.Contains(params["instructions"].(string), "escalation advisor") {
			t.Fatalf("advisor params = %v", params)
		}
		if got := phWireChoice(t, sender, 2); got != phCanonicalJSON(t, `{"type":"allowed_tools","mode":"required","tools":[{"type":"openrouter:advisor"}]}`) {
			t.Fatalf("escalated tool_choice = %s", got)
		}
		if got := dlReqField(t, sender, 2, "model"); got != "cheap-model" {
			t.Fatalf("advisor-only escalation keeps the model, got %v", got)
		}
		for _, i := range []int{0, 1, 3} {
			if dlAdvisorParams(t, sender, i) != nil {
				t.Fatalf("request %d must not carry the advisor tool", i)
			}
			if strings.Contains(phWireChoice(t, sender, i), "allowed_tools") {
				t.Fatalf("request %d must not carry the pinned advisor choice", i)
			}
		}
		if len(sender.requests[3].Tools) != len(sender.requests[1].Tools) {
			t.Fatal("tools revert after the escalated dispatch")
		}
	})
	t.Run("object config", func(t *testing.T) {
		sender := run(t, map[string]any{"model": "anthropic/claude-opus-4.6", "forwardTranscript": false})
		params := dlAdvisorParams(t, sender, 2)
		if params == nil || params["model"] != "anthropic/claude-opus-4.6" || params["forward_transcript"] != false {
			t.Fatalf("advisor params = %v", params)
		}
		if !strings.Contains(params["instructions"].(string), "escalation advisor") {
			t.Fatalf("default instructions must be kept: %v", params)
		}
	})
}

// Upstream: "consumes budget per APPLIED escalation and falls through when
// exhausted".
func TestDoomLoopEscalationBudgetExhaustion(t *testing.T) {
	det := &dlDetections{}
	var resps []components.OpenResponsesResult
	for i := 1; i <= 6; i++ {
		resps = append(resps, dlCallTurn(i, "web_search", `{"query":"same"}`))
	}
	resps = append(resps, phTextResp("resp_7", "gave up"))
	sender := phSender(resps...)
	acc := &phMemState{}
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10, StateAccessor: acc,
		DoomLoop: &DoomLoopConfig{Ladder: dlEscalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model", MaxEscalations: 1}},
		Hooks:    dlHooks(det, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "gave up" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	var tool []string
	for _, p := range det.get() {
		if p.Detector == DoomLoopDetectorToolFingerprint {
			tool = append(tool, string(p.Action)+"@"+itoa(p.Streak))
		}
	}
	want := []string{"observe@1", "escalate@2", "observe@3", "observe@4", "block@5", "block@6"}
	if !phEqualStrings(tool, want) {
		t.Fatalf("tool detections = %v, want %v", tool, want)
	}
	escalated := 0
	for i := range sender.requests {
		if dlReqField(t, sender, i, "model") == "frontier-model" {
			escalated++
		}
	}
	if escalated != 1 {
		t.Fatalf("exactly one dispatch uses the frontier model, got %d", escalated)
	}
	if s := acc.get(); s == nil || s.DoomLoop == nil || s.DoomLoop.EscalationsUsed != 1 {
		t.Fatalf("escalationsUsed must persist as 1: %+v", s)
	}
}

// Upstream: "hook overrideAction escalate is honored with budget and
// downgraded without config".
func TestDoomLoopHookEscalateOverride(t *testing.T) {
	run := func(t *testing.T, esc *DoomLoopEscalationConfig) *fakeSender {
		sender := phSender(
			dlCallTurn(1, "web_search", `{"query":"same"}`),
			dlCallTurn(2, "web_search", `{"query":"same"}`),
			phTextResp("resp_3", "done"),
		)
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10,
			DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 5, Stop: 8}, Escalation: esc},
			Hooks:    dlHooks(&dlDetections{}, func(DoomLoopDetectedPayload) DoomLoopAction { return DoomLoopActionEscalate }),
		})
		if err != nil {
			t.Fatal(err)
		}
		if text, err := result.Text(context.Background()); err != nil || text != "done" {
			t.Fatalf("text=%q err=%v", text, err)
		}
		return sender
	}
	if got := dlReqField(t, run(t, &DoomLoopEscalationConfig{Model: "frontier-model"}), 2, "model"); got != "frontier-model" {
		t.Fatalf("escalate override with config: request 3 model = %v", got)
	}
	sender := run(t, nil)
	if got := dlReqField(t, sender, 2, "model"); got != "cheap-model" {
		t.Fatalf("escalate override without config downgrades to observe: model = %v", got)
	}
	for _, s := range phUserTexts(requestInputItems(sender.requests[2].Input)) {
		if strings.Contains(s, "escalated turn follows") {
			t.Fatal("a downgraded escalate must not queue the escalation notice")
		}
	}
}

// Upstream: "text-detector verdicts can escalate too" and "two detectors
// firing in one window escalate ONCE (no double spend)".
func TestDoomLoopTextVerdictEscalatesOnce(t *testing.T) {
	t.Run("text streak escalates", func(t *testing.T) {
		filler := "Let me look somewhere else."
		sender := phSender(
			phResp("resp_1", phMessage("1", filler), phFnCall("call_1", "web_search", `{"query":"q1"}`)),
			phResp("resp_2", phMessage("2", filler), phFnCall("call_2", "web_search", `{"query":"q2"}`)),
			dlCallTurn(3, "web_search", `{"query":"q3"}`),
			phTextResp("resp_4", "unblocked"),
		)
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10,
			DoomLoop: &DoomLoopConfig{Ladder: dlEscalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(context.Background()); err != nil {
			t.Fatal(err)
		}
		var models []string
		for i := range sender.requests {
			models = append(models, dlReqField(t, sender, i, "model").(string))
		}
		if !phEqualStrings(models, []string{"cheap-model", "cheap-model", "frontier-model", "cheap-model"}) {
			t.Fatalf("models = %v", models)
		}
	})
	t.Run("two detectors one escalation", func(t *testing.T) {
		same := func(n int) components.OpenResponsesResult {
			return phResp("resp_"+itoa(n), phMessage(itoa(n), "Trying the same thing."), phFnCall("call_"+itoa(n), "web_search", `{"query":"same"}`))
		}
		acc := &phMemState{}
		sender := phSender(same(1), same(2), phTextResp("resp_3", "done"))
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "cheap-model", Input: "go", Tools: []Tool{dlSearchTool(nil)}, MaxTurns: 10, StateAccessor: acc,
			DoomLoop: &DoomLoopConfig{Ladder: dlEscalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model", MaxEscalations: 5}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s := acc.get(); s == nil || s.DoomLoop == nil || s.DoomLoop.EscalationsUsed != 1 {
			t.Fatalf("escalationsUsed = %+v, want 1", s.DoomLoop)
		}
	})
}

// dlRunPersistent runs two identical web_search calls then a text turn on
// acc with detection on (streak 2 persisted).
func dlRunPersistent(t *testing.T, acc *phMemState, query, finalText string) {
	t.Helper()
	args := `{"query":"` + query + `"}`
	sender := phSender(dlCallTurn(1, "web_search", args), dlCallTurn(2, "web_search", args), phTextResp("resp_3", finalText))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: acc, MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Upstream: "persists streaks into ConversationState and keeps counting
// across a resume" (= remediation "ON→ON") and "doomLoop state round-trips
// through JSON serialization".
func TestDoomLoopStreaksPersistAcrossResume(t *testing.T) {
	acc := &phMemState{}
	dlRunPersistent(t, acc, "persistent", "pausing here")
	persisted := acc.get()
	if persisted == nil || persisted.DoomLoop == nil || persisted.DoomLoop.Tools["web_search"].Streak != 2 {
		t.Fatalf("persisted doom-loop state = %+v", persisted)
	}
	serialized, err := SerializeConversationState(*persisted)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DeserializeConversationState(serialized)
	if err != nil {
		t.Fatal(err)
	}
	if phJSON(t, back.DoomLoop) != phJSON(t, persisted.DoomLoop) {
		t.Fatalf("DoomLoop must round-trip: %s vs %s", phJSON(t, back.DoomLoop), phJSON(t, persisted.DoomLoop))
	}

	det := &dlDetections{}
	sender := phSender(dlCallTurn(4, "web_search", `{"query":"persistent"}`), phTextResp("resp_5", "recovered"))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "continue", Tools: []Tool{dlSearchTool(nil)}, DoomLoop: &DoomLoopConfig{},
		StateAccessor: acc, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !phEqualStrings(det.summary(), []string{"block@3"}) {
		t.Fatalf("one more identical call crosses block: detections = %v", det.summary())
	}
}

// Upstream: "ON→OFF: stale doomLoop blob is inert; OFF run repeats freely".
func TestDoomLoopStaleStateInertWhenOff(t *testing.T) {
	acc := &phMemState{}
	dlRunPersistent(t, acc, "x", "run 1 done")
	before := phJSON(t, acc.get().DoomLoop)

	det := &dlDetections{}
	rec := &phCounter{}
	sender := phSender(dlCallTurn(4, "web_search", `{"query":"x"}`), dlCallTurn(5, "web_search", `{"query":"x"}`), phTextResp("resp_6", "run 2 done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "more", Tools: []Tool{dlSearchTool(rec)}, StateAccessor: acc, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "run 2 done" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if rec.count() != 2 || len(det.get()) != 0 {
		t.Fatalf("executed=%d detections=%v", rec.count(), det.summary())
	}
	if after := phJSON(t, acc.get().DoomLoop); after != before {
		t.Fatalf("an OFF run leaves the stale blob untouched: %s -> %s", before, after)
	}
}

// dlCondemned returns a state awaiting approval of call_p (risky) whose
// persisted doom-loop blob carries a stop verdict and a web_search streak.
func dlCondemned(t *testing.T, acc *phMemState) ConversationState {
	t.Helper()
	risky := phOKTool("risky", &phCounter{}, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	sender := phSender(dlCallTurn(1, "risky", `{"target":"t"}`))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{risky}, DoomLoop: &DoomLoopConfig{}, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := acc.get()
	if s == nil || s.Status != ConversationStatusAwaitingApproval || s.DoomLoop == nil {
		t.Fatalf("expected an approval pause with doom-loop state: %+v", s)
	}
	s.DoomLoop.Tools = map[string]DoomLoopStreak{"web_search": {Fingerprint: strings.Repeat("f", 64), Streak: 1}}
	s.DoomLoop.StopVerdict = &DoomLoopVerdict{
		Detector: DoomLoopDetectorToolFingerprint, Action: DoomLoopActionStop, Streak: 6,
		Fingerprint: strings.Repeat("f", 64), ToolName: "web_search", Message: "condemned",
	}
	if err := acc.Save(context.Background(), *s); err != nil {
		t.Fatal(err)
	}
	return *s
}

// Condemnation rule, decision resume: a persisted stop verdict is re-armed
// on ApproveToolCalls — the approved call runs, but no model request follows.
// Upstream D2: "doom-stop armed while executing approved calls makes no
// unsent-results request".
func TestDoomLoopStopVerdictReArmedOnDecisionResume(t *testing.T) {
	acc := &phMemState{}
	dlCondemned(t, acc)
	rec := &phCounter{}
	risky := phOKTool("risky", rec, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	sender := phSender(phTextResp("resp_x", "must never be fetched"))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Tools: []Tool{risky}, DoomLoop: &DoomLoopConfig{}, StateAccessor: acc,
		ApproveToolCalls: []string{"call_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// No response exists on this run (upstream's getResponse rejects too,
	// hence its `.catch`); settle through State instead.
	if _, err := result.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("a condemned run makes no model request on a decision resume; got %d", len(sender.requests))
	}
	if rec.count() != 1 {
		t.Fatalf("the approved call still resolves; executed %d", rec.count())
	}
	v := result.DoomLoopVerdict(context.Background())
	if v == nil || v.Message != "condemned" || v.Streak != 6 {
		t.Fatalf("verdict = %+v", v)
	}
	s := acc.get()
	if s == nil || s.DoomLoop == nil || s.DoomLoop.StopVerdict == nil || s.DoomLoop.StopVerdict.Message != "condemned" {
		t.Fatalf("stop verdict stays persisted: %+v", s)
	}
	if s.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("doom stop is terminal: consumed forced key must reset, got %q", s.ConsumedForcedToolChoiceKey)
	}
	dlAssertPaired(t, s.Messages)
}

// Upstream D3: "a fresh conversational turn clears the persisted stop
// verdict; streaks survive".
func TestDoomLoopFreshTurnClearsStopVerdict(t *testing.T) {
	acc := &phMemState{}
	sender := phSender(dlCallTurn(1, "web_search", `{"query":"x"}`), phTextResp("resp_2", "paused here"))
	first, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{dlSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := acc.get()
	base.DoomLoop.StopVerdict = &DoomLoopVerdict{Detector: DoomLoopDetectorToolFingerprint, Action: DoomLoopActionStop, Streak: 6, Fingerprint: strings.Repeat("f", 64), ToolName: "web_search", Message: "condemned"}
	if err := acc.Save(context.Background(), *base); err != nil {
		t.Fatal(err)
	}

	next := phSender(phTextResp("resp_3", "recovered"))
	result, err := CallModel(context.Background(), next, CallModelInput{
		Model: "openai/test", Input: "try something new", Tools: []Tool{dlSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: acc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "recovered" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if result.DoomLoopVerdict(context.Background()) != nil {
		t.Fatal("a fresh conversational turn clears the stop verdict")
	}
	s := acc.get()
	if s.DoomLoop == nil || s.DoomLoop.StopVerdict != nil {
		t.Fatalf("cleared from persistence too: %+v", s.DoomLoop)
	}
	if s.DoomLoop.Tools["web_search"].Streak != 1 {
		t.Fatalf("streaks survive the clear: %+v", s.DoomLoop.Tools)
	}
}

// Upstream D2 (second half): after an approval resume, the model repeating
// the call crosses stop at the checkpoint before a second approval pause.
func TestDoomLoopStopAfterApprovalResumeCheckpoint(t *testing.T) {
	acc := &phMemState{}
	cfg := &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 1, Block: DoomLoopDisabled, Stop: 2}}
	tools := func(rec *phCounter) []Tool {
		return []Tool{phOKTool("risky", rec, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })}
	}
	choice := phChoice(t, `"required"`)
	first, err := CallModel(context.Background(), phSender(dlCallTurn(1, "risky", `{"target":"same"}`)), CallModelInput{
		Model: "openai/test", Input: "do the risky thing", Tools: tools(&phCounter{}), DoomLoop: cfg, StateAccessor: acc,
		Request: components.ResponsesRequest{ToolChoice: choice},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := acc.get(); s == nil || s.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected approval pause: %+v", s)
	}

	rec := &phCounter{}
	sender := phSender(dlCallTurn(2, "risky", `{"target":"same"}`), phTextResp("resp_3", "never fetched"))
	resumed, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Tools: tools(rec), DoomLoop: cfg, StateAccessor: acc,
		Request: components.ResponsesRequest{ToolChoice: choice}, ApproveToolCalls: []string{"call_1"}, MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := resumed.DoomLoopVerdict(context.Background())
	if v == nil || v.Action != DoomLoopActionStop || v.Streak != 2 || v.ToolName != "risky" {
		t.Fatalf("verdict = %+v", v)
	}
	if len(sender.requests) != 1 || rec.count() != 1 {
		t.Fatalf("requests=%d executed=%d: no request after the doom stop", len(sender.requests), rec.count())
	}
	s := acc.get()
	if s == nil || s.DoomLoop == nil || s.DoomLoop.StopVerdict == nil || s.DoomLoop.StopVerdict.Streak != 2 {
		t.Fatalf("stop verdict persisted: %+v", s)
	}
	if s.ConsumedForcedToolChoiceKey != "" {
		t.Fatalf("consumed forced key must reset on a doom stop, got %q", s.ConsumedForcedToolChoiceKey)
	}
	if s.Status == ConversationStatusAwaitingApproval {
		t.Fatal("the condemned call must not open a second approval pause")
	}
	dlAssertPaired(t, s.Messages)
}

// dlServerSearch decodes a web_search_call server-tool output item.
func dlServerSearch(t *testing.T, n int, query string) components.OutputItems {
	t.Helper()
	var item components.OutputItems
	raw := `{"type":"web_search_call","id":"ws_` + itoa(n) + `","status":"completed","action":{"type":"search","query":` + phJSON(t, query) + `}}`
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("decode server item: %v", err)
	}
	return item
}

// Upstream H2: "repeated identical server-tool calls build a streak and can
// stop the run" and "distinct server-tool queries do not build a streak".
func TestDoomLoopServerToolRepetition(t *testing.T) {
	t.Run("identical", func(t *testing.T) {
		det := &dlDetections{}
		var resps []components.OpenResponsesResult
		for i := 1; i <= 3; i++ {
			resps = append(resps, phResp("resp_"+itoa(i), dlServerSearch(t, i, "same query"), phFnCall("call_"+itoa(i), "note", `{"target":"`+itoa(i)+`"}`)))
		}
		resps = append(resps, phTextResp("resp_4", "done"))
		sender := phSender(resps...)
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "openai/test", Input: "search", Tools: []Tool{phOKTool("note", &phCounter{}, nil)}, MaxTurns: 10,
			DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}}, Hooks: dlHooks(det, nil),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Response(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sender.requests) != 3 {
			t.Fatalf("requests = %d, want 3", len(sender.requests))
		}
		var detectors []string
		for _, p := range det.get() {
			detectors = append(detectors, string(p.Detector))
		}
		if !phEqualStrings(detectors, []string{"server-tool-fingerprint", "server-tool-fingerprint"}) {
			t.Fatalf("detectors = %v", detectors)
		}
		v := result.DoomLoopVerdict(context.Background())
		if v == nil || v.Detector != DoomLoopDetectorServerToolFingerprint || v.Action != DoomLoopActionStop || v.Streak != 3 || v.ToolName != "server:web_search_call" {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("distinct", func(t *testing.T) {
		det := &dlDetections{}
		sender := phSender(
			phResp("resp_1", dlServerSearch(t, 1, "first"), phFnCall("call_1", "note", `{"target":"1"}`)),
			phResp("resp_2", dlServerSearch(t, 2, "second"), phFnCall("call_2", "note", `{"target":"2"}`)),
			phTextResp("resp_3", "done"),
		)
		result, err := CallModel(context.Background(), sender, CallModelInput{
			Model: "openai/test", Input: "search", Tools: []Tool{phOKTool("note", &phCounter{}, nil)}, MaxTurns: 10,
			DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil),
		})
		if err != nil {
			t.Fatal(err)
		}
		if text, err := result.Text(context.Background()); err != nil || text != "done" || len(det.get()) != 0 {
			t.Fatalf("text=%q err=%v detections=%v", text, err, det.summary())
		}
	})
}

// Upstream D5: "loopKey returning undefined falls back to full args (varying
// args ⇒ no false streak)" (Go: (nil, nil)); and H1: a varying nonce evades
// the default fingerprint but a loopKey over the meaningful field catches it.
func TestDoomLoopFallbackAndNonceIdentity(t *testing.T) {
	type nonceInput struct {
		Target string `json:"target"`
		Nonce  string `json:"nonce"`
	}
	run := func(t *testing.T, key ToolLoopKey, args []string) *dlDetections {
		det := &dlDetections{}
		tool := MustNewTool(ToolConfig[nonceInput]{
			Name: "buggy", InputSchema: phObjectSchema("target"), LoopKey: key,
			Execute: func(context.Context, nonceInput, ToolExecuteContext) (any, error) {
				return map[string]any{"ok": true}, nil
			},
		})
		var resps []components.OpenResponsesResult
		for i, a := range args {
			resps = append(resps, dlCallTurn(i+1, "buggy", a))
		}
		resps = append(resps, phTextResp("resp_x", "done"))
		result, err := CallModel(context.Background(), phSender(resps...), CallModelInput{
			Model: "openai/test", Input: "go", Tools: []Tool{tool}, DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if text, err := result.Text(context.Background()); err != nil || text != "done" {
			t.Fatalf("text=%q err=%v", text, err)
		}
		return det
	}
	varying := []string{`{"target":"a"}`, `{"target":"b"}`, `{"target":"c"}`}
	if det := run(t, func(map[string]any) (any, error) { return nil, nil }, varying); len(det.get()) != 0 {
		t.Fatalf("(nil, nil) falls back to full args: distinct args must not streak, got %v", det.summary())
	}
	nonces := []string{`{"target":"same","nonce":"1"}`, `{"target":"same","nonce":"2"}`, `{"target":"same","nonce":"3"}`}
	if det := run(t, nil, nonces); len(det.get()) != 0 {
		t.Fatalf("documented miss: a fresh nonce each turn evades the default fingerprint, got %v", det.summary())
	}
	if det := run(t, LoopKeyFields("target"), nonces); !phEqualStrings(det.summary(), []string{"observe@2", "block@3"}) {
		t.Fatalf("a loopKey over the meaningful field catches the loop, got %v", det.summary())
	}
}

// Upstream: "escalates observe -> block through callModel and stops the
// whole fan-out" — verdicts come from the ROUND detector.
func TestDoomLoopDistinctFanOutRoundDetector(t *testing.T) {
	rec := &phCounter{}
	det := &dlDetections{}
	fan := func(n int) components.OpenResponsesResult {
		var items []components.OutputItems
		for i, p := range []string{"a", "b", "c"} {
			items = append(items, phFnCall("call_"+itoa(n)+"_"+itoa(i), "read", `{"target":"`+p+`"}`))
		}
		return phResp("resp_"+itoa(n), items...)
	}
	sender := phSender(fan(1), fan(2), fan(3), fan(4), phTextResp("resp_5", "recovered"))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "Summarize.", Tools: []Tool{phOKTool("read", rec, nil)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := result.Text(context.Background()); err != nil || text != "recovered" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	want := []string{"observe@2", "observe@2", "observe@2", "block@3", "block@3", "block@3", "block@4", "block@4", "block@4"}
	if !phEqualStrings(det.summary(), want) {
		t.Fatalf("detections = %v, want %v", det.summary(), want)
	}
	var executed []string
	for _, v := range rec.values() {
		executed = append(executed, v.(phTargetInput).Target)
	}
	// A round's calls run concurrently, so compare as a multiset.
	sort.Strings(executed)
	if !phEqualStrings(executed, []string{"a", "a", "b", "b", "c", "c"}) {
		t.Fatalf("only rounds 1-2 execute: %v", executed)
	}
	for _, d := range det.get() {
		if !strings.Contains(d.Message, "same set of 3 parallel calls") {
			t.Fatalf("verdict must come from the round detector: %q", d.Message)
		}
	}
}

// Upstream: "a repeat sharing its id with a varying sibling is still
// detected".
func TestDoomLoopDuplicateCallIDsStillDetected(t *testing.T) {
	det := &dlDetections{}
	dup := func(round int) components.OpenResponsesResult {
		mk := func(idx int, target string) components.OutputItems {
			id := "fc_" + itoa(round) + "_" + itoa(idx)
			return components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{
				ID: &id, CallID: "call_X", Name: "read", Arguments: `{"target":"` + target + `"}`,
				Type: components.OutputFunctionCallItemTypeFunctionCall,
			})
		}
		return phResp("resp_"+itoa(round), mk(0, "a"), mk(1, "b"+itoa(round)))
	}
	sender := phSender(dup(0), dup(1), dup(2), dup(3), phTextResp("resp_4", "done"))
	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "go", Tools: []Tool{phOKTool("read", &phCounter{}, nil)},
		DoomLoop: &DoomLoopConfig{}, Hooks: dlHooks(det, nil), MaxTurns: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = result.Text(context.Background()) // upstream tolerates a failure here (.catch)
	var observe, block bool
	for _, p := range det.get() {
		observe = observe || p.Action == DoomLoopActionObserve
		block = block || p.Action == DoomLoopActionBlock
	}
	if !observe || !block {
		t.Fatalf("the repeated call accumulates despite the id collision: %v", det.summary())
	}
}
