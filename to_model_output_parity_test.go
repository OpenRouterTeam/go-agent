package agent

// Ports tests/unit/to-model-output-parity.test.ts and the ToModelOutput
// result paths (normal round, auto-approved siblings at an approval pause,
// calls approved on resume).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

const (
	tmoSecret = "SENTINEL-PRIVATE-VALUE"
	tmoMask   = "[masked]"
)

// tmoInput lowercases `access` and defaults `verbose` to false on decode
// (z.string().toLowerCase(), z.boolean().default(false)).
type tmoInput struct {
	Access  string `json:"access"`
	Verbose bool   `json:"verbose"`
}

func (in *tmoInput) UnmarshalJSON(b []byte) error {
	var raw struct {
		Access  string `json:"access"`
		Verbose *bool  `json:"verbose"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	in.Access = strings.ToLower(raw.Access)
	if raw.Verbose != nil {
		in.Verbose = *raw.Verbose
	}
	return nil
}

var tmoSchema = map[string]any{"type": "object", "required": []any{"access"}, "properties": map[string]any{"access": map[string]any{"type": "string"}, "verbose": map[string]any{"type": "boolean"}}}

func tmoText(text string) ToolOutputContentItem {
	return components.CreateFunctionCallOutputItemOutputUnion1InputText(components.InputText{Text: text, Type: components.InputTextTypeInputText})
}

type tmoRecorder struct {
	exec, mapper phCounter
}

func tmoLookup(rec *tmoRecorder, cfg func(*ToolConfig[tmoInput])) *TypedTool[tmoInput] {
	c := ToolConfig[tmoInput]{
		Name: "lookup", InputSchema: tmoSchema,
		Execute: func(_ context.Context, in tmoInput, _ ToolExecuteContext) (any, error) {
			rec.exec.add(in)
			return map[string]any{"secret": tmoSecret}, nil
		},
		ToModelOutput: func(_ context.Context, in tmoInput, out any) (ToModelOutputResult, error) {
			rec.mapper.add([2]any{in, phCopyAny(out)})
			text := tmoSecret
			if in.Access == "private" {
				text = tmoMask
			}
			return ContentModelOutput(tmoText(text)), nil
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	return MustNewTool(c)
}

// tmoAssertMasked checks a function_call_output is the content-array form
// carrying exactly the masked text.
func tmoAssertMasked(t *testing.T, o components.FunctionCallOutputItem) {
	t.Helper()
	if o.Output.Str != nil {
		t.Fatalf("%s: a content result must be sent as the array form, got string %q", o.CallID, *o.Output.Str)
	}
	got := phJSON(t, o.Output.ArrayOfFunctionCallOutputItemOutputUnion1)
	if phCanonicalJSON(t, got) != `[{"text":"[masked]","type":"input_text"}]` {
		t.Fatalf("%s: content output = %s", o.CallID, got)
	}
}

func tmoAssertNoSecret(t *testing.T, s *fakeSender) {
	t.Helper()
	for i := range s.requests {
		for _, o := range phOutputs(t, s, i) {
			if strings.Contains(phJSON(t, o), tmoSecret) {
				t.Fatalf("request %d leaked the raw output to the model: %s", i, phJSON(t, o))
			}
		}
	}
}

// upstream: "passes inputSchema-normalized arguments to toModelOutput"
func TestToModelOutputReceivesDecodedInputAndSendsContentArray(t *testing.T) {
	ctx := context.Background()
	var rec tmoRecorder
	sender := phSender(phResp("resp_1", phFnCall("call_1", "lookup", `{"access":"PRIVATE"}`)), phTextResp("resp_2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "look it up", Tools: []Tool{tmoLookup(&rec, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := phDrainToolStream(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	parsed := tmoInput{Access: "private", Verbose: false}
	if got := rec.exec.values(); len(got) != 1 || got[0] != parsed {
		t.Fatalf("execute inputs = %v", got)
	}
	if got := rec.mapper.values(); len(got) != 1 || got[0].([2]any)[0] != parsed {
		t.Fatalf("toModelOutput must receive the same decoded input as execute, got %v", got)
	}
	outs := phOutputs(t, sender, 1)
	if len(outs) != 1 {
		t.Fatalf("outputs = %+v", outs)
	}
	tmoAssertMasked(t, outs[0])
	tmoAssertNoSecret(t, sender)

	// ToolStream still reports the raw tool output to the caller.
	var raw bool
	for _, ev := range events {
		if ev.Type == "tool.result" && ev.CallID == "call_1" && strings.Contains(phCopyAny(ev.Result), tmoSecret) {
			raw = true
		}
	}
	if !raw {
		t.Fatalf("tool.result must carry the raw output, events=%+v", events)
	}
	// The persisted state carries what the model saw, not the secret.
	st, _ := result.State(ctx)
	for _, item := range st.Messages {
		if item.FunctionCallOutputItem != nil && strings.Contains(phJSON(t, item), tmoSecret) {
			t.Fatalf("state must persist the mapped output: %s", phJSON(t, item))
		}
	}
}

// upstream: "maps the output of an approved call before sending it to the model"
func TestToModelOutputAppliedOnApprovalResume(t *testing.T) {
	var rec tmoRecorder
	tool := tmoLookup(&rec, func(c *ToolConfig[tmoInput]) { c.RequireApproval = true })
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("resp_1", phFnCall("call_1", "lookup", `{"access":"PRIVATE"}`))), CallModelInput{Model: "openai/test", Input: "look it up", Tools: []Tool{tool}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if mem.get().Status != ConversationStatusAwaitingApproval || rec.exec.count() != 0 {
		t.Fatalf("status=%s exec=%d", mem.get().Status, rec.exec.count())
	}
	resume := phSender(phTextResp("resp_2", "done"))
	if _, err := mustRun(t, resume, CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{}, Tools: []Tool{tool}, StateAccessor: mem, ApproveToolCalls: []string{"call_1"}}); err != nil {
		t.Fatal(err)
	}
	if rec.exec.count() != 1 || rec.mapper.count() != 1 {
		t.Fatalf("exec=%d mapper=%d", rec.exec.count(), rec.mapper.count())
	}
	m := rec.mapper.values()[0].([2]any)
	if m[0] != (tmoInput{Access: "private"}) || m[1] != `{"secret":"`+tmoSecret+`"}` {
		t.Fatalf("mapper called with %v", m)
	}
	outs := phOutputs(t, resume, 0)
	if len(outs) != 1 {
		t.Fatalf("outputs = %+v", outs)
	}
	tmoAssertMasked(t, outs[0])
	tmoAssertNoSecret(t, resume)
}

// An auto-approved sibling executed at an approval pause is recorded as an
// unsent result already mapped, and replayed in the array form on resume.
func TestToModelOutputAppliedToAutoApprovedSiblingAtPause(t *testing.T) {
	var rec tmoRecorder
	var gatedRuns phCounter
	gated := phOKTool("gated", &gatedRuns, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	mem := &phMemState{}
	sender := phSender(phResp("resp_1", phFnCall("call_lookup", "lookup", `{"access":"PRIVATE"}`), phFnCall("call_gated", "gated", `{"target":"x"}`)))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tmoLookup(&rec, nil), gated}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	st := mem.get()
	if st.Status != ConversationStatusAwaitingApproval || len(st.UnsentToolResults) != 1 || rec.mapper.count() != 1 {
		t.Fatalf("status=%s unsent=%+v mapper=%d", st.Status, st.UnsentToolResults, rec.mapper.count())
	}
	if strings.Contains(phCopyAny(st.UnsentToolResults[0].Output), tmoSecret) {
		t.Fatalf("the unsent result must already be the model-facing output: %v", st.UnsentToolResults[0].Output)
	}
	resume := phSender(phTextResp("resp_2", "done"))
	if _, err := mustRun(t, resume, CallModelInput{Model: "openai/test", Tools: []Tool{tmoLookup(&rec, nil), gated}, StateAccessor: mem, ApproveToolCalls: []string{"call_gated"}}); err != nil {
		t.Fatal(err)
	}
	outs := phOutputs(t, resume, 0)
	var lookup *components.FunctionCallOutputItem
	for i := range outs {
		if outs[i].CallID == "call_lookup" {
			lookup = &outs[i]
		}
	}
	if lookup == nil || len(outs) != 2 {
		t.Fatalf("resume must answer both calls, got %+v", outs)
	}
	tmoAssertMasked(t, *lookup)
	tmoAssertNoSecret(t, resume)
	if rec.mapper.count() != 1 || rec.exec.count() != 1 {
		t.Fatalf("the sibling is not re-run or re-mapped on resume: exec=%d mapper=%d", rec.exec.count(), rec.mapper.count())
	}
}

// The content array recorded for an auto-approved sibling must survive the
// versioned serialization contract: upstream detects content arrays
// structurally (isContentArray), so a JSON-persisted state still sends the
// array form.
func TestToModelOutputUnsentContentSurvivesSerialization(t *testing.T) {
	ctx := context.Background()
	var rec tmoRecorder
	var gatedRuns phCounter
	gated := phOKTool("gated", &gatedRuns, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	tools := []Tool{tmoLookup(&rec, nil), gated}
	result, err := mustRun(t, phSender(phResp("resp_1", phFnCall("call_lookup", "lookup", `{"access":"PRIVATE"}`), phFnCall("call_gated", "gated", `{"target":"x"}`))), CallModelInput{Model: "openai/test", Input: "go", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := result.State(ctx)
	blob, err := SerializeConversationState(st)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DeserializeConversationState(blob)
	if err != nil {
		t.Fatal(err)
	}
	resume := phSender(phTextResp("resp_2", "done"))
	if _, err := mustRun(t, resume, CallModelInput{Model: "openai/test", Tools: tools, State: &restored, ApproveToolCalls: []string{"call_gated"}}); err != nil {
		t.Fatal(err)
	}
	for _, o := range phOutputs(t, resume, 0) {
		if o.CallID == "call_lookup" {
			tmoAssertMasked(t, o)
			return
		}
	}
	t.Fatal("resume request did not answer call_lookup")
}

// A non-"content" ToModelOutput result falls back to the JSON-encoded raw
// output; a failed execution is never mapped.
func TestToModelOutputFallbacks(t *testing.T) {
	ctx := context.Background()
	t.Run("non-content type", func(t *testing.T) {
		var rec tmoRecorder
		tool := tmoLookup(&rec, func(c *ToolConfig[tmoInput]) {
			c.ToModelOutput = func(context.Context, tmoInput, any) (ToModelOutputResult, error) {
				return ToModelOutputResult{Type: "json"}, nil
			}
		})
		sender := phSender(phResp("r1", phFnCall("c1", "lookup", `{"access":"public"}`)), phTextResp("r2", "done"))
		if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tool}}); err != nil {
			t.Fatal(err)
		}
		outs := phOutputs(t, sender, 1)
		if len(outs) != 1 || phOutputString(outs[0]) != `{"secret":"`+tmoSecret+`"}` {
			t.Fatalf("outputs = %+v", outs)
		}
	})
	t.Run("execute error", func(t *testing.T) {
		var rec tmoRecorder
		tool := tmoLookup(&rec, func(c *ToolConfig[tmoInput]) {
			c.Execute = func(context.Context, tmoInput, ToolExecuteContext) (any, error) {
				return nil, errors.New("backend down")
			}
		})
		sender := phSender(phResp("r1", phFnCall("c1", "lookup", `{"access":"public"}`)), phTextResp("r2", "done"))
		if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tool}}); err != nil {
			t.Fatal(err)
		}
		outs := phOutputs(t, sender, 1)
		if rec.mapper.count() != 0 || len(outs) != 1 || phOutputError(outs[0]) != "backend down" {
			t.Fatalf("mapper=%d outputs=%+v", rec.mapper.count(), outs)
		}
	})
	t.Run("mapper error fails the run", func(t *testing.T) {
		var rec tmoRecorder
		tool := tmoLookup(&rec, func(c *ToolConfig[tmoInput]) {
			c.ToModelOutput = func(context.Context, tmoInput, any) (ToModelOutputResult, error) {
				return ToModelOutputResult{}, errors.New("mapper broke")
			}
		})
		sender := phSender(phResp("r1", phFnCall("c1", "lookup", `{"access":"public"}`)), phTextResp("r2", "done"))
		result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{tool}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(ctx); err == nil || !strings.Contains(err.Error(), "mapper broke") {
			t.Fatalf("a mapper error must not be degraded into a placeholder, got %v", err)
		}
		if sender.calls != 1 {
			t.Fatalf("no follow-up may be sent with an unmapped output, calls=%d", sender.calls)
		}
	})
}
