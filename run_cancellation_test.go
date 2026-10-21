package agent

// Run-level cancellation, ported from upstream tests/unit/run-cancellation.test.ts.
//
// Upstream adds a `signal` option on callModel; in Go the run's
// context.Context already plays that role (idiomatic divergence #1), so the
// behaviors under test are the ones upstream's signal guarantees: a
// pre-canceled run fails before any dispatch, cancellation between requests
// stops the loop at the next turn boundary with the cancellation cause, a
// cancellation mid-request aborts the in-flight dispatch, and a per-request
// timeout still bounds each request while the run ctx is cancelable.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
)

// blockingSender blocks every dispatch until the request ctx is done, after
// signaling that the dispatch started (a stalled provider).
type blockingSender struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (b *blockingSender) SendResponse(ctx context.Context, _ components.ResponsesRequest, _ *components.MetadataLevel, _ ...operations.Option) (*operations.CreateResponsesResponse, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunCancellationPreCanceledFailsBeforeDispatch(t *testing.T) {
	cause := errors.New("user aborted")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	sender := scripted(completedResponse("never"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("a pre-canceled run must fail with the cancellation cause, got %v", err)
	}
	if sender.calls != 0 {
		t.Fatalf("no network dispatch may happen; got %d", sender.calls)
	}
}

func TestRunCancellationBetweenRequestsStopsLoop(t *testing.T) {
	cause := errors.New("stop after the tool")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var s doomScript
	tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
		cancel(cause) // cancellation lands during tool execution, between requests
		return "r", nil
	}})
	sender := scripted(s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("never fetched"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	text, done := result.TextStream(context.Background())
	for range text {
	}
	if err := done(); !errors.Is(err, cause) {
		t.Fatalf("stream consumers must see the cancellation cause, got %v", err)
	}
	if _, err := result.Response(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("Response must surface the cancellation cause, got %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("the loop must stop at the next turn boundary; requests=%d", sender.calls)
	}
}

func TestRunCancellationAbortsInFlightRequest(t *testing.T) {
	for _, viaMethod := range []bool{false, true} {
		cause := errors.New("stalled provider")
		ctx, cancel := context.WithCancelCause(context.Background())
		sender := &blockingSender{started: make(chan struct{})}
		result, err := CallModel(ctx, sender, CallModelInput{Model: "test-model", Input: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		errCh := make(chan error, 1)
		go func() {
			_, err := result.Text(context.Background())
			errCh <- err
		}()
		<-sender.started
		if viaMethod {
			result.Cancel()
		} else {
			cancel(cause)
		}
		err = <-errCh
		cancel(nil)
		want := cause
		if viaMethod {
			want = context.Canceled
		}
		if !errors.Is(err, want) {
			t.Fatalf("viaMethod=%v: in-flight dispatch must fail fast with %v, got %v", viaMethod, want, err)
		}
	}
}

// hangingHTTPClient never answers: it waits for the request ctx to end.
type hangingHTTPClient struct{}

func (hangingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestRunCancellationPerRequestTimeoutComposesWithRunContext(t *testing.T) {
	// The Go SDK applies its timeout per Send by deriving a fresh
	// context.WithTimeout from the run ctx, so a cancelable run ctx never
	// disables the per-request bound (the TypeScript SDK bug upstream works
	// around does not exist here).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewOpenRouter(OpenRouterOptions{SDK: SDKOptions{APIKey: "test-key", HTTPClient: hangingHTTPClient{}, Timeout: 20 * time.Millisecond}})
	result, err := client.CallModel(ctx, CallModelInput{Model: "test-model", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the per-request timeout must bound the request, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the run ctx itself was not canceled")
	}
}

// A run canceled after a response lands (here from AfterTurn) runs none of
// that response's tools: upstream checks the signal at the top of each loop
// iteration, before tool execution.
func TestRunCancellationBeforeToolExecution(t *testing.T) {
	cause := errors.New("canceled after the response")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var s doomScript
	executions := 0
	tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) {
		executions++
		return "r", nil
	}})
	sender := scripted(s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("never fetched"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}, AfterTurn: func(context.Context, TurnContext, StepResult) error {
		cancel(cause)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("want the cancellation cause, got %v", err)
	}
	if executions != 0 || sender.calls != 1 {
		t.Fatalf("no tool may run after cancellation: executions=%d requests=%d", executions, sender.calls)
	}
}

// A cancellation during a later, text-only turn still fails the run, as
// upstream's loop-top signal check does on every follow-up response.
func TestRunCancellationOnTextOnlyFollowUpTurn(t *testing.T) {
	cause := errors.New("canceled on the final turn")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var s doomScript
	tool := MustNewTool(ToolConfig[searchArgs]{Name: "web_search", Execute: func(context.Context, searchArgs, ToolExecuteContext) (any, error) { return "r", nil }})
	sender := scripted(s.toolTurn("web_search", searchArgs{"x"}), s.textTurn("final"))
	result, err := CallModel(ctx, sender, CallModelInput{Model: "test-model", Input: "hi", Tools: []Tool{tool}, AfterTurn: func(_ context.Context, turn TurnContext, _ StepResult) error {
		if turn.NumberOfTurns == 1 {
			cancel(cause)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Text(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("want the cancellation cause, got %v", err)
	}
}
