package agent

// Ports tests/unit/tool-executor-return.test.ts: generator tools broadcast
// each yield live (tool.preliminary_result) and return a terminal result that does
// not retain the yields.

import (
	"context"
	"reflect"
	"testing"
)

type terEmpty struct{}

func terGenerator(yields int, ret any, rec *phCounter) *TypedTool[terEmpty] {
	return MustNewTool(ToolConfig[terEmpty]{
		Name: "streaming_tool", InputSchema: map[string]any{"type": "object"},
		EventSchema: map[string]any{"type": "object", "required": []any{"index"}, "properties": map[string]any{"index": map[string]any{"type": "number"}}},
		Generate: func(_ context.Context, _ terEmpty, _ ToolExecuteContext, yield func(any) error) (any, error) {
			for i := 0; i < yields; i++ {
				if err := yield(map[string]any{"index": i}); err != nil {
					return nil, err
				}
			}
			if rec != nil {
				rec.add(nil)
			}
			return ret, nil
		},
	})
}

// upstream: "broadcasts many preliminary results without retaining them on
// the terminal result"
func TestGeneratorBroadcastsYieldsWithoutRetainingThem(t *testing.T) {
	const yieldCount = 10_000
	var broadcasts int
	res, err := ExecuteTool(context.Background(), terGenerator(yieldCount, map[string]any{"done": true}, nil), ParsedToolCall{ID: "streaming-call", CallID: "streaming-call", Name: "streaming_tool", Arguments: map[string]any{}}, ToolExecuteContext{
		Emit: func(any) { broadcasts++ },
	})
	if err != nil || res.Error != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if broadcasts != yieldCount {
		t.Fatalf("broadcasts = %d, want %d", broadcasts, yieldCount)
	}
	if phJSON(t, res.Output) != `{"done":true}` {
		t.Fatalf("terminal result = %v", res.Output)
	}
	// The terminal result type has nowhere to accumulate yields.
	rt := reflect.TypeOf(ToolExecutionResult{})
	for _, field := range []string{"Events", "PreliminaryResults"} {
		if _, ok := rt.FieldByName(field); ok {
			t.Fatalf("ToolExecutionResult must not carry a %s field (yields are broadcast, not retained)", field)
		}
	}
}

// upstream: "should capture return value even with no yields", "should
// prefer return value over last yield when both present"
func TestGeneratorReturnValueIsTheResult(t *testing.T) {
	for _, yields := range []int{0, 3} {
		var events []any
		res, err := ExecuteTool(context.Background(), terGenerator(yields, "final", nil), ParsedToolCall{ID: "c", CallID: "c", Name: "streaming_tool"}, ToolExecuteContext{Emit: func(v any) { events = append(events, v) }})
		if err != nil || res.Error != nil || res.Output != "final" || len(events) != yields {
			t.Fatalf("yields=%d: output=%v err=%v/%v events=%d", yields, res.Output, err, res.Error, len(events))
		}
	}
	// A yield that violates the event schema fails the call.
	bad := MustNewTool(ToolConfig[terEmpty]{
		Name: "streaming_tool", InputSchema: map[string]any{"type": "object"},
		EventSchema: map[string]any{"type": "object", "required": []any{"index"}},
		Generate: func(_ context.Context, _ terEmpty, _ ToolExecuteContext, yield func(any) error) (any, error) {
			if err := yield(map[string]any{"nope": true}); err != nil {
				return nil, err
			}
			return "unreachable", nil
		},
	})
	res, err := ExecuteTool(context.Background(), bad, ParsedToolCall{ID: "c", CallID: "c", Name: "streaming_tool"}, ToolExecuteContext{})
	if err != nil || res.Error == nil {
		t.Fatalf("an invalid yield must fail the call: res=%+v err=%v", res, err)
	}
}

// Through the loop, yields reach ToolStream live as tool.preliminary_result events,
// in order, before the call's tool.result; the model only sees the return.
func TestGeneratorYieldsStreamAsPreliminaryEvents(t *testing.T) {
	ctx := context.Background()
	sender := phSender(phResp("r1", phFnCall("call_gen", "streaming_tool", `{}`)), phTextResp("r2", "done"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "openai/test", Input: "go", Tools: []Tool{terGenerator(3, map[string]any{"done": true}, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := phDrainToolStream(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	var indexes []float64
	for _, ev := range events {
		if ev.CallID != "call_gen" {
			continue
		}
		seq = append(seq, ev.Type)
		if ev.Type == "tool.preliminary_result" {
			m := phJSONMap(t, ev.Event)
			indexes = append(indexes, m["index"].(float64))
			if ev.Name != "streaming_tool" || ev.Source != ToolSourceClient {
				t.Fatalf("preliminary event = %+v", ev)
			}
		}
		if ev.Type == "tool.result" && phJSON(t, ev.Result) != `{"done":true}` {
			t.Fatalf("tool.result must carry the return value, got %v", ev.Result)
		}
	}
	if !phEqualStrings(seq, []string{"tool.preliminary_result", "tool.preliminary_result", "tool.preliminary_result", "tool.result", "tool.call_output"}) {
		t.Fatalf("tool stream order = %v", seq)
	}
	if len(indexes) != 3 || indexes[0] != 0 || indexes[1] != 1 || indexes[2] != 2 {
		t.Fatalf("yields out of order: %v", indexes)
	}
	outs := phOutputs(t, sender, 1)
	if len(outs) != 1 || phOutputString(outs[0]) != `{"done":true}` {
		t.Fatalf("the model gets only the return value, got %+v", outs)
	}
}
