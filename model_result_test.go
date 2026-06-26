package agent

import (
	"context"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

type fakeSender struct {
	responses []*operations.CreateResponsesResponse
	requests  []components.ResponsesRequest
	calls     int
}

func (f *fakeSender) SendResponse(ctx context.Context, req components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	f.calls++
	f.requests = append(f.requests, req)
	if f.calls > len(f.responses) {
		return f.responses[len(f.responses)-1], nil
	}
	return f.responses[f.calls-1], nil
}

func TestModelResultConsumesResponseAndReplaysStreams(t *testing.T) {
	resp := components.OpenResponsesResult{ID: "resp_1", Model: "openai/test", Status: components.OpenAIResponsesResponseStatusCompleted, OutputText: openrouter.String("hello")}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if sender.calls != 1 {
		t.Fatalf("expected lazy single send, got %d", sender.calls)
	}

	ch1, done1 := result.TextStream(context.Background())
	ch2, done2 := result.TextStream(context.Background())
	if got := <-ch1; got != "hello" {
		t.Fatalf("first stream got %q", got)
	}
	if got := <-ch2; got != "hello" {
		t.Fatalf("second stream got %q", got)
	}
	for range ch1 {
	}
	for range ch2 {
	}
	if err := done1(); err != nil {
		t.Fatal(err)
	}
	if err := done2(); err != nil {
		t.Fatal(err)
	}

	full, done := result.FullResponsesStream(context.Background())
	var start, end bool
	for ev := range full {
		if ev.Type == "turn.start" {
			start = true
		}
		if ev.Type == "turn.end" {
			end = true
		}
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if !start || !end {
		t.Fatalf("full response stream must include turn delimiters")
	}
}

func TestApprovalResumeReplaysFunctionCallBeforeOutput(t *testing.T) {
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", RequireApproval: true, Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	pauseResp := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	createdPause := operations.CreateCreateResponsesResponseOpenResponsesResult(pauseResp)
	pauseSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdPause}}
	paused, err := CallModel(context.Background(), pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := paused.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ConversationStatusAwaitingApproval || len(state.PendingToolCalls) != 1 {
		t.Fatalf("expected awaiting approval, got %s pending=%d", state.Status, len(state.PendingToolCalls))
	}

	finalResp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFinal := operations.CreateCreateResponsesResponseOpenResponsesResult(finalResp)
	resumeSender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFinal}}
	resumed, err := CallModel(context.Background(), resumeSender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, State: &state, ApproveToolCalls: []string{"call_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := resumeSender.requests[0].Input.ArrayOfInputsUnion1
	if len(items) < 2 || (items[0].FunctionCallItem == nil && items[0].OutputFunctionCallItem == nil) || items[1].FunctionCallOutputItem == nil {
		t.Fatalf("resume input must replay function_call before function_call_output: %#v", items)
	}
	if previousID, ok := resumeSender.requests[0].PreviousResponseID.GetOrZero(); !ok || previousID == "" {
		t.Fatalf("resume request should carry previous_response_id")
	}
}

func TestAllowFinalResponseSendsNoToolsRequestOnStop(t *testing.T) {
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) {
		return "tool output", nil
	}})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("final")}
	createdFirst := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	createdSecond := operations.CreateCreateResponsesResponseOpenResponsesResult(second)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFirst, &createdSecond}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{tool}, StopWhen: []StopCondition{StepCountIs(1)}, AllowFinalResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	text, err := result.Text(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "final" {
		t.Fatalf("text = %q", text)
	}
	if sender.calls != 2 {
		t.Fatalf("expected initial and final request, got %d", sender.calls)
	}
	if len(sender.requests[1].Tools) != 0 {
		t.Fatalf("final response request must remove tools")
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	if len(items) < 3 || items[0].EasyInputMessage == nil || (items[1].FunctionCallItem == nil && items[1].OutputFunctionCallItem == nil) || items[2].FunctionCallOutputItem == nil {
		t.Fatalf("final request should preserve original input, function_call, and output, got %#v", items)
	}
}

func TestFollowUpRequestPreservesAccumulatedInputHistory(t *testing.T) {
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "search", Execute: func(context.Context, sampleInput, ToolExecuteContext) (any, error) { return "tool output", nil }})
	call := components.OutputFunctionCallItem{CallID: "call_1", Name: "search", Arguments: `{"query":"go"}`}
	first := components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(call)}}
	second := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("done")}
	createdFirst := operations.CreateCreateResponsesResponseOpenResponsesResult(first)
	createdSecond := operations.CreateCreateResponsesResponseOpenResponsesResult(second)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&createdFirst, &createdSecond}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := sender.requests[1].Input.ArrayOfInputsUnion1
	if len(items) < 3 || items[0].EasyInputMessage == nil || (items[1].FunctionCallItem == nil && items[1].OutputFunctionCallItem == nil) || items[2].FunctionCallOutputItem == nil {
		t.Fatalf("follow-up request should preserve accumulated input/history, got %#v", items)
	}
}

func TestHITLOnResponseReceivedRewritesFreshToolOutput(t *testing.T) {
	called := false
	tool := MustNewTool(ToolConfig[sampleInput]{Name: "review", OnToolCalled: func(context.Context, sampleInput, ToolExecuteContext) (any, bool, error) {
		return nil, false, nil
	}, OnResponseReceived: func(ctx context.Context, output any, execCtx ToolExecuteContext) (any, error) {
		called = true
		return map[string]any{"accepted": output}, nil
	}})
	state := CreateInitialState()
	fc := components.OutputFunctionCallItem{CallID: "call_1", Name: "review", Arguments: `{"query":"go"}`}
	state = persistResponseToState(t, state, components.OpenResponsesResult{ID: "resp_1", Output: []components.OutputItems{components.CreateOutputItemsFunctionCall(fc)}})
	toolOutput := components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: "call_1", Output: components.CreateFunctionCallOutputItemOutputUnion2Str(`{"human":"yes"}`), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput})
	resp := components.OpenResponsesResult{ID: "resp_2", OutputText: openrouter.String("ok")}
	created := operations.CreateCreateResponsesResponseOpenResponsesResult(resp)
	sender := &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
	result, err := CallModel(context.Background(), sender, CallModelInput{Model: "openai/test", Tools: []Tool{tool}, State: &state, Input: []components.InputsUnion1{toolOutput}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatalf("OnResponseReceived was not invoked")
	}
	items := sender.requests[0].Input.ArrayOfInputsUnion1
	got := items[len(items)-1].FunctionCallOutputItem.Output.Str
	if got == nil || *got == `{"human":"yes"}` {
		t.Fatalf("expected rewritten HITL output, got %v", got)
	}
}

func TestReusableStreamSubscriberReceivesCompletionError(t *testing.T) {
	stream := NewReusableStream[string]()
	ch, done := stream.Subscribe(1)
	stream.Push("one")
	stream.Complete(context.Canceled)
	if got := <-ch; got != "one" {
		t.Fatalf("got %q", got)
	}
	for range ch {
	}
	if err := done(); err != context.Canceled {
		t.Fatalf("done err = %v", err)
	}
}
