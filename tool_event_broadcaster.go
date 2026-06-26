package agent

type ToolEventBroadcaster struct {
	stream *ReusableStream[ToolStreamEvent]
}

func NewToolEventBroadcaster() *ToolEventBroadcaster {
	return &ToolEventBroadcaster{stream: NewReusableStream[ToolStreamEvent]()}
}
func (b *ToolEventBroadcaster) Push(event ToolStreamEvent) { b.stream.Push(event) }
func (b *ToolEventBroadcaster) Complete()                  { b.stream.Complete(nil) }
func (b *ToolEventBroadcaster) Error(err error)            { b.stream.Complete(err) }
func (b *ToolEventBroadcaster) Subscribe() (<-chan ToolStreamEvent, func() error) {
	return b.stream.Subscribe(32)
}
func (b *ToolEventBroadcaster) Snapshot() []ToolStreamEvent { return b.stream.Snapshot() }
