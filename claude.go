package agent

const (
	ClaudeContentBlockTypeText               = "text"
	ClaudeContentBlockTypeToolUse            = "tool_use"
	ClaudeContentBlockTypeThinking           = "thinking"
	ClaudeContentBlockTypeUnsupportedContent = "unsupported_content"
	NonClaudeMessageRoleDeveloper            = "developer"
	NonClaudeMessageRoleSystem               = "system"
)

type ClaudeContentBlockType string
type NonClaudeMessageRole string

func IsClaudeStyleMessages(v any) bool { _, ok := v.([]ClaudeMessage); return ok }
