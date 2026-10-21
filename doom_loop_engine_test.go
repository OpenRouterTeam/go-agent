package agent

// Engine-level doom-loop tests, ported from upstream
// tests/unit/doom-loop-integration.test.ts, doom-loop-remediation.test.ts and
// the engine half of doom-loop-escalation.test.ts. No live model: the
// "simulated LLM" is fakeSender playing scripted responses back in order,
// and each scenario asserts the exact request on which the ladder fires and
// how the engine responds (hook emission, blocked tool output, halted loop,
// persisted state, escalated request shape).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// doomScript builds scripted model turns with unique call/item ids.
type doomScript struct{ n int }

func (s *doomScript) call(name string, args any) components.OutputItems {
	s.n++
	b, _ := json.Marshal(args)
	id := fmt.Sprintf("fc_%d", s.n)
	return components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{ID: &id, CallID: fmt.Sprintf("call_%d", s.n), Name: name, Arguments: string(b)})
}

func (s *doomScript) msg(text string) components.OutputItems {
	s.n++
	return components.CreateOutputItemsMessage(components.OutputMessageItem{
		ID:   fmt.Sprintf("msg_%d", s.n),
		Role: components.OutputMessageItemRoleAssistant,
		Type: components.OutputMessageItemTypeMessage,
		Content: []components.OutputMessageItemContent{components.CreateOutputMessageItemContentOutputText(components.ResponseOutputText{
			Text: text, Type: components.ResponseOutputTextTypeOutputText,
		})},
	})
}

// serverSearch is a web_search_call server-tool output item echoing its query.
func (s *doomScript) serverSearch(t *testing.T, query string) components.OutputItems {
	t.Helper()
	s.n++
	raw := fmt.Sprintf(`{"type":"web_search_call","id":"ws_%d","status":"completed","action":{"type":"search","query":%q}}`, s.n, query)
	var item components.OutputItems
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func (s *doomScript) turn(items ...components.OutputItems) components.OpenResponsesResult {
	s.n++
	return components.OpenResponsesResult{ID: fmt.Sprintf("resp_%d", s.n), Model: "test-model", Status: components.OpenAIResponsesResponseStatusCompleted, Output: items}
}

func (s *doomScript) toolTurn(name string, args any) components.OpenResponsesResult {
	return s.turn(s.call(name, args))
}

func (s *doomScript) textTurn(text string) components.OpenResponsesResult {
	return s.turn(s.msg(text))
}

func scripted(turns ...components.OpenResponsesResult) *fakeSender {
	sender := &fakeSender{}
	for _, turn := range turns {
		created := operations.CreateCreateResponsesResponseOpenResponsesResult(turn)
		sender.responses = append(sender.responses, &created)
	}
	return sender
}

// sentToolOutputs returns the function_call_output payloads in request i.
func sentToolOutputs(sender *fakeSender, i int) []string {
	var out []string
	if i >= len(sender.requests) || sender.requests[i].Input == nil {
		return nil
	}
	for _, item := range sender.requests[i].Input.ArrayOfInputsUnion1 {
		if item.FunctionCallOutputItem != nil && item.FunctionCallOutputItem.Output.Str != nil {
			out = append(out, *item.FunctionCallOutputItem.Output.Str)
		}
	}
	return out
}

// userMessages returns the user-role string messages in items.
func userMessages(items []components.InputsUnion1) []string {
	var out []string
	for _, item := range items {
		msg := item.EasyInputMessage
		if msg == nil || msg.Role.EasyInputMessageRoleUser == nil {
			continue
		}
		if content, ok := msg.Content.GetOrZero(); ok && content.Str != nil {
			out = append(out, *content.Str)
		}
	}
	return out
}

func requestInput(sender *fakeSender, i int) []components.InputsUnion1 {
	if i >= len(sender.requests) || sender.requests[i].Input == nil {
		return nil
	}
	return sender.requests[i].Input.ArrayOfInputsUnion1
}

// assertWellFormedHistory asserts every function_call has a matching output.
func assertWellFormedHistory(t *testing.T, items []components.InputsUnion1) {
	t.Helper()
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		switch {
		case item.FunctionCallItem != nil:
			calls[item.FunctionCallItem.CallID] = true
		case item.OutputFunctionCallItem != nil:
			calls[item.OutputFunctionCallItem.CallID] = true
		case item.FunctionCallOutputItem != nil:
			outputs[item.FunctionCallOutputItem.CallID] = true
		}
	}
	for id := range calls {
		if !outputs[id] {
			t.Fatalf("function_call %s has no matching output", id)
		}
	}
}

type searchArgs struct {
	Query string `json:"query"`
}

// doomSearchTool mirrors upstream's searchTool: identity is the normalized query.
func doomSearchTool(executions *int) Tool {
	return MustNewTool(ToolConfig[searchArgs]{
		Name: "web_search",
		LoopKey: LoopKeyFunc(func(in searchArgs) (any, error) {
			return strings.ToLower(strings.TrimSpace(in.Query)), nil
		}),
		Execute: func(_ context.Context, in searchArgs, _ ToolExecuteContext) (any, error) {
			if executions != nil {
				*executions++
			}
			return map[string]any{"results": []string{"result for " + in.Query}}, nil
		},
	})
}

type detectionLog struct{ events []DoomLoopDetectedPayload }

func (d *detectionLog) pairs() [][2]any {
	var out [][2]any
	for _, e := range d.events {
		out = append(out, [2]any{e.Action, e.Streak})
	}
	return out
}

func collectDetections(h *HooksManager) *detectionLog {
	log := &detectionLog{}
	h.OnDoomLoopDetected(HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]{Handler: func(p DoomLoopDetectedPayload, _ LifecycleHookContext) (HookHandlerResult[DoomLoopDetectedResult], error) {
		log.events = append(log.events, p)
		return VoidResult[DoomLoopDetectedResult](), nil
	}})
	return log
}

func runText(t *testing.T, sender *fakeSender, input CallModelInput) (*ModelResult, string) {
	t.Helper()
	if input.Model == "" {
		input.Model = "test-model"
	}
	if input.Input == nil && input.State == nil && input.StateAccessor == nil {
		input.Input = "go"
	}
	result, err := CallModel(context.Background(), sender, input)
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result, text
}

func verdictOf(t *testing.T, r *ModelResult) *DoomLoopVerdict {
	t.Helper()
	v, err := r.DoomLoopVerdict(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func pairsEqual(got [][2]any, want ...[2]any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDoomLoopObservesThenBlocksAndModelRecovers(t *testing.T) {
	var s doomScript
	executions := 0
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	sender := scripted(
		s.toolTurn("web_search", searchArgs{"openrouter sdk"}),
		s.toolTurn("web_search", searchArgs{"OpenRouter SDK"}),
		s.toolTurn("web_search", searchArgs{"  openrouter sdk  "}),
		s.textTurn("Giving up on repeating; here is my answer."),
	)
	_, text := runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(&executions)}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != "Giving up on repeating; here is my answer." {
		t.Fatalf("text = %q", text)
	}
	if executions != 2 {
		t.Fatalf("calls 1-2 execute, call 3 is blocked before execution; got %d executions", executions)
	}
	if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) {
		t.Fatalf("detections = %v", detections.pairs())
	}
	if d := detections.events[0]; d.Detector != DoomLoopDetectorToolFingerprint || d.ToolName != "web_search" || d.ToolInput["query"] != "OpenRouter SDK" {
		t.Fatalf("payload = %#v", d)
	}
	outputs := sentToolOutputs(sender, 3)
	blocked := outputs[len(outputs)-1]
	for _, want := range []string{"Doom loop suspected", "web_search", "3 consecutive rounds"} {
		if !strings.Contains(blocked, want) {
			t.Fatalf("blocked output %q must contain %q", blocked, want)
		}
	}
}

func TestDoomLoopStopsRunAtStopThreshold(t *testing.T) {
	var s doomScript
	var turns []components.OpenResponsesResult
	for i := 0; i < 10; i++ {
		turns = append(turns, s.toolTurn("web_search", searchArgs{"same query"}))
	}
	sender := scripted(turns...)
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "test-model", Input: "Find it.", MaxTurns: 20, Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := result.Response(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sender.calls != 4 {
		t.Fatalf("the loop must halt before a 5th request; got %d requests", sender.calls)
	}
	v := verdictOf(t, result)
	if v == nil || v.Action != DoomLoopActionStop || v.Detector != DoomLoopDetectorToolFingerprint || v.Streak != 4 || v.ToolName != "web_search" {
		t.Fatalf("verdict = %#v", v)
	}
	if resp.ID != turns[3].ID {
		t.Fatalf("the run surfaces the last response, got %s", resp.ID)
	}
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusComplete {
		t.Fatalf("status = %s", state.Status)
	}
	assertWellFormedHistory(t, state.Messages)
}

func sessionEndReasons(h *HooksManager) *[]SessionEndReason {
	reasons := &[]SessionEndReason{}
	h.OnSessionEnd(HookEntry[SessionEndPayload, EmptyHookResult]{Handler: func(p SessionEndPayload, _ LifecycleHookContext) (HookHandlerResult[EmptyHookResult], error) {
		*reasons = append(*reasons, p.Reason)
		return VoidResult[EmptyHookResult](), nil
	}})
	return reasons
}

func TestDoomLoopSessionEndReasonDoomLoop(t *testing.T) {
	var s doomScript
	hooks := NewHooksManager()
	reasons := sessionEndReasons(hooks)
	var turns []components.OpenResponsesResult
	for i := 0; i < 6; i++ {
		turns = append(turns, s.toolTurn("web_search", searchArgs{"x"}))
	}
	runText(t, scripted(turns...), CallModelInput{MaxTurns: 10, Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Stop: 3}}, Hooks: hooks})
	if len(*reasons) != 1 || (*reasons)[0] != SessionEndReasonDoomLoop {
		t.Fatalf("SessionEnd reasons = %v", *reasons)
	}
}

func TestDoomLoopHealthyRunAndInertWhenOff(t *testing.T) {
	var s doomScript
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	sender := scripted(s.toolTurn("web_search", searchArgs{"first"}), s.toolTurn("web_search", searchArgs{"second"}), s.toolTurn("web_search", searchArgs{"third"}), s.textTurn("All three found."))
	_, text := runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != "All three found." || len(detections.events) != 0 || sender.calls != 4 {
		t.Fatalf("healthy run disturbed: text=%q detections=%v calls=%d", text, detections.pairs(), sender.calls)
	}

	executions := 0
	sender = scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("done"))
	result, text := runText(t, sender, CallModelInput{MaxTurns: 10, Tools: []Tool{doomSearchTool(&executions)}})
	if text != "done" || executions != 4 {
		t.Fatalf("detection off must be inert: text=%q executions=%d", text, executions)
	}
	for i := 1; i <= 4; i++ {
		for _, out := range sentToolOutputs(sender, i) {
			if strings.Contains(out, "Doom loop") {
				t.Fatalf("request %d carries a doom output with detection off", i)
			}
		}
	}
	if state, _ := result.State(context.Background()); state.DoomLoop != nil {
		t.Fatalf("detection off must not persist detector state: %#v", state.DoomLoop)
	}
}

func TestDoomLoopRepeatedEmptyCallsBlock(t *testing.T) {
	var s doomScript
	listTool := MustNewTool(ToolConfig[struct{}]{Name: "list_tasks", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
		return map[string]any{"tasks": []string{"task-1"}}, nil
	}})
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	sender := scripted(s.toolTurn("list_tasks", map[string]any{}), s.toolTurn("list_tasks", map[string]any{}), s.toolTurn("list_tasks", map[string]any{}), s.textTurn("The list has not changed; stopping."))
	_, text := runText(t, sender, CallModelInput{Tools: []Tool{listTool}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != "The list has not changed; stopping." {
		t.Fatalf("text = %q", text)
	}
	if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) {
		t.Fatalf("detections = %v", detections.pairs())
	}
	outputs := sentToolOutputs(sender, 3)
	if !strings.Contains(outputs[len(outputs)-1], "Doom loop suspected") {
		t.Fatalf("blocked output = %q", outputs[len(outputs)-1])
	}
}

func TestDoomLoopTextRepetitionStopsNoToolsRun(t *testing.T) {
	var s doomScript
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	reasons := sessionEndReasons(hooks)
	doomText := strings.TrimSpace(strings.Repeat("I need to check the file again. ", 40))
	result, text := runText(t, scripted(s.textTurn(doomText)), CallModelInput{Input: "Summarize.", DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != doomText {
		t.Fatal("tokens already emitted are surfaced as-is")
	}
	if len(detections.events) != 1 || detections.events[0].Detector != DoomLoopDetectorTextRepetition || detections.events[0].Action != DoomLoopActionStop || detections.events[0].Streak != 40 {
		t.Fatalf("detections = %#v", detections.events)
	}
	if v := verdictOf(t, result); v == nil || v.Detector != DoomLoopDetectorTextRepetition || v.Action != DoomLoopActionStop {
		t.Fatalf("verdict = %#v", v)
	}
	if len(*reasons) != 1 || (*reasons)[0] != SessionEndReasonDoomLoop {
		t.Fatalf("text-only stop must report doom_loop, got %v", *reasons)
	}
}

func TestDoomLoopTextStreakHaltsToolLoop(t *testing.T) {
	var s doomScript
	filler := "Hmm, let me look somewhere else."
	var turns []components.OpenResponsesResult
	for i := 1; i <= 5; i++ {
		turns = append(turns, s.turn(s.msg(filler), s.call("web_search", searchArgs{fmt.Sprintf("q%d", i)})))
	}
	sender := scripted(turns...)
	result, _ := runText(t, sender, CallModelInput{MaxTurns: 10, Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}}})
	if sender.calls != 3 {
		t.Fatalf("text streak stop arms after turn 3; got %d requests", sender.calls)
	}
	if v := verdictOf(t, result); v == nil || v.Detector != DoomLoopDetectorTextStreak || v.Action != DoomLoopActionStop || v.Streak != 3 {
		t.Fatalf("verdict = %#v", v)
	}
}

type bashArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Verbose bool   `json:"verbose,omitempty"`
}

func TestDoomLoopToolLoopKeyForms(t *testing.T) {
	t.Run("field list: same command in a different cwd is not a loop", func(t *testing.T) {
		var s doomScript
		var runs []string
		bash := MustNewTool(ToolConfig[bashArgs]{Name: "bash", LoopKey: LoopKeyFields("command", "cwd"), Execute: func(_ context.Context, in bashArgs, _ ToolExecuteContext) (any, error) {
			runs = append(runs, in.Cwd+":"+in.Command)
			return map[string]any{"stdout": "ok"}, nil
		}})
		_, text := runText(t, scripted(s.toolTurn("bash", bashArgs{Command: "ls", Cwd: "/a"}), s.toolTurn("bash", bashArgs{Command: "ls", Cwd: "/b"}), s.textTurn("both listed")), CallModelInput{Tools: []Tool{bash}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Block: 2}}})
		if text != "both listed" || strings.Join(runs, ",") != "/a:ls,/b:ls" {
			t.Fatalf("text=%q runs=%v", text, runs)
		}
	})
	t.Run("field list collapses excluded-field variation", func(t *testing.T) {
		var s doomScript
		executions := 0
		bash := MustNewTool(ToolConfig[bashArgs]{Name: "bash", LoopKey: LoopKeyFields("command", "cwd"), Execute: func(context.Context, bashArgs, ToolExecuteContext) (any, error) {
			executions++
			return "ok", nil
		}})
		hooks := NewHooksManager()
		detections := collectDetections(hooks)
		runText(t, scripted(s.toolTurn("bash", bashArgs{Command: "ls", Cwd: "/a"}), s.toolTurn("bash", bashArgs{Command: "ls", Cwd: "/a", Verbose: true}), s.textTurn("done")), CallModelInput{Tools: []Tool{bash}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Block: 2}}, Hooks: hooks})
		if executions != 1 || !pairsEqual(detections.pairs(), [2]any{DoomLoopActionBlock, 2}) {
			t.Fatalf("executions=%d detections=%v", executions, detections.pairs())
		}
	})
	t.Run("exempt and nil-returning functions exempt legitimate repetition", func(t *testing.T) {
		for name, key := range map[string]*ToolLoopKey{
			"static":   LoopKeyExempt(),
			"per-call": {Func: func(map[string]any) (any, error) { return nil, nil }},
		} {
			var s doomScript
			polls := 0
			poller := MustNewTool(ToolConfig[struct{}]{Name: "poll_status", LoopKey: key, Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
				polls++
				return "pending", nil
			}})
			hooks := NewHooksManager()
			detections := collectDetections(hooks)
			var turns []components.OpenResponsesResult
			for i := 0; i < 5; i++ {
				turns = append(turns, s.toolTurn("poll_status", map[string]any{}))
			}
			turns = append(turns, s.textTurn("ready"))
			runText(t, scripted(turns...), CallModelInput{MaxTurns: 10, Tools: []Tool{poller}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
			if polls != 5 || len(detections.events) != 0 {
				t.Fatalf("%s: polls=%d detections=%v", name, polls, detections.pairs())
			}
		}
	})
	t.Run("erroring loop key falls back to full-arguments identity", func(t *testing.T) {
		var s doomScript
		tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", LoopKey: &ToolLoopKey{Func: func(map[string]any) (any, error) { return nil, fmt.Errorf("loopKey bug") }}, Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
			return "r", nil
		}})
		hooks := NewHooksManager()
		detections := collectDetections(hooks)
		logs := captureLog(t, func() {
			runText(t, scripted(s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("done")), CallModelInput{Tools: []Tool{tool}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
		})
		if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) {
			t.Fatalf("detections = %v", detections.pairs())
		}
		if !strings.Contains(logs, "loopKey threw (loopKey bug); falling back to full arguments") {
			t.Fatalf("fallback must warn: %q", logs)
		}
	})
	t.Run("MarkMcp attaches a loop key to a wrapped tool", func(t *testing.T) {
		var s doomScript
		var runs int
		inner := MustNewTool(ToolConfig[bashArgs]{Name: "remote_bash", Execute: func(context.Context, bashArgs, ToolExecuteContext) (any, error) {
			runs++
			return "ok", nil
		}})
		marked := MarkMcp(inner, MarkMcpOptions{LoopKey: LoopKeyFields("command")})
		if !IsMcpTool(marked) {
			t.Fatal("MarkMcp must still brand the tool")
		}
		runText(t, scripted(s.toolTurn("remote_bash", bashArgs{Command: "ls", Cwd: "/a"}), s.toolTurn("remote_bash", bashArgs{Command: "ls", Cwd: "/b"}), s.textTurn("done")), CallModelInput{Tools: []Tool{marked}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Block: 2}}})
		if runs != 1 {
			t.Fatalf("the MCP loop key (command only) must collapse different cwds; runs=%d", runs)
		}
	})
}

func TestDoomLoopNonceArgsEvadeDefaultIdentity(t *testing.T) {
	type nonceArgs struct {
		Command string `json:"command"`
		Nonce   string `json:"nonce"`
	}
	script := func(s *doomScript) []components.OpenResponsesResult {
		var turns []components.OpenResponsesResult
		for i := 0; i < 4; i++ {
			turns = append(turns, s.toolTurn("run", nonceArgs{Command: "deploy", Nonce: fmt.Sprintf("n%d", i)}))
		}
		return append(turns, s.textTurn("done"))
	}
	execute := func(context.Context, nonceArgs, ToolExecuteContext) (any, error) { return "ok", nil }
	var s doomScript
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	runText(t, scripted(script(&s)...), CallModelInput{MaxTurns: 10, Tools: []Tool{MustNewTool(ToolConfig[nonceArgs]{Name: "run", Execute: execute})}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if len(detections.events) != 0 {
		t.Fatalf("DOCUMENTED MISS: a fresh nonce each turn evades the default identity; got %v", detections.pairs())
	}
	hooks = NewHooksManager()
	detections = collectDetections(hooks)
	runText(t, scripted(script(&s)...), CallModelInput{MaxTurns: 10, Tools: []Tool{MustNewTool(ToolConfig[nonceArgs]{Name: "run", LoopKey: LoopKeyFields("command"), Execute: execute})}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if len(detections.events) == 0 {
		t.Fatal("a field-list loop key must catch the nonce loop")
	}
}

func TestDoomLoopCheckpointRunsBeforePreToolUseMutation(t *testing.T) {
	var s doomScript
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	preToolUse := 0
	nonce := 0
	hooks.OnPreToolUse(HookEntry[PreToolUsePayload, PreToolUseResult]{Handler: func(p PreToolUsePayload, _ LifecycleHookContext) (HookHandlerResult[PreToolUseResult], error) {
		preToolUse++
		nonce++
		return SyncResult(PreToolUseResult{MutatedInput: map[string]any{"query": fmt.Sprintf("%v-%d", p.ToolInput["query"], nonce)}}), nil
	}})
	executions := 0
	tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
		executions++
		return "r", nil
	}})
	runText(t, scripted(s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("done")), CallModelInput{Tools: []Tool{tool}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) {
		t.Fatalf("a mutating PreToolUse must not mask the model's repetition: %v", detections.pairs())
	}
	if preToolUse != 2 || executions != 2 {
		t.Fatalf("PreToolUse must not fire for a doom-blocked call: preToolUse=%d executions=%d", preToolUse, executions)
	}
}

func TestDoomLoopHookOverrideAction(t *testing.T) {
	override := func(action DoomLoopAction) *HooksManager {
		h := NewHooksManager()
		h.OnDoomLoopDetected(HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]{Handler: func(DoomLoopDetectedPayload, LifecycleHookContext) (HookHandlerResult[DoomLoopDetectedResult], error) {
			return SyncResult(DoomLoopDetectedResult{OverrideAction: action}), nil
		}})
		return h
	}
	var s doomScript
	executions := 0
	noisy := MustNewTool(ToolConfig[struct{}]{Name: "noisy", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
		executions++
		return map[string]any{"ok": true}, nil
	}})
	_, text := runText(t, scripted(s.toolTurn("noisy", map[string]any{}), s.toolTurn("noisy", map[string]any{}), s.toolTurn("noisy", map[string]any{}), s.textTurn("done")), CallModelInput{Tools: []Tool{noisy}, DoomLoop: &DoomLoopConfig{}, Hooks: override(DoomLoopActionObserve)})
	if text != "done" || executions != 3 {
		t.Fatalf("de-escalated block must execute: text=%q executions=%d", text, executions)
	}

	sender := scripted(s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}))
	result, _ := runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, Hooks: override(DoomLoopActionStop)})
	if sender.calls != 2 {
		t.Fatalf("observe escalated to stop halts after the 2nd turn; requests=%d", sender.calls)
	}
	if v := verdictOf(t, result); v == nil || v.Streak != 2 {
		t.Fatalf("verdict = %#v", v)
	}

	// A block override on a text verdict downgrades to observe (nothing to block).
	var inlineSeen int
	inline := InlineHookConfig{DoomLoopDetected: []HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]{{Handler: func(DoomLoopDetectedPayload, LifecycleHookContext) (HookHandlerResult[DoomLoopDetectedResult], error) {
		inlineSeen++
		return SyncResult(DoomLoopDetectedResult{OverrideAction: DoomLoopActionBlock}), nil
	}}}}
	result, _ = runText(t, scripted(s.textTurn(strings.Repeat("loop ", 30))), CallModelInput{DoomLoop: &DoomLoopConfig{}, Hooks: inline})
	if inlineSeen != 1 || verdictOf(t, result) != nil {
		t.Fatalf("inline DoomLoopDetected must fire, and a text block override must downgrade (not stop): seen=%d", inlineSeen)
	}
}

func TestDoomLoopStatePersistsAcrossResume(t *testing.T) {
	var s doomScript
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	runText(t, scripted(s.toolTurn("web_search", searchArgs{"persistent"}), s.toolTurn("web_search", searchArgs{"persistent"}), s.textTurn("pausing here")), CallModelInput{Input: "go", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: accessor})
	if accessor.state.DoomLoop == nil || accessor.state.DoomLoop.Tools["web_search"].Streak != 2 {
		t.Fatalf("streak 2 must persist: %#v", accessor.state.DoomLoop)
	}
	// The detector blob survives the versioned serialization contract.
	blob, err := SerializeConversationState(accessor.state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(blob)
	if err != nil {
		t.Fatal(err)
	}
	if restored.DoomLoop == nil || restored.DoomLoop.Tools["web_search"] != accessor.state.DoomLoop.Tools["web_search"] || *restored.DoomLoop.Text != *accessor.state.DoomLoop.Text {
		t.Fatalf("doomLoop must round-trip: %#v vs %#v", restored.DoomLoop, accessor.state.DoomLoop)
	}
	if !strings.Contains(blob, `"DoomLoop":{"tools":{"web_search":{"fingerprint":`) {
		t.Fatalf("serialized shape: %s", blob)
	}

	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	runText(t, scripted(s.toolTurn("web_search", searchArgs{"persistent"}), s.textTurn("recovered")), CallModelInput{Input: "continue", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: accessor, Hooks: hooks})
	if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionBlock, 3}) {
		t.Fatalf("one more identical call after resume crosses block: %v", detections.pairs())
	}

	// ON→OFF: the stale blob is inert and left untouched; the OFF run repeats freely.
	executions := 0
	before := *accessor.state.DoomLoop
	runText(t, scripted(s.toolTurn("web_search", searchArgs{"persistent"}), s.toolTurn("web_search", searchArgs{"persistent"}), s.textTurn("free")), CallModelInput{Input: "again", Tools: []Tool{doomSearchTool(&executions)}, StateAccessor: accessor})
	if executions != 2 || accessor.state.DoomLoop == nil || accessor.state.DoomLoop.Tools["web_search"] != before.Tools["web_search"] {
		t.Fatalf("OFF run must be inert: executions=%d state=%#v", executions, accessor.state.DoomLoop)
	}
}

func TestDoomLoopParallelDuplicatesAreOneRound(t *testing.T) {
	var s doomScript
	executions := 0
	fanout := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
		executions++
		return "r", nil
	}})
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	var items []components.OutputItems
	for i := 0; i < 6; i++ {
		items = append(items, s.call("web_search", searchArgs{"same"}))
	}
	_, text := runText(t, scripted(s.turn(items...), s.textTurn("All six ran.")), CallModelInput{Tools: []Tool{fanout}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != "All six ran." || executions != 6 || len(detections.events) != 0 {
		t.Fatalf("6-wide fan-out is one piece of evidence: executions=%d detections=%v", executions, detections.pairs())
	}

	executions = 0
	noisy := MustNewTool(ToolConfig[struct{}]{Name: "noisy", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) {
		executions++
		return "ok", nil
	}})
	hooks = NewHooksManager()
	detections = collectDetections(hooks)
	sender := scripted(s.toolTurn("noisy", map[string]any{}), s.toolTurn("noisy", map[string]any{}), s.turn(s.call("noisy", map[string]any{}), s.call("noisy", map[string]any{}), s.call("noisy", map[string]any{})), s.textTurn("done"))
	_, text = runText(t, sender, CallModelInput{Tools: []Tool{noisy}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	if text != "done" || executions != 2 || !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) {
		t.Fatalf("round-3 duplicates share one block decision: executions=%d detections=%v", executions, detections.pairs())
	}
	blocked := 0
	for _, out := range sentToolOutputs(sender, 3) {
		if strings.Contains(out, "Doom loop suspected") {
			blocked++
		}
	}
	if blocked != 3 {
		t.Fatalf("all three duplicates get the block output, got %d", blocked)
	}
}

func TestDoomLoopTextStopSealsDanglingCalls(t *testing.T) {
	var s doomScript
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	hooks := NewHooksManager()
	reasons := sessionEndReasons(hooks)
	doomText := strings.TrimSpace(strings.Repeat("I am stuck. ", 20))
	executions := 0
	sender := scripted(s.turn(s.msg(doomText), s.call("web_search", searchArgs{"never runs"})))
	runText(t, sender, CallModelInput{Input: "go", Tools: []Tool{doomSearchTool(&executions)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: accessor, Hooks: hooks})
	if sender.calls != 1 || executions != 0 {
		t.Fatalf("only the initial request; the call never executes: calls=%d executions=%d", sender.calls, executions)
	}
	if len(*reasons) != 1 || (*reasons)[0] != SessionEndReasonDoomLoop {
		t.Fatalf("reasons = %v", *reasons)
	}
	assertWellFormedHistory(t, accessor.state.Messages)
	var halts []string
	for _, item := range accessor.state.Messages {
		if item.FunctionCallOutputItem != nil {
			halts = append(halts, *item.FunctionCallOutputItem.Output.Str)
		}
	}
	if len(halts) != 1 || !strings.Contains(halts[0], "halted by doom-loop detection") {
		t.Fatalf("synthesized halt output: %v", halts)
	}

	resume := scripted(s.textTurn("resumed fine"))
	_, text := runText(t, resume, CallModelInput{Input: "continue please", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{}, StateAccessor: accessor})
	if text != "resumed fine" {
		t.Fatalf("text = %q", text)
	}
	assertWellFormedHistory(t, requestInput(resume, 0))
}

func TestDoomLoopStopSkipsFinalResponseRequest(t *testing.T) {
	var s doomScript
	sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("should never be fetched"))
	stopAfterTwoSteps := func(_ context.Context, steps []StepResult) (bool, error) { return len(steps) >= 2, nil }
	result, _ := runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 2, Stop: 2}}, StopWhen: []StopCondition{stopAfterTwoSteps}, AllowFinalResponse: true})
	if sender.calls != 2 {
		t.Fatalf("the final text-coercion request must be skipped after a doom stop; requests=%d", sender.calls)
	}
	if v := verdictOf(t, result); v == nil || v.Action != DoomLoopActionStop || v.Streak != 2 {
		t.Fatalf("verdict = %#v", v)
	}
}

func TestDoomLoopApprovalResumeStopAndCondemnation(t *testing.T) {
	type riskyArgs struct {
		Target string `json:"target"`
	}
	risky := MustNewTool(ToolConfig[riskyArgs]{Name: "risky", RequireApproval: true, Execute: func(context.Context, riskyArgs, ToolExecuteContext) (any, error) {
		return map[string]any{"done": true}, nil
	}})
	cfg := func() *DoomLoopConfig {
		return &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 1, Block: DoomLoopRungDisabled, Stop: 2}}
	}
	var s doomScript
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	first := scripted(s.toolTurn("risky", riskyArgs{"same"}))
	if _, err := mustResult(t, first, CallModelInput{Model: "test-model", Input: "do the risky thing", Tools: []Tool{risky}, DoomLoop: cfg(), StateAccessor: accessor}).Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval || accessor.state.DoomLoop == nil {
		t.Fatalf("paused state: %s %#v", accessor.state.Status, accessor.state.DoomLoop)
	}
	firstCall := accessor.state.PendingToolCalls[0].CallID

	// Resume 1: approve → executes (streak 1); the model calls it again → pauses.
	resume1 := scripted(s.toolTurn("risky", riskyArgs{"same"}))
	if _, err := mustResult(t, resume1, CallModelInput{Model: "test-model", Tools: []Tool{risky}, DoomLoop: cfg(), StateAccessor: accessor, ApproveToolCalls: []string{firstCall}}).Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("status after resume 1 = %s", accessor.state.Status)
	}
	secondCall := accessor.state.PendingToolCalls[0].CallID

	// Resume 2: approving the identical call crosses stop (restored streak 1 + 1).
	resume2 := scripted(s.textTurn("must not be requested"))
	result := mustResult(t, resume2, CallModelInput{Model: "test-model", Tools: []Tool{risky}, DoomLoop: cfg(), StateAccessor: accessor, ApproveToolCalls: []string{secondCall}})
	v := verdictOf(t, result)
	if v == nil || v.Action != DoomLoopActionStop || v.Streak != 2 || v.ToolName != "risky" {
		t.Fatalf("verdict = %#v", v)
	}
	if resume2.calls != 0 {
		t.Fatalf("no unsent-results request after a doom stop; requests=%d", resume2.calls)
	}
	if accessor.state.DoomLoop.StopVerdict == nil || accessor.state.DoomLoop.StopVerdict.Streak != 2 {
		t.Fatalf("stop verdict must persist for decision-only resumes: %#v", accessor.state.DoomLoop)
	}
	assertWellFormedHistory(t, accessor.state.Messages)

	// D3: a fresh conversational turn clears the verdict; streaks survive.
	fresh := scripted(s.textTurn("recovered"))
	result, text := runText(t, fresh, CallModelInput{Input: "try something new", Tools: []Tool{risky}, DoomLoop: cfg(), StateAccessor: accessor})
	if text != "recovered" || verdictOf(t, result) != nil {
		t.Fatalf("fresh turn: text=%q", text)
	}
	if accessor.state.DoomLoop.StopVerdict != nil || accessor.state.DoomLoop.Tools["risky"].Streak != 2 {
		t.Fatalf("verdict cleared, streak kept: %#v", accessor.state.DoomLoop)
	}
}

func mustResult(t *testing.T, sender *fakeSender, input CallModelInput) *ModelResult {
	t.Helper()
	result, err := CallModel(context.Background(), sender, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDoomLoopSteer(t *testing.T) {
	steerLadder := DoomLoopLadder{Observe: 2, Steer: 2, Block: DoomLoopRungDisabled, Stop: 6}
	var s doomScript
	sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("adjusted course"))
	_, text := runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: steerLadder}})
	if text != "adjusted course" {
		t.Fatalf("text = %q", text)
	}
	steers := 0
	for _, m := range userMessages(requestInput(sender, 2)) {
		if strings.Contains(m, "Doom loop suspected") {
			steers++
		}
	}
	if steers != 1 {
		t.Fatalf("the third request must carry exactly one steer message, got %d", steers)
	}
	for _, m := range userMessages(requestInput(sender, 1)) {
		if strings.Contains(m, "Doom loop suspected") {
			t.Fatal("no steer before the verdict fired")
		}
	}

	// Steer queued at a HITL pause persists and is delivered on resume.
	type keyArgs struct {
		Key string `json:"key"`
	}
	type questionArgs struct {
		Question string `json:"question"`
	}
	lookup := MustNewTool(ToolConfig[keyArgs]{Name: "lookup", Execute: func(context.Context, keyArgs, ToolExecuteContext) (any, error) { return "v", nil }})
	askHuman := MustNewTool(ToolConfig[questionArgs]{Name: "ask_human", OnToolCalled: func(context.Context, questionArgs, ToolExecuteContext) (any, bool, error) {
		return nil, false, nil
	}})
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	runText(t, scripted(s.toolTurn("lookup", keyArgs{"same"}), s.turn(s.call("lookup", keyArgs{"same"}), s.call("ask_human", questionArgs{"help?"}))), CallModelInput{Input: "go", Tools: []Tool{lookup, askHuman}, DoomLoop: &DoomLoopConfig{Ladder: steerLadder}, StateAccessor: accessor})
	if accessor.state.Status != ConversationStatusAwaitingHITL {
		t.Fatalf("status = %s", accessor.state.Status)
	}
	if accessor.state.DoomLoop == nil || len(accessor.state.DoomLoop.PendingSteer) != 1 || !strings.Contains(accessor.state.DoomLoop.PendingSteer[0], "Doom loop suspected") {
		t.Fatalf("pending steer must persist, not be injected mid-pause: %#v", accessor.state.DoomLoop)
	}
	for _, m := range userMessages(accessor.state.Messages) {
		if strings.Contains(m, "Doom loop suspected") {
			t.Fatal("steer must not be injected between a dangling function_call and its output")
		}
	}
}

func TestDoomLoopServerToolRepetition(t *testing.T) {
	type noteArgs struct {
		N int `json:"n"`
	}
	note := MustNewTool(ToolConfig[noteArgs]{Name: "note", Execute: func(context.Context, noteArgs, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	var s doomScript
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	sender := scripted(
		s.turn(s.serverSearch(t, "same query"), s.call("note", noteArgs{1})),
		s.turn(s.serverSearch(t, "same query"), s.call("note", noteArgs{2})),
		s.turn(s.serverSearch(t, "same query"), s.call("note", noteArgs{3})),
		s.textTurn("done"),
	)
	result, _ := runText(t, sender, CallModelInput{Tools: []Tool{note}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}}, Hooks: hooks})
	if sender.calls != 3 {
		t.Fatalf("server streak stops after turn 3; requests=%d", sender.calls)
	}
	if len(detections.events) != 2 || detections.events[0].Detector != DoomLoopDetectorServerToolFingerprint || detections.events[1].Detector != DoomLoopDetectorServerToolFingerprint {
		t.Fatalf("detections = %#v", detections.events)
	}
	if v := verdictOf(t, result); v == nil || v.Detector != DoomLoopDetectorServerToolFingerprint || v.Action != DoomLoopActionStop || v.Streak != 3 || v.ToolName != "server:web_search_call" {
		t.Fatalf("verdict = %#v", v)
	}

	hooks = NewHooksManager()
	detections = collectDetections(hooks)
	sender = scripted(
		s.turn(s.serverSearch(t, "q1"), s.call("note", noteArgs{1})),
		s.turn(s.serverSearch(t, "q2"), s.call("note", noteArgs{2})),
		s.turn(s.serverSearch(t, "q3"), s.call("note", noteArgs{3})),
		s.textTurn("done"),
	)
	runText(t, sender, CallModelInput{Tools: []Tool{note}, DoomLoop: &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Block: 3, Stop: 3}}, Hooks: hooks})
	if len(detections.events) != 0 || sender.calls != 4 {
		t.Fatalf("distinct server queries must not streak: %v calls=%d", detections.pairs(), sender.calls)
	}
}

var escalateAt2 = DoomLoopLadder{Observe: 1, Steer: DoomLoopRungDisabled, Escalate: 2, Block: 5, Stop: 8}

func modelOf(req components.ResponsesRequest) string {
	if req.Model == nil {
		return ""
	}
	return *req.Model
}

func TestDoomLoopEscalationRecovery(t *testing.T) {
	t.Run("swaps the model for exactly one turn, then reverts", func(t *testing.T) {
		var s doomScript
		sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"different now"}), s.textTurn("recovered"))
		_, text := runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model"}}})
		if text != "recovered" {
			t.Fatalf("text = %q", text)
		}
		got := []string{modelOf(sender.requests[0]), modelOf(sender.requests[1]), modelOf(sender.requests[2]), modelOf(sender.requests[3])}
		if strings.Join(got, ",") != "cheap-model,cheap-model,frontier-model,cheap-model" {
			t.Fatalf("models = %v", got)
		}
		notices := 0
		for _, m := range userMessages(requestInput(sender, 2)) {
			if strings.Contains(m, "escalated turn follows") {
				notices++
			}
		}
		if notices != 1 {
			t.Fatalf("a steer notice must accompany the escalated turn, got %d", notices)
		}
	})
	t.Run("injects the advisor tool with pinned toolChoice for the escalated turn only", func(t *testing.T) {
		var s doomScript
		sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"unblocked"}), s.textTurn("done"))
		runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{Advisor: true}}})
		escalated := sender.requests[2]
		advisor := findAdvisor(escalated)
		if advisor == nil || advisor.Parameters == nil || advisor.Parameters.ForwardTranscript == nil || !*advisor.Parameters.ForwardTranscript || !strings.Contains(*advisor.Parameters.Instructions, "escalation advisor") {
			t.Fatalf("advisor tool = %#v", advisor)
		}
		if escalated.ToolChoice == nil || escalated.ToolChoice.ToolChoiceAllowed == nil || escalated.ToolChoice.ToolChoiceAllowed.Mode.ModeRequired == nil || escalated.ToolChoice.ToolChoiceAllowed.Tools[0]["type"] != "openrouter:advisor" {
			t.Fatalf("toolChoice must be pinned to the advisor: %#v", escalated.ToolChoice)
		}
		if modelOf(escalated) != "cheap-model" {
			t.Fatal("advisor-only escalation keeps the model")
		}
		b, err := json.Marshal(escalated)
		if err != nil || !strings.Contains(string(b), `"type":"allowed_tools"`) || !strings.Contains(string(b), `"forward_transcript":true`) {
			t.Fatalf("escalated request must serialize the advisor consult: %v %s", err, b)
		}
		reverted := sender.requests[3]
		if findAdvisor(reverted) != nil || (reverted.ToolChoice != nil && reverted.ToolChoice.ToolChoiceAllowed != nil) {
			t.Fatal("the next dispatch must revert")
		}
		if findAdvisor(sender.requests[1]) != nil {
			t.Fatal("no advisor before the verdict")
		}
	})
	t.Run("advisor parameters merge over the defaults", func(t *testing.T) {
		var s doomScript
		model := "anthropic/claude-opus-4.6"
		forward := false
		sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("done"))
		runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{AdvisorParameters: &components.AdvisorServerToolConfig{Model: &model, ForwardTranscript: &forward}}}})
		advisor := findAdvisor(sender.requests[2])
		if advisor == nil || *advisor.Parameters.Model != model || *advisor.Parameters.ForwardTranscript || !strings.Contains(*advisor.Parameters.Instructions, "escalation advisor") {
			t.Fatalf("advisor params = %#v", advisor)
		}
	})
	t.Run("consumes budget per applied escalation and falls through when exhausted", func(t *testing.T) {
		var s doomScript
		hooks := NewHooksManager()
		detections := collectDetections(hooks)
		var turns []components.OpenResponsesResult
		for i := 0; i < 6; i++ {
			turns = append(turns, s.toolTurn("web_search", searchArgs{"same"}))
		}
		turns = append(turns, s.textTurn("gave up"))
		sender := scripted(turns...)
		accessor := &memoryStateAccessor{state: CreateInitialState()}
		runText(t, sender, CallModelInput{Model: "cheap-model", Input: "go", MaxTurns: 10, Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model", MaxEscalations: 1}}, StateAccessor: accessor, Hooks: hooks})
		var tool [][2]any
		for _, e := range detections.events {
			if e.Detector == DoomLoopDetectorToolFingerprint {
				tool = append(tool, [2]any{e.Action, e.Streak})
			}
		}
		if !pairsEqual(tool, [2]any{DoomLoopActionObserve, 1}, [2]any{DoomLoopActionEscalate, 2}, [2]any{DoomLoopActionObserve, 3}, [2]any{DoomLoopActionObserve, 4}, [2]any{DoomLoopActionBlock, 5}, [2]any{DoomLoopActionBlock, 6}) {
			t.Fatalf("tool ladder = %v", tool)
		}
		frontier := 0
		for _, r := range sender.requests {
			if modelOf(r) == "frontier-model" {
				frontier++
			}
		}
		if frontier != 1 || accessor.state.DoomLoop.EscalationsUsed != 1 {
			t.Fatalf("exactly one escalated dispatch: frontier=%d used=%d", frontier, accessor.state.DoomLoop.EscalationsUsed)
		}
	})
	t.Run("hook override escalate is honored with budget and downgraded without config", func(t *testing.T) {
		escalateHook := func() *HooksManager {
			h := NewHooksManager()
			h.OnDoomLoopDetected(HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]{Handler: func(DoomLoopDetectedPayload, LifecycleHookContext) (HookHandlerResult[DoomLoopDetectedResult], error) {
				return SyncResult(DoomLoopDetectedResult{OverrideAction: DoomLoopActionEscalate}), nil
			}})
			return h
		}
		ladder := DoomLoopLadder{Observe: 2, Block: 5, Stop: 8}
		var s doomScript
		sender := scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("done"))
		runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: ladder, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model"}}, Hooks: escalateHook()})
		if modelOf(sender.requests[2]) != "frontier-model" {
			t.Fatalf("override escalate with config = %s", modelOf(sender.requests[2]))
		}
		sender = scripted(s.toolTurn("web_search", searchArgs{"same"}), s.toolTurn("web_search", searchArgs{"same"}), s.textTurn("done"))
		_, text := runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: ladder}, Hooks: escalateHook()})
		if text != "done" || modelOf(sender.requests[2]) != "cheap-model" {
			t.Fatalf("override escalate without config must downgrade: %s", modelOf(sender.requests[2]))
		}
	})
	t.Run("text-detector verdicts can escalate too", func(t *testing.T) {
		var s doomScript
		filler := "Let me look somewhere else."
		sender := scripted(s.turn(s.msg(filler), s.call("web_search", searchArgs{"q1"})), s.turn(s.msg(filler), s.call("web_search", searchArgs{"q2"})), s.toolTurn("web_search", searchArgs{"q3"}), s.textTurn("unblocked"))
		runText(t, sender, CallModelInput{Model: "cheap-model", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model"}}})
		frontier := 0
		for _, r := range sender.requests {
			if modelOf(r) == "frontier-model" {
				frontier++
			}
		}
		if frontier != 1 {
			t.Fatalf("one escalated dispatch, got %d", frontier)
		}
	})
	t.Run("two detectors firing in one window escalate once", func(t *testing.T) {
		var s doomScript
		same := func() components.OpenResponsesResult {
			return s.turn(s.msg("Trying the same thing."), s.call("web_search", searchArgs{"same"}))
		}
		accessor := &memoryStateAccessor{state: CreateInitialState()}
		runText(t, scripted(same(), same(), s.textTurn("done")), CallModelInput{Model: "cheap-model", Input: "go", Tools: []Tool{doomSearchTool(nil)}, DoomLoop: &DoomLoopConfig{Ladder: escalateAt2, Escalation: &DoomLoopEscalationConfig{Model: "frontier-model", MaxEscalations: 5}}, StateAccessor: accessor})
		if accessor.state.DoomLoop.EscalationsUsed != 1 {
			t.Fatalf("escalationsUsed = %d", accessor.state.DoomLoop.EscalationsUsed)
		}
	})
}

func findAdvisor(req components.ResponsesRequest) *components.AdvisorServerToolOpenRouter {
	for _, tool := range req.Tools {
		if tool.AdvisorServerToolOpenRouter != nil {
			return tool.AdvisorServerToolOpenRouter
		}
	}
	return nil
}

type riskyArgs struct {
	Target string `json:"target"`
}

func riskyTool(executions *int) Tool {
	return MustNewTool(ToolConfig[riskyArgs]{Name: "risky", RequireApproval: true, Execute: func(context.Context, riskyArgs, ToolExecuteContext) (any, error) {
		if executions != nil {
			*executions++
		}
		return map[string]any{"done": true}, nil
	}})
}

// A stop armed during the auto-approve batch of a mixed turn pauses for
// approval first; the condemned run then stays halted on a decision-only
// resume (upstream's condemnation rule), making no further model request.
func TestDoomLoopCondemnedRunStaysHaltedOnDecisionResume(t *testing.T) {
	var s doomScript
	noisy := MustNewTool(ToolConfig[struct{}]{Name: "noisy", Execute: func(context.Context, struct{}, ToolExecuteContext) (any, error) { return "ok", nil }})
	riskyRuns := 0
	tools := []Tool{noisy, riskyTool(&riskyRuns)}
	cfg := func() *DoomLoopConfig {
		return &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 1, Block: DoomLoopRungDisabled, Stop: 2}}
	}
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	first := scripted(s.toolTurn("noisy", map[string]any{}), s.turn(s.call("noisy", map[string]any{}), s.call("risky", riskyArgs{"x"})))
	if _, err := mustResult(t, first, CallModelInput{Model: "test-model", Input: "go", Tools: tools, DoomLoop: cfg(), StateAccessor: accessor}).Response(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accessor.state.Status != ConversationStatusAwaitingApproval || accessor.state.DoomLoop.StopVerdict == nil || accessor.state.DoomLoop.StopVerdict.ToolName != "noisy" {
		t.Fatalf("mixed turn must pause with the stop verdict persisted: %s %#v", accessor.state.Status, accessor.state.DoomLoop)
	}
	resume := scripted(s.textTurn("must not be requested"))
	result := mustResult(t, resume, CallModelInput{Model: "test-model", Tools: tools, DoomLoop: cfg(), StateAccessor: accessor, ApproveToolCalls: []string{accessor.state.PendingToolCalls[0].CallID}})
	v := verdictOf(t, result)
	if v == nil || v.Action != DoomLoopActionStop || v.ToolName != "noisy" || v.Streak != 2 {
		t.Fatalf("restored verdict = %#v", v)
	}
	if resume.calls != 0 {
		t.Fatalf("a condemned run must not request again on a decision-only resume; requests=%d", resume.calls)
	}
	if riskyRuns != 1 {
		t.Fatalf("the approved call itself still executes; runs=%d", riskyRuns)
	}
	if accessor.state.DoomLoop.StopVerdict == nil {
		t.Fatal("the stop verdict must stay persisted")
	}
	assertWellFormedHistory(t, accessor.state.Messages)
}

// Steer guidance queued right before a pause is delivered by the resumed
// run at its next safe flush point (and deduplicated).
func TestDoomLoopPendingSteerDeliveredAfterResume(t *testing.T) {
	type keyArgs struct {
		Key string `json:"key"`
	}
	var s doomScript
	lookup := MustNewTool(ToolConfig[keyArgs]{Name: "lookup", Execute: func(context.Context, keyArgs, ToolExecuteContext) (any, error) { return "v", nil }})
	tools := []Tool{lookup, riskyTool(nil)}
	cfg := func() *DoomLoopConfig {
		return &DoomLoopConfig{Ladder: DoomLoopLadder{Observe: 2, Steer: 2, Block: DoomLoopRungDisabled, Stop: 6}}
	}
	accessor := &memoryStateAccessor{state: CreateInitialState()}
	runText(t, scripted(s.toolTurn("lookup", keyArgs{"same"}), s.turn(s.call("lookup", keyArgs{"same"}), s.call("risky", riskyArgs{"x"}))), CallModelInput{Input: "go", Tools: tools, DoomLoop: cfg(), StateAccessor: accessor})
	if accessor.state.Status != ConversationStatusAwaitingApproval || len(accessor.state.DoomLoop.PendingSteer) != 1 {
		t.Fatalf("steer must be queued at the pause: %s %#v", accessor.state.Status, accessor.state.DoomLoop)
	}
	steer := accessor.state.DoomLoop.PendingSteer[0]
	// A duplicate queued entry must still be delivered once.
	accessor.state.DoomLoop.PendingSteer = []string{steer, steer}

	resume := scripted(s.toolTurn("lookup", keyArgs{"other"}), s.textTurn("done"))
	_, text := runText(t, resume, CallModelInput{Tools: tools, DoomLoop: cfg(), StateAccessor: accessor, ApproveToolCalls: []string{accessor.state.PendingToolCalls[0].CallID}})
	if text != "done" {
		t.Fatalf("text = %q", text)
	}
	count := func(i int) int {
		n := 0
		for _, m := range userMessages(requestInput(resume, i)) {
			if m == steer {
				n++
			}
		}
		return n
	}
	if count(0) != 0 {
		t.Fatal("the continuation request carries no steer (it follows dangling outputs)")
	}
	if count(1) != 1 {
		t.Fatalf("the follow-up after the next tool round must carry the steer exactly once, got %d", count(1))
	}
	if len(accessor.state.DoomLoop.PendingSteer) != 0 {
		t.Fatalf("delivered steer must leave the queue: %#v", accessor.state.DoomLoop.PendingSteer)
	}
}

// On the first response of a call, the approval and client-tools pauses take
// precedence over a doom stop (upstream checks them before the loop's gate).
func TestDoomLoopFirstTurnPauseTakesPrecedenceOverDoomStop(t *testing.T) {
	doomText := strings.TrimSpace(strings.Repeat("I am stuck. ", 20))
	var s doomScript
	hooks := NewHooksManager()
	reasons := sessionEndReasons(hooks)
	result, _ := runText(t, scripted(s.turn(s.msg(doomText), s.call("risky", riskyArgs{"x"}))), CallModelInput{Tools: []Tool{riskyTool(nil)}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	state, _ := result.State(context.Background())
	if state.Status != ConversationStatusAwaitingApproval || len(state.PendingToolCalls) != 1 {
		t.Fatalf("approval pause must win on the first response: %s", state.Status)
	}
	if state.DoomLoop == nil || state.DoomLoop.StopVerdict == nil || state.DoomLoop.StopVerdict.Detector != DoomLoopDetectorTextRepetition {
		t.Fatalf("the armed stop verdict persists for the decision resume: %#v", state.DoomLoop)
	}
	if len(*reasons) != 1 || (*reasons)[0] != SessionEndReasonComplete {
		t.Fatalf("a paused run reports complete, got %v", *reasons)
	}

	manual := MustNewTool(ToolConfig[struct{}]{Name: "client_side", Manual: true})
	result, _ = runText(t, scripted(s.turn(s.msg(doomText), s.call("client_side", map[string]any{}))), CallModelInput{Tools: []Tool{manual}, DoomLoop: &DoomLoopConfig{}})
	state, _ = result.State(context.Background())
	if state.Status != ConversationStatusAwaitingClientTools || len(state.PendingToolCalls) != 1 {
		t.Fatalf("client-tools pause must win on the first response: %s", state.Status)
	}

	// Later turns hit the doom gate first: a stop armed on turn 2 seals
	// instead of pausing for approval.
	executions := 0
	sender := scripted(s.toolTurn("web_search", searchArgs{"a"}), s.turn(s.msg(doomText), s.call("risky", riskyArgs{"x"})))
	result, _ = runText(t, sender, CallModelInput{Tools: []Tool{doomSearchTool(nil), riskyTool(&executions)}, DoomLoop: &DoomLoopConfig{}})
	state, _ = result.State(context.Background())
	if state.Status != ConversationStatusComplete || executions != 0 || verdictOf(t, result) == nil {
		t.Fatalf("turn-2 stop must seal: status=%s executions=%d", state.Status, executions)
	}
	assertWellFormedHistory(t, state.Messages)
}

// Unhashable computed key material falls back to the full-arguments identity
// in the engine; detection keeps working and the call is never failed.
func TestDoomLoopUnhashableLoopKeyFallsBack(t *testing.T) {
	var s doomScript
	executions := 0
	tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", LoopKey: LoopKeyFunc(func(searchArgs) (any, error) { return math.NaN(), nil }), Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
		executions++
		return "r", nil
	}})
	hooks := NewHooksManager()
	detections := collectDetections(hooks)
	logs := captureLog(t, func() {
		runText(t, scripted(s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("done")), CallModelInput{Tools: []Tool{tool}, DoomLoop: &DoomLoopConfig{}, Hooks: hooks})
	})
	if !pairsEqual(detections.pairs(), [2]any{DoomLoopActionObserve, 2}, [2]any{DoomLoopActionBlock, 3}) || executions != 2 {
		t.Fatalf("fallback identity must keep detecting: %v executions=%d", detections.pairs(), executions)
	}
	if !strings.Contains(logs, "falling back to full arguments") {
		t.Fatalf("fallback must warn: %q", logs)
	}
}
