package agent

// Shared fixtures for the upstream-parity suites (approval gate, tool-choice
// relaxation, nextTurnParams, toModelOutput, usage, ...). They wrap the
// existing fakeSender rather than introducing a parallel fake: every helper
// here only builds SDK values or reads back what fakeSender recorded.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// phFnCall builds a function_call output item the way the Responses API
// emits it (wire id `fc_<callID>` plus call_id).
func phFnCall(callID, name, args string) components.OutputItems {
	id := "fc_" + callID
	return components.CreateOutputItemsFunctionCall(components.OutputFunctionCallItem{
		ID: &id, CallID: callID, Name: name, Arguments: args,
		Type: components.OutputFunctionCallItemTypeFunctionCall,
	})
}

// phMessage builds an assistant message output item carrying text.
func phMessage(id, text string) components.OutputItems {
	return components.CreateOutputItemsMessage(components.OutputMessageItem{
		ID:   "msg_" + id,
		Role: components.OutputMessageItemRoleAssistant,
		Type: components.OutputMessageItemTypeMessage,
		Content: []components.OutputMessageItemContent{
			components.CreateOutputMessageItemContentOutputText(components.ResponseOutputText{
				Text: text, Type: components.ResponseOutputTextTypeOutputText,
			}),
		},
	})
}

// phResp builds a completed response with the given output items.
func phResp(id string, items ...components.OutputItems) components.OpenResponsesResult {
	return components.OpenResponsesResult{
		ID: id, Model: "openai/test", Status: components.OpenAIResponsesResponseStatusCompleted,
		Output: items,
	}
}

// phTextResp is a completed text-only response.
func phTextResp(id, text string) components.OpenResponsesResult {
	return phResp(id, phMessage(id, text))
}

// phSender is a fakeSender over non-streaming canned responses.
func phSender(resps ...components.OpenResponsesResult) *fakeSender {
	out := make([]*operations.CreateResponsesResponse, 0, len(resps))
	for _, r := range resps {
		created := operations.CreateCreateResponsesResponseOpenResponsesResult(r)
		out = append(out, &created)
	}
	return &fakeSender{responses: out}
}

// phUsage builds a usage block with cache/reasoning details and an optional cost.
func phUsage(in, out, cached, reasoning int64, cost *float64) optionalnullable.OptionalNullable[components.Usage] {
	u := components.Usage{
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		InputTokensDetails:  components.InputTokensDetails{CachedTokens: cached},
		OutputTokensDetails: components.OutputTokensDetails{ReasoningTokens: reasoning},
	}
	if cost != nil {
		u.Cost = optionalnullable.From(cost)
	}
	return optionalnullable.From(&u)
}

func phFloat(v float64) *float64 { return &v }

// phOutputs returns the function_call_output items of request i, in order.
func phOutputs(t *testing.T, s *fakeSender, i int) []components.FunctionCallOutputItem {
	t.Helper()
	if i >= len(s.requests) {
		t.Fatalf("request %d was never sent (only %d requests)", i, len(s.requests))
	}
	var out []components.FunctionCallOutputItem
	for _, item := range requestInputItems(s.requests[i].Input) {
		if item.FunctionCallOutputItem != nil {
			out = append(out, *item.FunctionCallOutputItem)
		}
	}
	return out
}

// phOutputCallIDs lists the call ids of request i's function_call_outputs.
func phOutputCallIDs(t *testing.T, s *fakeSender, i int) []string {
	t.Helper()
	var ids []string
	for _, o := range phOutputs(t, s, i) {
		ids = append(ids, o.CallID)
	}
	return ids
}

// phOutputString returns a function_call_output's string form ("" for the
// content-array form).
func phOutputString(o components.FunctionCallOutputItem) string {
	if o.Output.Str != nil {
		return *o.Output.Str
	}
	return ""
}

// phOutputError decodes the {"error": "..."} form of a function_call_output
// ("" when the output is not an error object).
func phOutputError(o components.FunctionCallOutputItem) string {
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(phOutputString(o)), &v); err != nil {
		return ""
	}
	return v.Error
}

// phJSON marshals v (an SDK value) to its wire JSON.
func phJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// phJSONMap marshals v and decodes it back into a generic map.
func phJSONMap(t *testing.T, v any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(phJSON(t, v)), &m); err != nil {
		t.Fatalf("decode %T as object: %v", v, err)
	}
	return m
}

// phChoice decodes a JSON tool choice into the SDK union.
func phChoice(t *testing.T, raw string) *components.OpenAIResponsesToolChoiceUnion {
	t.Helper()
	var c components.OpenAIResponsesToolChoiceUnion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decode tool choice %s: %v", raw, err)
	}
	return &c
}

// phWireChoice is the canonical wire JSON of request i's tool_choice ("" when
// the request carried none).
func phWireChoice(t *testing.T, s *fakeSender, i int) string {
	t.Helper()
	if i >= len(s.requests) {
		t.Fatalf("request %d was never sent (only %d requests)", i, len(s.requests))
	}
	c := s.requests[i].ToolChoice
	if c == nil {
		return ""
	}
	return phCanonicalJSON(t, phJSON(t, c))
}

// phCanonicalJSON re-encodes raw JSON with sorted keys.
func phCanonicalJSON(t *testing.T, raw string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// phToolNames lists the function tool names on request i.
func phToolNames(t *testing.T, s *fakeSender, i int) []string {
	t.Helper()
	var names []string
	for _, tool := range s.requests[i].Tools {
		if tool.ResponsesRequestToolFunction != nil {
			names = append(names, tool.ResponsesRequestToolFunction.Name)
		}
	}
	return names
}

// phUserTexts lists the plain-string contents of user messages in items.
func phUserTexts(items []components.InputsUnion1) []string {
	var out []string
	for _, item := range items {
		msg := item.EasyInputMessage
		if msg == nil || msg.Role.EasyInputMessageRoleUser == nil {
			continue
		}
		if c, ok := msg.Content.GetOrZero(); ok && c.Str != nil {
			out = append(out, *c.Str)
		}
	}
	return out
}

// phItemKinds summarizes items as a list of discriminators, e.g.
// ["user", "function_call", "function_call_output", "assistant"].
func phItemKinds(t *testing.T, items []components.InputsUnion1) []string {
	t.Helper()
	var out []string
	for _, item := range items {
		m := phJSONMap(t, item)
		typ, _ := m["type"].(string)
		role, _ := m["role"].(string)
		switch {
		case typ == "function_call" || typ == "function_call_output":
			out = append(out, typ)
		case role != "":
			out = append(out, role)
		default:
			out = append(out, typ)
		}
	}
	return out
}

// phMemState is an in-memory StateAccessor. Save copies the slices so later
// in-place mutation by the run cannot alter what was "persisted". The mutex
// makes it safe to read from the test goroutine while a run is live.
type phMemState struct {
	mu    sync.Mutex
	state *ConversationState
	saves int
	loads int
}

func phCloneState(s ConversationState) ConversationState {
	s.Messages = append([]components.InputsUnion1(nil), s.Messages...)
	s.PendingToolCalls = append([]ParsedToolCall(nil), s.PendingToolCalls...)
	s.UnsentToolResults = append([]UnsentToolResult(nil), s.UnsentToolResults...)
	return s
}

func (a *phMemState) Load(context.Context) (*ConversationState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loads++
	if a.state == nil {
		return nil, nil
	}
	s := phCloneState(*a.state)
	return &s, nil
}

func (a *phMemState) Save(_ context.Context, s ConversationState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saves++
	c := phCloneState(s)
	a.state = &c
	return nil
}

func (a *phMemState) get() *ConversationState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == nil {
		return nil
	}
	s := phCloneState(*a.state)
	return &s
}

// phSnapshot is a stable serialized form of the persisted state, for
// "state was not touched" assertions.
func phSnapshot(t *testing.T, a *phMemState) string {
	t.Helper()
	s := a.get()
	if s == nil {
		return "<nil>"
	}
	out, err := SerializeConversationState(*s)
	if err != nil {
		t.Fatalf("serialize state: %v", err)
	}
	return out
}

// phCounter is a mutex-guarded call recorder for tool bodies and hook
// handlers, which run on the ModelResult's goroutine.
type phCounter struct {
	mu   sync.Mutex
	n    int
	seen []any
}

func (c *phCounter) add(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.seen = append(c.seen, v)
}

func (c *phCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *phCounter) values() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.seen...)
}

// phPendingIDs lists pending call keys.
func phPendingIDs(calls []ParsedToolCall) []string {
	var ids []string
	for _, c := range calls {
		ids = append(ids, callKey(c))
	}
	return ids
}

func phEqualStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00") && len(a) == len(b)
}

// phDrainToolStream collects every ToolStream event and the stream error.
func phDrainToolStream(ctx context.Context, r *ModelResult) ([]ToolStreamEvent, error) {
	ch, wait := r.ToolStream(ctx)
	var events []ToolStreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events, wait()
}

// Shared tool input shapes.
type phTargetInput struct {
	Target string `json:"target" jsonschema:"required"`
}

type phVInput struct {
	V string `json:"v" jsonschema:"required"`
}

// phObjectSchema builds an explicit object schema whose listed properties are
// all required strings, so fixtures pin the exact schema they validate with
// (the derived-schema path is covered by TestDerivedSchemaEnforcesRequiredFields).
func phObjectSchema(required ...string) map[string]any {
	props := map[string]any{}
	req := []any{}
	for _, name := range required {
		props[name] = map[string]any{"type": "string"}
		req = append(req, name)
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

// phOKTool is an executable tool recording each input it ran on.
func phOKTool(name string, rec *phCounter, cfg func(*ToolConfig[phTargetInput])) *TypedTool[phTargetInput] {
	c := ToolConfig[phTargetInput]{Name: name, InputSchema: phObjectSchema("target"), Execute: func(_ context.Context, in phTargetInput, _ ToolExecuteContext) (any, error) {
		rec.add(in)
		return map[string]any{"ok": true}, nil
	}}
	if cfg != nil {
		cfg(&c)
	}
	return MustNewTool(c)
}

// streamingResponsesOf wraps one SSE stream as a fakeSender response list.
func streamingResponsesOf(t *testing.T, events []components.StreamEvents) []*operations.CreateResponsesResponse {
	t.Helper()
	return []*operations.CreateResponsesResponse{streamingResponse(t, events)}
}

// phErrSender wraps fakeSender so its first failN sends fail at the
// transport level (the SDK returning an error), then delegates.
type phErrSender struct {
	*fakeSender
	failN int
	err   error
}

func (s *phErrSender) SendResponse(ctx context.Context, req components.ResponsesRequest, metadata *components.MetadataLevel, opts ...operations.Option) (*operations.CreateResponsesResponse, error) {
	if s.failN > 0 {
		s.failN--
		s.fakeSender.requests = append(s.fakeSender.requests, req)
		return nil, s.err
	}
	return s.fakeSender.SendResponse(ctx, req, metadata, opts...)
}
