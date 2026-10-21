package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

type GetResponseOptions struct{ Refresh bool }

// DefaultFinalResponseDirective is appended as a final user message on the
// forced final turn (AllowFinalResponse defaulting to on, or explicitly
// true). Forbidding tools via ToolChoice=none alone is not enough: models
// that emit tool-call syntax as text (e.g. GLM) will attempt another call
// and leak it into content as unparsed text unless they are told this is the
// final turn. Pass a non-empty string to AllowFinalResponse to override the
// wording, or "" to append no message at all (legacy behavior).
const DefaultFinalResponseDirective = "You have reached the tool-use limit, and tools are no longer available. Do not attempt to call any more tools. Using the information you already have, write your final answer now."

// sessionUsageAggregate accumulates ModelCallUsage across every model call in a run.
type sessionUsageAggregate struct {
	modelCalls      int
	inputTokens     int64
	outputTokens    int64
	totalTokens     int64
	cachedTokens    int64
	reasoningTokens int64
	cost            float64
	hasCost         bool
}

type ModelResult struct {
	ctx                 context.Context
	client              ResponseSender
	input               CallModelInput
	req                 components.ResponsesRequest
	store               *ToolContextStore
	once                sync.Once
	done                chan struct{}
	mu                  sync.RWMutex
	resp                components.OpenResponsesResult
	state               ConversationState
	steps               []StepResult
	toolCalls           []ParsedToolCall
	err                 error
	textStream          *ReusableStream[string]
	reasoningStream     *ReusableStream[string]
	toolStream          *ToolEventBroadcaster
	toolCallStream      *ReusableStream[ParsedToolCall]
	fullStream          *ReusableStream[ResponseStreamEvent]
	newMessages         *ReusableStream[components.InputsUnion1]
	cancel              context.CancelFunc
	hooksManager        *HooksManager
	sessionStartEmitted bool
	sessionEndEmitted   bool
	sessionUsage        sessionUsageAggregate
	toolRoundsExecuted  int
}

func newModelResult(ctx context.Context, client ResponseSender, input CallModelInput, req components.ResponsesRequest, state ConversationState) *ModelResult {
	ctx2, cancel := context.WithCancel(ctx)
	return &ModelResult{ctx: ctx2, cancel: cancel, client: client, input: input, req: req, state: state, store: NewToolContextStore(input.Context), done: make(chan struct{}), textStream: NewReusableStream[string](), reasoningStream: NewReusableStream[string](), toolStream: NewToolEventBroadcaster(), toolCallStream: NewReusableStream[ParsedToolCall](), fullStream: NewReusableStream[ResponseStreamEvent](), newMessages: NewReusableStream[components.InputsUnion1](), hooksManager: ResolveHooks(input.Hooks)}
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
	mr.toolStream.Complete()
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
	// PreviousResponseID is tracked in state for observability but never sent
	// on the wire: this port always sends full message history (mirroring
	// upstream, which stores previousResponseId in state only), and the live
	// Responses API rejects previous_response_id on stateless requests
	// ("expected null, received string").
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
		// Error(nil) is the broadcaster's no-error completion, so this is
		// equivalent to the sibling Complete(m.err) calls for both outcomes.
		m.toolStream.Error(m.err)
		m.toolCallStream.Complete(m.err)
		m.fullStream.Complete(m.err)
		m.newMessages.Complete(m.err)
	}()
	// Session teardown must run on every exit path — success, early return,
	// or error — so fire-and-forget hook work from a run that fails before
	// any tool ever executes (a "no-tools" path) is still drained. Never
	// masks m.err: finishHooksSession only logs its own failures.
	defer func() {
		reason := SessionEndReasonComplete
		if m.err != nil {
			reason = SessionEndReasonError
		}
		m.finishHooksSession(reason)
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
	isResume := false
	forceResumeCount := 0
	// An approval/HITL resume-with-decisions bypasses SessionStart/
	// UserPromptSubmit entirely (upstream model-result.ts:2506-2515: "This
	// path bypasses the SessionStart block below but still fires tool hooks
	// [PreToolUse/PostToolUse] during the resume"). It is a continuation of
	// the same logical session, not a fresh one — SessionEnd's own
	// !sessionStartEmitted guard then makes it correctly a no-op too, while
	// PostModelCall/PreToolUse/PostToolUse still fire during the resume via
	// their own call sites below, and finishHooksSession's unconditional
	// defer still drains any pending async hook work.
	if isAwaitingResume(m.state) && (len(m.input.ApproveToolCalls) > 0 || len(m.input.RejectToolCalls) > 0) {
		if m.hooksManager != nil {
			m.hooksManager.SetSessionID(m.state.ID)
		}
		paused, err := m.prepareResumeRequest(&req)
		if err != nil {
			m.err = err
			return
		}
		if paused {
			// The resume itself left calls pending (not every approved/
			// rejected decision resolved the round) — persist the updated
			// state before returning, mirroring upstream's
			// processApprovalDecisions, which calls saveStateSafely
			// unconditionally regardless of whether the result still has
			// calls pending. Without this, a StateAccessor-backed caller
			// that reloads on the next CallModel call would see the stale
			// pre-resume state and could re-execute an already-decided call.
			if m.input.StateAccessor != nil {
				if err := m.input.StateAccessor.Save(m.ctx, m.state); err != nil {
					m.err = err
				}
			}
			return
		}
		isResume = true
	} else {
		m.emitSessionStartOnce()
		if err := m.maybeRunUserPromptSubmit(&req); err != nil {
			m.err = err
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
		turnStartedAt := time.Now()
		res, err := m.client.SendResponse(m.ctx, req, m.input.MetadataLevel, operations.WithSetHeaders(map[string]string{"x-openrouter-callmodel": "true"}))
		if err != nil {
			m.err = err
			return
		}
		resp, events, err := consumeCreateResponse(res, func(ev components.StreamEvents) {
			if delta, ok := textDeltaText(ev); ok {
				m.textStream.Push(delta)
			}
			if delta, ok := reasoningDeltaText(ev); ok {
				m.reasoningStream.Push(delta)
			}
			m.fullStream.Push(ResponseStreamEvent{Type: "response.event", Turn: turn, Event: &ev})
		})
		if err != nil {
			m.err = err
			return
		}
		turnType := ModelCallTurnTypeToolRound
		if turn == 0 {
			if isResume {
				turnType = ModelCallTurnTypeResume
			} else {
				turnType = ModelCallTurnTypeInitial
			}
		}
		m.emitPostModelCall(resp, turnStartedAt, turnType, turn+1)
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
			resume, err := m.runStopHook(&req, forceResumeCount)
			if err != nil {
				m.err = err
				return
			}
			if resume {
				// A Stop-hook handler forced resumption (optionally after
				// injecting an AppendPrompt message). Do NOT treat this as a
				// halt: fall through to the same tool-execution + continue
				// path used when stopWhen didn't fire at all, so the run
				// proceeds with a fresh model call next iteration.
				forceResumeCount++
			} else {
				forceResumeCount = 0
				if !allowFinalResponseEnabled(m.input.AllowFinalResponse) {
					m.state.Status = ConversationStatusComplete
					break
				}
				approved, pending, err := PartitionToolCalls(m.ctx, m.input.Tools, calls, BuildTurnContext(nil, turn, &req), m.input.Approval)
				if err != nil {
					m.err = err
					return
				}
				var deniedOutputs []components.InputsUnion1
				if len(pending) > 0 {
					gateApproved, stillPending, gateDenied, gateErr := m.applyPermissionRequestGate(pending)
					if gateErr != nil {
						m.err = gateErr
						return
					}
					approved = append(approved, gateApproved...)
					deniedOutputs = gateDenied
					if len(stillPending) > 0 {
						unsent, err := m.executeAutoApproveTools(approved, turn, &req)
						if err != nil {
							m.err = err
							return
						}
						m.state.UnsentToolResults = append(m.state.UnsentToolResults, unsent...)
						m.state.PendingToolCalls = stillPending
						m.state.Status = ConversationStatusAwaitingApproval
						break
					}
				}
				outputs, err := m.executeToolCallsForTurn(approved, turn, &req)
				if err != nil {
					m.err = err
					return
				}
				outputs = append(outputs, deniedOutputs...)
				if isPausedStatus(m.state.Status) {
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
		}
		approved, pending, err := PartitionToolCalls(m.ctx, m.input.Tools, calls, BuildTurnContext(nil, turn, &req), m.input.Approval)
		if err != nil {
			m.err = err
			return
		}
		var deniedOutputs []components.InputsUnion1
		if len(pending) > 0 {
			gateApproved, stillPending, gateDenied, gateErr := m.applyPermissionRequestGate(pending)
			if gateErr != nil {
				m.err = gateErr
				return
			}
			approved = append(approved, gateApproved...)
			deniedOutputs = gateDenied
			if len(stillPending) > 0 {
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
				m.state.PendingToolCalls = stillPending
				m.state.Status = ConversationStatusAwaitingApproval
				break
			}
		}
		outputs, err := m.executeToolCallsForTurn(approved, turn, &req)
		if err != nil {
			m.err = err
			return
		}
		outputs = append(outputs, deniedOutputs...)
		if isPausedStatus(m.state.Status) {
			break
		}
		if len(outputs) > 0 {
			// A tool round with observable progress resets the consecutive
			// forceResume counter so an earlier override doesn't count
			// against a later, independent one (mirrors upstream).
			forceResumeCount = 0
		}
		base := requestInputItems(req.Input)
		base = append(base, responseItems...)
		base = append(base, outputs...)
		req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(base))
		if resp.ID != "" {
			m.state.PreviousResponseID = &resp.ID
		}
	}
	// Mini-class models intermittently return an empty final turn after a
	// successful tool round (the tool call itself was the answer). Retry
	// once so a completed run doesn't surface an empty answer when the model
	// simply needs a nudge. StrictFinalResponse opts out of the retry
	// (legacy behavior): unlike upstream, this never turns into a hard
	// error — go-sdk's OpenResponsesResult carries a separate `OutputText`
	// convenience field alongside `Output` items, so an empty `Output` array
	// is not on its own a reliable "invalid response" signal the way it is
	// upstream, where `output` is the sole content carrier. See
	// upstreamer-changelog.md for this compatibility note.
	if m.state.Status == ConversationStatusComplete && m.toolRoundsExecuted > 0 && !m.input.StrictFinalResponse && isEmptyFinalResponse(m.resp) {
		retryResp, err := m.retryCurrentRequest(req, len(m.steps)+1)
		if err != nil {
			m.err = err
			return
		}
		m.mu.Lock()
		m.resp = retryResp
		m.steps = append(m.steps, StepResult{Text: ExtractTextFromResponse(retryResp), Response: retryResp, Usage: usagePtr(retryResp), FinishReason: string(retryResp.Status)})
		m.mu.Unlock()
		retryItems, err := responseInputItemsWithError(retryResp)
		if err != nil {
			m.err = err
			return
		}
		m.state = appendResponseItemsToState(m.state, retryResp, retryItems)
	}
	if m.input.StateAccessor != nil {
		if err := m.input.StateAccessor.Save(m.ctx, m.state); err != nil {
			m.err = err
		}
	}
}

// isEmptyFinalResponse reports whether resp carries no assistant-visible
// content at all: no Output items and no text via ExtractTextFromResponse
// (which also checks the SDK's OutputText convenience field).
func isEmptyFinalResponse(resp components.OpenResponsesResult) bool {
	return len(resp.Output) == 0 && ExtractTextFromResponse(resp) == ""
}

// isPausedStatus reports whether status represents a mid-turn pause that
// should stop the loop without sending a (necessarily incomplete) follow-up
// request: an interruption, a HITL pause, or unresolved manual/client tool
// calls (upstream #64's awaiting_client_tools).
func isPausedStatus(status ConversationStatus) bool {
	return status == ConversationStatusInterrupted || status == ConversationStatusAwaitingHITL || status == ConversationStatusAwaitingClientTools
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

// allowFinalResponseEnabled reports whether the forced final ("toolChoice:
// none") turn is enabled for v. Default-on: nil, bare true, and any string
// (including "") enable it; only explicit `false` opts out (upstream #68).
func allowFinalResponseEnabled(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return true
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
			source := ToolSourceClient
			if IsMcpTool(t) {
				source = ToolSourceMCP
			}
			execCtx := BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
				m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Source: source, Event: v, Turn: turn})
			})
			effectiveCall, result, blocked, err := m.runToolWithHooks(t, call, execCtx)
			if err != nil {
				return false, err
			}
			m.toolRoundsExecuted++
			if blocked {
				unsent = append(unsent, CreateRejectedResult(effectiveCall, result.Error.Error()))
				m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Error: result.Error.Error(), Turn: turn})
				continue
			}
			if errors.Is(result.Error, ErrHITLPause) {
				remaining = append(remaining, effectiveCall)
				continue
			}
			if result.Error != nil {
				unsent = append(unsent, CreateRejectedResult(effectiveCall, result.Error.Error()))
			} else {
				unsent = append(unsent, CreateUnsentResult(effectiveCall, result.Output))
			}
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Result: result.Output, Turn: turn})
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
// applyPermissionRequestGate runs the PermissionRequest hook for each tool
// call that needs human approval, letting hooks short-circuit the approval
// flow in either direction: `allow` promotes the call past the gate (so the
// normal round executes it once, appended to approved), `deny` synthesizes a
// rejection (appended to deniedOutputs as a ready-to-send output item —
// there's no pause to persist it against, so it must be sent this round, not
// stashed as an UnsentToolResult), `ask_user` (the default, and always the
// decision when no HooksManager is configured) falls through to the human
// approval flow (left in stillPending, which the caller should pause on if
// non-empty).
func (m *ModelResult) applyPermissionRequestGate(pending []ParsedToolCall) (approved []ParsedToolCall, stillPending []ParsedToolCall, deniedOutputs []components.InputsUnion1, err error) {
	if m.hooksManager == nil {
		return nil, pending, nil, nil
	}
	var denied []UnsentToolResult
	for _, call := range pending {
		decision, reason, hookErr := m.emitPermissionRequest(call)
		if hookErr != nil {
			return nil, nil, nil, hookErr
		}
		switch decision {
		case PermissionDecisionAllow:
			approved = append(approved, call)
		case PermissionDecisionDeny:
			if reason == "" {
				reason = "Denied by PermissionRequest hook"
			}
			denied = append(denied, CreateRejectedResult(call, reason))
		default:
			stillPending = append(stillPending, call)
		}
	}
	if len(denied) > 0 {
		deniedOutputs, err = UnsentResultsToAPIFormatWithError(denied)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, item := range deniedOutputs {
			m.state = AppendToMessages(m.state, item)
		}
	}
	return approved, stillPending, deniedOutputs, nil
}

func (m *ModelResult) executeAutoApproveTools(calls []ParsedToolCall, turn int, req *components.ResponsesRequest) ([]UnsentToolResult, error) {
	var results []UnsentToolResult
	for _, call := range calls {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			// Not executable without approval/manual output — skip (upstream
			// returns null and contributes no unsent result this round).
			continue
		}
		source := ToolSourceClient
		if IsMcpTool(t) {
			source = ToolSourceMCP
		}
		execCtx := BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Source: source, Event: v, Turn: turn})
		})
		effectiveCall, result, blocked, err := m.runToolWithHooks(t, call, execCtx)
		if err != nil {
			return nil, err
		}
		m.toolRoundsExecuted++
		if blocked {
			results = append(results, CreateRejectedResult(effectiveCall, result.Error.Error()))
			continue
		}
		if errors.Is(result.Error, ErrHITLPause) {
			// HITL tool paused during auto-approve — no unsent result this round.
			continue
		}
		if result.Error != nil {
			results = append(results, CreateRejectedResult(effectiveCall, result.Error.Error()))
			continue
		}
		event := ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Result: result.Output, Turn: turn}
		m.toolStream.Push(event)
		results = append(results, CreateUnsentResult(effectiveCall, result.Output))
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
			// Unresolved manual (client-executed) tool call: neither an
			// `execute` fn nor an `OnToolCalled` hook. Persist as
			// awaiting_client_tools (upstream #64) rather than the generic
			// `interrupted` status, so callers can discriminate "the caller
			// must execute this externally" from an arbitrary interruption.
			m.state.PendingToolCalls = append(m.state.PendingToolCalls, call)
			m.state.Status = ConversationStatusAwaitingClientTools
			continue
		}
		source := ToolSourceClient
		if IsMcpTool(t) {
			source = ToolSourceMCP
		}
		execCtx := BuildToolExecuteContext(call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary", CallID: call.CallID, Name: call.Name, Source: source, Event: v, Turn: turn})
		})
		effectiveCall, result, blocked, err := m.runToolWithHooks(t, call, execCtx)
		if err != nil {
			return nil, err
		}
		m.toolRoundsExecuted++
		if blocked {
			out, ferr := FormatToolOutputWithError(result)
			if ferr != nil {
				return nil, ferr
			}
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Error: result.Error.Error(), Turn: turn})
			outputItem := components.CreateInputsUnion1FunctionCallOutputItem(out)
			outputs = append(outputs, outputItem)
			m.state = AppendToMessages(m.state, outputItem)
			continue
		}
		if errors.Is(result.Error, ErrHITLPause) {
			m.state.PendingToolCalls = append(m.state.PendingToolCalls, effectiveCall)
			m.state.Status = ConversationStatusAwaitingHITL
			break
		}
		event := ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Result: result.Output, Turn: turn}
		if result.Error != nil {
			event.Error = result.Error.Error()
		}
		m.toolStream.Push(event)
		out, err := FormatToolOutputWithError(result)
		if err != nil {
			return nil, err
		}
		m.toolStream.Push(ToolStreamEvent{Type: "tool.call.output", CallID: call.CallID, Name: call.Name, Source: source, Output: result.Output, Error: event.Error, Turn: turn})
		outputItem := components.CreateInputsUnion1FunctionCallOutputItem(out)
		outputs = append(outputs, outputItem)
		// Persist this output to state immediately so it survives a later HITL
		// pause in the same round (upstream saveToolResultsToState per output).
		m.state = AppendToMessages(m.state, outputItem)
	}
	return outputs, nil
}

// finalResponseDirective resolves the AllowFinalResponse option to the
// directive text appended as a final user message: default-on (nil) and
// bare true get DefaultFinalResponseDirective; a string (including "")
// overrides the wording — "" means "append nothing".
func finalResponseDirective(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return DefaultFinalResponseDirective
}

// noneToolChoice is the API tool_choice value that forbids tool calls while
// leaving `tools` in the request — stripping `tools` entirely would bust the
// prompt-cache prefix.
func noneToolChoice() *components.OpenAIResponsesToolChoiceUnion {
	choice := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceNone(components.OpenAIResponsesToolChoiceNoneNone)
	return &choice
}

func (m *ModelResult) sendFinalResponseRequest(req components.ResponsesRequest, resp components.OpenResponsesResult, outputs []components.InputsUnion1, turn int) (components.OpenResponsesResult, error) {
	finalReq := req
	// Forbid tool calls without dropping `tools` — removing it would
	// invalidate the prompt-cache prefix (upstream #68).
	if len(finalReq.Tools) > 0 {
		finalReq.ToolChoice = noneToolChoice()
	}
	base := requestInputItems(req.Input)
	items, err := responseInputItemsWithError(resp)
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	base = append(base, items...)
	base = append(base, outputs...)
	if directive := finalResponseDirective(m.input.AllowFinalResponse); directive != "" {
		msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(directive)))}
		base = append(base, components.CreateInputsUnion1EasyInputMessage(msg))
	}
	finalReq.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(base))
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: turn})
	startedAt := time.Now()
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
	m.emitPostModelCall(finalResp, startedAt, ModelCallTurnTypeFinal, turn)
	text := ExtractTextFromResponse(finalResp)
	if len(events) == 0 && text != "" {
		m.textStream.Push(text)
	}
	m.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Turn: turn, Response: &finalResp})
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: turn})
	return finalResp, nil
}

// retryCurrentRequest re-sends req (the same accumulated input) once, used
// when a follow-up after tool execution returned an empty output array.
// ToolChoice is forced to none when tools are present (mirroring
// sendFinalResponseRequest) so the retry coerces a text turn instead of
// risking a fresh (unexecuted) function_call.
func (m *ModelResult) retryCurrentRequest(req components.ResponsesRequest, turn int) (components.OpenResponsesResult, error) {
	retryReq := req
	if len(retryReq.Tools) > 0 {
		retryReq.ToolChoice = noneToolChoice()
	}
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: turn})
	startedAt := time.Now()
	res, err := m.client.SendResponse(m.ctx, retryReq, m.input.MetadataLevel, operations.WithSetHeaders(map[string]string{"x-openrouter-callmodel": "true"}))
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	retryResp, events, err := consumeCreateResponse(res, func(ev components.StreamEvents) {
		if delta, ok := textDeltaText(ev); ok {
			m.textStream.Push(delta)
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "response.event", Turn: turn, Event: &ev})
	})
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
	m.emitPostModelCall(retryResp, startedAt, ModelCallTurnTypeRetry, turn)
	text := ExtractTextFromResponse(retryResp)
	if len(events) == 0 && text != "" {
		m.textStream.Push(text)
	}
	m.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Turn: turn, Response: &retryResp})
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: turn})
	return retryResp, nil
}

func usagePtr(resp components.OpenResponsesResult) *components.Usage {
	if usage, ok := resp.Usage.GetOrZero(); ok {
		return &usage
	}
	return nil
}

// extractModelCallUsage converts an SDK Usage into the hooks ModelCallUsage
// shape, or nil when resp carried no usage at all.
func extractModelCallUsage(resp components.OpenResponsesResult) *ModelCallUsage {
	usage := usagePtr(resp)
	if usage == nil {
		return nil
	}
	out := &ModelCallUsage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		TotalTokens:     usage.TotalTokens,
		CachedTokens:    usage.InputTokensDetails.CachedTokens,
		ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens,
	}
	if cost, ok := usage.Cost.GetOrZero(); ok {
		out.Cost = &cost
	}
	return out
}

// hookEmitOptions builds the per-emit EmitOptions for lifecycle hook emits.
// Threads this run's session identity into every emit so a HooksManager
// instance shared across concurrent runs never leaks one run's id into
// another's handlers (the manager-level SetSessionID default is a single
// mutable field and would be clobbered by the last run to start).
func (m *ModelResult) hookEmitOptions(toolName string) EmitOptions {
	return EmitOptions{ToolName: toolName, SessionID: m.state.ID}
}

// emitSessionStartOnce emits SessionStart exactly once per run.
func (m *ModelResult) emitSessionStartOnce() {
	if m.hooksManager == nil || m.sessionStartEmitted {
		return
	}
	m.sessionStartEmitted = true
	m.hooksManager.SetSessionID(m.state.ID)
	if _, err := m.hooksManager.EmitSessionStart(SessionStartPayload{}, m.hookEmitOptions("")); err != nil {
		log.Printf("[SessionStart] hook error: %v", err)
	}
}

// emitSessionEndOnce emits SessionEnd exactly once, and only when a matching
// SessionStart actually fired. Safe to call from multiple teardown paths;
// never returns an error since teardown must not mask the run's own outcome.
func (m *ModelResult) emitSessionEndOnce(reason SessionEndReason) {
	if m.hooksManager == nil || !m.sessionStartEmitted || m.sessionEndEmitted {
		return
	}
	m.sessionEndEmitted = true
	payload := SessionEndPayload{Reason: reason}
	if m.sessionUsage.modelCalls > 0 {
		totals := SessionUsageTotals{
			ModelCallUsage: ModelCallUsage{
				InputTokens:     m.sessionUsage.inputTokens,
				OutputTokens:    m.sessionUsage.outputTokens,
				TotalTokens:     m.sessionUsage.totalTokens,
				CachedTokens:    m.sessionUsage.cachedTokens,
				ReasoningTokens: m.sessionUsage.reasoningTokens,
			},
			ModelCalls: m.sessionUsage.modelCalls,
		}
		if m.sessionUsage.hasCost {
			cost := m.sessionUsage.cost
			totals.Cost = &cost
		}
		payload.TotalUsage = &totals
	}
	if _, err := m.hooksManager.EmitSessionEnd(payload, m.hookEmitOptions("")); err != nil {
		log.Printf("[SessionEnd] hook error: %v", err)
	}
}

// finishHooksSession emits SessionEnd (if not already emitted) and drains
// pending hook work. Never panics: teardown must not mask the run's own
// error. Call unconditionally on every exit path (success or error) so
// fire-and-forget hook work is never silently dropped — including no-tools
// paths that error before any tool ever runs.
func (m *ModelResult) finishHooksSession(reason SessionEndReason) {
	if m.hooksManager == nil {
		return
	}
	m.emitSessionEndOnce(reason)
	m.hooksManager.Drain()
}

// emitPostModelCall emits PostModelCall for a completed model response and
// folds its usage into the session aggregate. One emit per materialized response.
func (m *ModelResult) emitPostModelCall(resp components.OpenResponsesResult, startedAt time.Time, turnType ModelCallTurnType, turnNumber int) {
	if m.hooksManager == nil {
		return
	}
	usage := extractModelCallUsage(resp)
	m.sessionUsage.modelCalls++
	if usage != nil {
		m.sessionUsage.inputTokens += usage.InputTokens
		m.sessionUsage.outputTokens += usage.OutputTokens
		m.sessionUsage.totalTokens += usage.TotalTokens
		m.sessionUsage.cachedTokens += usage.CachedTokens
		m.sessionUsage.reasoningTokens += usage.ReasoningTokens
		if usage.Cost != nil {
			m.sessionUsage.cost += *usage.Cost
			m.sessionUsage.hasCost = true
		}
	}
	payload := PostModelCallPayload{
		SessionID:  m.state.ID,
		ResponseID: resp.ID,
		Model:      resp.Model,
		DurationMs: float64(time.Since(startedAt).Milliseconds()),
		TurnType:   turnType,
		TurnNumber: turnNumber,
		Usage:      usage,
	}
	if _, err := m.hooksManager.EmitPostModelCall(payload, m.hookEmitOptions("")); err != nil {
		log.Printf("[PostModelCall] hook error: %v", err)
	}
}

// maxForceResumeOverrides caps consecutive Stop-hook forceResume overrides so
// a misbehaving handler cannot spin the loop forever.
const maxForceResumeOverrides = 3

// runStopHook emits the Stop hook when a stopWhen condition halts the loop,
// and decides whether the loop should resume.
//
//   - AppendPrompt values from all handlers are concatenated (newline
//     separated) and injected as a user message, independently of ForceResume.
//   - ForceResume is honored if ANY handler returns it, capped at
//     maxForceResumeOverrides consecutive overrides without tool progress.
//
// Returns true when the loop should resume, false to stop for real. No
// HooksManager means "no hook to consult" -> always stop.
func (m *ModelResult) runStopHook(req *components.ResponsesRequest, forceResumeCount int) (bool, error) {
	if m.hooksManager == nil {
		return false, nil
	}

	stopResult, err := m.hooksManager.EmitStop(StopPayload{Reason: StopReasonMaxTurns}, m.hookEmitOptions(""))
	if err != nil {
		return false, fmt.Errorf("stop hook: %w", err)
	}

	shouldForceResume := false
	var appendPrompts []string
	for _, r := range stopResult.Results {
		if r.ForceResume {
			shouldForceResume = true
		}
		if r.AppendPrompt != "" {
			appendPrompts = append(appendPrompts, r.AppendPrompt)
		}
	}
	if len(appendPrompts) > 0 {
		m.injectAppendPromptMessage(req, strings.Join(appendPrompts, "\n"))
	}

	if !shouldForceResume {
		return false, nil
	}
	if forceResumeCount >= maxForceResumeOverrides {
		// Don't let the hook loop the engine forever. Log and stop.
		log.Printf("[Stop hook] forceResume honored %d times without new progress; stopping to prevent an infinite loop.", maxForceResumeOverrides)
		return false, nil
	}
	return true, nil
}

// injectAppendPromptMessage appends prompt as a user message to both the
// in-memory conversation state (so progress is observable even without a
// StateAccessor — a bare ForceResume needs state to change to avoid looping)
// and the in-flight request's accumulated input (so it reaches the next
// model call).
func (m *ModelResult) injectAppendPromptMessage(req *components.ResponsesRequest, prompt string) {
	msg := components.EasyInputMessage{Role: components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser), Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(prompt)))}
	item := components.CreateInputsUnion1EasyInputMessage(msg)
	m.state = AppendToMessages(m.state, item)

	base := requestInputItems(req.Input)
	base = append(base, item)
	req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(base))
}

// emitPermissionRequest emits the PermissionRequest hook before the SDK
// blocks for user approval. Returns the hook's collective decision:
// allow (proceed as if auto-approved), deny (synthesize a rejection without
// running the tool), or ask_user (fall through to the existing approval
// flow — the default, and what a nil HooksManager always returns).
// Last-wins when multiple handlers return conflicting decisions.
func (m *ModelResult) emitPermissionRequest(call ParsedToolCall) (PermissionDecision, string, error) {
	if m.hooksManager == nil {
		return PermissionDecisionAskUser, "", nil
	}
	// Raw-string arguments mean the model produced invalid JSON. Fail closed
	// (fall through to the human approval flow) rather than emitting a
	// malformed payload — mirrors the guard in runToolWithHooks.
	if call.RawArgs != "" && call.Arguments == nil {
		if _, ok := toolInputFromRawArgs(call.RawArgs); !ok {
			return PermissionDecisionAskUser, "", nil
		}
	}

	// Derive risk level from the approval gate's shape: a callback (tool- or
	// call-level requireApproval function) => high (caller actively decides
	// per call), blanket true => medium, otherwise low.
	t := FindToolByName(m.input.Tools, call.Name)
	riskLevel := RiskLevelLow
	if m.input.Approval != nil {
		riskLevel = RiskLevelHigh
	} else if t != nil {
		if approvalTool, ok := t.(interface {
			RequiresApproval(context.Context, ParsedToolCall, TurnContext) (bool, error)
		}); ok {
			if req, err := approvalTool.RequiresApproval(m.ctx, call, TurnContext{}); err == nil && req {
				riskLevel = RiskLevelMedium
			}
		}
	}

	emit, err := m.hooksManager.EmitPermissionRequest(PermissionRequestPayload{ToolName: call.Name, ToolInput: toolInputMap(call), RiskLevel: riskLevel}, m.hookEmitOptions(call.Name))
	if err != nil {
		return PermissionDecisionAskUser, "", fmt.Errorf("PermissionRequest hook: %w", err)
	}
	if len(emit.Results) == 0 {
		return PermissionDecisionAskUser, "", nil
	}
	last := emit.Results[len(emit.Results)-1]
	return last.Decision, last.Reason, nil
}

// toolInputFromRawArgs reports whether rawArgs is valid JSON, mirroring the
// "raw-string arguments" fail-closed guard shared by runToolWithHooks and
// emitPermissionRequest.
func toolInputFromRawArgs(rawArgs string) (map[string]any, bool) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// maybeRunUserPromptSubmit runs the UserPromptSubmit hook against the latest
// user-role string message in req's accumulated input (mirroring upstream's
// extractPromptAndApplier), mutating that message's content in place when a
// handler returns MutatedPrompt. A structured input with no plain-string
// user message (e.g. image content, or no user message at all) has nothing
// for the hook to inspect, so it is skipped without error — matching
// upstream's dev-only warn-and-skip behavior.
//
// Returns an error if any handler rejects the prompt.
func (m *ModelResult) maybeRunUserPromptSubmit(req *components.ResponsesRequest) error {
	if m.hooksManager == nil || req.Input == nil {
		return nil
	}
	items := requestInputItems(req.Input)
	idx, prompt, ok := latestUserStringMessage(items)
	if !ok {
		return nil
	}

	emit, err := m.hooksManager.EmitUserPromptSubmit(UserPromptSubmitPayload{Prompt: prompt}, m.hookEmitOptions(""))
	if err != nil {
		return fmt.Errorf("UserPromptSubmit hook: %w", err)
	}
	if emit.Blocked {
		reason := emit.BlockReason
		if reason == "" {
			reason = "Prompt rejected by hook"
		}
		return errors.New(reason)
	}
	if !emit.Mutated {
		return nil
	}
	mutated := emit.FinalPayload.Prompt
	msg := *items[idx].EasyInputMessage
	msg.Content = optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(mutated)))
	items[idx] = components.CreateInputsUnion1EasyInputMessage(msg)
	req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(items))
	return nil
}

// latestUserStringMessage finds the last user-role EasyInputMessage in items
// whose content is a plain string, returning its index and text.
func latestUserStringMessage(items []components.InputsUnion1) (int, string, bool) {
	for i := len(items) - 1; i >= 0; i-- {
		msg := items[i].EasyInputMessage
		if msg == nil || msg.Role.EasyInputMessageRoleUser == nil {
			continue
		}
		content, ok := msg.Content.GetOrZero()
		if !ok || content.Str == nil {
			continue
		}
		return i, *content.Str, true
	}
	return -1, "", false
}

// runToolWithHooks executes one tool call and emits the PreToolUse/
// PostToolUse/PostToolUseFailure lifecycle hooks around it, so every
// execution path (auto-approve, manual approval, approved-on-resume) fires
// them consistently. Returns the (possibly input-mutated) call actually
// executed, the execution result, whether PreToolUse blocked execution (in
// which case result carries a synthetic rejection and the tool never ran),
// and a fatal (non-tool) error such as a request-encoding failure — this
// last one should abort the run exactly like a direct ExecuteTool error
// would, not be treated as a per-call rejection.
func (m *ModelResult) runToolWithHooks(t Tool, call ParsedToolCall, execCtx ToolExecuteContext) (effectiveCall ParsedToolCall, result ToolExecutionResult, blocked bool, fatalErr error) {
	effectiveCall = call
	if m.hooksManager == nil {
		res, err := ExecuteTool(m.ctx, t, effectiveCall, execCtx)
		if err != nil {
			return effectiveCall, ToolExecutionResult{}, false, err
		}
		return effectiveCall, res, false, nil
	}

	toolInput := toolInputMap(call)
	preResult, err := m.hooksManager.EmitPreToolUse(PreToolUsePayload{ToolName: call.Name, ToolInput: toolInput}, m.hookEmitOptions(call.Name))
	if err != nil {
		return effectiveCall, ToolExecutionResult{}, false, fmt.Errorf("PreToolUse hook: %w", err)
	}
	if preResult.Blocked {
		reason := preResult.BlockReason
		if reason == "" {
			reason = "Blocked by PreToolUse hook"
		}
		return effectiveCall, ToolExecutionResult{CallID: call.CallID, Name: call.Name, Error: errors.New(reason)}, true, nil
	}
	if preResult.Mutated {
		effectiveCall.Arguments = preResult.FinalPayload.ToolInput
		effectiveCall.RawArgs = ""
	}

	startedAt := time.Now()
	execCtx.ToolCall = effectiveCall
	res, err := ExecuteTool(m.ctx, t, effectiveCall, execCtx)
	if err != nil {
		return effectiveCall, ToolExecutionResult{}, false, err
	}
	durationMs := float64(time.Since(startedAt).Milliseconds())

	if res.Error != nil {
		if _, hookErr := m.hooksManager.EmitPostToolUseFailure(PostToolUseFailurePayload{ToolName: effectiveCall.Name, ToolInput: toolInputMap(effectiveCall), Error: res.Error}, m.hookEmitOptions(effectiveCall.Name)); hookErr != nil {
			log.Printf("[PostToolUseFailure] hook error: %v", hookErr)
		}
	} else {
		if _, hookErr := m.hooksManager.EmitPostToolUse(PostToolUsePayload{ToolName: effectiveCall.Name, ToolInput: toolInputMap(effectiveCall), ToolOutput: res.Output, DurationMs: durationMs}, m.hookEmitOptions(effectiveCall.Name)); hookErr != nil {
			log.Printf("[PostToolUse] hook error: %v", hookErr)
		}
	}
	return effectiveCall, res, false, nil
}

// toolInputMap best-effort converts a ParsedToolCall's arguments to
// map[string]any for hook payloads, coercing null/missing arguments to an
// empty map (mirroring upstream's PreToolUse payload construction).
func toolInputMap(call ParsedToolCall) map[string]any {
	if m, ok := call.Arguments.(map[string]any); ok {
		return m
	}
	if call.RawArgs != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(call.RawArgs), &decoded); err == nil {
			return decoded
		}
	}
	return map[string]any{}
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
	// The broadcaster already applies the same buffer size its sibling streams
	// pass explicitly.
	return m.toolStream.Subscribe()
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
