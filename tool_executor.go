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
	ToolCall ParsedToolCall
	Turn     TurnContext
	Context  map[string]any
	Shared   map[string]any
	Store    *ToolContextStore
	Emit     func(any)
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
	raw := json.RawMessage("{}")
	if call.RawArgs != "" {
		raw = json.RawMessage(call.RawArgs)
	} else if call.Arguments != nil {
		b, err := json.Marshal(call.Arguments)
		if err != nil {
			return ToolExecutionResult{}, fmt.Errorf("marshal tool arguments for %s: %w", call.Name, err)
		}
		raw = b
	}
	execCtx.ToolCall = call
	result, err := exec.Execute(ctx, raw, execCtx)
	if err != nil {
		return result, err
	}
	result.Source = source
	return result, nil
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
	return validateValue(value, schema, "$", true)
}

func validateValue(value any, schema map[string]any, path string, requireRequired bool) error {
	if schema == nil {
		return nil
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
						if err := validateValue(child, propMap, path+"."+k, true); err != nil {
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
					if err := validateValue(child, itemSchema, fmt.Sprintf("%s[%d]", path, i), true); err != nil {
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
