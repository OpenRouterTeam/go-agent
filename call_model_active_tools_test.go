package agent

// Ports the loop-level cases of tests/unit/call-model-active-tools.test.ts:
// CallModelInput.ActiveTools filters which tools are sent and executable.

import (
	"context"
	"testing"
)

func cmatTools() (a, b Tool, aRuns, bRuns *phCounter) {
	aRuns, bRuns = &phCounter{}, &phCounter{}
	return phOKTool("a", aRuns, nil), phOKTool("b", bRuns, nil), aRuns, bRuns
}

func cmatWireHasTools(t *testing.T, s *fakeSender, i int) bool {
	t.Helper()
	_, ok := phJSONMap(t, s.requests[i])["tools"]
	return ok
}

// upstream: "sends only active tools when activeTools is provided" and
// "silently ignores unknown activeTools names"
func TestActiveToolsFiltersSentTools(t *testing.T) {
	for _, active := range [][]string{{"a"}, {"a", "missing"}} {
		a, b, _, _ := cmatTools()
		sender := phSender(phTextResp("r", "ok"))
		if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{a, b}, ActiveTools: active}); err != nil {
			t.Fatal(err)
		}
		if names := phToolNames(t, sender, 0); !phEqualStrings(names, []string{"a"}) {
			t.Fatalf("ActiveTools %v: sent tools = %v, want [a]", active, names)
		}
	}
}

// upstream: "sends all tools when activeTools is omitted"
func TestActiveToolsNilSendsAllTools(t *testing.T) {
	a, b, _, _ := cmatTools()
	sender := phSender(phTextResp("r", "ok"))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{a, b}}); err != nil {
		t.Fatal(err)
	}
	if names := phToolNames(t, sender, 0); !phEqualStrings(names, []string{"a", "b"}) {
		t.Fatalf("sent tools = %v", names)
	}
}

// upstream: "omits the tools key when tools is explicitly empty" and "omits
// the tools key entirely (not an empty array) when activeTools filters out
// every tool"
func TestActiveToolsFilteringEverythingOmitsToolsKey(t *testing.T) {
	a, b, _, _ := cmatTools()
	for name, in := range map[string]CallModelInput{
		"explicitly empty tools":   {Model: "openai/test", Input: "hi", Tools: []Tool{}},
		"everything filtered out":  {Model: "openai/test", Input: "hi", Tools: []Tool{a, b}, ActiveTools: []string{"missing"}},
		"empty non-nil activeTool": {Model: "openai/test", Input: "hi", Tools: []Tool{a, b}, ActiveTools: []string{}},
	} {
		sender := phSender(phTextResp("r", "ok"))
		if _, err := mustRun(t, sender, in); err != nil {
			t.Fatal(err)
		}
		if cmatWireHasTools(t, sender, 0) {
			t.Fatalf("%s: the wire request must not carry a tools key: %s", name, phJSON(t, sender.requests[0]))
		}
	}
}

// Server tools are always kept; only client tools are filtered.
func TestActiveToolsKeepsServerTools(t *testing.T) {
	a, b, _, _ := cmatTools()
	server := NewServerTool(ServerToolConfig{Name: "web_search", Config: webSearchServerTool()})
	sender := phSender(phTextResp("r", "ok"))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{a, server, b}, ActiveTools: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	tools := sender.requests[0].Tools
	if len(tools) != 2 || phJSON(t, tools[0]) != phJSON(t, webSearchServerTool()) || phToolNames(t, sender, 0)[0] != "b" {
		t.Fatalf("tools = %s", phJSON(t, tools))
	}
}

// A call to a tool filtered out by ActiveTools is not executable: it is
// surfaced like an unknown tool, never run.
func TestActiveToolsFilteredToolIsNotExecutable(t *testing.T) {
	ctx := context.Background()
	a, b, aRuns, bRuns := cmatTools()
	sender := phSender(phResp("r1", phFnCall("call_a", "a", `{"target":"x"}`), phFnCall("call_b", "b", `{"target":"y"}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{a, b}, ActiveTools: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := result.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bRuns.count() != 0 {
		t.Fatal("a filtered-out tool must never execute")
	}
	if aRuns.count() != 1 {
		t.Fatalf("the active tool runs, ran %d", aRuns.count())
	}
	if st.Status != ConversationStatusAwaitingClientTools || !phEqualStrings(phPendingIDs(st.PendingToolCalls), []string{"call_b"}) {
		t.Fatalf("the filtered call is surfaced as unresolved: status=%s pending=%v", st.Status, phPendingIDs(st.PendingToolCalls))
	}
	if sender.calls != 1 {
		t.Fatalf("no follow-up with an unanswered function_call, calls=%d", sender.calls)
	}
}

// A nil tool in a filtered list is reported as an error, not a panic.
func TestActiveToolsRejectsNilTool(t *testing.T) {
	a, _, _, _ := cmatTools()
	if _, err := CallModel(context.Background(), phSender(phTextResp("r", "ok")), CallModelInput{Model: "openai/test", Input: "hi", Tools: []Tool{a, nil}, ActiveTools: []string{"a"}}); err == nil {
		t.Fatal("a nil tool must be reported as an error")
	}
}
