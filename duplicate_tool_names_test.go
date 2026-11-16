package agent

// Ports tests/unit/duplicate-tool-names.test.ts.

import (
	"context"
	"strings"
	"testing"
)

// upstream: "rejects a tools list where two client tools share a name"
func TestDuplicateToolNamesRejectedByCallModel(t *testing.T) {
	var rec phCounter
	first := phOKTool("read_value", &rec, nil)
	second := phOKTool("read_value", &rec, nil)
	sender := phSender(phTextResp("r1", "never"))

	result, err := CallModel(context.Background(), sender, CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{first, second},
	})
	if err == nil {
		t.Fatalf("CallModel must reject duplicate client tool names, got result %v", result)
	}
	if !strings.Contains(err.Error(), `duplicate tool name "read_value"`) {
		t.Fatalf("error should name the duplicate tool, got %q", err)
	}
	if sender.calls != 0 || rec.count() != 0 {
		t.Fatalf("nothing may be sent or run on a rejected tools list: calls=%d runs=%d", sender.calls, rec.count())
	}
}

func TestFindDuplicateClientToolNameSkipsServerTools(t *testing.T) {
	var rec phCounter
	a := phOKTool("a", &rec, nil)
	b := phOKTool("b", &rec, nil)
	// Two server tools share an empty client name; they are not dispatched
	// by name and must not trip the check.
	s1 := NewServerTool(ServerToolConfig{Config: webSearchServerTool()})
	s2 := NewServerTool(ServerToolConfig{Config: webSearchServerTool()})

	if name, dup := FindDuplicateClientToolName([]Tool{a, s1, b, s2}); dup {
		t.Fatalf("server tools must be skipped, got duplicate %q", name)
	}
	if err := AssertUniqueClientToolNames([]Tool{a, s1, b, s2}); err != nil {
		t.Fatalf("unique client names + server tools must pass: %v", err)
	}
	if err := AssertUniqueClientToolNames(nil); err != nil {
		t.Fatalf("empty tools list must pass: %v", err)
	}

	// The first repeated name is reported, even when wrapped as MCP.
	name, dup := FindDuplicateClientToolName([]Tool{a, b, MarkMcp(phOKTool("b", &rec, nil)), phOKTool("a", &rec, nil)})
	if !dup || name != "b" {
		t.Fatalf("FindDuplicateClientToolName = (%q, %v), want (\"b\", true)", name, dup)
	}
	if _, err := CallModel(context.Background(), phSender(phTextResp("r", "x")), CallModelInput{
		Model: "openai/test", Input: "hi", Tools: []Tool{s1, s2, a},
	}); err != nil {
		t.Fatalf("server tools without client names must not be treated as duplicates: %v", err)
	}
}

// upstream: "does not run an approved call on a different same-named tool
// added before resume"
func TestDuplicateToolNamesRejectedOnApprovalResume(t *testing.T) {
	ctx := context.Background()
	var approvedRuns, otherRuns phCounter
	approved := phOKTool("read_value", &approvedRuns, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	other := phOKTool("read_value", &otherRuns, nil)

	mem := &phMemState{}
	pauseSender := phSender(phResp("resp_pause", phFnCall("call_1", "read_value", `{"target":"synthetic-marker"}`)))
	first, err := CallModel(ctx, pauseSender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{approved}, StateAccessor: mem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.State(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mem.get(); got == nil || got.Status != ConversationStatusAwaitingApproval {
		t.Fatalf("expected an awaiting_approval pause, got %+v", got)
	}
	before := phSnapshot(t, mem)

	resumeSender := phSender(phTextResp("resp_done", "Done."))
	_, err = CallModel(ctx, resumeSender, CallModelInput{
		Model: "openai/test", Tools: []Tool{other, approved}, StateAccessor: mem,
		ApproveToolCalls: []string{"call_1"},
	})
	if err == nil || !strings.Contains(err.Error(), `duplicate tool name "read_value"`) {
		t.Fatalf("resume with duplicate tool names must fail, got %v", err)
	}
	if otherRuns.count() != 0 || approvedRuns.count() != 0 {
		t.Fatalf("no tool may run: other=%d approved=%d", otherRuns.count(), approvedRuns.count())
	}
	if resumeSender.calls != 0 {
		t.Fatalf("no request may be sent, got %d", resumeSender.calls)
	}
	if after := phSnapshot(t, mem); after != before {
		t.Fatalf("state must stay awaiting_approval and untouched\nbefore=%s\nafter=%s", before, after)
	}
}
