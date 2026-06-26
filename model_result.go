package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

type GetResponseOptions struct{ Refresh bool }

type ModelResult struct {
	ctx             context.Context
	client          ResponseSender
	input           CallModelInput
	req             components.ResponsesRequest
	store           *ToolContextStore
	once            sync.Once
	done            chan struct{}
	mu              sync.RWMutex
	resp            components.OpenResponsesResult
	state           ConversationState
	steps           []StepResult
	toolCalls       []ParsedToolCall
	err             error
	textStream      *ReusableStream[string]
	reasoningStream *ReusableStream[string]
	toolStream      *ReusableStream[ToolStreamEvent]
	toolCallStream  *ReusableStream[ParsedToolCall]
	fullStream      *ReusableStream[ResponseStreamEvent]
	newMessages     *ReusableStream[components.InputsUnion1]
	cancel          context.CancelFunc
}

func newModelResult(ctx context.Context, client ResponseSender, input CallModelInput, req components.ResponsesRequest, state ConversationState) *ModelResult {
	ctx2, cancel := context.WithCancel(ctx)
	return &ModelResult{ctx: ctx2, cancel: cancel, client: client, input: input, req: req, state: state, store: NewToolContextStore(input.Context), done: make(chan struct{}), textStream: NewReusableStream[string](), reasoningStream: NewReusableStream[string](), toolStream: NewReusableStream[ToolStreamEvent](), toolCallStream: NewReusableStream[ParsedToolCall](), fullStream: NewReusableStream[ResponseStreamEvent](), newMessages: NewReusableStream[components.InputsUnion1]()}
}

func NewModelResultFromResponse(resp components.OpenResponsesResult) *ModelResult {
	mr := newModelResult(context.Background(), nil, CallModelInput{}, components.ResponsesRequest{}, CreateInitialState())
	mr.resp = resp
	mr.toolCalls = ExtractToolCallsFromResponse(resp)
	close(mr.done)
	text := ExtractTextFromResponse(resp)
	if text != "" {
		mr.textStream.Push(text)
	}
	mr.textStream.Complete(nil)
	mr.reasoningStream.Complete(nil)
	mr.toolStream.Complete(nil)
	mr.toolCallStream.Complete(nil)
	mr.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: 0})
	mr.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Response: &resp})
	mr.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: 0})
	mr.fullStream.Complete(nil)
	mr.newMessages.Complete(nil)
	return mr
}

func CallModel(ctx context.Context, client ResponseSender, input CallModelInput) (*ModelResult, error) {
	if client == nil {
		return nil, errors.New("client is required")
	}
	resolved, err := ResolveAsyncFunctions(ctx, input, TurnContext{})
	if err != nil {
		return nil, err
	}
	input = resolved
	state := CreateInitialState()
	if input.State != nil {
		state = *input.State
	} else if input.StateAccessor != nil {
		loaded, err := input.StateAccessor.Load(ctx)
		if err != nil {
			return nil, err
		}
		if loaded != nil {
			state = *loaded
		}
	}
	req := input.Request
	if input.Model != "" {
		req.Model = openrouter.Pointer(input.Model)
	}
	if input.Input != nil {
		fresh, err := inputItemsOnly(input.Input)
		if err != nil {
			return nil, err
		}
		fresh, err = applyOnResponseReceivedHooks(ctx, input.Tools, state.Messages, fresh, NewToolContextStore(input.Context))
		if err != nil {
			return nil, err
		}
		items := append([]components.InputsUnion1{}, state.Messages...)
		items = append(items, fresh...)
		req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(items))
	} else if len(state.Messages) > 0 {
		req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(state.Messages))
	}
	if state.PreviousResponseID != nil {
		req.PreviousResponseID = optionalnullable.From(state.PreviousResponseID)
	}
	if input.AdditionalInstructions != "" {
		req.Instructions = optionalnullable.From(openrouter.String(input.AdditionalInstructions))
	}
	req.Stream = openrouter.Bool(true)
	// Explicitly pin service_tier. The generated SDK (v0.5.x) applies the
	// `default:"auto"` struct tag by emitting the raw token `auto` (unquoted)
	// for the OptionalNullable-wrapped field, producing invalid JSON during
	// request serialization when the field is left zero. Setting it here works
	// around that SDK marshalling bug so real Responses requests serialize.
	if _, ok := req.ServiceTier.GetOrZero(); !ok {
		tier := components.ResponsesRequestServiceTierAuto
		req.ServiceTier = optionalnullable.From(&tier)
	}
	for _, t := range input.Tools {
		req.Tools = append(req.Tools, t.ToAPITool())
	}
	return newModelResult(ctx, client, input, req, state), nil
}

func inputItemsOnly(input any) ([]components.InputsUnion1, error) {
	switch v := input.(type) {
	case string:
		msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(v)))}
		return []components.InputsUnion1{components.CreateInputsUnion1EasyInputMessage(msg)}, nil
	case components.InputsUnion:
		if v.Str != nil {
			msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(*v.Str)))}
			return []components.InputsUnion1{components.CreateInputsUnion1EasyInputMessage(msg)}, nil
		}
		return append([]components.InputsUnion1{}, v.ArrayOfInputsUnion1...), nil
	case []components.InputsUnion1:
		return append([]components.InputsUnion1{}, v...), nil
	}
	return nil, fmt.Errorf("unsupported input type %T", input)
}

func (m *ModelResult) ensure() {
	m.start()
	<-m.done
}

func (m *ModelResult) start() { m.once.Do(func() { go m.run() }) }

func (m *ModelResult) run() {
	defer close(m.done)
	// Complete every stream with the *final* error (read at defer-execution
	// time via a closure, not captured at registration). This is what lets an
	// SDK stream/transport error propagate to every ModelResult consumer
	// (text/tool/full) instead of completing them with a stale nil error.
	defer func() {
		m.textStream.Complete(m.err)
		m.reasoningStream.Complete(m.err)
		m.toolStream.Complete(m.err)
		m.toolCallStream.Complete(m.err)
		m.fullStream.Complete(m.err)
		m.newMessages.Complete(m.err)
	}()
	if m.client == nil {
		return
	}
	req := m.req
	maxTurns := m.input.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 5
	}
	steps := []StepResult{}
	if isAwaitingResume(m.state) && (len(m.input.ApproveToolCalls) > 0 || len(m.input.RejectToolCalls) > 0) {
		paused, err := m.prepareResumeRequest(&req)
		if err != nil {
			m.err = err
			return
		}
		if paused {
			return
		}
	}
	for turn := 0; turn < maxTurns; turn++ {
		if m.input.BeforeTurn != nil {
			if err := m.input.BeforeTurn(m.ctx, BuildTurnContext(nil, turn, &req)); err != nil {
				m.err = err
				return
			}
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: turn})
		res, err := m.client.SendResponse(m.ctx, req, m.input.MetadataLevel, operations.WithSetHeaders(map[string]string{"x-openrouter-callmodel": "true"}))
		if err != nil {
			m.err = err
			return
		}
		resp, events, err := consumeCreateResponse(res, func(ev components.StreamEvents) {
			if ev.TextDeltaEvent != nil {
				m.textStream.Push(ev.TextDeltaEvent.Delta)
			}
			if ev.ReasoningDeltaEvent != nil {
				m.reasoningStream.Push(ev.ReasoningDeltaEvent.Delta)
			}
			if ev.ReasoningSummaryTextDeltaEvent != nil {
				m.reasoningStream.Push(ev.ReasoningSummaryTextDeltaEvent.Delta)
			}
			m.fullStream.Push(ResponseStreamEvent{Type: "response.event", Turn: turn, Event: &ev})
		})
		if err != nil {
			m.err = err
			return
		}
		text := ExtractTextFromResponse(resp)
		if len(events) == 0 && text != "" {
			m.textStream.Push(text)
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Turn: turn, Response: &resp})
		m.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: turn})
		calls := ExtractToolCallsFromResponse(resp)
		for _, call := range calls {
			m.toolCallStream.Push(call)
		}
		usage := usagePtr(resp)
		step := StepResult{Text: text, ToolCalls: calls, Response: resp, Usage: usage, FinishReason: string(resp.Status)}
		if m.input.AfterTurn != nil {
			if err := m.input.AfterTurn(m.ctx, BuildTurnContext(nil, turn, &req), step); err != nil {
				m.err = err
				return
			}
		}
		m.mu.Lock()
		m.resp = resp
		m.toolCalls = append(m.toolCalls, calls...)
		m.steps = append(m.steps, step)
		m.mu.Unlock()
		steps = append(steps, step)
		responseItems, err := responseInputItemsWithError(resp)
		if err != nil {
			m.err = err
			return
		}
		m.state = appendResponseItemsToState(m.state, resp, responseItems)
		for _, item := range responseItems {
			m.newMessages.Push(item)
		}
		if len(calls) == 0 {
			m.state.Status = ConversationStatusComplete
			break
		}
		if stop, err := IsStopConditionMet(m.ctx, m.input.StopWhen, steps); err != nil {
			m.err = err
			return
		} else if stop {
			if !allowFinalResponseEnabled(m.input.AllowFinalResponse) {
				m.state.Status = ConversationStatusComplete
				break
			}
			approved, pending, err := PartitionToolCalls(m.ctx, m.input.Tools, calls, BuildTurnContext(nil, turn, &req), m.input.Approval)
			if err != nil {
				m.err = err
				return
			}
			if len(pending) > 0 {
				unsent, err := m.executeAutoApproveTools(approved, turn, &req)
				if err != nil {
					m.err = err
					return
				}
				m.state.UnsentToolResults = append(m.state.UnsentToolResults, unsent...)
				m.state.PendingToolCalls = pending
				m.state.Status = ConversationStatusAwaitingApproval
				break
			}
			outputs, err := m.executeToolCallsForTurn(approved, turn, &req)
			if err != nil {
				m.err = err
				return
			}
			if m.state.Status == ConversationStatusInterrupted || m.state.Status == ConversationStatusAwaitingHITL {
				break
			}
			finalResp, err := m.sendFinalResponseRequest(req, resp, outputs, turn+1)
			if err != nil {
				m.err = err
				return
			}
			m.mu.Lock()
			m.resp = finalResp
			m.steps = append(m.steps, StepResult{Text: ExtractTextFromResponse(finalResp), Response: finalResp, Usage: usagePtr(finalResp), FinishReason: string(finalResp.Status)})
			m.mu.Unlock()
			finalItems, err := responseInputItemsWithError(finalResp)
			if err != nil {
				m.err = err
				return
			}
			m.state = appendResponseItemsToState(m.state, finalResp, finalItems)
			m.state.Status = ConversationStatusComplete
			break
		}
		approved, pending, err := PartitionToolCalls(m.ctx, m.input.Tools, calls, BuildTurnContext(nil, turn, &req), m.input.Approval)
		if err != nil {
			m.err = err
			return
		}
		if len(pending) > 0 {
			// Mixed approval turn (Run-2 punch-list item 14): execute the
			// auto-approved calls and persist their outputs as unsent results
			// *before* pausing for the approval-required calls, matching
			// upstream handleApprovalCheck -> executeAutoApproveTools ordering.
			unsent, err := m.executeAutoApproveTools(approved, turn, &req)
			if err != nil {
				m.err = err
				return
			}
			m.state.UnsentToolResults = append(m.state.UnsentToolResults, unsent...)
			m.state.PendingToolCalls = pending
			m.state.Status = ConversationStatusAwaitingApproval
			break
		}
		outputs, err := m.executeToolCallsForTurn(approved, turn, &req)
		if err != nil {
			m.err = err
			return
		}
		if m.state.Status == ConversationStatusInterrupted || m.state.Status == ConversationStatusAwaitingHITL {
			break
		}
		base := requestInputItems(req.Input)
		base = append(base, responseItems...)
		base = append(base, outputs...)
		req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(base))
		if resp.ID != "" {
			req.PreviousResponseID = optionalnullable.From(openrouter.String(resp.ID))
			m.state.PreviousResponseID = &resp.ID
		}
	}
	if m.input.StateAccessor != nil {
		if err := m.input.StateAccessor.Save(m.ctx, m.state); err != nil {
			m.err = err
		}
	}
}

func consumeCreateResponse(res *operations.CreateResponsesResponse, onEvent func(components.StreamEvents)) (components.OpenResponsesResult, []components.StreamEvents, error) {
	if res == nil {
		return components.OpenResponsesResult{}, nil, errors.New("nil response")
	}
	if res.OpenResponsesResult != nil {
		return *res.OpenResponsesResult, nil, nil
	}
	if res.EventStream == nil {
		return components.OpenResponsesResult{}, nil, errors.New("empty response")
	}
	var events []components.StreamEvents
	var completed *components.OpenResponsesResult
	stream := res.EventStream
	defer stream.Close()
	for stream.Next() {
		val := stream.Value()
		if val == nil {
			continue
		}
		data := val.GetData()
		events = append(events, data)
		if onEvent != nil {
			onEvent(data)
		}
		if data.StreamEventsResponseCompleted != nil {
			resp := data.StreamEventsResponseCompleted.Response
			completed = &resp
		}
	}
	if err := stream.Err(); err != nil {
		return components.OpenResponsesResult{}, events, err
	}
	if completed == nil {
		return components.OpenResponsesResult{}, events, errors.New("stream ended without response.completed")
	}
	return *completed, events, nil
}

func isAwaitingResume(state ConversationState) bool {
	return state.Status == ConversationStatusAwaitingApproval || state.Status == ConversationStatusAwaitingHITL
}

func allowFinalResponseEnabled(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return true
	default:
		return false
	}
}

func callKey(call ParsedToolCall) string {
	if call.ID != "" {
		return call.ID
	}
	return call.CallID
}

func hasDecision(ids []string, call ParsedToolCall) bool {
	key := callKey(call)
	for _, id := range ids {
		if id == key || id == call.CallID || id == call.ID {
			return true
		}
	}
	return false
}

func (m *ModelResult) prepareResumeRequest(req *components.ResponsesRequest) (bool, error) {
	unsent := append([]UnsentToolResult{}, m.state.UnsentToolResults...)
	remaining := []ParsedToolCall{}
	turn := len(m.steps)
	for _, call := range m.state.PendingToolCalls {
		switch {
		case hasDecision(m.input.RejectToolCalls, call):
			unsent = append(unsent, CreateRejectedResult(call, "Rejected by user"))
		case hasDecision(m.input.ApproveToolCalls, call):
			t := FindToolByName(m.input.Tools, call.Name)
			if t == nil || !IsAutoResolvableTool(t) {
				unsent = append(unsent, CreateRejectedResult(call, "Tool not found or not executable"))
				continue
			}
			result, err := ExecuteTool(m.ctx, t, call, BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
				m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Event: v, Turn: turn})
			}))
			if err != nil {
				return false, err
			}
			if errors.Is(result.Error, ErrHITLPause) {
				remaining = append(remaining, call)
				continue
			}
			if result.Error != nil {
				unsent = append(unsent, CreateRejectedResult(call, result.Error.Error()))
			} else {
				unsent = append(unsent, CreateUnsentResult(call, result.Output))
			}
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Result: result.Output, Turn: turn})
		default:
			remaining = append(remaining, call)
		}
	}
	if len(remaining) > 0 {
		m.state.PendingToolCalls = remaining
		if m.state.Status != ConversationStatusAwaitingHITL {
			m.state.Status = ConversationStatusAwaitingApproval
		}
		return true, nil
	}
	outputs, err := UnsentResultsToAPIFormatWithError(unsent)
	if err != nil {
		return false, err
	}
	if len(outputs) == 0 {
		m.state.Status = ConversationStatusComplete
		return true, nil
	}
	items := append([]components.InputsUnion1{}, m.state.Messages...)
	items = append(items, outputs...)
	m.state.Messages = items
	m.state.PendingToolCalls = nil
	m.state.UnsentToolResults = nil
	m.state.Status = ConversationStatusInProgress
	req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(items))
	if m.state.PreviousResponseID != nil {
		req.PreviousResponseID = optionalnullable.From(m.state.PreviousResponseID)
	}
	return false, nil
}

// executeAutoApproveTools runs the auto-executable (non-approval-required) tool
// calls in a turn that also contains approval-required calls, returning their
// results as UnsentToolResult entries (success -> unsent, error -> rejected).
//
// Mirrors upstream `model-result.ts` `executeAutoApproveTools`: it is invoked by
// `handleApprovalCheck` so that, when a turn mixes auto-approved and
// approval-required tool calls, the auto-approved outputs are produced and
// recorded before the loop pauses with `awaiting_approval` (Run-2 punch-list
// item 14). The returned results are stored on `state.UnsentToolResults` and are
// flushed to the model on resume alongside the approve/reject decisions.
//
// A HITL tool that pauses (returns ErrHITLPause) yields no unsent result for
// that call this round, matching upstream's null return.
func (m *ModelResult) executeAutoApproveTools(calls []ParsedToolCall, turn int, req *components.ResponsesRequest) ([]UnsentToolResult, error) {
	var results []UnsentToolResult
	for _, call := range calls {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			// Not executable without approval/manual output — skip (upstream
			// returns null and contributes no unsent result this round).
			continue
		}
		execCtx := BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Event: v, Turn: turn})
		})
		result, err := ExecuteTool(m.ctx, t, call, execCtx)
		if err != nil {
			return nil, err
		}
		if errors.Is(result.Error, ErrHITLPause) {
			// HITL tool paused during auto-approve — no unsent result this round.
			continue
		}
		if result.Error != nil {
			results = append(results, CreateRejectedResult(call, result.Error.Error()))
			continue
		}
		event := ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Result: result.Output, Turn: turn}
		m.toolStream.Push(event)
		results = append(results, CreateUnsentResult(call, result.Output))
	}
	return results, nil
}

// executeToolCallsForTurn runs a round of auto-resolvable tool calls in order.
//
// Mirrors upstream `model-result.ts` `executeToolRound` + `saveToolResultsToState`
// ordering: each produced `function_call_output` is persisted to
// `m.state.Messages` *as it is produced*, so that if a later HITL tool in the
// same round pauses, the already-executed regular-tool outputs survive the
// pause and are replayed on resume (Run-2 punch-list item 15). When a HITL tool
// returns ErrHITLPause, the remaining (un-run) calls are left for resume and the
// loop stops without sending an incomplete output set to the model.
func (m *ModelResult) executeToolCallsForTurn(calls []ParsedToolCall, turn int, req *components.ResponsesRequest) ([]components.InputsUnion1, error) {
	var outputs []components.InputsUnion1
	for _, call := range calls {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || IsManualTool(t) {
			m.state.PendingToolCalls = append(m.state.PendingToolCalls, call)
			m.state.Status = ConversationStatusInterrupted
			continue
		}
		execCtx := BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Event: v, Turn: turn})
		})
		result, err := ExecuteTool(m.ctx, t, call, execCtx)
		if err != nil {
			return nil, err
		}
		if errors.Is(result.Error, ErrHITLPause) {
			m.state.PendingToolCalls = append(m.state.PendingToolCalls, call)
			m.state.Status = ConversationStatusAwaitingHITL
			break
		}
		event := ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Result: result.Output, Turn: turn}
		if result.Error != nil {
			event.Error = result.Error.Error()
		}
		m.toolStream.Push(event)
		out, err := FormatToolOutputWithError(result)
		if err != nil {
			return nil, err
		}
		m.toolStream.Push(ToolStreamEvent{Type: "tool.call.output", CallID: call.CallID, Name: call.Name, Output: result.Output, Error: event.Error, Turn: turn})
		outputItem := components.CreateInputsUnion1FunctionCallOutputItem(out)
		outputs = append(outputs, outputItem)
		// Persist this output to state immediately so it survives a later HITL
		// pause in the same round (upstream saveToolResultsToState per output).
		m.state = AppendToMessages(m.state, outputItem)
	}
	return outputs, nil
}

func (m *ModelResult) sendFinalResponseRequest(req components.ResponsesRequest, resp components.OpenResponsesResult, outputs []components.InputsUnion1, turn int) (components.OpenResponsesResult, error) {
	finalReq := req
	finalReq.Tools = nil
	base := requestInputItems(req.Input)
	items, err := responseInputItemsWithError(resp)
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	base = append(base, items...)
	base = append(base, outputs...)
	if s, ok := m.input.AllowFinalResponse.(string); ok && s != "" {
		msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(s)))}
		base = append(base, components.CreateInputsUnion1EasyInputMessage(msg))
	}
	finalReq.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(base))
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: turn})
	res, err := m.client.SendResponse(m.ctx, finalReq, m.input.MetadataLevel, operations.WithSetHeaders(map[string]string{"x-openrouter-callmodel": "true"}))
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	finalResp, events, err := consumeCreateResponse(res, func(ev components.StreamEvents) {
		if ev.TextDeltaEvent != nil {
			m.textStream.Push(ev.TextDeltaEvent.Delta)
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "response.event", Turn: turn, Event: &ev})
	})
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	text := ExtractTextFromResponse(finalResp)
	if len(events) == 0 && text != "" {
		m.textStream.Push(text)
	}
	m.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Turn: turn, Response: &finalResp})
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: turn})
	return finalResp, nil
}

func usagePtr(resp components.OpenResponsesResult) *components.Usage {
	if usage, ok := resp.Usage.GetOrZero(); ok {
		return &usage
	}
	return nil
}
func (m *ModelResult) Text(ctx context.Context) (string, error) {
	m.ensure()
	return ExtractTextFromResponse(m.resp), m.err
}
func (m *ModelResult) Response(ctx context.Context, opts ...GetResponseOptions) (components.OpenResponsesResult, error) {
	m.ensure()
	return m.resp, m.err
}
func (m *ModelResult) TextStream(ctx context.Context) (<-chan string, func() error) {
	m.start()
	return m.textStream.Subscribe(32)
}
func (m *ModelResult) ReasoningStream(ctx context.Context) (<-chan string, func() error) {
	m.start()
	return m.reasoningStream.Subscribe(32)
}
func (m *ModelResult) ToolStream(ctx context.Context) (<-chan ToolStreamEvent, func() error) {
	m.start()
	return m.toolStream.Subscribe(32)
}
func (m *ModelResult) ToolCallsStream(ctx context.Context) (<-chan ParsedToolCall, func() error) {
	m.start()
	return m.toolCallStream.Subscribe(32)
}
func (m *ModelResult) ToolCalls(ctx context.Context) ([]ParsedToolCall, error) {
	m.ensure()
	return append([]ParsedToolCall{}, m.toolCalls...), m.err
}
func (m *ModelResult) FullResponsesStream(ctx context.Context) (<-chan ResponseStreamEvent, func() error) {
	m.start()
	return m.fullStream.Subscribe(32)
}
func (m *ModelResult) NewMessagesStream(ctx context.Context) (<-chan components.InputsUnion1, func() error) {
	m.start()
	return m.newMessages.Subscribe(32)
}
func (m *ModelResult) ItemsStream(ctx context.Context) (<-chan components.OutputItems, func() error) {
	m.ensure()
	ch := make(chan components.OutputItems, len(m.resp.Output))
	for _, item := range m.resp.Output {
		ch <- item
	}
	close(ch)
	return ch, func() error { return m.err }
}
func (m *ModelResult) ContextUpdates(ctx context.Context) (<-chan map[string]map[string]any, func() error) {
	ch := make(chan map[string]map[string]any, 1)
	if m.store != nil {
		ch <- m.store.Snapshot()
	}
	close(ch)
	return ch, func() error { return nil }
}
func (m *ModelResult) RequiresApproval(ctx context.Context) (bool, error) {
	m.ensure()
	return m.state.Status == ConversationStatusAwaitingApproval, m.err
}
func (m *ModelResult) PendingToolCalls(ctx context.Context) ([]ParsedToolCall, error) {
	m.ensure()
	return append([]ParsedToolCall{}, m.state.PendingToolCalls...), m.err
}
func (m *ModelResult) State(ctx context.Context) (ConversationState, error) {
	m.ensure()
	return m.state, m.err
}
func (m *ModelResult) Cancel() {
	if m.cancel != nil {
		m.cancel()
	}
}

func FindToolByName(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.ToolName() == name {
			return t
		}
	}
	return nil
}

func responseInputItemsWithError(resp components.OpenResponsesResult) ([]components.InputsUnion1, error) {
	items := make([]components.InputsUnion1, 0, len(resp.Output))
	for _, item := range resp.Output {
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("marshal response output item: %w", err)
		}
		var input components.InputsUnion1
		if err := json.Unmarshal(b, &input); err != nil {
			return nil, fmt.Errorf("convert response output item to input item: %w", err)
		}
		items = append(items, input)
	}
	return items, nil
}

func appendResponseItemsToState(state ConversationState, resp components.OpenResponsesResult, items []components.InputsUnion1) ConversationState {
	state = AppendToMessages(state, items...)
	if resp.ID != "" {
		state.PreviousResponseID = &resp.ID
	}
	return state
}

func requestInputItems(input *components.InputsUnion) []components.InputsUnion1 {
	if input == nil {
		return nil
	}
	if input.ArrayOfInputsUnion1 != nil {
		return append([]components.InputsUnion1{}, input.ArrayOfInputsUnion1...)
	}
	if input.Str != nil {
		msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(*input.Str)))}
		return []components.InputsUnion1{components.CreateInputsUnion1EasyInputMessage(msg)}
	}
	return nil
}

func (m *ModelResult) String() string {
	m.ensure()
	return fmt.Sprintf("ModelResult{text=%q, calls=%d}", ExtractTextFromResponse(m.resp), len(m.toolCalls))
}

func applyOnResponseReceivedHooks(ctx context.Context, tools []Tool, history []components.InputsUnion1, fresh []components.InputsUnion1, store *ToolContextStore) ([]components.InputsUnion1, error) {
	if len(fresh) == 0 || len(tools) == 0 {
		return fresh, nil
	}
	callNames := map[string]string{}
	for _, item := range history {
		if item.OutputFunctionCallItem != nil {
			callNames[item.OutputFunctionCallItem.CallID] = item.OutputFunctionCallItem.Name
		}
		if item.FunctionCallItem != nil {
			callNames[item.FunctionCallItem.CallID] = item.FunctionCallItem.Name
		}
	}
	out := append([]components.InputsUnion1{}, fresh...)
	for i, item := range out {
		if item.FunctionCallOutputItem == nil {
			continue
		}
		outputItem := *item.FunctionCallOutputItem
		toolName := callNames[outputItem.CallID]
		if toolName == "" {
			continue
		}
		tool := FindToolByName(tools, toolName)
		if tool == nil || !IsHITLTool(tool) {
			continue
		}
		value := functionOutputValue(outputItem.Output)
		updated, err := tool.HandleResponseReceived(ctx, value, ToolExecuteContext{ToolCall: ParsedToolCall{CallID: outputItem.CallID, Name: toolName}, Store: store})
		if err != nil {
			return nil, fmt.Errorf("handle response for HITL tool %s: %w", toolName, err)
		}
		encoded, err := encodeFunctionOutput(updated)
		if err != nil {
			return nil, err
		}
		outputItem.Output = encoded
		out[i] = components.CreateInputsUnion1FunctionCallOutputItem(outputItem)
	}
	return out, nil
}

func functionOutputValue(output components.FunctionCallOutputItemOutputUnion2) any {
	if output.Str != nil {
		var decoded any
		if err := json.Unmarshal([]byte(*output.Str), &decoded); err == nil {
			return decoded
		}
		return *output.Str
	}
	if output.ArrayOfFunctionCallOutputItemOutputUnion1 != nil {
		return output.ArrayOfFunctionCallOutputItemOutputUnion1
	}
	return nil
}

func encodeFunctionOutput(output any) (components.FunctionCallOutputItemOutputUnion2, error) {
	if s, ok := output.(string); ok {
		return components.CreateFunctionCallOutputItemOutputUnion2Str(s), nil
	}
	b, err := json.Marshal(output)
	if err != nil {
		return components.FunctionCallOutputItemOutputUnion2{}, fmt.Errorf("marshal function call output: %w", err)
	}
	return components.CreateFunctionCallOutputItemOutputUnion2Str(string(b)), nil
}
