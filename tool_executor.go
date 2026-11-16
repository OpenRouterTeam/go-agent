package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

var ErrManualTool = errors.New("manual tool is not executable")
var ErrHITLPause = errors.New("hitl tool paused awaiting human response")

type ToolExecuteContext struct {
	// CallID is the executing call's id; ConversationID is the run's
	// ConversationState.ID. Cancellation arrives through the context.Context
	// passed to the tool body (cancelled by ModelResult.Cancel, the run's
	// context, or the tool's Timeout).
	CallID         string
	ConversationID string
	ToolCall       ParsedToolCall
	Turn           TurnContext
	Context        map[string]any
	Shared         map[string]any
	Store          *ToolContextStore
	Emit           func(any)
}

func ConvertToolToAPI(t Tool) components.ResponsesRequestToolUnion { return t.ToAPITool() }

func ParseToolArguments(raw string) (json.RawMessage, any, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return json.RawMessage(raw), raw, err
	}
	return json.RawMessage(raw), v, nil
}

func ExecuteTool(ctx context.Context, t Tool, call ParsedToolCall, execCtx ToolExecuteContext) (ToolExecutionResult, error) {
	source := ToolSourceClient
	if IsMcpTool(t) {
		source = ToolSourceMCP
	}
	exec, ok := t.(ToolWithExecute)
	if !ok {
		return ToolExecutionResult{CallID: call.CallID, Name: call.Name, Source: source, Error: ErrManualTool}, nil
	}
	raw, err := callArgumentsJSON(call)
	if err != nil {
		return ToolExecutionResult{}, err
	}
	execCtx.ToolCall = call
	result, err := exec.Execute(ctx, raw, execCtx)
	if err != nil {
		return result, err
	}
	result.Source = source
	return result, nil
}

// hasUnparsedArguments reports whether call carries arguments the model
// emitted as invalid JSON (kept as the raw string). Such calls skip hooks
// and approval preparation, and execution reports a parse error.
func hasUnparsedArguments(call ParsedToolCall) bool {
	if _, isString := call.Arguments.(string); !isString && call.Arguments != nil {
		return false
	}
	return call.RawArgs != "" && !json.Valid([]byte(call.RawArgs))
}

// callArgumentsJSON returns the call's arguments as JSON: RawArgs when
// present, otherwise the encoded Arguments, defaulting to `{}`.
func callArgumentsJSON(call ParsedToolCall) (json.RawMessage, error) {
	if call.RawArgs != "" {
		return json.RawMessage(call.RawArgs), nil
	}
	if call.Arguments == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(call.Arguments)
	if err != nil {
		return nil, fmt.Errorf("marshal tool arguments for %s: %w", call.Name, err)
	}
	return b, nil
}

// unwrapTool strips additive wrappers (the MCP brand) so optional
// capabilities implemented by the concrete tool are reachable.
func unwrapTool(t Tool) Tool {
	for {
		b, ok := t.(mcpBrand)
		if !ok {
			return t
		}
		t = b.Tool
	}
}

// AssertUniqueClientToolNames returns an error when two client tools share
// a name. Calls are dispatched by name, so a duplicate would let whichever
// tool is listed first receive arguments meant for the other. Server tools
// have no client name and are skipped.
func AssertUniqueClientToolNames(tools []Tool) error {
	if name, ok := FindDuplicateClientToolName(tools); ok {
		return fmt.Errorf("duplicate tool name %q: every tool passed to CallModel must have a unique name; rename one, or use a name prefix when combining MCP tools with local tools", name)
	}
	return nil
}

// FindDuplicateClientToolName returns the first client tool name that
// appears more than once.
func FindDuplicateClientToolName(tools []Tool) (string, bool) {
	seen := map[string]bool{}
	for _, t := range tools {
		if t == nil || IsServerTool(t) {
			continue
		}
		name := t.ToolName()
		if seen[name] {
			return name, true
		}
		seen[name] = true
	}
	return "", false
}

func SanitizeSchema(schema map[string]any) map[string]any {
	out := copyMap(schema)
	stripTildeKeys(out)
	return out
}

func stripTildeKeys(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if strings.HasPrefix(k, "~") {
				delete(x, k)
				continue
			}
			stripTildeKeys(child)
		}
	case []any:
		for _, child := range x {
			stripTildeKeys(child)
		}
	}
}

func ValidateAgainstSchema(raw json.RawMessage, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return validateValue(value, schema, "$", true, schema, 0)
}

// resolveLocalRef resolves a local "#/$defs/Name" (or "#/definitions/Name")
// reference against root.
func resolveLocalRef(ref string, root map[string]any) map[string]any {
	for _, prefix := range []string{"#/$defs/", "#/definitions/"} {
		if strings.HasPrefix(ref, prefix) {
			defs, _ := root[strings.TrimSuffix(strings.TrimPrefix(prefix, "#/"), "/")].(map[string]any)
			def, _ := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
			return def
		}
	}
	return nil
}

func validateValue(value any, schema map[string]any, path string, requireRequired bool, root map[string]any, depth int) error {
	if schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok && depth < 64 {
		if target := resolveLocalRef(ref, root); target != nil {
			if err := validateValue(value, target, path, requireRequired, root, depth+1); err != nil {
				return err
			}
		}
	}
	if typ, _ := schema["type"].(string); typ != "" {
		switch typ {
		case "object":
			m, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: expected object", path)
			}
			if requireRequired {
				if req, ok := schema["required"].([]any); ok {
					for _, r := range req {
						key, _ := r.(string)
						if _, exists := m[key]; key != "" && !exists {
							return fmt.Errorf("%s.%s: required", path, key)
						}
					}
				}
			}
			props, _ := schema["properties"].(map[string]any)
			for k, prop := range props {
				if child, exists := m[k]; exists {
					if propMap, ok := prop.(map[string]any); ok {
						if err := validateValue(child, propMap, path+"."+k, true, root, depth+1); err != nil {
							return err
						}
					}
				}
			}
		case "string":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s: expected string", path)
			}
		case "number":
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("%s: expected number", path)
			}
		case "integer":
			f, ok := value.(float64)
			if !ok || f != float64(int64(f)) {
				return fmt.Errorf("%s: expected integer", path)
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s: expected boolean", path)
			}
		case "array":
			arr, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s: expected array", path)
			}
			if itemSchema, ok := schema["items"].(map[string]any); ok {
				for i, child := range arr {
					if err := validateValue(child, itemSchema, fmt.Sprintf("%s[%d]", path, i), true, root, depth+1); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func FormatToolOutput(result ToolExecutionResult) components.FunctionCallOutputItem {
	out, err := FormatToolOutputWithError(result)
	if err != nil {
		return components.FunctionCallOutputItem{CallID: result.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(fmt.Sprintf(`{"error":%q}`, err.Error())), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}
	}
	return out
}

func FormatToolOutputWithError(result ToolExecutionResult) (components.FunctionCallOutputItem, error) {
	output := result.Output
	if result.Error != nil {
		output = map[string]any{"error": result.Error.Error()}
	} else if content, ok := output.([]ToolOutputContentItem); ok {
		// A ToModelOutput "content" result is sent as the array form.
		return components.FunctionCallOutputItem{CallID: result.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2ArrayOfFunctionCallOutputItemOutputUnion1(content), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}, nil
	}
	var s string
	if str, ok := output.(string); ok {
		s = str
	} else {
		b, err := json.Marshal(output)
		if err != nil {
			return components.FunctionCallOutputItem{}, fmt.Errorf("marshal tool output for %s: %w", result.CallID, err)
		}
		s = string(b)
	}
	return components.FunctionCallOutputItem{CallID: result.CallID, Output: components.CreateFunctionCallOutputItemOutputUnion2Str(s), Type: components.FunctionCallOutputItemTypeFunctionCallOutputFunctionCallOutput}, nil
}
