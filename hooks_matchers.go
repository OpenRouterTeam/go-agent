package agent

import "regexp"

// MatchesTool evaluates a ToolMatcher against a tool name.
//
//   - nil -> wildcard, matches all tools
//   - string -> exact match
//   - *regexp.Regexp -> MatchString
//   - func(string) bool -> arbitrary predicate
//
// Unlike upstream's JS RegExp (whose `/g`/`/y` flags advance `lastIndex`
// across calls, making repeated `.test()` calls alternate true/false), Go's
// regexp.Regexp has no such stateful footgun, so MatchesTool needs no
// lastIndex reset — it is stateless by construction.
func MatchesTool(matcher ToolMatcher, toolName string) bool {
	switch m := matcher.(type) {
	case nil:
		return true
	case string:
		return m == toolName
	case *regexp.Regexp:
		return m.MatchString(toolName)
	case func(string) bool:
		return m(toolName)
	default:
		return false
	}
}
