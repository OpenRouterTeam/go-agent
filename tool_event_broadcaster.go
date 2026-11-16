package agent

// ToolEventBroadcaster is a push-based fan-out of tool stream events built on
// ReusableStream. Every subscriber sees the retained events plus the terminal
// error. Under StreamReplayFull (the default) late subscribers replay every
// event; under StreamReplayActiveConsumers history is compacted to what the
// attached subscribers still need, and completing with nobody attached
// releases the backlog (upstream ToolEventBroadcaster.cleanup()).
type ToolEventBroadcaster struct {
	stream *ReusableStream[ToolStreamEvent]
}

// NewToolEventBroadcaster returns a broadcaster with StreamReplayFull semantics.
func NewToolEventBroadcaster() *ToolEventBroadcaster {
	return NewToolEventBroadcasterWithReplay(StreamReplayFull)
}

// NewToolEventBroadcasterWithReplay returns a broadcaster with the given
// replay policy.
func NewToolEventBroadcasterWithReplay(replay StreamReplay) *ToolEventBroadcaster {
	stream := NewReusableStreamWithReplay[ToolStreamEvent](replay)
	stream.dropOnIdleComplete = true
	return &ToolEventBroadcaster{stream: stream}
}

func (b *ToolEventBroadcaster) Push(event ToolStreamEvent) { b.stream.Push(event) }
func (b *ToolEventBroadcaster) Complete()                  { b.stream.Complete(nil) }
func (b *ToolEventBroadcaster) Error(err error)            { b.stream.Complete(err) }
func (b *ToolEventBroadcaster) Subscribe() (<-chan ToolStreamEvent, func() error) {
	return b.stream.Subscribe(32)
}
func (b *ToolEventBroadcaster) Snapshot() []ToolStreamEvent { return b.stream.Snapshot() }
