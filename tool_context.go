package agent

import (
	"encoding/json"
	"sync"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

const SharedContextKey = "shared"

type ContextInput map[string]map[string]any

type ToolContextStore struct {
	mu        sync.RWMutex
	values    map[string]map[string]any
	listeners []func(string, map[string]any)
}

func NewToolContextStore(initial ContextInput) *ToolContextStore {
	s := &ToolContextStore{values: map[string]map[string]any{}}
	for k, v := range initial {
		s.values[k] = copyAnyMap(v)
	}
	return s
}

func (s *ToolContextStore) Snapshot() map[string]map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]any{}
	for k, v := range s.values {
		out[k] = copyAnyMap(v)
	}
	return out
}
func (s *ToolContextStore) Get(name string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyAnyMap(s.values[name])
}
func (s *ToolContextStore) Set(name string, value map[string]any) {
	s.mu.Lock()
	s.values[name] = copyAnyMap(value)
	listeners := append([]func(string, map[string]any){}, s.listeners...)
	s.mu.Unlock()
	for _, l := range listeners {
		l(name, copyAnyMap(value))
	}
}
func (s *ToolContextStore) Merge(name string, value map[string]any) {
	cur := s.Get(name)
	for k, v := range value {
		cur[k] = v
	}
	s.Set(name, cur)
}
func (s *ToolContextStore) Subscribe(listener func(string, map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, listener)
}

func BuildToolExecuteContext(call ParsedToolCall, turn TurnContext, store *ToolContextStore, emit func(any)) ToolExecuteContext {
	// TurnContext.ToolCall is part of the tool-facing contract, but the
	// streaming loop builds its turn context without one. Every executor
	// path has the call in hand, so thread it: a caller-provided ToolCall
	// wins, the executed call fills the gap otherwise (upstream #91).
	if turn.ToolCall == nil && (call.CallID != "" || call.Name != "") {
		turn.ToolCall = toFunctionCallItem(call)
	}
	var local, shared map[string]any
	if store != nil {
		local = store.Get(call.Name)
		shared = store.Get(SharedContextKey)
	}
	return ToolExecuteContext{ToolCall: call, Turn: turn, Context: local, Shared: shared, Store: store, Emit: emit}
}

func (tc ToolExecuteContext) LocalContext() map[string]any {
	if tc.Store == nil {
		return copyAnyMap(tc.Context)
	}
	return tc.Store.Get(tc.ToolCall.Name)
}

func (tc ToolExecuteContext) SharedContext() map[string]any {
	if tc.Store == nil {
		return copyAnyMap(tc.Shared)
	}
	return tc.Store.Get(SharedContextKey)
}

func (tc ToolExecuteContext) SetContext(value map[string]any) {
	if tc.Store != nil {
		tc.Store.Set(tc.ToolCall.Name, value)
	}
}

func (tc ToolExecuteContext) MergeContext(value map[string]any) {
	if tc.Store != nil {
		tc.Store.Merge(tc.ToolCall.Name, value)
	}
}

func (tc ToolExecuteContext) SetSharedContext(value map[string]any) {
	if tc.Store != nil {
		tc.Store.Set(SharedContextKey, value)
	}
}

func (tc ToolExecuteContext) MergeSharedContext(value map[string]any) {
	if tc.Store != nil {
		tc.Store.Merge(SharedContextKey, value)
	}
}

func copyAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toFunctionCallItem converts an executor ParsedToolCall back into the
// wire-shaped item TurnContext.ToolCall declares. Arguments are
// re-serialized from the parsed value when present, so the string is
// semantically equal to the wire arguments but not guaranteed byte-identical.
func toFunctionCallItem(call ParsedToolCall) *components.OutputFunctionCallItem {
	args := call.RawArgs
	if call.Arguments != nil {
		if b, err := json.Marshal(call.Arguments); err == nil {
			args = string(b)
		}
	}
	if args == "" {
		args = "{}"
	}
	item := &components.OutputFunctionCallItem{Arguments: args, CallID: call.CallID, Name: call.Name, Type: components.OutputFunctionCallItemTypeFunctionCall}
	if call.ID != "" {
		id := call.ID
		item.ID = &id
	}
	return item
}
