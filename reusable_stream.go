package agent

import "sync"

// StreamReplay selects how much history a ReusableStream (and the
// ToolEventBroadcaster built on it) retains for delayed and sequential
// consumers. It mirrors upstream's `streamReplay: 'full' | 'active-consumers'`.
type StreamReplay string

const (
	// StreamReplayFull retains the complete history: every subscriber,
	// including one that attaches after completion, replays every value.
	// This is the default.
	StreamReplayFull StreamReplay = "full"
	// StreamReplayActiveConsumers retains history only as far back as the
	// slowest currently attached subscriber still needs. A new subscriber
	// starts at the current trim watermark rather than at the beginning, and
	// once every subscriber has departed the remaining backlog is released.
	// The terminal error still reaches every subscriber, late or not.
	StreamReplayActiveConsumers StreamReplay = "active-consumers"
)

// replayCompactionMinHead mirrors upstream BUFFER_COMPACTION_MIN_HEAD: the
// retained slice is copied down (releasing its old backing array) once at
// least this many leading slots are dead and they make up at least half of it.
const replayCompactionMinHead = 1024

// ReusableStream is a fan-out broadcast of a sequence of values that supports
// multiple concurrent consumers, each of which sees every retained value plus
// the terminal error — the Go-idiomatic stand-in for the upstream
// ReusableReadableStream. Under StreamReplayFull (the default) new
// subscribers replay the full history; under StreamReplayActiveConsumers they
// start at the trim watermark.
//
// Values live in one shared buffer addressed by absolute position. Each
// subscriber gets its own goroutine (the only sender on, and closer of, its
// output channel) and an absolute read position, so a slow consumer never
// blocks the producer and the producer never sends on a channel that is being
// closed concurrently (which would panic).
type ReusableStream[T any] struct {
	mu     sync.Mutex
	replay StreamReplay
	// dropOnIdleComplete mirrors upstream ToolEventBroadcaster.cleanup(): in
	// active-consumers mode, completing with no attached subscriber releases
	// the backlog even if nobody ever subscribed.
	dropOnIdleComplete bool

	// buf[head:] holds the retained values; buf[head] has absolute position
	// trimOffset. Slots before head are zeroed so they pin nothing.
	buf        []T
	head       int
	trimOffset int

	subs           map[*subscriber[T]]struct{}
	everSubscribed bool
	done           bool
	err            error
}

type subscriber[T any] struct {
	out  chan T
	wake chan struct{} // cap 1; nudged on Push/Complete
	quit chan struct{} // closed on unsubscribe/cancel

	// Guarded by the owning stream's mu.
	pos      int  // absolute position of the next value to hand to out
	inflight bool // a value counted in pos is mid blocking-send
	finished bool // stream done and every value handed to out
	closed   bool // unsubscribed/cancelled
}

// NewReusableStream returns a stream with StreamReplayFull semantics.
func NewReusableStream[T any]() *ReusableStream[T] {
	return NewReusableStreamWithReplay[T](StreamReplayFull)
}

// NewReusableStreamWithReplay returns a stream with the given replay policy.
// Any value other than StreamReplayActiveConsumers behaves as StreamReplayFull.
func NewReusableStreamWithReplay[T any](replay StreamReplay) *ReusableStream[T] {
	if replay != StreamReplayActiveConsumers {
		replay = StreamReplayFull
	}
	return &ReusableStream[T]{replay: replay, subs: map[*subscriber[T]]struct{}{}}
}

// Replay reports the stream's replay policy.
func (r *ReusableStream[T]) Replay() StreamReplay { return r.replay }

func (r *ReusableStream[T]) Push(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	// Opportunistic compaction before growing (never drops on zero
	// subscribers: with nobody attached the backlog is catch-up history).
	r.trimLocked(false)
	r.buf = append(r.buf, v)
	r.wakeAllLocked()
}

func (r *ReusableStream[T]) Complete(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	r.err = err
	r.trimLocked(false)
	if r.replay == StreamReplayActiveConsumers && r.dropOnIdleComplete && len(r.subs) == 0 {
		r.dropBacklogLocked(true)
	}
	r.wakeAllLocked()
}

// Cancel terminates the stream without an error: every attached subscriber's
// channel is closed promptly (values already sitting in a channel buffer stay
// readable), later subscribers receive nothing, and — once anyone has ever
// subscribed — the buffered backlog is released in either replay mode
// (upstream ReusableReadableStream.cancel()).
func (r *ReusableStream[T]) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	for s := range r.subs {
		r.closeSubLocked(s)
	}
	r.subs = map[*subscriber[T]]struct{}{}
	r.dropBacklogLocked(false)
}

// Subscribe returns a channel of values plus a cancel/wait function that returns
// the terminal error after the stream completes (or the consumer stops early).
// Calling the function detaches the subscriber, closes its channel and
// releases whatever backlog only it was holding.
func (r *ReusableStream[T]) Subscribe(buffer int) (<-chan T, func() error) {
	if buffer <= 0 {
		buffer = 16
	}
	s := &subscriber[T]{
		out:  make(chan T, buffer),
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
	}

	r.mu.Lock()
	r.trimLocked(false)
	s.pos = r.trimOffset
	r.everSubscribed = true
	r.subs[s] = struct{}{}
	r.mu.Unlock()

	go r.pump(s)

	return s.out, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[s]; ok {
			delete(r.subs, s)
			r.trimLocked(true)
		}
		r.closeSubLocked(s)
		return r.err
	}
}

// Snapshot returns a copy of the retained history: everything under
// StreamReplayFull, only the not-yet-trimmed tail under
// StreamReplayActiveConsumers.
func (r *ReusableStream[T]) Snapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]T{}, r.buf[r.head:]...)
}

func (r *ReusableStream[T]) Err() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }

// IsComplete reports whether the source has been fully read into the
// buffer. A fresh consumer created after this point replays the retained
// buffer without waiting on a producer.
func (r *ReusableStream[T]) IsComplete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *ReusableStream[T]) wakeAllLocked() {
	for s := range r.subs {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (r *ReusableStream[T]) closeSubLocked(s *subscriber[T]) {
	if !s.closed {
		s.closed = true
		close(s.quit)
	}
}

// received is a conservative (never over-) estimate of how many values the
// consumer has actually taken off its channel: values handed to the channel
// minus those still buffered in it, minus one mid-send value.
func (s *subscriber[T]) received() int {
	n := s.pos - len(s.out)
	if s.inflight {
		n--
	}
	return n
}

// trimLocked prunes finished-and-drained subscribers and, in active-consumers
// mode, drops history every attached subscriber has received. departed means
// a subscriber has just left; when that (or pruning) leaves nobody attached
// the whole backlog is released.
func (r *ReusableStream[T]) trimLocked(departed bool) {
	for s := range r.subs {
		if s.finished && len(s.out) == 0 {
			delete(r.subs, s)
			departed = true
		}
	}
	if r.replay != StreamReplayActiveConsumers {
		return
	}
	if len(r.subs) == 0 {
		if departed {
			r.dropBacklogLocked(false)
		}
		return
	}
	minPos := -1
	for s := range r.subs {
		if p := s.received(); minPos < 0 || p < minPos {
			minPos = p
		}
	}
	nextHead := r.head + minPos - r.trimOffset
	if nextHead <= r.head {
		return
	}
	r.trimOffset = minPos
	if nextHead >= len(r.buf) {
		r.buf = nil
		r.head = 0
		return
	}
	clear(r.buf[r.head:nextHead])
	if nextHead >= replayCompactionMinHead && nextHead*2 >= len(r.buf) {
		r.buf = append([]T(nil), r.buf[nextHead:]...)
		r.head = 0
		return
	}
	r.head = nextHead
}

// dropBacklogLocked releases the retained backlog. Unless forced, a stream
// nobody has subscribed to yet keeps it: it is the catch-up history late
// joiners replay.
func (r *ReusableStream[T]) dropBacklogLocked(force bool) {
	if !force && !r.everSubscribed {
		return
	}
	r.trimOffset += len(r.buf) - r.head
	r.buf = nil
	r.head = 0
}

// pump is the single goroutine that owns the output channel; it is the only
// place that sends on or closes s.out, eliminating send-on-closed races. It
// exits (closing s.out) as soon as the subscriber is cancelled, even while
// blocked on a consumer that stopped reading.
func (r *ReusableStream[T]) pump(s *subscriber[T]) {
	defer close(s.out)
	for {
		r.mu.Lock()
		if s.closed {
			r.mu.Unlock()
			return
		}
		if idx := r.head + s.pos - r.trimOffset; idx < len(r.buf) {
			v := r.buf[idx]
			select {
			case s.out <- v:
				s.pos++
				r.trimLocked(false)
				r.mu.Unlock()
				continue
			default:
			}
			// Channel full: block outside the lock, counted as in flight so
			// trimming never assumes the consumer already has it.
			s.pos++
			s.inflight = true
			r.mu.Unlock()
			select {
			case s.out <- v:
			case <-s.quit:
				r.mu.Lock()
				s.inflight = false
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			s.inflight = false
			r.trimLocked(false)
			r.mu.Unlock()
			continue
		}
		if r.done {
			s.finished = true
			r.trimLocked(false)
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.quit:
			return
		}
	}
}
