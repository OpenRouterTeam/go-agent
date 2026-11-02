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
	var local, shared map[string]any
	if store != nil {
		local = store.Get(call.Name)
		shared = store.Get(SharedContextKey)
	}
	// The executed call is part of the tool-facing turn context. A
	// caller-provided turn.ToolCall still wins; otherwise the executed call
	// is converted back to its wire-shaped function_call item, so the
	// streaming loop's tools see the same TurnContext.ToolCall the
	// orchestrator's do (upstream #91).
	if turn.ToolCall == nil && (call.Name != "" || call.CallID != "" || call.ID != "") {
		turn.ToolCall = toFunctionCallItem(call)
	}
	return ToolExecuteContext{ToolCall: call, Turn: turn, Context: local, Shared: shared, Store: store, Emit: emit}
}

// toFunctionCallItem converts a parsed call back to its wire-shaped
// function_call item.
func toFunctionCallItem(call ParsedToolCall) *components.OutputFunctionCallItem {
	id := callKey(call)
	callID := call.CallID
	if callID == "" {
		callID = id
	}
	args := call.RawArgs
	if args == "" {
		args = "{}"
		if call.Arguments != nil {
			if b, err := json.Marshal(call.Arguments); err == nil {
				args = string(b)
			}
		}
	}
	item := &components.OutputFunctionCallItem{CallID: callID, Name: call.Name, Arguments: args, Type: components.OutputFunctionCallItemTypeFunctionCall}
	if id != "" {
		item.ID = &id
	}
	return item
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
