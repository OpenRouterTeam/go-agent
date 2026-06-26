package agent

import "sync"

// ReusableStream is a fan-out broadcast of a sequence of values that supports
// multiple concurrent consumers, each of which sees every value plus the
// terminal error — the Go-idiomatic stand-in for the upstream
// ReusableReadableStream. New subscribers also replay the full history.
//
// Each subscriber gets its own goroutine and an internal queue, so a slow
// consumer never blocks the producer and the producer never sends on a channel
// that is being closed concurrently (which would panic).
type ReusableStream[T any] struct {
	mu      sync.Mutex
	history []T
	subs    map[*subscriber[T]]struct{}
	done    bool
	err     error
}

type subscriber[T any] struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []T
	out     chan T
	closed  bool // subscriber unsubscribed/cancelled
	flushed bool // stream completed and all queued values drained
}

func NewReusableStream[T any]() *ReusableStream[T] {
	return &ReusableStream[T]{subs: map[*subscriber[T]]struct{}{}}
}

func (r *ReusableStream[T]) Push(v T) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.history = append(r.history, v)
	subs := make([]*subscriber[T], 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		s.enqueue(v)
	}
}

func (r *ReusableStream[T]) Complete(err error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	r.err = err
	subs := make([]*subscriber[T], 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.subs = map[*subscriber[T]]struct{}{}
	r.mu.Unlock()
	for _, s := range subs {
		s.finish()
	}
}

// Subscribe returns a channel of values plus a cancel/wait function that returns
// the terminal error after the stream completes (or the consumer stops early).
func (r *ReusableStream[T]) Subscribe(buffer int) (<-chan T, func() error) {
	if buffer <= 0 {
		buffer = 16
	}
	s := &subscriber[T]{out: make(chan T, buffer)}
	s.cond = sync.NewCond(&s.mu)

	r.mu.Lock()
	s.queue = append(s.queue, r.history...)
	done := r.done
	if !done {
		r.subs[s] = struct{}{}
	}
	r.mu.Unlock()

	go s.pump()
	if done {
		s.finish()
	}

	return s.out, func() error {
		r.mu.Lock()
		delete(r.subs, s)
		err := r.err
		r.mu.Unlock()
		s.close()
		return err
	}
}

func (r *ReusableStream[T]) Snapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]T{}, r.history...)
}

func (r *ReusableStream[T]) Err() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }

// enqueue appends a value for delivery to this subscriber.
func (s *subscriber[T]) enqueue(v T) {
	s.mu.Lock()
	if !s.closed && !s.flushed {
		s.queue = append(s.queue, v)
		s.cond.Signal()
	}
	s.mu.Unlock()
}

// finish marks the stream as complete; the pump drains remaining queued values
// then closes the output channel.
func (s *subscriber[T]) finish() {
	s.mu.Lock()
	s.flushed = true
	s.cond.Signal()
	s.mu.Unlock()
}

// close stops delivery immediately (consumer unsubscribed/cancelled).
func (s *subscriber[T]) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
}

// pump is the single goroutine that owns the output channel; it is the only
// place that sends on or closes s.out, eliminating send-on-closed races.
func (s *subscriber[T]) pump() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.flushed && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			close(s.out)
			return
		}
		if len(s.queue) == 0 && s.flushed {
			s.mu.Unlock()
			close(s.out)
			return
		}
		v := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.out <- v
	}
}
