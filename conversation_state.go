package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func GenerateConversationID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("conv_%d", time.Now().UnixNano())
	}
	return "conv_" + hex.EncodeToString(buf)
}

func CreateInitialState() ConversationState {
	now := time.Now().UnixMilli()
	return ConversationState{Version: ConversationStateVersion, ID: GenerateConversationID(), Messages: []components.InputsUnion1{}, Status: ConversationStatusInProgress, CreatedAt: now, UpdatedAt: now}
}

// UnsupportedStateVersionError is returned by DeserializeConversationState
// when a state blob's version is not supported by this SDK build.
type UnsupportedStateVersionError struct {
	Found     int
	Supported []int
}

func (e *UnsupportedStateVersionError) Error() string {
	return fmt.Sprintf("unsupported ConversationState version %d; supported version(s): %s", e.Found, joinInts(e.Supported))
}

// InvalidStateError is returned by DeserializeConversationState when the
// input is not well-formed JSON, or is missing/has the wrong type for a
// required ConversationState field.
type InvalidStateError struct {
	Message string
}

func (e *InvalidStateError) Error() string { return e.Message }

func joinInts(vals []int) string {
	out := ""
	for i, v := range vals {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%d", v)
	}
	return out
}

// SerializeConversationState serializes state to a stable JSON string for
// durable storage. Guarantees the version field is present (injects
// ConversationStateVersion when the input state lacks one). Treat the
// returned JSON as opaque: round-trip via SerializeConversationState /
// DeserializeConversationState rather than introspecting the shape directly.
//
// Compat policy: additive field changes within a major version. On version
// bumps, migrations run inside DeserializeConversationState. The
// StateAccessor Load/Save contract is unchanged — these helpers are opt-in.
func SerializeConversationState(state ConversationState) (string, error) {
	toEncode := state
	if toEncode.Version == 0 {
		toEncode.Version = ConversationStateVersion
	}
	b, err := json.Marshal(toEncode)
	if err != nil {
		return "", fmt.Errorf("serialize conversation state: %w", err)
	}
	return string(b), nil
}

// DeserializeConversationState parses and validates a previously serialized
// ConversationState.
//
// Accepts version-less legacy blobs and states with version 1, normalizing
// both to ConversationStateVersion. Returns *UnsupportedStateVersionError for
// any other version (fail loudly so stores don't silently misinterpret
// future shapes) and *InvalidStateError for malformed JSON or missing
// required fields (id, messages, status, createdAt, updatedAt).
//
// Compat policy: absence of version means v1. Migrations for future
// versions happen here; callers should treat the JSON as opaque.
func DeserializeConversationState(data string) (ConversationState, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf("invalid ConversationState JSON: %v", err)}
	}

	// Version check runs before structural validation: a future-version blob
	// may have a different shape, and it must fail with
	// UnsupportedStateVersionError rather than a misleading InvalidStateError
	// about v1 fields.
	if rawVersion, present := raw["Version"]; present {
		version, ok := rawVersion.(float64)
		if !ok {
			return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState field "Version" must be a number when present (got %s)`, describeJSONType(rawVersion))}
		}
		if int(version) != ConversationStateVersion {
			return ConversationState{}, &UnsupportedStateVersionError{Found: int(version), Supported: []int{ConversationStateVersion}}
		}
	}

	if _, ok := raw["ID"].(string); !ok {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState missing or invalid field "ID" (expected string, got %s)`, describeJSONType(raw["ID"]))}
	}
	if _, ok := raw["Messages"].([]any); !ok {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState missing or invalid field "Messages" (expected array, got %s)`, describeJSONType(raw["Messages"]))}
	}
	if _, ok := raw["Status"].(string); !ok {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState missing or invalid field "Status" (expected string, got %s)`, describeJSONType(raw["Status"]))}
	}
	if _, ok := raw["CreatedAt"].(float64); !ok {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState missing or invalid field "CreatedAt" (expected number, got %s)`, describeJSONType(raw["CreatedAt"]))}
	}
	if _, ok := raw["UpdatedAt"].(float64); !ok {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf(`ConversationState missing or invalid field "UpdatedAt" (expected number, got %s)`, describeJSONType(raw["UpdatedAt"]))}
	}

	var state ConversationState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return ConversationState{}, &InvalidStateError{Message: fmt.Sprintf("invalid ConversationState JSON: %v", err)}
	}
	state.Version = ConversationStateVersion
	return state, nil
}

func describeJSONType(value any) string {
	if value == nil {
		return "null"
	}
	switch value.(type) {
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func UpdateState(state ConversationState, mutate func(*ConversationState)) ConversationState {
	copyState := state
	copyState.Messages = append([]components.InputsUnion1{}, state.Messages...)
	copyState.PendingToolCalls = append([]ParsedToolCall{}, state.PendingToolCalls...)
	copyState.UnsentToolResults = append([]UnsentToolResult{}, state.UnsentToolResults...)
	if state.DoomLoop != nil {
		doomLoop := cloneDoomLoopState(*state.DoomLoop)
		copyState.DoomLoop = &doomLoop
	}
	mutate(&copyState)
	copyState.UpdatedAt = time.Now().UnixMilli()
	return copyState
}

func AppendToMessages(state ConversationState, items ...components.InputsUnion1) ConversationState {
	return UpdateState(state, func(s *ConversationState) { s.Messages = append(s.Messages, items...) })
}

func ToolRequiresApproval(ctx context.Context, t Tool, call ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (bool, error) {
	if IsServerTool(t) {
		return false, nil
	}
	if callLevel != nil {
		return callLevel(ctx, call, turn)
	}
	return t.RequiresApproval(ctx, call, turn)
}

func PartitionToolCalls(ctx context.Context, tools []Tool, calls []ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (approved []ParsedToolCall, pending []ParsedToolCall, err error) {
	for _, call := range calls {
		t := FindToolByName(tools, call.Name)
		if t == nil {
			approved = append(approved, call)
			continue
		}
		req, err := ToolRequiresApproval(ctx, t, call, turn, callLevel)
		if err != nil {
			return nil, nil, err
		}
		if req {
			pending = append(pending, call)
		} else {
			approved = append(approved, call)
		}
	}
	return approved, pending, nil
}

func CreateUnsentResult(call ParsedToolCall, output any) UnsentToolResult {
	return UnsentToolResult{CallID: call.CallID, Name: call.Name, Output: output}
}
func CreateRejectedResult(call ParsedToolCall, reason string) UnsentToolResult {
	return UnsentToolResult{CallID: call.CallID, Name: call.Name, Error: reason}
}

func UnsentResultsToAPIFormat(results []UnsentToolResult) []components.InputsUnion1 {
	items, err := UnsentResultsToAPIFormatWithError(results)
	if err != nil {
		return nil
	}
	return items
}

func UnsentResultsToAPIFormatWithError(results []UnsentToolResult) ([]components.InputsUnion1, error) {
	items := make([]components.InputsUnion1, 0, len(results))
	for _, r := range results {
		out := r.Output
		if r.Error != "" {
			out = map[string]any{"error": r.Error}
		}
		var s string
		if str, ok := out.(string); ok {
			s = str
		} else {
			b, err := json.Marshal(out)
			if err != nil {
				return nil, fmt.Errorf("marshal unsent tool output for %s: %w", r.CallID, err)
			}
			s = string(b)
		}
		item := components.FunctionCallOutputItem{CallID: r.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(s), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}
		items = append(items, components.CreateInputsUnion1FunctionCallOutputItem(item))
	}
	return items, nil
}

func cloneDoomLoopState(s DoomLoopSerializedState) DoomLoopSerializedState {
	out := s
	out.Tools = make(map[string]DoomLoopStreak, len(s.Tools))
	for k, v := range s.Tools {
		out.Tools[k] = v
	}
	if s.Text != nil {
		text := *s.Text
		out.Text = &text
	}
	if s.StopVerdict != nil {
		v := *s.StopVerdict
		out.StopVerdict = &v
	}
	out.PendingSteer = append([]string(nil), s.PendingSteer...)
	return out
}
