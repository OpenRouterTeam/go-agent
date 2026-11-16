package agent

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// Ports of upstream replay-policy tests (reusable-stream.test.ts,
// replay-buffer-compaction.test.ts, tool-event-broadcaster.test.ts) for the
// StreamReplay option. Timeouts below are hang guards only; no test relies on
// sleeping for ordering.

const replayTestTimeout = 5 * time.Second

func recvOne[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed, expected a value")
		}
		return v
	case <-time.After(replayTestTimeout):
		t.Fatalf("timed out waiting for a value")
	}
	var zero T
	return zero
}

func drainAll[T any](t *testing.T, ch <-chan T) []T {
	t.Helper()
	out := []T{}
	deadline := time.After(replayTestTimeout)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, v)
		case <-deadline:
			t.Fatalf("timed out draining channel; got %v so far", out)
		}
	}
}

func filledStream(replay StreamReplay, values ...int) *ReusableStream[int] {
	s := NewReusableStreamWithReplay[int](replay)
	for _, v := range values {
		s.Push(v)
	}
	s.Complete(nil)
	return s
}

func retained[T any](r *ReusableStream[T]) (live, capacity int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf) - r.head, cap(r.buf)
}

func eventIDs(events []ToolStreamEvent) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.CallID)
	}
	return out
}

func ev(id string) ToolStreamEvent { return ToolStreamEvent{CallID: id} }

func TestStreamReplayDefaultsAndUnknownFallBackToFull(t *testing.T) {
	if got := NewReusableStream[int]().Replay(); got != StreamReplayFull {
		t.Fatalf("default replay = %q", got)
	}
	if got := NewReusableStreamWithReplay[int]("bogus").Replay(); got != StreamReplayFull {
		t.Fatalf("unknown replay = %q", got)
	}
	if got := NewToolEventBroadcaster().stream.Replay(); got != StreamReplayFull {
		t.Fatalf("broadcaster default replay = %q", got)
	}
}

// "replays the complete history to sequential and post-completion consumers by default"
func TestReusableStreamFullReplaysSequentialConsumers(t *testing.T) {
	s := filledStream(StreamReplayFull, 1, 2, 3)
	for i := 0; i < 2; i++ {
		ch, done := s.Subscribe(0)
		if got := drainAll(t, ch); !reflect.DeepEqual(got, []int{1, 2, 3}) {
			t.Fatalf("consumer %d got %v", i, got)
		}
		if err := done(); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Snapshot(); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("full snapshot = %v", got)
	}
}

// "starts new active-consumer consumers at the current watermark"
func TestReusableStreamActiveStartsAtWatermark(t *testing.T) {
	s := filledStream(StreamReplayActiveConsumers, 1, 2, 3)
	first, doneFirst := s.Subscribe(0)
	if v := recvOne(t, first); v != 1 {
		t.Fatalf("first got %d", v)
	}
	second, doneSecond := s.Subscribe(0)
	if got := drainAll(t, first); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("first rest = %v", got)
	}
	if got := drainAll(t, second); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("second = %v", got)
	}
	_ = doneFirst()
	_ = doneSecond()
}

// "continues to replay active-consumer events through repeated compaction"
// + "a fully caught-up consumer leaves nothing for later joiners"
func TestReusableStreamActiveRepeatedCompaction(t *testing.T) {
	values := make([]int, 2500)
	for i := range values {
		values[i] = i
	}
	s := filledStream(StreamReplayActiveConsumers, values...)
	ch, _ := s.Subscribe(0)
	if got := drainAll(t, ch); !reflect.DeepEqual(got, values) {
		t.Fatalf("consumer saw %d values", len(got))
	}
	late, done := s.Subscribe(0)
	if got := drainAll(t, late); len(got) != 0 {
		t.Fatalf("late consumer replayed %d values", len(got))
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if live, _ := retained(s); live != 0 {
		t.Fatalf("backlog still retains %d values", live)
	}
}

// "active-consumers: backlog follows the slowest remaining consumer"
func TestReusableStreamActiveBacklogFollowsSlowest(t *testing.T) {
	s := NewReusableStreamWithReplay[int](StreamReplayActiveConsumers)
	fast, doneFast := s.Subscribe(0)
	slow, doneSlow := s.Subscribe(0)
	s.Push(1)
	s.Push(2)
	if a, b := recvOne(t, fast), recvOne(t, fast); a != 1 || b != 2 {
		t.Fatalf("fast got %d,%d", a, b)
	}
	_ = doneFast()
	if got := s.Snapshot(); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("slow consumer's backlog trimmed: %v", got)
	}
	if a, b := recvOne(t, slow), recvOne(t, slow); a != 1 || b != 2 {
		t.Fatalf("slow got %d,%d", a, b)
	}
	s.Complete(nil)
	if rest := drainAll(t, slow); len(rest) != 0 {
		t.Fatalf("slow extra values %v", rest)
	}
	_ = doneSlow()
}

// "active-consumers: backlog is dropped once every consumer departs"
func TestReusableStreamActiveDropsBacklogWhenAllDepart(t *testing.T) {
	s := NewReusableStreamWithReplay[int](StreamReplayActiveConsumers)
	only, done := s.Subscribe(0)
	s.Push(1)
	s.Push(2)
	recvOne(t, only)
	recvOne(t, only)
	_ = done()
	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("backlog retained after last consumer departed: %v", got)
	}
	s.Push(3)
	late, doneLate := s.Subscribe(0)
	if v := recvOne(t, late); v != 3 {
		t.Fatalf("late got %d, want watermark value 3", v)
	}
	s.Complete(nil)
	if rest := drainAll(t, late); len(rest) != 0 {
		t.Fatalf("late extra %v", rest)
	}
	_ = doneLate()
}

// An unconsumed stream keeps its catch-up history in active-consumers mode.
func TestReusableStreamActiveKeepsHistoryBeforeFirstConsumer(t *testing.T) {
	s := filledStream(StreamReplayActiveConsumers, 1, 2)
	ch, _ := s.Subscribe(0)
	if got := drainAll(t, ch); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("first consumer got %v", got)
	}
}

// "return() wakes a pending next() instead of hanging" — in Go, the
// unsubscribe func closes a channel a goroutine is blocked receiving on.
func TestReusableStreamUnsubscribeWakesPendingReceive(t *testing.T) {
	for _, replay := range []StreamReplay{StreamReplayFull, StreamReplayActiveConsumers} {
		s := NewReusableStreamWithReplay[int](replay)
		ch, done := s.Subscribe(0)
		s.Push(1)
		if v := recvOne(t, ch); v != 1 {
			t.Fatalf("%s: got %d", replay, v)
		}
		result := make(chan bool, 1)
		go func() {
			_, ok := <-ch
			result <- ok
		}()
		_ = done()
		select {
		case ok := <-result:
			if ok {
				t.Fatalf("%s: pending receive got a value", replay)
			}
		case <-time.After(replayTestTimeout):
			t.Fatalf("%s: pending receive hung after unsubscribe", replay)
		}
	}
}

// 250e64e: a consumer that stopped reading must not pin its backlog (or its
// pump goroutine) once it unsubscribes.
func TestReusableStreamUnsubscribeReleasesStalledConsumer(t *testing.T) {
	for _, replay := range []StreamReplay{StreamReplayFull, StreamReplayActiveConsumers} {
		s := NewReusableStreamWithReplay[int](replay)
		ch, done := s.Subscribe(1)
		for i := 0; i < 100; i++ {
			s.Push(i)
		}
		if live, _ := retained(s); live != 100 {
			t.Fatalf("%s: retained %d before unsubscribe", replay, live)
		}
		_ = done()
		if got := drainAll(t, ch); len(got) > 2 {
			t.Fatalf("%s: stalled consumer still delivered %d values", replay, len(got))
		}
		live, _ := retained(s)
		switch replay {
		case StreamReplayFull:
			if live != 100 {
				t.Fatalf("full mode dropped history: %d", live)
			}
		case StreamReplayActiveConsumers:
			if live != 0 {
				t.Fatalf("active mode kept %d values after last consumer left", live)
			}
		}
	}
}

// "cancel() settles waiters and leaves no buffered backlog"
func TestReusableStreamCancelReleasesBacklog(t *testing.T) {
	s := NewReusableStream[int]()
	ch, done := s.Subscribe(0)
	s.Push(1)
	s.Push(2)
	s.Cancel()
	got := drainAll(t, ch) // must close; may include already-buffered values
	if len(got) > 2 {
		t.Fatalf("cancelled consumer got %v", got)
	}
	if err := done(); err != nil {
		t.Fatalf("cancel err = %v", err)
	}
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("backlog survived cancel: %v", snap)
	}
	fresh, freshDone := s.Subscribe(0)
	if rest := drainAll(t, fresh); len(rest) != 0 {
		t.Fatalf("fresh consumer after cancel got %v", rest)
	}
	if err := freshDone(); err != nil {
		t.Fatal(err)
	}
	s.Push(3) // no-op after cancel
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("push after cancel retained %v", snap)
	}
}

// Contract: a stream error reaches every consumer, including late joiners,
// even when active-consumers replay has released the history.
func TestReusableStreamActiveErrorReachesEveryConsumer(t *testing.T) {
	boom := errors.New("boom")
	s := NewReusableStreamWithReplay[int](StreamReplayActiveConsumers)
	a, doneA := s.Subscribe(0)
	b, doneB := s.Subscribe(0)
	s.Push(1)
	s.Complete(boom)
	if got := drainAll(t, a); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("a got %v", got)
	}
	if got := drainAll(t, b); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("b got %v", got)
	}
	if err := doneA(); !errors.Is(err, boom) {
		t.Fatalf("a err = %v", err)
	}
	if err := doneB(); !errors.Is(err, boom) {
		t.Fatalf("b err = %v", err)
	}
	late, doneLate := s.Subscribe(0)
	if got := drainAll(t, late); len(got) != 0 {
		t.Fatalf("late replayed %v", got)
	}
	if err := doneLate(); !errors.Is(err, boom) {
		t.Fatalf("late err = %v", err)
	}
}

// The retained buffer stays bounded (and its backing array is released by
// compaction) while a consumer lags by a fixed amount; full mode keeps all.
func TestReusableStreamActiveCompactionBoundsMemory(t *testing.T) {
	const total, lag = 20000, 2000
	for _, replay := range []StreamReplay{StreamReplayFull, StreamReplayActiveConsumers} {
		s := NewReusableStreamWithReplay[int](replay)
		ch, done := s.Subscribe(1)
		next := 0
		maxLive, maxCap := 0, 0
		for i := 0; i < total; i++ {
			s.Push(i)
			if i >= lag {
				if v := recvOne(t, ch); v != next {
					t.Fatalf("%s: got %d want %d", replay, v, next)
				}
				next++
			}
			live, c := retained(s)
			maxLive, maxCap = max(maxLive, live), max(maxCap, c)
		}
		s.Complete(nil)
		for _, v := range drainAll(t, ch) {
			if v != next {
				t.Fatalf("%s: got %d want %d", replay, v, next)
			}
			next++
		}
		if next != total {
			t.Fatalf("%s: delivered %d", replay, next)
		}
		switch replay {
		case StreamReplayFull:
			if live, _ := retained(s); live != total {
				t.Fatalf("full retained %d", live)
			}
		case StreamReplayActiveConsumers:
			if maxLive > lag+4 {
				t.Fatalf("active retained up to %d values (lag %d)", maxLive, lag)
			}
			if maxCap > 4*lag+2*replayCompactionMinHead {
				t.Fatalf("active backing array grew to %d", maxCap)
			}
		}
		_ = done()
		if replay == StreamReplayActiveConsumers {
			if live, c := retained(s); live != 0 || c != 0 {
				t.Fatalf("active kept live=%d cap=%d after consumer left", live, c)
			}
		}
	}
}

// tool-event-broadcaster.test.ts: "retains full replay after completion by default"
func TestToolEventBroadcasterFullReplayAfterCompletion(t *testing.T) {
	b := NewToolEventBroadcaster()
	first, _ := b.Subscribe()
	b.Push(ev("1"))
	b.Complete()
	if got := eventIDs(drainAll(t, first)); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("first = %v", got)
	}
	second, _ := b.Subscribe()
	if got := eventIDs(drainAll(t, second)); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("second = %v", got)
	}
}

// "compacts active-consumer history at the watermark"
func TestToolEventBroadcasterActiveCompactsAtWatermark(t *testing.T) {
	b := NewToolEventBroadcasterWithReplay(StreamReplayActiveConsumers)
	first, _ := b.Subscribe()
	b.Push(ev("1"))
	b.Push(ev("2"))
	b.Complete()
	if got := recvOne(t, first); got.CallID != "1" {
		t.Fatalf("first got %q", got.CallID)
	}
	second, _ := b.Subscribe()
	if got := eventIDs(drainAll(t, first)); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("first rest = %v", got)
	}
	if got := eventIDs(drainAll(t, second)); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("second = %v", got)
	}
}

// "drops backlog once every consumer departs, late joiner starts at watermark"
func TestToolEventBroadcasterActiveDropsBacklogOnDeparture(t *testing.T) {
	b := NewToolEventBroadcasterWithReplay(StreamReplayActiveConsumers)
	only, done := b.Subscribe()
	b.Push(ev("1"))
	b.Push(ev("2"))
	recvOne(t, only)
	recvOne(t, only)
	_ = done()
	if snap := b.Snapshot(); len(snap) != 0 {
		t.Fatalf("backlog retained after departure: %v", eventIDs(snap))
	}
	b.Push(ev("3"))
	// Go has no microtask tick between Complete and a synchronous late
	// subscribe, so the late joiner attaches before completion here.
	late, _ := b.Subscribe()
	b.Complete()
	if got := eventIDs(drainAll(t, late)); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("late = %v", got)
	}
}

// "backlog survives while at least one consumer remains"
func TestToolEventBroadcasterActiveBacklogSurvivesSlowConsumer(t *testing.T) {
	b := NewToolEventBroadcasterWithReplay(StreamReplayActiveConsumers)
	fast, doneFast := b.Subscribe()
	slow, _ := b.Subscribe()
	b.Push(ev("1"))
	b.Push(ev("2"))
	recvOne(t, fast)
	recvOne(t, fast)
	_ = doneFast()
	if a, c := recvOne(t, slow), recvOne(t, slow); a.CallID != "1" || c.CallID != "2" {
		t.Fatalf("slow got %q,%q", a.CallID, c.CallID)
	}
	b.Complete()
	if rest := drainAll(t, slow); len(rest) != 0 {
		t.Fatalf("slow extra %v", eventIDs(rest))
	}
}

// "return() wakes a pending next() instead of hanging" (broadcaster).
func TestToolEventBroadcasterUnsubscribeWakesPendingReceive(t *testing.T) {
	b := NewToolEventBroadcasterWithReplay(StreamReplayActiveConsumers)
	ch, done := b.Subscribe()
	b.Push(ev("1"))
	recvOne(t, ch)
	result := make(chan bool, 1)
	go func() {
		_, ok := <-ch
		result <- ok
	}()
	_ = done()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("pending receive got a value")
		}
	case <-time.After(replayTestTimeout):
		t.Fatal("pending receive hung after unsubscribe")
	}
}

// Upstream cleanup(): completing an active-consumers broadcaster with nobody
// attached releases the backlog; the terminal error still reaches late
// subscribers. Full mode keeps replaying.
func TestToolEventBroadcasterCompleteWithoutConsumers(t *testing.T) {
	boom := errors.New("boom")
	for _, replay := range []StreamReplay{StreamReplayFull, StreamReplayActiveConsumers} {
		b := NewToolEventBroadcasterWithReplay(replay)
		b.Push(ev("1"))
		b.Push(ev("2"))
		b.Error(boom)
		late, done := b.Subscribe()
		got := eventIDs(drainAll(t, late))
		want := []string{"1", "2"}
		if replay == StreamReplayActiveConsumers {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: late got %v want %v", replay, got, want)
		}
		if err := done(); !errors.Is(err, boom) {
			t.Fatalf("%s: late err = %v", replay, err)
		}
	}
}
