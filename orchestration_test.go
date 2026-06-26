package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/types/stream"
)

// countFunctionCallOutputs counts function_call_output items in a message slice.
func countFunctionCallOutputs(items []components.InputsUnion1) int {
	n := 0
	for _, it := range items {
		if it.FunctionCallOutputItem != nil {
			n++
		}
	}
	return n
}

func hasUnsentForCall(results []UnsentToolResult, callID string) bool {
	for _, r := range results {
		if r.CallID == callID {
			return true
		}
	}
	return false
}

// TestMixedApprovalExecutesAutoToolsBeforePausing covers Run-2 punch-list item
// 14: when a single turn mixes an auto-executable tool call and an
// approval-required tool call, the auto-executable tool must be executed and its
// output recorded BEFORE the loop pauses with awaiting_approval. The recorded
// auto output must then be replayed on resume in both approve and reject cases.
func TestMixedApprovalExecutesAutoToolsBeforePausing(t *testing.T) {
	autoRan := 0
	autoTool := MustNewTool(ToolConfig[sampleInput]{Name: "auto", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		autoRan++
		return map[string]any{"auto": true}, nil
	}})
	approveTool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"danger": "done"}, nil
	}})

	autoCall := components.OutputFunctionCallItem{CallID: "call_auto", Name: "auto", Arguments: `{"query":"a"}`}
	dangerCall := components.OutputFunctionCallItem{CallID: "call_danger", Name: "danger", Arguments: `{"query":"b"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(autoCall),
		components.CreateOutputItemsFunctionCall(dangerCall),
	}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}

	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{autoTool, approveTool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := paused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state.Status)
	}
	if autoRan != 1 {
		t.Fatalf("auto tool should have executed once before pausing, ran %d", autoRan)
	}
	if !hasUnsentForCall(state.UnsentToolResults, "call_auto") {
		t.Fatalf("auto-approved output not saved before pause: %#v", state.UnsentToolResults)
	}
	if len(state.PendingToolCalls) != 1 || state.PendingToolCalls[0].CallID != "call_danger" {
		t.Fatalf("only the approval-required call should be pending, got %#v", state.PendingToolCalls)
	}

	// Resume with approval — both auto output and approved output must be sent.
	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}
	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{Model: "openai/test", Tools: []Tool{autoTool, approveTool}, State: &state, ApproveToolCalls: []string{"call_danger"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := resumeSender.requests[0].Input.ArrayOfInputsUnion1
	if got := countFunctionCallOutputs(items); got < 2 {
		t.Fatalf("resume should replay both auto and approved outputs, got %d outputs in %#v", got, items)
	}
}

// TestMixedApprovalRejectStillSendsAutoOutput covers item 14 for the reject
// path: the auto-approved output is still saved before pausing and sent on a
// pause->reject resume, with the rejected call surfaced as a rejected output.
func TestMixedApprovalRejectStillSendsAutoOutput(t *testing.T) {
	autoTool := MustNewTool(ToolConfig[sampleInput]{Name: "auto", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"auto": true}, nil
	}})
	approveTool := MustNewTool(ToolConfig[sampleInput]{Name: "danger", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "should not run", nil
	}})
	autoCall := components.OutputFunctionCallItem{CallID: "call_auto", Name: "auto", Arguments: `{"query":"a"}`}
	dangerCall := components.OutputFunctionCallItem{CallID: "call_danger", Name: "danger", Arguments: `{"query":"b"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(autoCall),
		components.CreateOutputItemsFunctionCall(dangerCall),
	}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}
	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{autoTool, approveTool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := paused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnsentForCall(state.UnsentToolResults, "call_auto") {
		t.Fatalf("auto output must be saved before pause even when other call is rejected")
	}

	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("ok")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}
	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{Model: "openai/test", Tools: []Tool{autoTool, approveTool}, State: &state, RejectToolCalls: []string{"call_danger"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := resumeSender.requests[0].Input.ArrayOfInputsUnion1
	if got := countFunctionCallOutputs(items); got < 2 {
		t.Fatalf("reject resume should send auto output + rejected output, got %d", got)
	}
}

// TestMixedRegularHITLPreservesRegularOutput covers Run-2 punch-list item 15:
// when a regular tool executes and a later HITL tool pauses the same turn, the
// already-produced regular-tool output must be persisted to state (survive the
// HITL pause) and the function_call ordering must be preserved on resume.
func TestMixedRegularHITLPreservesRegularOutput(t *testing.T) {
	regularRan := 0
	regular := MustNewTool(ToolConfig[sampleInput]{Name: "fetch", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		regularRan++
		return map[string]any{"fetched": 1}, nil
	}})
	hitl := MustNewTool(ToolConfig[sampleInput]{Name: "confirm", OnToolCalled: func(context.Context, sampleInput, ToolExecuteContext) (any, bool, error) {
		return nil, false, nil // pause
	}})
	regularCall := components.OutputFunctionCallItem{CallID: "call_reg", Name: "fetch", Arguments: `{"query":"a"}`}
	hitlCall := components.OutputFunctionCallItem{CallID: "call_hitl", Name: "confirm", Arguments: `{"query":"b"}`}
	resp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{
		components.CreateOutputItemsFunctionCall(regularCall),
		components.CreateOutputItemsFunctionCall(hitlCall),
	}}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{regular, hitl}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := result.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingHITL {
		t.Fatalf("expected awaiting_hitl, got %s", state.Status)
	}
	if regularRan != 1 {
		t.Fatalf("regular tool should have executed once, ran %d", regularRan)
	}
	if got := countFunctionCallOutputs(state.Messages); got != 1 {
		t.Fatalf("regular tool output must survive the HITL pause in state, found %d outputs", got)
	}
	// Ordering: the regular function_call must appear before its output.
	var callIdx, outIdx = -1, -1
	for i, it := range state.Messages {
		if (it.OutputFunctionCallItem != nil && it.OutputFunctionCallItem.CallID == "call_reg") || (it.FunctionCallItem != nil && it.FunctionCallItem.CallID == "call_reg") {
			callIdx = i
		}
		if it.FunctionCallOutputItem != nil && it.FunctionCallOutputItem.CallID == "call_reg" {
			outIdx = i
		}
	}
	if callIdx == -1 || outIdx == -1 || callIdx > outIdx {
		t.Fatalf("function_call must precede function_call_output (call=%d out=%d)", callIdx, outIdx)
	}
	if len(state.PendingToolCalls) != 1 || state.PendingToolCalls[0].CallID != "call_hitl" {
		t.Fatalf("HITL call should be pending, got %#v", state.PendingToolCalls)
	}
}

// errReader returns data once then an error, to drive a failing event stream.
type errReader struct {
	data []byte
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.done && len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		if len(e.data) == 0 {
			e.done = true
		}
		return n, nil
	}
	return 0, errors.New("simulated stream transport failure")
}

func failingEventStream() *stream.EventStream[components.ResponsesStreamingResponse] {
	r := &errReader{data: []byte("data: {")} // partial SSE frame, then error
	return stream.NewEventStream(context.Background(), r, func(b []byte) (components.ResponsesStreamingResponse, error) {
		var v components.ResponsesStreamingResponse
		return v, nil
	}, "[DONE]")
}

// TestSDKStreamErrorPropagatesToConsumers covers Run-2 punch-list item 16b: an
// SDK stream error must surface through ModelResult consumers (text/tool/full)
// rather than hanging or silently ending.
func TestSDKStreamErrorPropagatesToConsumers(t *testing.T) {
	created := operations.CreateCreateResponsesResponseEventStream(failingEventStream())
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	// Text consumer surfaces the error.
	if _, err := result.Text(context.Background()); err == nil {
		t.Fatalf("Text should surface stream error")
	}

	// Full stream consumer terminates with the error rather than hanging.
	full, doneFull := result.FullResponsesStream(context.Background())
	for range full {
	}
	if err := doneFull(); err == nil {
		t.Fatalf("FullResponsesStream consumer should surface stream error")
	}

	// Tool stream consumer terminates with the error.
	ts, doneTool := result.ToolStream(context.Background())
	for range ts {
	}
	if err := doneTool(); err == nil {
		t.Fatalf("ToolStream consumer should surface stream error")
	}
}

// stubHTTPClient returns a fixed non-streaming Responses JSON body, so a real
// SDK call through NewOpenRouter completes without network.
type stubHTTPClient struct {
	body    string
	hits    int
	lastReq *http.Request
}

func (s *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	s.hits++
	s.lastReq = req
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

// TestPublicMiddlewareFiresThroughCallModel covers Run-2 punch-list item 16a:
// user middleware registered on NewOpenRouter must actually fire on the
// Responses request issued by the public CallModel path (not only the internal
// wrapper).
func TestPublicMiddlewareFiresThroughCallModel(t *testing.T) {
	stub := &stubHTTPClient{body: `{"id":"resp_1","model":"openai/test","status":"completed","output":[],"output_text":"hi"}`}
	var beforeFired, afterFired bool
	or := NewOpenRouter(OpenRouterOptions{
		SDK: SDKOptions{APIKey: "test-key", HTTPClient: stub},
		BeforeRequest: []RequestMiddleware{func(ctx context.Context, req *http.Request) error {
			beforeFired = true
			req.Header.Set("x-agent-mw", "1")
			return nil
		}},
		AfterResponse: []ResponseMiddleware{func(ctx context.Context, res *http.Response) error {
			afterFired = true
			return nil
		}},
	})
	result, err := or.CallModel(context.Background(), CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !beforeFired || !afterFired {
		t.Fatalf("public CallModel path did not fire middleware (before=%v after=%v)", beforeFired, afterFired)
	}
	if stub.hits == 0 {
		t.Fatalf("SDK never issued an HTTP request")
	}
	if stub.lastReq == nil || stub.lastReq.Header.Get("x-agent-mw") != "1" {
		t.Fatalf("before middleware did not mutate the actual outgoing request")
	}
}
