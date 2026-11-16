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
	mutate(&copyState)
	copyState.UpdatedAt = time.Now().UnixMilli()
	return copyState
}

func AppendToMessages(state ConversationState, items ...components.InputsUnion1) ConversationState {
	return UpdateState(state, func(s *ConversationState) { s.Messages = append(s.Messages, items...) })
}

// ToolRequiresApproval reports whether call needs human approval.
//
// A call-level check always takes precedence; it receives the arguments
// normalized exactly as the tool's Execute would decode them when they are
// valid, and the raw call otherwise. A tool-level approval predicate
// (ToolConfig.Approval) receives the normalized arguments too, and fails
// closed (requires approval) when they do not decode/validate or are not an
// object: there is no trustworthy value for it to judge, and a PreToolUse
// hook may later repair the input into an executable call. t may be nil
// (an unknown tool name), in which case only a call-level check applies.
func ToolRequiresApproval(ctx context.Context, t Tool, call ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (bool, error) {
	if t != nil && IsServerTool(t) {
		return false, nil
	}
	if callLevel != nil {
		if t != nil {
			if norm, ok := normalizedToolCall(t, call); ok {
				return callLevel(ctx, norm, turn)
			}
		}
		return callLevel(ctx, call, turn)
	}
	if t == nil {
		return false, nil
	}
	if hasToolApprovalPredicate(t) {
		norm, ok := normalizedToolCall(t, call)
		if !ok {
			return true, nil
		}
		return t.RequiresApproval(ctx, norm, turn)
	}
	return t.RequiresApproval(ctx, call, turn)
}

// normalizedToolCall returns a copy of call whose Arguments are the values
// the tool body would receive. ok is false when the tool cannot normalize
// (not a typed tool) or the arguments are invalid / not an object.
func normalizedToolCall(t Tool, call ParsedToolCall) (ParsedToolCall, bool) {
	n, ok := unwrapTool(t).(interface {
		normalizeArguments(json.RawMessage) (map[string]any, bool)
	})
	if !ok {
		return call, false
	}
	raw, err := callArgumentsJSON(call)
	if err != nil {
		return call, false
	}
	args, ok := n.normalizeArguments(raw)
	if !ok {
		return call, false
	}
	norm := call
	norm.Arguments = args
	norm.RawArgs = ""
	return norm, true
}

// hasBlanketApproval reports whether t requires approval unconditionally
// (ToolConfig.RequireApproval without a predicate).
func hasBlanketApproval(t Tool) bool {
	b, ok := unwrapTool(t).(interface{ hasBlanketApproval() bool })
	return ok && b.hasBlanketApproval()
}

func hasToolApprovalPredicate(t Tool) bool {
	p, ok := unwrapTool(t).(interface{ hasApprovalPredicate() bool })
	return ok && p.hasApprovalPredicate()
}

func PartitionToolCalls(ctx context.Context, tools []Tool, calls []ParsedToolCall, turn TurnContext, callLevel ToolApprovalCheck) (approved []ParsedToolCall, pending []ParsedToolCall, err error) {
	for _, call := range calls {
		t := FindToolByName(tools, call.Name)
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

// IsContentArray reports whether value is a non-empty array of tool output
// content parts (input_text / input_image / input_file), which is sent to the
// model as the array form of function_call_output rather than JSON-encoded.
func IsContentArray(value any) bool {
	_, ok := asContentArray(value)
	return ok
}

// asContentArray returns value as content parts. It accepts the typed form
// produced by ToModelOutput and the generic []any form a content array takes
// after a ConversationState JSON round trip.
func asContentArray(value any) ([]ToolOutputContentItem, bool) {
	switch v := value.(type) {
	case []ToolOutputContentItem:
		return v, len(v) > 0
	case []any:
		if len(v) == 0 {
			return nil, false
		}
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			switch m["type"] {
			case "input_text", "input_image", "input_file":
			default:
				return nil, false
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var items []ToolOutputContentItem
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, false
		}
		return items, true
	}
	return nil, false
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
		} else if content, ok := asContentArray(out); ok {
			item := components.FunctionCallOutputItem{CallID: r.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2ArrayOfFunctionCallOutputItemOutputUnion1(content), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}
			items = append(items, components.CreateInputsUnion1FunctionCallOutputItem(item))
			continue
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
