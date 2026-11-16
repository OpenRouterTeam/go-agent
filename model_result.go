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

// maxPendingIDsInError caps the pending call IDs listed in a
// ConversationPausedError message; the full list is available from
// ModelResult.PendingToolCalls.
const maxPendingIDsInError = 10

// ErrResponseNotAvailable is returned by response getters when the run
// produced no response and the conversation is not paused.
var ErrResponseNotAvailable = errors.New("response not available")

// ConversationPausedError is returned by Response, Text, ToolCalls and the
// stream getters when a call left the conversation paused without producing
// a response: an awaiting_approval/awaiting_hitl state resumed with no (or
// only some) decisions. RequiresApproval, PendingToolCalls and State keep
// working on such a result.
type ConversationPausedError struct {
	Status         ConversationStatus
	PendingCallIDs []string
	// InputDropped is true when input was supplied with the call; it was not
	// sent to the model and not saved to state.
	InputDropped bool
}

func (e *ConversationPausedError) Error() string {
	shown := e.PendingCallIDs
	hidden := 0
	if len(shown) > maxPendingIDsInError {
		hidden = len(shown) - maxPendingIDsInError
		shown = shown[:maxPendingIDsInError]
	}
	idList := strings.Join(shown, ", ")
	if hidden > 0 {
		idList = fmt.Sprintf("%s, ... (%d more)", idList, hidden)
	}
	what := "awaiting approval"
	howToResume := "Resume with ApproveToolCalls and/or RejectToolCalls for the pending calls."
	if e.Status == ConversationStatusAwaitingHITL {
		what = "awaiting HITL input"
		howToResume = "Resume by passing a function_call_output for a pending call ID in Input, or with ApproveToolCalls / RejectToolCalls (approving re-runs OnToolCalled)."
	}
	msg := fmt.Sprintf("conversation is %s (status '%s'); no response is available. Pending tool call IDs: %s. %s Use RequiresApproval() or PendingToolCalls() to inspect a paused conversation.", what, e.Status, idList, howToResume)
	if e.InputDropped {
		msg += " The input supplied with this call was not sent to the model and was not saved to state."
	}
	return msg
}

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

// preparedCall is the outcome of running PreToolUse for a call before it is
// executed (the approval "preparation" phase). blocked calls get a rejected
// output with reason and never run; ready calls execute with call (the
// hook's effective input) and do not run PreToolUse again.
type preparedCall struct {
	blocked bool
	reason  string
	call    ParsedToolCall
	mutated bool
}

// toolGate carries per-call decisions made before a round executes.
type toolGate struct {
	// denied maps call key -> rejection reason (PermissionRequest deny).
	denied map[string]string
	// prepared maps call key -> PreToolUse preparation outcome.
	prepared map[string]preparedCall
	// doomBlocked maps call key -> doom-loop verdict message for calls the
	// doom-loop checkpoint blocked (block/stop). Like a denial they get a
	// rejected output and never execute, but unlike one they were never
	// partitioned for approval: on an approval pause they still join the
	// executable-now batch (as rejected results), mirroring upstream's
	// blockedCalls.
	doomBlocked map[string]string
}

func newToolGate() toolGate {
	return toolGate{denied: map[string]string{}, prepared: map[string]preparedCall{}, doomBlocked: map[string]string{}}
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

	// gotResponse is set once any model response materialized in this call.
	gotResponse bool
	// pausedWithoutDecisions: the loaded state is awaiting a decision that
	// this call did not supply, so the run stays paused without a request.
	pausedWithoutDecisions bool
	// freshCount is the number of trailing items of the initial request input
	// that were supplied with this call (not loaded history). They are
	// persisted to state together with the first response.
	freshCount int

	// Forced tool-choice policy (upstream DEV-785): a caller-configured
	// choice that forces a tool call is one-shot per logical run.
	configuredToolChoice *components.OpenAIResponsesToolChoiceUnion
	consumedForcedKey    string

	// Doom-loop detection (all zero when CallModelInput.DoomLoop is nil).
	// The monitor is the pure state machine; these fields own the engine
	// consequences (hook emission, block synthesis, steer injection,
	// escalation, stopping). See the doom-loop region at the end of file.
	doomMonitor    *DoomLoopMonitor
	doomEscalation *ResolvedEscalationConfig
	// doomStop is the verdict that condemned the run (persisted as
	// DoomLoop.StopVerdict).
	doomStop *DoomLoopVerdict
	// doomHalted is set when the run actually halted on doomStop, so
	// SessionEnd reports SessionEndReasonDoomLoop.
	doomHalted bool
	// doomPendingSteer is queued steer guidance (persisted as
	// DoomLoop.PendingSteer until flushed before a follow-up request).
	doomPendingSteer []string
	// steerMu guards doomPendingSteer: QueueUserMessage may be called from
	// any goroutine while the run goroutine flushes it.
	steerMu sync.Mutex
	// doomPendingEscalation is armed by an escalate verdict and consumed as
	// one-turn overrides by the next follow-up dispatch. Not persisted.
	doomPendingEscalation *DoomLoopVerdict
	// doomRound is the monotonic round counter; doomRoundDecisions and
	// doomRoundKeys are per-round caches (see beginDoomLoopRound).
	doomRound          int
	doomRoundDecisions map[string]doomDecision
	doomRoundKeys      map[string]doomKeyEntry
	// doomGatedTurn/doomGateBlocked memoize the doom-loop gate for one
	// response so the checkpoint runs exactly once per response even when
	// several gate call sites visit it.
	doomGatedTurn   int
	doomGateBlocked map[string]string
}

func newModelResult(ctx context.Context, client ResponseSender, input CallModelInput, req components.ResponsesRequest, state ConversationState) *ModelResult {
	ctx2, cancel := context.WithCancel(ctx)
	return &ModelResult{ctx: ctx2, cancel: cancel, client: client, input: input, req: req, state: state, store: NewToolContextStore(input.Context), done: make(chan struct{}), textStream: NewReusableStreamWithReplay[string](input.StreamReplay), reasoningStream: NewReusableStreamWithReplay[string](input.StreamReplay), toolStream: NewToolEventBroadcasterWithReplay(input.StreamReplay), toolCallStream: NewReusableStreamWithReplay[ParsedToolCall](input.StreamReplay), fullStream: NewReusableStreamWithReplay[ResponseStreamEvent](input.StreamReplay), newMessages: NewReusableStreamWithReplay[components.InputsUnion1](input.StreamReplay), hooksManager: ResolveHooks(input.Hooks), configuredToolChoice: req.ToolChoice, consumedForcedKey: state.ConsumedForcedToolChoiceKey}
}

func NewModelResultFromResponse(resp components.OpenResponsesResult) *ModelResult {
	mr := newModelResult(context.Background(), nil, CallModelInput{}, components.ResponsesRequest{}, CreateInitialState())
	mr.resp = resp
	mr.gotResponse = true
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

// dedupeIDs removes duplicate IDs, keeping first occurrences in order.
func dedupeIDs(ids []string) []string {
	if ids == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func CallModel(ctx context.Context, client ResponseSender, input CallModelInput) (*ModelResult, error) {
	if client == nil {
		return nil, errors.New("client is required")
	}
	// Tool calls are dispatched by name: two client tools sharing a name
	// would let the first one receive calls (including approved calls) meant
	// for the other.
	if err := AssertUniqueClientToolNames(input.Tools); err != nil {
		return nil, err
	}
	// De-duplicate decisions so a call ID listed twice cannot execute an
	// approval-gated tool twice, and refuse contradictory input outright: an
	// ID that is both approved and rejected has no safe interpretation.
	input.ApproveToolCalls = dedupeIDs(input.ApproveToolCalls)
	input.RejectToolCalls = dedupeIDs(input.RejectToolCalls)
	rejected := map[string]bool{}
	for _, id := range input.RejectToolCalls {
		rejected[id] = true
	}
	var conflicting []string
	for _, id := range input.ApproveToolCalls {
		if rejected[id] {
			conflicting = append(conflicting, id)
		}
	}
	if len(conflicting) > 0 {
		return nil, fmt.Errorf("tool call IDs cannot be both approved and rejected: %s; pass each pending call ID in exactly one of ApproveToolCalls or RejectToolCalls", strings.Join(conflicting, ", "))
	}
	if input.ActiveTools != nil {
		filtered, err := FilterToolsByIDs(input.Tools, input.ActiveTools)
		if err != nil {
			return nil, err
		}
		input.Tools = filtered
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
	hasDecisions := len(input.ApproveToolCalls) > 0 || len(input.RejectToolCalls) > 0
	var fresh []components.InputsUnion1
	if input.Input != nil {
		fresh, err = inputItemsOnly(input.Input)
		if err != nil {
			return nil, err
		}
	}
	// Paused for a decision this call does not supply: stay paused. Falling
	// through would send a fresh model request and advance the conversation
	// past calls that still owe a decision.
	stayPaused := !hasDecisions && staysPausedWithoutDecisions(state, fresh)
	freshCount := 0
	if input.Input != nil && !stayPaused {
		fresh, err = applyOnResponseReceivedHooks(ctx, input.Tools, state.Messages, fresh, NewToolContextStore(input.Context))
		if err != nil {
			return nil, err
		}
		items := append([]components.InputsUnion1{}, state.Messages...)
		items = append(items, fresh...)
		req.Input = openrouter.Pointer(components.CreateInputsUnionArrayOfInputsUnion1(items))
		freshCount = len(fresh)
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
	mr := newModelResult(ctx, client, input, req, state)
	mr.pausedWithoutDecisions = stayPaused
	mr.freshCount = freshCount
	mr.initDoomLoop(hasDecisions)
	return mr, nil
}

// staysPausedWithoutDecisions reports whether a state loaded as
// awaiting_approval / awaiting_hitl stays paused when the caller supplied no
// approve/reject decisions.
//
//   - No pending calls: nothing is owed a decision; continue normally.
//   - awaiting_approval: always paused. Only a recorded approve/reject
//     decision releases an approval gate; a caller-supplied
//     function_call_output for the gated call does not.
//   - awaiting_hitl: paused unless fresh input carries a function_call_output
//     for a pending call — the documented HITL resume (the output goes
//     through OnResponseReceived and on to the model).
func staysPausedWithoutDecisions(state ConversationState, fresh []components.InputsUnion1) bool {
	if !isAwaitingResume(state) || len(state.PendingToolCalls) == 0 {
		return false
	}
	if state.Status != ConversationStatusAwaitingHITL {
		return true
	}
	pending := map[string]bool{}
	for _, call := range state.PendingToolCalls {
		pending[callKey(call)] = true
		if call.CallID != "" {
			pending[call.CallID] = true
		}
	}
	for _, item := range fresh {
		if item.FunctionCallOutputItem != nil && pending[item.FunctionCallOutputItem.CallID] {
			return false
		}
	}
	return true
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

// unavailableError is the error response/stream getters report when the run
// produced no response: a ConversationPausedError when the conversation is
// paused awaiting the caller, ErrResponseNotAvailable otherwise. nil when a
// response exists (or the result never ran a model call at all).
func (m *ModelResult) unavailableError() error {
	if m.gotResponse || m.client == nil {
		return nil
	}
	if isAwaitingResume(m.state) {
		ids := make([]string, 0, len(m.state.PendingToolCalls))
		for _, call := range m.state.PendingToolCalls {
			ids = append(ids, callKey(call))
		}
		return &ConversationPausedError{Status: m.state.Status, PendingCallIDs: ids, InputDropped: m.inputSupplied()}
	}
	return ErrResponseNotAvailable
}

func (m *ModelResult) inputSupplied() bool {
	switch v := m.input.Input.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []components.InputsUnion1:
		return len(v) > 0
	case components.InputsUnion:
		return (v.Str != nil && *v.Str != "") || len(v.ArrayOfInputsUnion1) > 0
	}
	return true
}

// getterErr is the error every response/stream getter reports.
func (m *ModelResult) getterErr() error {
	if m.err != nil {
		return m.err
	}
	return m.unavailableError()
}

func (m *ModelResult) run() {
	defer close(m.done)
	// Complete every stream with the *final* error (read at defer-execution
	// time via a closure, not captured at registration). This is what lets an
	// SDK stream/transport error propagate to every ModelResult consumer
	// (text/tool/full) instead of completing them with a stale nil error. A
	// paused call that produced no response completes them with the pause
	// error so a stream consumer learns why nothing arrived.
	defer func() {
		err := m.getterErr()
		m.textStream.Complete(err)
		m.reasoningStream.Complete(err)
		// Error(nil) is the broadcaster's no-error completion, so this is
		// equivalent to the sibling Complete(err) calls for both outcomes.
		m.toolStream.Error(err)
		m.toolCallStream.Complete(err)
		m.fullStream.Complete(err)
		m.newMessages.Complete(err)
	}()
	// Session teardown must run on every exit path — success, early return,
	// or error — so fire-and-forget hook work from a run that fails before
	// any tool ever executes (a "no-tools" path) is still drained. Never
	// masks m.err: finishHooksSession only logs its own failures.
	defer func() {
		reason := SessionEndReasonComplete
		if m.err != nil {
			reason = SessionEndReasonError
		} else if m.doomHalted {
			reason = SessionEndReasonDoomLoop
		}
		m.finishHooksSession(reason)
	}()
	if m.client == nil {
		return
	}
	if m.pausedWithoutDecisions {
		// Nothing to send: the conversation still owes a decision. State is
		// left untouched (the supplied input is neither sent nor saved).
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
	// UserPromptSubmit entirely (upstream: "This path bypasses the
	// SessionStart block below but still fires tool hooks
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
		m.freshCount = 0
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
			m.saveState()
			return
		}
		if m.doomStop != nil {
			// A condemned run resumed with decisions (the persisted stop
			// verdict was re-armed): the approved/rejected calls have their
			// outputs, but no model request follows (upstream: "doom stop
			// armed during resume -> no unsent-results request"). Terminal,
			// so the forced-tool-choice consumption is reset.
			m.doomHalted = true
			m.consumedForcedKey = ""
			m.saveState()
			return
		}
		isResume = true
	} else {
		// A fresh turn (or a HITL resume via a function_call_output in
		// Input) is in progress. A client-tools resume keeps its status
		// (and pending calls) until the resumed request succeeds, so a
		// failed request does not lose the only copy.
		if m.state.Status != ConversationStatusAwaitingClientTools {
			m.state.Status = ConversationStatusInProgress
		}
		if m.saveStateFailed() {
			return
		}
		m.emitSessionStartOnce()
		if err := m.maybeRunUserPromptSubmit(&req); err != nil {
			m.err = err
			return
		}
	}
	for turn := 0; turn < maxTurns; turn++ {
		// Run-level cancellation: fail at the turn boundary with the
		// context's error (in-flight requests are cancelled by the SDK).
		if err := m.ctx.Err(); err != nil {
			m.err = err
			return
		}
		if m.input.BeforeTurn != nil {
			if err := m.input.BeforeTurn(m.ctx, BuildTurnContext(nil, turn, &req)); err != nil {
				m.err = err
				return
			}
		}
		turnType := ModelCallTurnTypeToolRound
		if turn == 0 {
			if isResume {
				turnType = ModelCallTurnTypeResume
			} else {
				turnType = ModelCallTurnTypeInitial
			}
		}
		forcedKey := m.applyToolChoicePolicy(&req)
		// Doom-loop escalation: one-turn overrides applied to THIS dispatch
		// only — req keeps the configured model/tools, so the next turn
		// reverts automatically.
		sendReq, callerChoiceDispatched, err := m.takeDoomLoopEscalationOverrides(req)
		if err != nil {
			m.err = err
			return
		}
		resp, events, err := m.sendTurn(sendReq, turn, turnType, turn+1, true)
		if err != nil {
			m.err = err
			return
		}
		// An engine-owned toolChoice override (forced advisor consult) means
		// the caller's choice never reached the wire: it must not commit.
		if callerChoiceDispatched {
			m.commitToolChoice(forcedKey, ResponseHasToolCalls(resp))
		}
		text := ExtractTextFromResponse(resp)
		if len(events) == 0 && text != "" {
			m.textStream.Push(text)
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "response.completed", Turn: turn, Response: &resp})
		m.fullStream.Push(ResponseStreamEvent{Type: "turn.end", Turn: turn})
		calls := ExtractToolCallsFromResponse(resp)
		// The tool-calls stream is a consumer view of what the model emitted,
		// so it still reports the calls of a max_output_tokens-truncated turn
		// even though none of them execute.
		for _, call := range emittedToolCalls(resp) {
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
		// Fresh caller input is persisted atomically with the first
		// response, so a request that fails never leaves an orphaned user
		// turn in state (a retry would otherwise duplicate it).
		if turn == 0 && m.freshCount > 0 {
			items := requestInputItems(m.reqInputAtStart(req))
			if m.freshCount <= len(items) {
				m.state = AppendToMessages(m.state, items[len(items)-m.freshCount:]...)
			}
			m.freshCount = 0
		}
		m.state = appendResponseItemsToState(m.state, resp, responseItems)
		if turn == 0 && !isResume && m.state.Status == ConversationStatusAwaitingClientTools {
			// Resuming from client-executed tools: the caller supplied the
			// outputs and the resumed request succeeded, so the manual calls
			// are resolved (upstream resumingFromClientTools). They were kept
			// until now so a failed request would not lose the only copy.
			m.state.PendingToolCalls = nil
			m.state.Status = ConversationStatusInProgress
		}
		for _, item := range responseItems {
			m.newMessages.Push(item)
		}
		// Persist every turn (upstream saveResponseToState), so a
		// StateAccessor caller never loses already-materialized work to a
		// later turn's failure.
		if m.saveStateFailed() {
			return
		}
		// Doom-loop text checkpoint (within-response repetition, cross-step
		// streak) plus server-tool fingerprints, on every fresh response.
		if err := m.checkDoomLoopForResponse(resp); err != nil {
			m.err = err
			return
		}
		if len(calls) == 0 {
			// A text verdict may have armed a stop: report the run as
			// doom-stopped, not as a normal completion.
			if m.doomStop != nil {
				m.doomHalted = true
			}
			m.markComplete()
			break
		}
		if m.doomMonitor != nil {
			// Upstream runs the initial response's approval gate (and with
			// it the per-call doom checkpoint) before the loop's stop
			// checks; later responses are gated after them.
			if turn == 0 {
				if _, err := m.doomLoopGate(calls, turn); err != nil {
					m.err = err
					return
				}
			}
			// A stop verdict armed by this response's text (or, on the
			// initial response, by its calls) halts the run before any
			// further spend. Seal first: none of this response's calls ran.
			if m.doomStop != nil {
				m.haltForDoomLoop(resp, nil)
				break
			}
		}
		// Upstream gates the initial response and, when none of its calls is
		// engine-executable (all manual / unknown), pauses for client tools
		// BEFORE consulting stop conditions.
		if turn == 0 && !m.hasExecutableToolCalls(calls) {
			paused, err := m.gateAndMaybePause(calls, turn, &req, false)
			if err != nil {
				m.err = err
				return
			}
			if !paused {
				m.state.PendingToolCalls = append([]ParsedToolCall{}, calls...)
				m.state.Status = ConversationStatusAwaitingClientTools
			}
			break
		}
		// The initial response goes through the approval gate BEFORE stop
		// conditions and the Stop hook are consulted (upstream runs
		// handleApprovalCheck on it ahead of the loop): an approval-gated
		// call pauses without firing Stop. With AllowFinalResponse=false and
		// stopWhen already met, the pause must not execute the auto-approved
		// siblings — they are saved as skipped results instead.
		var initialGate *toolGate
		if turn == 0 {
			skipAuto := false
			if !allowFinalResponseEnabled(m.input.AllowFinalResponse) {
				met, err := IsStopConditionMet(m.ctx, m.input.StopWhen, steps)
				if err != nil {
					m.err = err
					return
				}
				skipAuto = met
			}
			g, stillPending, err := m.gateToolCalls(calls, turn, &req)
			if err != nil {
				m.err = err
				return
			}
			if len(stillPending) > 0 {
				if err := m.pauseForApproval(calls, stillPending, g, turn, &req, skipAuto); err != nil {
					m.err = err
					return
				}
				break
			}
			initialGate = &g
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
					// With AllowFinalResponse=false a met stopWhen means no
					// tool from this response runs.
					m.markComplete()
					break
				}
				if err := m.runFinalResponseTurn(req, resp, calls, turn, initialGate); err != nil {
					m.err = err
					return
				}
				break
			}
		}
		var gate toolGate
		var stillPending []ParsedToolCall
		if initialGate != nil {
			gate = *initialGate
		} else {
			gate, stillPending, err = m.gateToolCalls(calls, turn, &req)
			if err != nil {
				m.err = err
				return
			}
		}
		if len(stillPending) > 0 {
			// Mixed approval turn: execute the auto-approved calls and
			// persist their outputs as unsent results *before* pausing for
			// the approval-required calls, matching upstream
			// handleApprovalCheck -> executeAutoApproveTools ordering.
			if err := m.pauseForApproval(calls, stillPending, gate, turn, &req, false); err != nil {
				m.err = err
				return
			}
			break
		}
		outputs, executed, err := m.executeToolCallsForTurn(calls, gate, turn, &req, false)
		if err != nil {
			m.err = err
			return
		}
		if isPausedStatus(m.state.Status) {
			break
		}
		// Persist the round's tool results (upstream saveToolResultsToState).
		if m.saveStateFailed() {
			return
		}
		// A doom-loop stop armed during this round halts the run before the
		// next model request (no final text-coercion request either).
		if m.doomStop != nil {
			m.haltForDoomLoop(resp, outputs)
			break
		}
		if len(outputs) > 0 {
			// A tool round with observable progress resets the consecutive
			// forceResume counter so an earlier override doesn't count
			// against a later, independent one (mirrors upstream).
			forceResumeCount = 0
		}
		// Inject queued doom-loop steer guidance so the very next request
		// carries it (lands before this round's call/output items).
		m.flushDoomLoopSteer(&req)
		if err := m.applyNextTurnParams(executed, &req); err != nil {
			m.err = err
			return
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
	if m.state.Status == ConversationStatusComplete && !m.doomHalted && m.toolRoundsExecuted > 0 && !m.input.StrictFinalResponse && isEmptyFinalResponse(m.resp) {
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
	m.saveState()
}

// reqInputAtStart returns the request input the first turn was sent with.
// Turn 0's req.Input is still the initial (history + fresh) input,
// including any UserPromptSubmit mutation.
func (m *ModelResult) reqInputAtStart(req components.ResponsesRequest) *components.InputsUnion {
	return req.Input
}

// saveState persists the current state through the StateAccessor, if any.
// The consumed forced-tool-choice key rides along so a pause/resume keeps the
// relaxation.
func (m *ModelResult) saveState() {
	m.state.ConsumedForcedToolChoiceKey = m.consumedForcedKey
	m.snapshotDoomLoopState()
	if m.input.StateAccessor != nil {
		if err := m.input.StateAccessor.Save(m.ctx, m.state); err != nil && m.err == nil {
			m.err = err
		}
	}
}

// saveStateFailed saves state through the StateAccessor (no-op without
// one) and reports whether the save failed, in which case m.err is set and
// the run must stop.
func (m *ModelResult) saveStateFailed() bool {
	if m.input.StateAccessor == nil {
		return false
	}
	m.saveState()
	return m.err != nil
}

// markComplete marks the conversation complete. A later conversational turn
// is a new logical run and may force its first tool call again, so the
// forced-tool-choice consumption is reset.
func (m *ModelResult) markComplete() {
	m.state.Status = ConversationStatusComplete
	m.consumedForcedKey = ""
}

// sendTurn dispatches req and consumes the response, streaming text and
// reasoning deltas and full-stream events for turn. The caller pushes the
// response.completed/turn.end events.
func (m *ModelResult) sendTurn(req components.ResponsesRequest, turn int, turnType ModelCallTurnType, turnNumber int, withReasoning bool) (components.OpenResponsesResult, []components.StreamEvents, error) {
	m.fullStream.Push(ResponseStreamEvent{Type: "turn.start", Turn: turn})
	startedAt := time.Now()
	res, err := m.client.SendResponse(m.ctx, req, m.input.MetadataLevel, operations.WithSetHeaders(map[string]string{"x-openrouter-callmodel": "true"}))
	if err != nil {
		return components.OpenResponsesResult{}, nil, err
	}
	resp, events, err := consumeCreateResponse(res, func(ev components.StreamEvents) {
		if delta, ok := textDeltaText(ev); ok {
			m.textStream.Push(delta)
		}
		if withReasoning {
			if delta, ok := reasoningDeltaText(ev); ok {
				m.reasoningStream.Push(delta)
			}
		}
		m.fullStream.Push(ResponseStreamEvent{Type: "response.event", Turn: turn, Event: &ev})
	})
	if err != nil {
		return components.OpenResponsesResult{}, events, err
	}
	m.gotResponse = true
	m.emitPostModelCall(resp, startedAt, turnType, turnNumber)
	return resp, events, nil
}

// runFinalResponseTurn handles a stopWhen halt with AllowFinalResponse
// enabled: gate the halted turn's calls exactly like a normal round (an
// approval-required call pauses instead of executing unguarded), execute
// them, then issue one more `toolChoice: none` request for a closing answer.
//
// gated, when non-nil, is the gate this response already went through (the
// initial response is gated before stop conditions are consulted); each
// response is gated at most once per run.
func (m *ModelResult) runFinalResponseTurn(req components.ResponsesRequest, resp components.OpenResponsesResult, calls []ParsedToolCall, turn int, gated *toolGate) error {
	if !m.hasExecutableToolCalls(calls) {
		// Nothing the engine can run: finalize on the halted response.
		m.markComplete()
		return nil
	}
	var gate toolGate
	if gated != nil {
		gate = *gated
	} else {
		g, stillPending, err := m.gateToolCalls(calls, turn, &req)
		if err != nil {
			return err
		}
		if len(stillPending) > 0 {
			return m.pauseForApproval(calls, stillPending, g, turn, &req, false)
		}
		gate = g
	}
	outputs, executed, err := m.executeToolCallsForTurn(calls, gate, turn, &req, true)
	if err != nil {
		return err
	}
	if isPausedStatus(m.state.Status) {
		return nil
	}
	// A doom-loop stop armed while executing the halted turn's calls: seal
	// and skip the final text-coercion request ("no further model requests
	// after stop").
	if m.doomStop != nil {
		m.haltForDoomLoop(resp, outputs)
		return nil
	}
	// Apply nextTurnParams from the executed tools so they affect the final
	// text-coercion request (mirrors the in-loop behavior).
	if err := m.applyNextTurnParams(executed, &req); err != nil {
		return err
	}
	// Pair any calls that produced no output (manual tools, unknown names)
	// with stub outputs so every function_call in the request has a match.
	// Stubs are NOT persisted to state — only real outputs are.
	answered := map[string]bool{}
	for _, out := range outputs {
		if out.FunctionCallOutputItem != nil {
			answered[out.FunctionCallOutputItem.CallID] = true
		}
	}
	requestOutputs := append([]components.InputsUnion1{}, outputs...)
	for _, call := range calls {
		if answered[call.CallID] {
			continue
		}
		requestOutputs = append(requestOutputs, components.CreateInputsUnion1FunctionCallOutputItem(components.FunctionCallOutputItem{CallID: call.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str("Tool execution skipped: step limit reached."), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}))
	}
	finalResp, err := m.sendFinalResponseRequest(req, resp, requestOutputs, turn+1)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.resp = finalResp
	m.steps = append(m.steps, StepResult{Text: ExtractTextFromResponse(finalResp), Response: finalResp, Usage: usagePtr(finalResp), FinishReason: string(finalResp.Status)})
	m.mu.Unlock()
	finalItems, err := responseInputItemsWithError(finalResp)
	if err != nil {
		return err
	}
	m.state = appendResponseItemsToState(m.state, finalResp, finalItems)
	m.markComplete()
	return nil
}

// hasExecutableToolCalls reports whether any call maps to a tool the engine
// can run itself (not manual, not unknown).
func (m *ModelResult) hasExecutableToolCalls(calls []ParsedToolCall) bool {
	for _, call := range calls {
		if t := FindToolByName(m.input.Tools, call.Name); t != nil && IsAutoResolvableTool(t) {
			return true
		}
	}
	return false
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
	var failed error
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
		switch {
		case data.StreamEventsResponseCompleted != nil:
			resp := data.StreamEventsResponseCompleted.Response
			completed = &resp
		case data.StreamEventsResponseIncomplete != nil:
			// An incomplete response (e.g. truncated at max_output_tokens)
			// is still a materialized response.
			resp := data.StreamEventsResponseIncomplete.Response
			completed = &resp
		case data.StreamEventsResponseFailed != nil:
			b, _ := json.Marshal(data.StreamEventsResponseFailed.Response.Error)
			failed = fmt.Errorf("response failed: %s", b)
		}
		// Stop at the terminal event (upstream streamTerminationEvents): a
		// provider that keeps the connection open after response.completed
		// must not stall the run waiting for the close frame.
		if completed != nil || failed != nil {
			break
		}
	}
	if err := stream.Err(); err != nil {
		return components.OpenResponsesResult{}, events, err
	}
	if failed != nil && completed == nil {
		return components.OpenResponsesResult{}, events, failed
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

// findPendingCall returns the index of the pending call matching id.
func findPendingCall(pending []ParsedToolCall, id string) int {
	for i, call := range pending {
		if id == callKey(call) || id == call.CallID || id == call.ID {
			return i
		}
	}
	return -1
}

func toolSource(t Tool) ToolSource {
	if IsMcpTool(t) {
		return ToolSourceMCP
	}
	return ToolSourceClient
}

// prepareResumeRequest processes approve/reject decisions against the
// persisted pending calls. Approvals run first, in the order supplied, then
// rejections (mirroring upstream processApprovalDecisions); their results
// join any unsent results recorded at the pause. Returns paused=true when
// the resume leaves the conversation paused (or has nothing to send).
func (m *ModelResult) prepareResumeRequest(req *components.ResponsesRequest) (bool, error) {
	unsent := append([]UnsentToolResult{}, m.state.UnsentToolResults...)
	pending := append([]ParsedToolCall{}, m.state.PendingToolCalls...)
	turn := len(m.steps)
	processed := map[int]bool{}
	hitlPaused := map[int]bool{}
	secondGatePaused := map[int]bool{}
	for _, id := range m.input.ApproveToolCalls {
		idx := findPendingCall(pending, id)
		if idx < 0 || processed[idx] {
			continue
		}
		processed[idx] = true
		call := pending[idx]
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			unsent = append(unsent, CreateRejectedResult(call, "Tool not found or not executable"))
			continue
		}
		source := toolSource(t)
		var prepared *preparedCall
		if call.PreToolUseApplied {
			// PreToolUse already ran before the pause and Arguments carry
			// its effective input: do not run it a second time.
			p := preparedCall{call: call}
			prepared = &p
		} else {
			// Legacy pending call (or one approved straight from the first
			// gate): prepare now; a PreToolUse mutation of the approved
			// input requires approval again (second gate).
			p, outcome, err := m.prepareAfterInitialApproval(call, BuildTurnContext(nil, turn, req))
			if err != nil {
				return false, err
			}
			if outcome == prepareOutcomePending {
				next := p.call
				next.PreToolUseApplied = true
				pending[idx] = next
				secondGatePaused[idx] = true
				continue
			}
			prepared = &p
		}
		if prepared.blocked {
			unsent = append(unsent, CreateRejectedResult(call, prepared.reason))
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Error: prepared.reason, Turn: turn})
			continue
		}
		execCtx := BuildToolExecuteContext(prepared.call, BuildTurnContext(nil, turn, req), m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary_result", CallID: call.CallID, Name: call.Name, Source: source, Event: v, Turn: turn})
		})
		effectiveCall, result, blocked, err := m.runToolWithHooks(t, prepared.call, execCtx, true)
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
			hitlPaused[idx] = true
			continue
		}
		if result.Error != nil {
			unsent = append(unsent, CreateRejectedResult(effectiveCall, result.Error.Error()))
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Error: result.Error.Error(), Turn: turn})
			continue
		}
		m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: source, Result: result.Output, Turn: turn})
		out, err := m.modelFacingOutput(t, effectiveCall, result.Output)
		if err != nil {
			return false, err
		}
		unsent = append(unsent, CreateUnsentResult(effectiveCall, out))
	}
	for _, id := range m.input.RejectToolCalls {
		idx := findPendingCall(pending, id)
		if idx < 0 || processed[idx] {
			continue
		}
		processed[idx] = true
		call := pending[idx]
		const reason = "Rejected by user"
		if call.PreToolUseApplied {
			m.emitPreparedFailure(call, reason)
		}
		unsent = append(unsent, CreateRejectedResult(call, reason))
	}
	remaining := []ParsedToolCall{}
	unresolvedApprovals := 0
	for i, call := range pending {
		if processed[i] && !hitlPaused[i] && !secondGatePaused[i] {
			continue
		}
		remaining = append(remaining, call)
		if !hitlPaused[i] {
			unresolvedApprovals++
		}
	}
	if len(remaining) > 0 {
		m.state.PendingToolCalls = remaining
		m.state.UnsentToolResults = unsent
		// Upstream processApprovalDecisions: any remaining call that is not
		// an approved HITL call that paused again still owes a decision, so
		// the conversation is awaiting_approval (an Input-only resume then
		// stays paused); otherwise only re-paused HITL calls remain.
		if unresolvedApprovals > 0 {
			m.state.Status = ConversationStatusAwaitingApproval
		} else {
			m.state.Status = ConversationStatusAwaitingHITL
		}
		return true, nil
	}
	outputs, err := UnsentResultsToAPIFormatWithError(unsent)
	if err != nil {
		return false, err
	}
	if len(outputs) == 0 {
		m.markComplete()
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

type prepareOutcome int

const (
	prepareOutcomeReady prepareOutcome = iota
	prepareOutcomeBlocked
	prepareOutcomePending
)

// prepareToolCallForApproval runs PreToolUse for call before it executes.
// Returns ok=false when there is nothing to prepare (no hooks, or arguments
// that are not valid JSON), in which case execution runs the hook as usual.
func (m *ModelResult) prepareToolCallForApproval(call ParsedToolCall) (preparedCall, bool, error) {
	if m.hooksManager == nil {
		return preparedCall{}, false, nil
	}
	if hasUnparsedArguments(call) {
		return preparedCall{}, false, nil
	}
	preResult, err := m.hooksManager.EmitPreToolUse(PreToolUsePayload{ToolName: call.Name, ToolInput: toolInputMap(call)}, m.hookEmitOptions(call.Name))
	if err != nil {
		return preparedCall{}, false, fmt.Errorf("PreToolUse hook: %w", err)
	}
	if preResult.Blocked {
		reason := preResult.BlockReason
		if reason == "" {
			reason = "Blocked by PreToolUse hook"
		}
		return preparedCall{blocked: true, reason: reason, call: call}, true, nil
	}
	effective := call
	if preResult.Mutated {
		effective.Arguments = preResult.FinalPayload.ToolInput
		effective.RawArgs = ""
	}
	return preparedCall{call: effective, mutated: preResult.Mutated}, true, nil
}

// prepareAfterInitialApproval runs the preparation phase for a call that
// passed the initial approval gate (auto-approved, allowed by a
// PermissionRequest hook, or approved by the user on resume). When
// PreToolUse mutated the input, the mutation must still be executable
// (schema-valid object) and, if an input-dependent approval source now
// requires approval for it, it goes through a second gate: PermissionRequest
// allow -> ready, deny -> blocked, otherwise pending.
func (m *ModelResult) prepareAfterInitialApproval(call ParsedToolCall, turn TurnContext) (preparedCall, prepareOutcome, error) {
	p, ok, err := m.prepareToolCallForApproval(call)
	if err != nil {
		return preparedCall{}, prepareOutcomeReady, err
	}
	if !ok {
		return preparedCall{call: call}, prepareOutcomeReady, nil
	}
	if p.blocked {
		return p, prepareOutcomeBlocked, nil
	}
	if !p.mutated {
		return p, prepareOutcomeReady, nil
	}
	t := FindToolByName(m.input.Tools, p.call.Name)
	if t != nil && IsClientTool(t) {
		if _, valid := normalizedToolCall(t, p.call); !valid {
			if _, isTyped := unwrapTool(t).(interface {
				normalizeArguments(json.RawMessage) (map[string]any, bool)
			}); isTyped {
				reason := fmt.Sprintf("PreToolUse produced invalid input for %q", p.call.Name)
				m.emitPreparedFailure(p.call, reason)
				return preparedCall{blocked: true, reason: reason, call: p.call}, prepareOutcomeBlocked, nil
			}
		}
	}
	requires, err := m.mutatedInputRequiresApproval(t, p.call, turn)
	if err != nil {
		return preparedCall{}, prepareOutcomeReady, err
	}
	if !requires {
		return p, prepareOutcomeReady, nil
	}
	decision, reason, err := m.emitPermissionRequest(p.call)
	if err != nil {
		return preparedCall{}, prepareOutcomeReady, err
	}
	switch decision {
	case PermissionDecisionAllow:
		return p, prepareOutcomeReady, nil
	case PermissionDecisionDeny:
		if reason == "" {
			reason = "Denied by PermissionRequest hook"
		}
		m.emitPreparedFailure(p.call, reason)
		return preparedCall{blocked: true, reason: reason, call: p.call}, prepareOutcomeBlocked, nil
	}
	return p, prepareOutcomePending, nil
}

// mutatedInputRequiresApproval re-checks only approval sources whose answer
// can depend on input: a call-level check, or a tool-level predicate.
func (m *ModelResult) mutatedInputRequiresApproval(t Tool, call ParsedToolCall, turn TurnContext) (bool, error) {
	if m.input.Approval != nil {
		return ToolRequiresApproval(m.ctx, t, call, turn, m.input.Approval)
	}
	if t == nil || !hasToolApprovalPredicate(t) {
		return false, nil
	}
	return ToolRequiresApproval(m.ctx, t, call, turn, nil)
}

// emitPreparedFailure reports a call that PreToolUse already prepared but
// that will not run (denied/blocked after preparation, or rejected on
// resume) as a PostToolUseFailure.
func (m *ModelResult) emitPreparedFailure(call ParsedToolCall, reason string) {
	if m.hooksManager == nil {
		return
	}
	if _, err := m.hooksManager.EmitPostToolUseFailure(PostToolUseFailurePayload{ToolName: call.Name, ToolInput: toolInputMap(call), Error: errors.New(reason)}, m.hookEmitOptions(call.Name)); err != nil {
		log.Printf("[PostToolUseFailure] hook error: %v", err)
	}
}

// gateToolCalls runs the approval gate for one response's calls:
//
//  1. Partition by RequiresApproval (predicates see normalized arguments).
//  2. PermissionRequest for each approval-required call: allow -> survivor,
//     deny -> synthesized rejection, ask_user (default) -> still pending.
//  3. PreToolUse preparation for every auto-resolvable survivor, so approval
//     decisions and persisted pending calls see the hook's effective input;
//     a mutation that now needs approval goes through a second gate.
//
// Returns the per-call decisions and the calls still awaiting a human.
func (m *ModelResult) gateToolCalls(calls []ParsedToolCall, turn int, req *components.ResponsesRequest) (toolGate, []ParsedToolCall, error) {
	gate := newToolGate()
	turnCtx := BuildTurnContext(nil, turn, req)
	if m.doomMonitor != nil {
		// The doom-loop checkpoint runs before approval partitioning
		// (upstream classifyInitialApprovalCalls): a blocked call is never
		// offered for approval and never reaches PermissionRequest/PreToolUse.
		blocked, err := m.doomLoopGate(calls, turn)
		if err != nil {
			return gate, nil, err
		}
		if len(blocked) > 0 {
			gate.doomBlocked = blocked
			unblocked := make([]ParsedToolCall, 0, len(calls))
			for _, call := range calls {
				if _, ok := blocked[callKey(call)]; !ok {
					unblocked = append(unblocked, call)
				}
			}
			calls = unblocked
		}
	}
	approved, needsApproval, err := PartitionToolCalls(m.ctx, m.input.Tools, calls, turnCtx, m.input.Approval)
	if err != nil {
		return gate, nil, err
	}
	var stillPending []ParsedToolCall
	survivors := append([]ParsedToolCall{}, approved...)
	for _, call := range needsApproval {
		if m.hooksManager == nil {
			stillPending = append(stillPending, call)
			continue
		}
		decision, reason, err := m.emitPermissionRequest(call)
		if err != nil {
			return gate, nil, err
		}
		switch decision {
		case PermissionDecisionAllow:
			survivors = append(survivors, call)
		case PermissionDecisionDeny:
			if reason == "" {
				reason = "Denied by PermissionRequest hook"
			}
			gate.denied[callKey(call)] = reason
		default:
			stillPending = append(stillPending, call)
		}
	}
	for _, call := range survivors {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			continue
		}
		p, outcome, err := m.prepareAfterInitialApproval(call, turnCtx)
		if err != nil {
			return gate, nil, err
		}
		if outcome == prepareOutcomePending {
			pendingCall := p.call
			pendingCall.PreToolUseApplied = true
			stillPending = append(stillPending, pendingCall)
			continue
		}
		gate.prepared[callKey(call)] = p
	}
	return gate, stillPending, nil
}

// gateAndMaybePause gates calls and, when any still need a human decision,
// pauses (see pauseForApproval). Returns whether it paused.
func (m *ModelResult) gateAndMaybePause(calls []ParsedToolCall, turn int, req *components.ResponsesRequest, skipAutoExecution bool) (bool, error) {
	gate, stillPending, err := m.gateToolCalls(calls, turn, req)
	if err != nil {
		return false, err
	}
	if len(stillPending) == 0 {
		return false, nil
	}
	return true, m.pauseForApproval(calls, stillPending, gate, turn, req, skipAutoExecution)
}

// pauseForApproval persists an awaiting_approval pause. The normal round will
// not run for this response, so the auto-approved calls execute now and
// their results are stored as unsent (replayed with the decisions on
// resume) — unless skipAutoExecution (a met stopWhen with
// AllowFinalResponse=false), in which case they are recorded as skipped.
// Hook-denied calls are recorded as rejections.
func (m *ModelResult) pauseForApproval(calls, stillPending []ParsedToolCall, gate toolGate, turn int, req *components.ResponsesRequest, skipAutoExecution bool) error {
	pendingKeys := map[string]bool{}
	for _, call := range stillPending {
		pendingKeys[callKey(call)] = true
	}
	var executableNow []ParsedToolCall
	var deniedResults []UnsentToolResult
	for _, call := range calls {
		key := callKey(call)
		if pendingKeys[key] {
			continue
		}
		if reason, ok := gate.denied[key]; ok {
			deniedResults = append(deniedResults, CreateRejectedResult(call, reason))
			continue
		}
		executableNow = append(executableNow, call)
	}
	var results []UnsentToolResult
	if skipAutoExecution {
		for _, call := range executableNow {
			results = append(results, CreateRejectedResult(call, "Tool execution skipped: stop condition met"))
		}
	} else {
		var err error
		results, err = m.executeAutoApproveTools(executableNow, gate, turn, req)
		if err != nil {
			return err
		}
	}
	m.state.UnsentToolResults = append(m.state.UnsentToolResults, append(results, deniedResults...)...)
	m.state.PendingToolCalls = stillPending
	m.state.Status = ConversationStatusAwaitingApproval
	return nil
}

// toolJob is one call the engine executes in a round.
type toolJob struct {
	t       Tool
	call    ParsedToolCall // the call as emitted (for events)
	runCall ParsedToolCall // the call to execute (PreToolUse-prepared input)
	skipPre bool
}

// toolJobOutcome is the settled result of a toolJob.
type toolJobOutcome struct {
	effective ParsedToolCall
	result    ToolExecutionResult
	blocked   bool
	err       error
}

// runToolJobs executes a round's calls concurrently (upstream runs a round
// with Promise.allSettled) and returns their outcomes in call order, so
// outputs, tool.result events and state updates stay in call order however
// the bodies interleave. Preliminary (generator) events stream live as each
// body yields them.
func (m *ModelResult) runToolJobs(jobs []toolJob, turn int, req *components.ResponsesRequest) []toolJobOutcome {
	outcomes := make([]toolJobOutcome, len(jobs))
	turnCtx := BuildTurnContext(nil, turn, req)
	var wg sync.WaitGroup
	for i, job := range jobs {
		source := toolSource(job.t)
		call := job.call
		execCtx := BuildToolExecuteContext(job.runCall, turnCtx, m.store, func(v any) {
			m.toolStream.Push(ToolStreamEvent{Type: "tool.preliminary_result", CallID: call.CallID, Name: call.Name, Source: source, Event: v, Turn: turn})
		})
		run := func(i int, job toolJob, execCtx ToolExecuteContext) {
			effective, result, blocked, err := m.runToolWithHooks(job.t, job.runCall, execCtx, job.skipPre)
			outcomes[i] = toolJobOutcome{effective: effective, result: result, blocked: blocked, err: err}
		}
		if len(jobs) == 1 {
			run(i, job, execCtx)
			continue
		}
		wg.Add(1)
		go func(i int, job toolJob, execCtx ToolExecuteContext) {
			defer wg.Done()
			run(i, job, execCtx)
		}(i, job, execCtx)
	}
	wg.Wait()
	return outcomes
}

// executeAutoApproveTools runs the auto-executable (non-approval-required)
// tool calls of a turn that also contains approval-required calls, returning
// their results as UnsentToolResult entries (success -> unsent with the
// model-facing output, error/blocked -> rejected), in call order.
//
// Mirrors upstream `executeAutoApproveTools`: invoked when a turn mixes
// auto-approved and approval-required calls, so the auto-approved outputs are
// produced and recorded before the loop pauses with `awaiting_approval`. The
// calls run concurrently, like upstream's Promise.allSettled. A HITL tool
// that pauses yields no unsent result this round (upstream's null).
func (m *ModelResult) executeAutoApproveTools(calls []ParsedToolCall, gate toolGate, turn int, req *components.ResponsesRequest) ([]UnsentToolResult, error) {
	// slot i holds either a pre-decided result or a job index.
	type slot struct {
		result *UnsentToolResult
		job    int
	}
	var slots []slot
	var jobs []toolJob
	for _, call := range calls {
		if reason, ok := gate.doomBlocked[callKey(call)]; ok {
			r := CreateRejectedResult(call, reason)
			slots = append(slots, slot{result: &r, job: -1})
			continue
		}
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			// Not executable without approval/manual output — skip (upstream
			// returns null and contributes no unsent result this round).
			continue
		}
		job := toolJob{t: t, call: call, runCall: call}
		if p, ok := gate.prepared[callKey(call)]; ok {
			if p.blocked {
				r := CreateRejectedResult(call, p.reason)
				slots = append(slots, slot{result: &r, job: -1})
				continue
			}
			job.runCall, job.skipPre = p.call, true
		}
		slots = append(slots, slot{job: len(jobs)})
		jobs = append(jobs, job)
	}
	outcomes := m.runToolJobs(jobs, turn, req)
	var results []UnsentToolResult
	for _, sl := range slots {
		if sl.job < 0 {
			results = append(results, *sl.result)
			continue
		}
		job, o := jobs[sl.job], outcomes[sl.job]
		if o.err != nil {
			return nil, o.err
		}
		m.toolRoundsExecuted++
		if o.blocked {
			results = append(results, CreateRejectedResult(o.effective, o.result.Error.Error()))
			continue
		}
		if errors.Is(o.result.Error, ErrHITLPause) {
			// HITL tool paused during auto-approve — no unsent result this round.
			continue
		}
		if o.result.Error != nil {
			results = append(results, CreateRejectedResult(o.effective, o.result.Error.Error()))
			continue
		}
		m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: job.call.CallID, Name: job.call.Name, Source: toolSource(job.t), Result: o.result.Output, Turn: turn})
		out, err := m.modelFacingOutput(job.t, o.effective, o.result.Output)
		if err != nil {
			return nil, err
		}
		results = append(results, CreateUnsentResult(o.effective, out))
	}
	return results, nil
}

// modelFacingOutput applies the tool's ToModelOutput mapper (if any) to a
// successful output, returning the value to send to the model.
func (m *ModelResult) modelFacingOutput(t Tool, call ParsedToolCall, output any) (any, error) {
	mapper, ok := unwrapTool(t).(interface {
		modelOutput(context.Context, ParsedToolCall, any) (any, bool, error)
	})
	if !ok {
		return output, nil
	}
	mapped, applied, err := mapper.modelOutput(m.ctx, call, output)
	if err != nil {
		return nil, err
	}
	if !applied {
		return output, nil
	}
	return mapped, nil
}

// executedToolCall is a call whose tool body ran and returned without error,
// with the input it ran on and its result. Only these drive nextTurnParams.
type executedToolCall struct {
	call   ParsedToolCall
	result ToolExecutionResult
}

// executeToolCallsForTurn runs a round of tool calls. Executable calls run
// concurrently (upstream executeToolRound uses Promise.allSettled); their
// outcomes are then processed in call order.
//
// Mirrors upstream `executeToolRound` + `saveToolResultsToState` +
// `persistHitlPause`: every call in the round runs, even when a HITL call in
// it pauses; each produced `function_call_output` is persisted to
// `m.state.Messages` in call order, so the regular-tool outputs survive the
// pause and are replayed on resume, and every paused HITL call becomes a
// pending call (status awaiting_hitl) — no follow-up request is sent with an
// incomplete output set. Hook-denied, doom-loop-blocked and
// PreToolUse-blocked calls get rejected outputs in their call position.
// Unresolved manual/unknown calls pause with awaiting_client_tools when no
// HITL call paused.
//
// finalTurn marks the AllowFinalResponse path, where manual/unknown calls do
// not pause the run (the caller pairs them with stub outputs instead).
func (m *ModelResult) executeToolCallsForTurn(calls []ParsedToolCall, gate toolGate, turn int, req *components.ResponsesRequest, finalTurn bool) ([]components.InputsUnion1, []executedToolCall, error) {
	const (
		slotRejected = iota
		slotManual
		slotJob
	)
	type slot struct {
		kind   int
		call   ParsedToolCall
		source ToolSource
		reason string
		job    int
	}
	var slots []slot
	var jobs []toolJob
	for _, call := range calls {
		key := callKey(call)
		t := FindToolByName(m.input.Tools, call.Name)
		reason, ok := gate.denied[key]
		if !ok {
			// Doom-loop blocked: refused before execution with the verdict's
			// explanation as the tool error.
			reason, ok = gate.doomBlocked[key]
		}
		if ok {
			source := ToolSourceClient
			if t != nil {
				source = toolSource(t)
			}
			slots = append(slots, slot{kind: slotRejected, call: call, source: source, reason: reason})
			continue
		}
		if t == nil || IsManualTool(t) {
			if !finalTurn {
				slots = append(slots, slot{kind: slotManual, call: call})
			}
			continue
		}
		job := toolJob{t: t, call: call, runCall: call}
		if p, ok := gate.prepared[key]; ok {
			if p.blocked {
				slots = append(slots, slot{kind: slotRejected, call: call, source: toolSource(t), reason: p.reason})
				continue
			}
			job.runCall, job.skipPre = p.call, true
		}
		slots = append(slots, slot{kind: slotJob, call: call, source: toolSource(t), job: len(jobs)})
		jobs = append(jobs, job)
	}
	outcomes := m.runToolJobs(jobs, turn, req)

	var outputs []components.InputsUnion1
	var executed []executedToolCall
	var hitlPaused, manualPending []ParsedToolCall
	appendOutput := func(item components.FunctionCallOutputItem) {
		outputItem := components.CreateInputsUnion1FunctionCallOutputItem(item)
		outputs = append(outputs, outputItem)
		// Persist each output to state so it survives a HITL pause in the
		// same round (upstream saveToolResultsToState).
		m.state = AppendToMessages(m.state, outputItem)
	}
	for _, sl := range slots {
		call := sl.call
		switch sl.kind {
		case slotRejected:
			out, err := FormatToolOutputWithError(ToolExecutionResult{CallID: call.CallID, Name: call.Name, Error: errors.New(sl.reason)})
			if err != nil {
				return nil, nil, err
			}
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: sl.source, Error: sl.reason, Turn: turn})
			m.toolStream.Push(ToolStreamEvent{Type: "tool.call_output", CallID: call.CallID, Name: call.Name, Source: sl.source, Error: sl.reason, Turn: turn})
			appendOutput(out)
			continue
		case slotManual:
			// Unresolved manual (client-executed) tool call: neither an
			// `execute` fn nor an `OnToolCalled` hook (upstream #64).
			manualPending = append(manualPending, call)
			continue
		}
		job, o := jobs[sl.job], outcomes[sl.job]
		if o.err != nil {
			return nil, nil, o.err
		}
		m.toolRoundsExecuted++
		result := o.result
		if o.blocked {
			out, ferr := FormatToolOutputWithError(result)
			if ferr != nil {
				return nil, nil, ferr
			}
			m.toolStream.Push(ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: sl.source, Error: result.Error.Error(), Turn: turn})
			m.toolStream.Push(ToolStreamEvent{Type: "tool.call_output", CallID: call.CallID, Name: call.Name, Source: sl.source, Error: result.Error.Error(), Turn: turn})
			appendOutput(out)
			continue
		}
		if errors.Is(result.Error, ErrHITLPause) {
			hitlPaused = append(hitlPaused, o.effective)
			continue
		}
		event := ToolStreamEvent{Type: "tool.result", CallID: call.CallID, Name: call.Name, Source: sl.source, Result: result.Output, Turn: turn}
		if result.Error != nil {
			event.Error = result.Error.Error()
		}
		m.toolStream.Push(event)
		formatted := result
		if result.Error == nil {
			executed = append(executed, executedToolCall{call: o.effective, result: result})
			modelOut, err := m.modelFacingOutput(job.t, o.effective, result.Output)
			if err != nil {
				return nil, nil, err
			}
			formatted.Output = modelOut
		}
		out, err := FormatToolOutputWithError(formatted)
		if err != nil {
			return nil, nil, err
		}
		m.toolStream.Push(ToolStreamEvent{Type: "tool.call_output", CallID: call.CallID, Name: call.Name, Source: sl.source, Output: result.Output, Error: event.Error, Turn: turn})
		appendOutput(out)
	}
	switch {
	case len(hitlPaused) > 0:
		// Upstream persistHitlPause: the paused HITL calls are what the
		// caller must resolve; the round's real outputs are already saved.
		// Replace (not append): a pending list carried over from an earlier
		// pause that was resumed via Input is stale.
		m.state.PendingToolCalls = hitlPaused
		m.state.Status = ConversationStatusAwaitingHITL
	case len(manualPending) > 0:
		// Persist as awaiting_client_tools rather than the generic
		// `interrupted`, so callers can tell "execute this yourself" apart
		// from an arbitrary interruption.
		m.state.PendingToolCalls = manualPending
		m.state.Status = ConversationStatusAwaitingClientTools
	}
	return outputs, executed, nil
}

// applyNextTurnParams runs the nextTurnParams functions of tools whose calls
// succeeded this round (tools-list order, then call order), composing their
// values onto req. A tool-computed toolChoice becomes the new caller-level
// tool-choice policy, not a one-turn override.
func (m *ModelResult) applyNextTurnParams(executed []executedToolCall, req *components.ResponsesRequest) error {
	if len(executed) == 0 {
		return nil
	}
	working := *req
	changedToolChoice := false
	for _, t := range m.input.Tools {
		ntp, ok := unwrapTool(t).(interface {
			nextTurnParams() NextTurnParamsFunctions
		})
		if !ok || IsServerTool(t) {
			continue
		}
		funcs := ntp.nextTurnParams()
		if len(funcs) == 0 {
			continue
		}
		for _, ex := range executed {
			if ex.call.Name != t.ToolName() {
				continue
			}
			if ex.call.Arguments != nil {
				if _, isObj := ex.call.Arguments.(map[string]any); !isObj {
					return fmt.Errorf("tool call arguments for %s must be an object, got %T", t.ToolName(), ex.call.Arguments)
				}
			}
			computed, err := ExecuteNextTurnParamsFunctions(m.ctx, funcs, BuildNextTurnParamsContext(ex.call, ex.result, working))
			if err != nil {
				return err
			}
			if _, ok := computed["toolChoice"]; ok {
				changedToolChoice = true
			}
			working = ApplyNextTurnParamsToRequest(working, computed)
		}
	}
	*req = working
	if changedToolChoice {
		m.configuredToolChoice = req.ToolChoice
	}
	return nil
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

// toolChoiceJSON decodes a tool choice to its generic JSON form.
func toolChoiceJSON(choice *components.OpenAIResponsesToolChoiceUnion) (any, bool) {
	if choice == nil {
		return nil, false
	}
	b, err := json.Marshal(choice)
	if err != nil {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false
	}
	return v, true
}

// forcedToolChoiceKey returns a stable identity for a tool choice that forces
// a tool call (`required`, a specific tool, allowed_tools in required mode),
// or "" when the choice does not force one.
func forcedToolChoiceKey(choice *components.OpenAIResponsesToolChoiceUnion) string {
	v, ok := toolChoiceJSON(choice)
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		if x != "required" {
			return ""
		}
	case map[string]any:
		if x["type"] == "allowed_tools" && x["mode"] != "required" {
			return ""
		}
	default:
		return ""
	}
	// encoding/json sorts map keys, so this is canonical.
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// relaxForcedToolChoice relaxes a forcing tool choice for follow-up turns:
// `required` and a specific-tool object become `auto`; allowed_tools in
// required mode keeps its tool set with mode `auto` (which tools may be used
// still applies; only the force-a-call part is spent). Anything else passes
// through unchanged.
func relaxForcedToolChoice(choice *components.OpenAIResponsesToolChoiceUnion) *components.OpenAIResponsesToolChoiceUnion {
	v, ok := toolChoiceJSON(choice)
	if !ok {
		return choice
	}
	auto := components.CreateOpenAIResponsesToolChoiceUnionOpenAIResponsesToolChoiceAuto(components.OpenAIResponsesToolChoiceAutoAuto)
	switch x := v.(type) {
	case string:
		if x == "required" {
			return &auto
		}
		return choice
	case map[string]any:
		if x["type"] != "allowed_tools" {
			return &auto
		}
		if x["mode"] != "required" {
			return choice
		}
		x["mode"] = "auto"
		relaxed, err := toolChoiceFromJSONValue(x)
		if err != nil {
			return choice
		}
		return relaxed
	}
	return choice
}

// applyToolChoicePolicy sets the wire tool choice for a caller-policy turn.
// A forced choice that already produced a tool call in this logical run
// (DEV-785) is relaxed so the model can answer in text instead of looping
// through tool calls until the step budget runs out. Returns the configured
// choice's forced key for commitToolChoice.
func (m *ModelResult) applyToolChoicePolicy(req *components.ResponsesRequest) string {
	key := forcedToolChoiceKey(m.configuredToolChoice)
	if key != "" && key == m.consumedForcedKey {
		req.ToolChoice = relaxForcedToolChoice(m.configuredToolChoice)
	} else {
		req.ToolChoice = m.configuredToolChoice
	}
	return key
}

// commitToolChoice records the outcome of a caller-policy dispatch: an
// unforced choice clears any consumed key (re-arming the forced value if it
// appears again); a forced choice is consumed only when the response called
// a tool.
func (m *ModelResult) commitToolChoice(forcedKey string, hasToolCalls bool) {
	if forcedKey == "" {
		m.consumedForcedKey = ""
		return
	}
	if hasToolCalls {
		m.consumedForcedKey = forcedKey
	}
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
	finalResp, events, err := m.sendTurn(finalReq, turn, ModelCallTurnTypeFinal, turn, false)
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
	retryResp, events, err := m.sendTurn(retryReq, turn, ModelCallTurnTypeRetry, turn, false)
	if err != nil {
		return components.OpenResponsesResult{}, err
	}
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

// snapshotSessionUsage materializes the running session aggregate. Cost is
// present only when at least one response reported it (a summed 0 from
// cost-less responses would read as "free" rather than "unknown"). Single
// source of truth for SessionEnd.TotalUsage and ModelResult.Usage.
func (m *ModelResult) snapshotSessionUsage() SessionUsageTotals {
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
	return totals
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
		totals := m.snapshotSessionUsage()
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

// emitPostModelCall folds a completed model response's usage into the
// session aggregate and emits PostModelCall. Accumulation runs
// unconditionally — Usage() reports these totals to callers who configured
// no hooks at all; only the hook emit is gated on a HooksManager.
func (m *ModelResult) emitPostModelCall(resp components.OpenResponsesResult, startedAt time.Time, turnType ModelCallTurnType, turnNumber int) {
	usage := extractModelCallUsage(resp)
	m.mu.Lock()
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
	m.mu.Unlock()
	if m.hooksManager == nil {
		return
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
	if hasUnparsedArguments(call) {
		return PermissionDecisionAskUser, "", nil
	}

	// Derive risk level from the approval gate's shape (never by invoking a
	// predicate): a callback (tool- or call-level approval function) => high
	// (caller actively decides per call), blanket true => medium, else low.
	t := FindToolByName(m.input.Tools, call.Name)
	riskLevel := RiskLevelLow
	if m.input.Approval != nil {
		riskLevel = RiskLevelHigh
	} else if t != nil && hasToolApprovalPredicate(t) {
		riskLevel = RiskLevelHigh
	} else if t != nil && hasBlanketApproval(t) {
		riskLevel = RiskLevelMedium
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
//
// skipPreToolUse is set for a call the approval preparation phase already
// ran PreToolUse for (its arguments are the hook's effective input).
func (m *ModelResult) runToolWithHooks(t Tool, call ParsedToolCall, execCtx ToolExecuteContext, skipPreToolUse bool) (effectiveCall ParsedToolCall, result ToolExecutionResult, blocked bool, fatalErr error) {
	effectiveCall = call
	// Unparseable arguments never enter the hook lifecycle: execution
	// reports the parse error directly.
	if m.hooksManager == nil || hasUnparsedArguments(call) {
		res, err := m.executeWithDeadline(t, effectiveCall, execCtx)
		if err != nil {
			return effectiveCall, ToolExecutionResult{}, false, err
		}
		return effectiveCall, res, false, nil
	}

	if !skipPreToolUse {
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
	}

	startedAt := time.Now()
	execCtx.ToolCall = effectiveCall
	res, err := m.executeWithDeadline(t, effectiveCall, execCtx)
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

// ToolTimeoutError is the error a tool call reports when it outlives its
// deadline (ToolConfig.Timeout, or CallModelInput.ToolTimeout as the run
// default). The model receives it as an error output and the loop
// continues; the abandoned body's context is cancelled so cooperative tools
// stop, but its goroutine is not waited for. Code is "tool_timeout".
type ToolTimeoutError struct {
	ToolName string
	Timeout  time.Duration
}

func (e *ToolTimeoutError) Error() string {
	return fmt.Sprintf("Tool %q timed out after %dms", e.ToolName, e.Timeout.Milliseconds())
}

// Code returns the upstream error code, "tool_timeout".
func (e *ToolTimeoutError) Code() string { return "tool_timeout" }

// toolTimeout resolves the deadline for t: the tool's own Timeout wins,
// otherwise the run-level CallModelInput.ToolTimeout.
func (m *ModelResult) toolTimeout(t Tool) time.Duration {
	if tt, ok := unwrapTool(t).(interface{ timeout() time.Duration }); ok {
		if d := tt.timeout(); d > 0 {
			return d
		}
	}
	return m.input.ToolTimeout
}

// executeWithDeadline runs the tool body under the run's context, racing it
// against the tool's deadline when one is configured. The body sees a
// context that is cancelled when the run is cancelled (ModelResult.Cancel)
// or the deadline fires.
func (m *ModelResult) executeWithDeadline(t Tool, call ParsedToolCall, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	execCtx.CallID = call.CallID
	execCtx.ConversationID = m.state.ID
	timeout := m.toolTimeout(t)
	if timeout <= 0 {
		return ExecuteTool(m.ctx, t, call, execCtx)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	type outcome struct {
		res ToolExecutionResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := ExecuteTool(ctx, t, call, execCtx)
		done <- outcome{res, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case o := <-done:
		cancel()
		return o.res, o.err
	case <-timer.C:
		cancel()
		return ToolExecutionResult{CallID: call.CallID, Name: call.Name, Source: toolSource(t), Error: &ToolTimeoutError{ToolName: call.Name, Timeout: timeout}}, nil
	}
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

// Text returns the final response text. A call that left the conversation
// paused without producing a response returns a *ConversationPausedError.
func (m *ModelResult) Text(ctx context.Context) (string, error) {
	m.ensure()
	if err := m.getterErr(); err != nil {
		return ExtractTextFromResponse(m.resp), err
	}
	return ExtractTextFromResponse(m.resp), nil
}

// Response returns the final model response. In a multi-round tool loop this
// is the final round's response only — its usage covers that generation, not
// the run; use Usage for aggregate totals.
func (m *ModelResult) Response(ctx context.Context, opts ...GetResponseOptions) (components.OpenResponsesResult, error) {
	m.ensure()
	return m.resp, m.getterErr()
}

// Usage returns aggregate token/cost usage across every model call this run
// made: the initial request, each tool-round follow-up, the empty-final
// retry, the AllowFinalResponse final turn, and approval-resume requests.
// Same shape and numbers as the SessionEnd hook's TotalUsage, available
// without hooks. It waits for the run like Response does but never returns
// an error: a failed or paused run still reports the totals accrued so far
// (ModelCalls 0 with zeroed tokens when no call completed). Cost is set only
// when the server reported cost for at least one call.
func (m *ModelResult) Usage(ctx context.Context) SessionUsageTotals {
	m.ensure()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotSessionUsage()
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
	return append([]ParsedToolCall{}, m.toolCalls...), m.getterErr()
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
	err := m.getterErr()
	return ch, func() error { return err }
}
func (m *ModelResult) ContextUpdates(ctx context.Context) (<-chan map[string]map[string]any, func() error) {
	ch := make(chan map[string]map[string]any, 1)
	if m.store != nil {
		ch <- m.store.Snapshot()
	}
	close(ch)
	return ch, func() error { return nil }
}

// RequiresApproval reports whether the conversation is paused waiting on
// the caller: approval (awaiting_approval), a HITL resume (awaiting_hitl),
// or client-executed manual tools (awaiting_client_tools) — or whenever
// pending tool calls are populated. Read State().Status to branch on the
// pause kind.
func (m *ModelResult) RequiresApproval(ctx context.Context) (bool, error) {
	m.ensure()
	switch m.state.Status {
	case ConversationStatusAwaitingApproval, ConversationStatusAwaitingHITL, ConversationStatusAwaitingClientTools:
		return true, m.err
	}
	return len(m.state.PendingToolCalls) > 0, m.err
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
		b = flattenMessageContentForUnion(b)
		var input components.InputsUnion1
		if err := json.Unmarshal(b, &input); err != nil {
			return nil, fmt.Errorf("convert response output item to input item: %w", err)
		}
		items = append(items, input)
	}
	return items, nil
}

// flattenMessageContentForUnion rewrites a marshaled `message` item whose
// content is an array of output_text blocks into the equivalent
// string-content form. go-sdk v0.5.4's InputsUnion1 unmarshaller panics
// (reflect on slice value) on array-content assistant messages — the exact
// shape every live response echoes back on follow-up turns — but accepts the
// same message with string content. Text is preserved; annotations are not
// (they are response-side metadata the next request does not need). Remove
// once the pinned go-sdk's union decoding handles array content.
func flattenMessageContentForUnion(b []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return b
	}
	if raw["type"] != "message" {
		return b
	}
	blocks, ok := raw["content"].([]any)
	if !ok {
		return b
	}
	var text strings.Builder
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			return b // unknown block shape: leave untouched
		}
		if m["type"] != "output_text" {
			return b // non-text content: leave untouched
		}
		if s, ok := m["text"].(string); ok {
			text.WriteString(s)
		}
	}
	raw["content"] = text.String()
	flattened, err := json.Marshal(raw)
	if err != nil {
		return b
	}
	return flattened
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

//#region Doom-loop integration
//
// Port of the engine side of upstream model-result.ts's doom-loop handling
// (beginDoomLoopRound, enqueueDoomLoopEvaluation,
// checkDoomLoopBeforeExecution, applyDoomLoopVerdict,
// applyDoomLoopSideEffects, takeDoomLoopEscalationOverrides,
// checkDoomLoopForResponse, queueDoomLoopSteer, flushDoomLoopSteer,
// sealDoomLoopStop, getDoomLoopVerdict and the initStream restore). The
// detection itself lives in doom_loop.go. Every method is a no-op when
// CallModelInput.DoomLoop is nil. All of this runs on the run goroutine, so
// upstream's promise chain serializing evaluations is unnecessary: calls are
// evaluated synchronously in model-emission order.

// doomDecision is the final decision for one (tool, fingerprint) in the
// current round. action "" means proceed.
type doomDecision struct {
	action  DoomLoopAction
	message string
}

// doomKeyEntry is a loop-key resolution cached by call key for the current
// round, so a tool's LoopKey (user code) runs at most once per call.
type doomKeyEntry struct {
	resolution LoopKeyResolution
	// usedArgs: the key material IS the call's arguments (no loop key, or a
	// fallback), so there is no different identity to fall back to.
	usedArgs bool
	// duplicate poisons a call id seen twice in one batch: both calls fall
	// through to per-call resolution at the checkpoint (upstream
	// DUPLICATE_CALL_ID).
	duplicate bool
}

// initDoomLoop builds the monitor when detection is configured and
// rehydrates persisted detector state: streaks always, queued steer
// guidance, and the stop verdict under the condemnation rule — a
// doom-stopped conversation stays stopped across decision-only resumes
// (ApproveToolCalls/RejectToolCalls), while a fresh conversational turn
// clears it (operator input is new information). Streaks are kept either
// way, so renewed repetition re-condemns quickly.
func (m *ModelResult) initDoomLoop(hasDecisions bool) {
	cfg := ResolveDoomLoopOption(m.input.DoomLoop)
	if cfg == nil {
		return
	}
	m.doomMonitor = NewDoomLoopMonitor(cfg, nil)
	m.doomEscalation = cfg.Escalation
	m.doomGatedTurn = -1
	// Upstream initializes the round-decision map eagerly: the step-level
	// server-tool checkpoint can record (and write a decision) on the very
	// first response, before any tool round has begun.
	m.doomRoundDecisions = map[string]doomDecision{}
	persisted := m.state.DoomLoop
	if persisted == nil {
		return
	}
	m.doomMonitor.Restore(*persisted)
	for _, msg := range persisted.PendingSteer {
		m.queueDoomLoopSteer(msg)
	}
	if persisted.StopVerdict != nil && hasDecisions {
		v := *persisted.StopVerdict
		m.doomStop = &v
	}
}

// snapshotDoomLoopState piggybacks the detector snapshot (plus the
// engine-owned stop verdict and pending steer) on every persist. Untouched
// when detection is off, so a stale blob from an earlier run stays inert.
func (m *ModelResult) snapshotDoomLoopState() {
	if m.doomMonitor == nil {
		return
	}
	snap := m.doomMonitor.GetState()
	if m.doomStop != nil {
		v := *m.doomStop
		snap.StopVerdict = &v
	}
	m.steerMu.Lock()
	if len(m.doomPendingSteer) > 0 {
		snap.PendingSteer = append([]string(nil), m.doomPendingSteer...)
	}
	m.steerMu.Unlock()
	m.state.DoomLoop = &snap
}

// DoomLoopVerdict returns the doom-loop verdict that stopped this run, or
// nil when the run was not stopped by doom-loop detection (including when
// detection is off). Waits for the run to settle. For per-event observation
// use the DoomLoopDetected hook. Upstream: getDoomLoopVerdict.
func (m *ModelResult) DoomLoopVerdict(ctx context.Context) *DoomLoopVerdict {
	m.ensure()
	if m.doomStop == nil {
		return nil
	}
	v := *m.doomStop
	return &v
}

// doomCallArguments returns a call's arguments as an object (nil/absent
// arguments coerce to {}), or nil plus the raw value for non-object
// arguments.
func doomCallArguments(call ParsedToolCall) (map[string]any, any) {
	switch a := call.Arguments.(type) {
	case map[string]any:
		return a, a
	case nil:
		empty := map[string]any{}
		return empty, empty
	}
	return nil, call.Arguments
}

// resolveDoomLoopKey resolves t's loop key against one call. Non-object
// arguments cannot be read by a field-based key, so their value is the
// identity.
func resolveDoomLoopKey(t Tool, call ParsedToolCall) doomKeyEntry {
	args, raw := doomCallArguments(call)
	if args == nil {
		return doomKeyEntry{resolution: LoopKeyResolution{Kind: LoopKeyKey, KeyMaterial: raw}, usedArgs: true}
	}
	loopKey := toolLoopKey(t)
	res := ResolveLoopKeyMaterial(loopKey, args)
	return doomKeyEntry{resolution: res, usedArgs: loopKey == nil || res.Kind == LoopKeyFallback}
}

// doomLoopGate runs the doom-loop checkpoint for one response's calls
// exactly once (memoized per turn): declare the round, then check each
// auto-resolvable call in emission order. Returns call key -> verdict
// message for blocked calls (block or stop).
func (m *ModelResult) doomLoopGate(calls []ParsedToolCall, turn int) (map[string]string, error) {
	if m.doomMonitor == nil {
		return nil, nil
	}
	if m.doomGatedTurn == turn {
		return m.doomGateBlocked, nil
	}
	m.doomGatedTurn = turn
	m.doomGateBlocked = nil
	m.beginDoomLoopRound(calls)
	blocked := map[string]string{}
	for _, call := range calls {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			continue
		}
		isBlocked, reason, err := m.checkDoomLoopBeforeExecution(t, call)
		if err != nil {
			return nil, err
		}
		if isBlocked {
			blocked[callKey(call)] = reason
		}
	}
	m.doomGateBlocked = blocked
	return blocked, nil
}

// beginDoomLoopRound advances the round counter and declares the round's
// complete call set before any call is scored (a round's identity for one
// tool is the SET of fingerprints it was called with). Only calls the round
// will actually check are declared — manual/unknown tools are never
// recorded, so they are never declared and their LoopKey never runs — and
// exempt calls are left out. Every declared member is recorded by the
// checkpoint that follows.
func (m *ModelResult) beginDoomLoopRound(calls []ParsedToolCall) {
	m.doomRound++
	m.doomRoundDecisions = map[string]doomDecision{}
	m.doomRoundKeys = map[string]doomKeyEntry{}
	var declared []DoomLoopRoundCall
	for _, call := range calls {
		t := FindToolByName(m.input.Tools, call.Name)
		if t == nil || !IsAutoResolvableTool(t) {
			continue
		}
		entry := resolveDoomLoopKey(t, call)
		key := callKey(call)
		if _, seen := m.doomRoundKeys[key]; seen {
			m.doomRoundKeys[key] = doomKeyEntry{duplicate: true}
		} else {
			m.doomRoundKeys[key] = entry
		}
		if entry.resolution.Kind == LoopKeyExempt {
			continue
		}
		declared = append(declared, DoomLoopRoundCall{ToolName: call.Name, KeyMaterial: entry.resolution.KeyMaterial})
	}
	m.doomMonitor.DeclareRound(m.doomRound, declared)
}

// checkDoomLoopBeforeExecution is the per-call checkpoint, run before
// PermissionRequest/PreToolUse/execution. block refuses the call with the
// verdict's message as the tool error; stop arms the run stop AND blocks
// the call (executing a call the detector just condemned would be
// incoherent). observe/steer/escalate let it run.
func (m *ModelResult) checkDoomLoopBeforeExecution(t Tool, call ParsedToolCall) (bool, string, error) {
	entry, ok := m.doomRoundKeys[callKey(call)]
	if !ok || entry.duplicate {
		entry = resolveDoomLoopKey(t, call)
	}
	switch entry.resolution.Kind {
	case LoopKeyExempt:
		return false, "", nil
	case LoopKeyFallback:
		doomLoopWarnf("[DoomLoop] tool %q: %s", call.Name, entry.resolution.Warning)
	}
	var fallback any
	if !entry.usedArgs {
		_, fallback = doomCallArguments(call)
	}
	decision, err := m.evaluateDoomLoopCall(call.Name, entry.resolution.KeyMaterial, fallback, !entry.usedArgs, true, DoomLoopDetectorToolFingerprint, &call)
	if err != nil {
		return false, "", err
	}
	if decision.action == DoomLoopActionBlock || decision.action == DoomLoopActionStop {
		reason := decision.message
		if reason == "" {
			reason = fmt.Sprintf("Doom loop suspected: tool %q is repeating identical calls. Repeating the call will not change the result. Take a different approach.", call.Name)
		}
		return true, reason, nil
	}
	return false, "", nil
}

// evaluateDoomLoopCall records one call against the monitor and resolves
// the final decision (upstream enqueueDoomLoopEvaluation). Unhashable key
// material degrades to fallback; if that is also unhashable (or there is
// none) detection is skipped for the call — detection never fails a call
// except through its ladder actions. A duplicate of an already-decided
// (tool, fingerprint) in this round reuses that decision without a second
// streak increment or hook emission. Side effects (steer queue, escalation,
// stop) are applied here.
func (m *ModelResult) evaluateDoomLoopCall(toolName string, keyMaterial, fallback any, hasFallback, allowBlock bool, detector DoomLoopDetectorKind, call *ParsedToolCall) (doomDecision, error) {
	opts := &RecordToolCallOptions{DisallowBlock: !allowBlock, Detector: detector}
	record, err := m.doomMonitor.RecordToolCall(toolName, keyMaterial, m.doomRound, opts)
	if err != nil {
		if !hasFallback {
			doomLoopWarnf("[DoomLoop] could not fingerprint call to %q; skipping detection for this call: %v", toolName, err)
			return doomDecision{}, nil
		}
		doomLoopWarnf("[DoomLoop] could not fingerprint call to %q; falling back to full arguments: %v", toolName, err)
		record, err = m.doomMonitor.RecordToolCall(toolName, fallback, m.doomRound, opts)
		if err != nil {
			doomLoopWarnf("[DoomLoop] fallback identity for %q also unhashable; skipping detection for this call: %v", toolName, err)
			return doomDecision{}, nil
		}
	}
	decisionKey := toolName + "\n" + record.Fingerprint
	if record.DuplicateInRound {
		return m.doomRoundDecisions[decisionKey], nil
	}
	if record.Verdict == nil {
		m.doomRoundDecisions[decisionKey] = doomDecision{}
		return doomDecision{}, nil
	}
	action, err := m.applyDoomLoopVerdict(*record.Verdict, call)
	if err != nil {
		return doomDecision{}, err
	}
	decision := doomDecision{action: action, message: record.Verdict.Message}
	m.doomRoundDecisions[decisionKey] = decision
	m.applyDoomLoopSideEffects(action, *record.Verdict)
	return decision, nil
}

// applyDoomLoopVerdict emits DoomLoopDetected and resolves the final action.
// The last valid OverrideAction wins (PermissionRequest's last-wins
// convention). Non-tool verdicts (text, server tools — already emitted /
// already ran) downgrade a block to observe; an escalate without a
// configured mechanism or remaining budget downgrades to observe (never
// silently to something stronger). A hook error (ThrowOnHandlerError)
// fails the run, like the other lifecycle hooks.
func (m *ModelResult) applyDoomLoopVerdict(verdict DoomLoopVerdict, call *ParsedToolCall) (DoomLoopAction, error) {
	action := verdict.Action
	if m.hooksManager != nil {
		payload := DoomLoopDetectedPayload{
			Detector:    verdict.Detector,
			Action:      verdict.Action,
			Streak:      verdict.Streak,
			Fingerprint: verdict.Fingerprint,
			ToolName:    verdict.ToolName,
			Message:     verdict.Message,
		}
		if call != nil {
			if args, ok := call.Arguments.(map[string]any); ok {
				payload.ToolInput = args
			}
		}
		emit, err := m.hooksManager.EmitDoomLoopDetected(payload, m.hookEmitOptions(verdict.ToolName))
		if err != nil {
			return action, fmt.Errorf("DoomLoopDetected hook: %w", err)
		}
		for _, r := range emit.Results {
			if _, valid := actionStrength[r.OverrideAction]; valid {
				action = r.OverrideAction
			}
		}
	}
	if verdict.Detector != DoomLoopDetectorToolFingerprint && action == DoomLoopActionBlock {
		action = DoomLoopActionObserve
	}
	if action == DoomLoopActionEscalate && !m.doomMonitor.CanEscalate() {
		action = DoomLoopActionObserve
	}
	return action, nil
}

// applyDoomLoopSideEffects applies the shared consequences of a resolved
// action: queue steer guidance, latch the first escalation for the next
// request (with a steer notice so the model knows why its next turn looks
// different), or arm the stop. block has no shared side effect — the
// calling checkpoint synthesizes the blocked output itself.
func (m *ModelResult) applyDoomLoopSideEffects(action DoomLoopAction, verdict DoomLoopVerdict) {
	switch action {
	case DoomLoopActionSteer:
		m.queueDoomLoopSteer(verdict.Message)
	case DoomLoopActionEscalate:
		if m.doomPendingEscalation == nil {
			v := verdict
			m.doomPendingEscalation = &v
			m.queueDoomLoopSteer(verdict.Message + " An escalated turn follows: use the additional guidance to change course.")
		}
	case DoomLoopActionStop:
		v := verdict
		m.doomStop = &v
	}
}

// doomAdvisorInstructions is the default advisor instruction prefix.
const doomAdvisorInstructions = "You are an escalation advisor. The executing model appears stuck in a loop: "

// takeDoomLoopEscalationOverrides consumes a pending escalation into
// ONE-TURN overrides on a copy of req: the configured escalation model
// (with the Models fallback list cleared so it cannot shadow the swap),
// and/or an appended openrouter:advisor server tool with tool_choice pinned
// to it (allowed_tools, required). Burns one unit of budget at APPLICATION
// time. callerChoiceDispatched is false when the override replaced the
// caller's tool choice (so it must not be committed as consumed).
func (m *ModelResult) takeDoomLoopEscalationOverrides(req components.ResponsesRequest) (components.ResponsesRequest, bool, error) {
	verdict := m.doomPendingEscalation
	m.doomPendingEscalation = nil
	if m.doomMonitor == nil || verdict == nil || m.doomEscalation == nil || !m.doomMonitor.CanEscalate() {
		return req, true, nil
	}
	m.doomMonitor.ConsumeEscalation()
	cfg := m.doomEscalation
	out := req
	if cfg.Model != "" {
		out.Model = openrouter.String(cfg.Model)
		out.Models = nil
	}
	if !advisorEnabled(cfg.Advisor) {
		return out, true, nil
	}
	advisor, err := doomAdvisorTool(cfg.Advisor, verdict.Message)
	if err != nil {
		return req, true, err
	}
	out.Tools = append(append([]components.ResponsesRequestToolUnion{}, req.Tools...), advisor)
	choice, err := toolChoiceFromJSONValue(map[string]any{
		"type":  "allowed_tools",
		"mode":  "required",
		"tools": []any{map[string]any{"type": "openrouter:advisor"}},
	})
	if err != nil {
		return req, true, fmt.Errorf("doom-loop advisor tool choice: %w", err)
	}
	out.ToolChoice = choice
	prepared, _ := json.Marshal(req.ToolChoice)
	actual, _ := json.Marshal(out.ToolChoice)
	return out, string(prepared) == string(actual), nil
}

// doomAdvisorTool builds the openrouter:advisor server tool for an
// escalated turn. Defaults (forward_transcript, loop-describing
// instructions) are overlaid by the configured advisor parameters: a
// map[string]any (upstream camelCase keys such as forwardTranscript are
// accepted alongside the wire snake_case) or a components
// AdvisorServerToolConfig value/pointer.
func doomAdvisorTool(advisor any, message string) (components.ResponsesRequestToolUnion, error) {
	params := map[string]any{
		"forward_transcript": true,
		"instructions": doomAdvisorInstructions + message + " Diagnose why its approach is failing and give concrete, " +
			"specific instructions for a DIFFERENT approach. Do not restate the problem.",
	}
	var overlay map[string]any
	switch a := advisor.(type) {
	case map[string]any:
		overlay = a
	case components.AdvisorServerToolConfig, *components.AdvisorServerToolConfig:
		b, err := json.Marshal(a)
		if err != nil {
			return components.ResponsesRequestToolUnion{}, fmt.Errorf("doom-loop advisor config: %w", err)
		}
		if err := json.Unmarshal(b, &overlay); err != nil {
			return components.ResponsesRequestToolUnion{}, fmt.Errorf("doom-loop advisor config: %w", err)
		}
	}
	for k, v := range overlay {
		params[camelToSnake(k)] = v
	}
	b, err := json.Marshal(params)
	if err != nil {
		return components.ResponsesRequestToolUnion{}, fmt.Errorf("doom-loop advisor config: %w", err)
	}
	var cfg components.AdvisorServerToolConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return components.ResponsesRequestToolUnion{}, fmt.Errorf("doom-loop advisor config: %w", err)
	}
	return components.CreateResponsesRequestToolUnionOpenrouterAdvisor(components.AdvisorServerToolOpenRouter{Parameters: &cfg}), nil
}

// camelToSnake converts an upstream camelCase parameter name to the wire
// snake_case form (forwardTranscript -> forward_transcript). snake_case
// input is returned unchanged.
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// checkDoomLoopForResponse is the step-level checkpoint on a fresh model
// response: the text detectors (within-response repetition, cross-step
// identical-text streak), then server-tool fingerprints from the response's
// server-tool output items (post-execution, so block is meaningless:
// observe/steer/escalate/stop only).
func (m *ModelResult) checkDoomLoopForResponse(resp components.OpenResponsesResult) error {
	if m.doomMonitor == nil {
		return nil
	}
	if verdict := m.doomMonitor.RecordAssistantText(ExtractTextFromResponse(resp)); verdict != nil {
		action, err := m.applyDoomLoopVerdict(*verdict, nil)
		if err != nil {
			return err
		}
		m.applyDoomLoopSideEffects(action, *verdict)
	}
	for _, item := range resp.Output {
		itemType, identity := serverToolIdentity(item)
		if identity == nil {
			continue
		}
		if _, err := m.evaluateDoomLoopCall("server:"+itemType, identity, nil, false, false, DoomLoopDetectorServerToolFingerprint, nil); err != nil {
			return err
		}
	}
	return nil
}

// serverToolIdentity extracts the call identity echoed on a server-tool
// output item (every item except message/reasoning/function_call): all
// fields except volatile per-invocation ones (id, status) and the result
// payload. Returns nil when nothing identifying remains.
func serverToolIdentity(item components.OutputItems) (string, map[string]any) {
	b, err := json.Marshal(item)
	if err != nil {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		return "", nil
	}
	itemType, _ := fields["type"].(string)
	switch itemType {
	case "", "message", "reasoning", "function_call":
		return "", nil
	}
	for _, k := range []string{"id", "type", "status", "output", "result", "results", "error"} {
		delete(fields, k)
	}
	for k, v := range fields {
		if v == nil {
			delete(fields, k)
		}
	}
	if len(fields) == 0 {
		return "", nil
	}
	return itemType, fields
}

// queueDoomLoopSteer queues steer guidance, deduplicating identical
// messages (one verdict can repeat across calls in a round).
func (m *ModelResult) queueDoomLoopSteer(message string) {
	m.steerMu.Lock()
	defer m.steerMu.Unlock()
	for _, queued := range m.doomPendingSteer {
		if queued == message {
			return
		}
	}
	m.doomPendingSteer = append(m.doomPendingSteer, message)
}

// flushDoomLoopSteer injects the queued steer messages as ONE user message
// (joined by newlines) before the next follow-up request, through the
// Stop-hook AppendPrompt injection path. Only called once a round is fully
// paired: at a pause the guidance stays queued and is persisted as
// DoomLoop.PendingSteer instead (a user message between a dangling
// function_call and its output would be invalid history).
func (m *ModelResult) flushDoomLoopSteer(req *components.ResponsesRequest) {
	m.steerMu.Lock()
	if len(m.doomPendingSteer) == 0 {
		m.steerMu.Unlock()
		return
	}
	prompt := strings.Join(m.doomPendingSteer, "\n")
	m.doomPendingSteer = nil
	m.steerMu.Unlock()
	m.injectAppendPromptMessage(req, prompt)
}

// QueueUserMessage queues a user message for the run to pick up at the next
// safe turn boundary: it is injected (with any queued doom-loop guidance) as
// a user message into the next follow-up request after a fully paired tool
// round. Safe to call from any goroutine while the run is in flight. A
// message queued after the run's last follow-up is never sent. Upstream:
// ModelResult.queueUserMessage.
func (m *ModelResult) QueueUserMessage(text string) {
	m.queueDoomLoopSteer(text)
}

// haltForDoomLoop halts the run for the armed stop verdict: every
// function_call in resp without an output among produced gets a synthesized
// halt-error output persisted to state (so a stateful resume never sends a
// dangling function_call), and the run completes with SessionEnd reason
// doom_loop. No further model request is made.
func (m *ModelResult) haltForDoomLoop(resp components.OpenResponsesResult, produced []components.InputsUnion1) {
	resolved := map[string]bool{}
	for _, out := range produced {
		if out.FunctionCallOutputItem != nil {
			resolved[out.FunctionCallOutputItem.CallID] = true
		}
	}
	haltMessage := "Run halted by doom-loop detection."
	if m.doomStop != nil {
		haltMessage = "Run halted by doom-loop detection: " + m.doomStop.Message
	}
	for _, call := range ExtractToolCallsFromResponse(resp) {
		if resolved[call.CallID] {
			continue
		}
		resolved[call.CallID] = true
		item := components.FunctionCallOutputItem{CallID: call.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(doomHaltOutput(haltMessage)), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}
		m.state = AppendToMessages(m.state, components.CreateInputsUnion1FunctionCallOutputItem(item))
	}
	m.doomHalted = true
	m.markComplete()
}

// doomHaltOutput encodes {"error": message} as the halt output.
func doomHaltOutput(message string) string {
	b, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		// Marshaling a map of strings cannot fail.
		return `{"error":"Run halted by doom-loop detection."}`
	}
	return string(b)
}

//#endregion
