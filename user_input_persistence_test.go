package agent

// Ports tests/unit/user-input-persistence.test.ts: fresh caller input is
// persisted to ConversationState.Messages together with the first response,
// and never when the request fails.

import (
	"context"
	"errors"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	openrouter "github.com/OpenRouterTeam/go-sdk"
)

func uipUser(text string) components.InputsUnion1 {
	return components.CreateInputsUnion1EasyInputMessage(components.EasyInputMessage{
		Role:    components.CreateEasyInputMessageRoleUnionEasyInputMessageRoleUser(components.EasyInputMessageRoleUserUser),
		Content: optionalnullable.From(openrouter.Pointer(components.CreateEasyInputMessageContentUnion2Str(text))),
	})
}

// upstream: "should persist array user input items to state.messages after
// successful API call" and "should persist string input normalized as user
// message"
func TestFreshInputPersistedWithFirstResponse(t *testing.T) {
	for name, input := range map[string]any{
		"array input":  []components.InputsUnion1{uipUser("Hello")},
		"string input": "Hello",
	} {
		mem := &phMemState{}
		if _, err := mustRun(t, phSender(phTextResp("r1", "Hello back!")), CallModelInput{Model: "openai/test", Input: input, StateAccessor: mem}); err != nil {
			t.Fatal(err)
		}
		st := mem.get()
		if st == nil {
			t.Fatalf("%s: nothing saved", name)
		}
		if kinds := phItemKinds(t, st.Messages); !phEqualStrings(kinds, []string{"user", "assistant"}) {
			t.Fatalf("%s: messages = %v, want user then the response", name, kinds)
		}
		if texts := phUserTexts(st.Messages); !phEqualStrings(texts, []string{"Hello"}) {
			t.Fatalf("%s: user texts = %v", name, texts)
		}
	}
}

// upstream: "should persist user input alongside tool results after tool execution"
func TestFreshInputPersistedBeforeToolRoundItems(t *testing.T) {
	type msg struct {
		Message string `json:"message"`
	}
	echo := MustNewTool(ToolConfig[msg]{Name: "echo", InputSchema: phObjectSchema("message"), Execute: func(_ context.Context, in msg, _ ToolExecuteContext) (any, error) {
		return in.Message, nil
	}})
	mem := &phMemState{}
	sender := phSender(phResp("r1", phFnCall("call_1", "echo", `{"message":"hi"}`)), phTextResp("r2", "Done echoing."))
	if _, err := mustRun(t, sender, CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{uipUser("Echo hi")}, Tools: []Tool{echo}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if kinds := phItemKinds(t, mem.get().Messages); !phEqualStrings(kinds, []string{"user", "function_call", "function_call_output", "assistant"}) {
		t.Fatalf("messages = %v", kinds)
	}
	// The follow-up request carries the same ordered history.
	if kinds := phItemKinds(t, requestInputItems(sender.requests[1].Input)); !phEqualStrings(kinds, []string{"user", "function_call", "function_call_output"}) {
		t.Fatalf("follow-up input = %v", kinds)
	}
}

// upstream: "should include prior user input in API request when resuming
// from state" and "should accumulate user input from both calls in state
// after two callModel invocations"
func TestFreshInputAccumulatesAcrossCalls(t *testing.T) {
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phTextResp("r1", "First response")), CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{uipUser("First message")}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	second := phSender(phTextResp("r2", "Second response"))
	if _, err := mustRun(t, second, CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{uipUser("Second message")}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if texts := phUserTexts(requestInputItems(second.requests[0].Input)); !phEqualStrings(texts, []string{"First message", "Second message"}) {
		t.Fatalf("second request user messages = %v", texts)
	}
	st := mem.get()
	if texts := phUserTexts(st.Messages); !phEqualStrings(texts, []string{"First message", "Second message"}) {
		t.Fatalf("state user messages = %v", texts)
	}
	if kinds := phItemKinds(t, st.Messages); !phEqualStrings(kinds, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("state messages = %v", kinds)
	}
}

// upstream: "should not persist user input when stream fails mid-read" and
// "should not duplicate user input in state when retrying after API failure"
func TestFreshInputNotPersistedWhenRequestFails(t *testing.T) {
	ctx := context.Background()
	failures := map[string]func() ResponseSender{
		"stream fails mid-read": func() ResponseSender {
			created := operations.CreateCreateResponsesResponseEventStream(failingEventStream())
			return &fakeSender{responses: []*operations.CreateResponsesResponse{&created}}
		},
		"transport error": func() ResponseSender {
			return &phErrSender{fakeSender: phSender(phTextResp("never", "x")), failN: 1, err: errors.New("API failure")}
		},
	}
	for name, failing := range failures {
		mem := &phMemState{}
		result, err := CallModel(ctx, failing(), CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{uipUser("Hello")}, StateAccessor: mem})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := result.Text(ctx); err == nil {
			t.Fatalf("%s: Text must surface the failure", name)
		}
		if st := mem.get(); st != nil && len(phUserTexts(st.Messages)) != 0 {
			t.Fatalf("%s: a failed request must not persist the user turn: %v", name, phItemKinds(t, st.Messages))
		}
		if st, _ := result.State(ctx); len(phUserTexts(st.Messages)) != 0 {
			t.Fatalf("%s: in-memory state must not carry the orphaned user turn", name)
		}

		// Retrying the same input leaves exactly one user turn.
		retry := phSender(phTextResp("r1", "Hello back!"))
		if _, err := mustRun(t, retry, CallModelInput{Model: "openai/test", Input: []components.InputsUnion1{uipUser("Hello")}, StateAccessor: mem}); err != nil {
			t.Fatal(err)
		}
		if texts := phUserTexts(requestInputItems(retry.requests[0].Input)); !phEqualStrings(texts, []string{"Hello"}) {
			t.Fatalf("%s: retry request user messages = %v", name, texts)
		}
		if kinds := phItemKinds(t, mem.get().Messages); !phEqualStrings(kinds, []string{"user", "assistant"}) {
			t.Fatalf("%s: after retry messages = %v", name, kinds)
		}
	}
}

// Fresh input also persists when the first response pauses the run, so the
// resume request replays it ahead of the paused turn.
func TestFreshInputPersistedOnApprovalPause(t *testing.T) {
	var runs phCounter
	gated := phOKTool("gated", &runs, func(c *ToolConfig[phTargetInput]) { c.RequireApproval = true })
	mem := &phMemState{}
	if _, err := mustRun(t, phSender(phResp("r1", phFnCall("call_1", "gated", `{"target":"x"}`))), CallModelInput{Model: "openai/test", Input: "please", Tools: []Tool{gated}, StateAccessor: mem}); err != nil {
		t.Fatal(err)
	}
	if kinds := phItemKinds(t, mem.get().Messages); !phEqualStrings(kinds, []string{"user", "function_call"}) {
		t.Fatalf("paused messages = %v", kinds)
	}
	resume := phSender(phTextResp("r2", "done"))
	if _, err := mustRun(t, resume, CallModelInput{Model: "openai/test", Tools: []Tool{gated}, StateAccessor: mem, ApproveToolCalls: []string{"call_1"}}); err != nil {
		t.Fatal(err)
	}
	if kinds := phItemKinds(t, requestInputItems(resume.requests[0].Input)); !phEqualStrings(kinds, []string{"user", "function_call", "function_call_output"}) {
		t.Fatalf("resume request = %v", kinds)
	}
	if texts := phUserTexts(mem.get().Messages); !phEqualStrings(texts, []string{"please"}) {
		t.Fatalf("the user turn must be persisted exactly once, got %v", texts)
	}
}
