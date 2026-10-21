package agent

import "log"

// InlineHookConfig is a lightweight alternative to constructing a
// HooksManager by hand: pass built-in hook entries directly on
// CallModelInput.Hooks. Only built-in hooks are supported inline; register
// custom hooks through a HooksManager instance via On.
type InlineHookConfig struct {
	PreToolUse         []HookEntry[PreToolUsePayload, PreToolUseResult]
	PostToolUse        []HookEntry[PostToolUsePayload, EmptyHookResult]
	PostToolUseFailure []HookEntry[PostToolUseFailurePayload, EmptyHookResult]
	UserPromptSubmit   []HookEntry[UserPromptSubmitPayload, UserPromptSubmitResult]
	Stop               []HookEntry[StopPayload, StopResult]
	PermissionRequest  []HookEntry[PermissionRequestPayload, PermissionRequestResult]
	SessionStart       []HookEntry[SessionStartPayload, EmptyHookResult]
	SessionEnd         []HookEntry[SessionEndPayload, EmptyHookResult]
	PostModelCall      []HookEntry[PostModelCallPayload, EmptyHookResult]
	DoomLoopDetected   []HookEntry[DoomLoopDetectedPayload, DoomLoopDetectedResult]
}

// ResolveHooks normalizes a CallModelInput.Hooks value into a *HooksManager.
//
//   - nil -> nil (no hooks)
//   - *HooksManager -> passthrough
//   - InlineHookConfig -> construct a HooksManager and register every entry
//
// Any other value logs a warning and is treated as nil, mirroring upstream's
// defensive handling of a config value that bypassed the typed surface.
func ResolveHooks(hooks any) *HooksManager {
	if hooks == nil {
		return nil
	}
	switch v := hooks.(type) {
	case *HooksManager:
		return v
	case InlineHookConfig:
		manager := NewHooksManager()
		for _, e := range v.PreToolUse {
			manager.OnPreToolUse(e)
		}
		for _, e := range v.PostToolUse {
			manager.OnPostToolUse(e)
		}
		for _, e := range v.PostToolUseFailure {
			manager.OnPostToolUseFailure(e)
		}
		for _, e := range v.UserPromptSubmit {
			manager.OnUserPromptSubmit(e)
		}
		for _, e := range v.Stop {
			manager.OnStop(e)
		}
		for _, e := range v.PermissionRequest {
			manager.OnPermissionRequest(e)
		}
		for _, e := range v.SessionStart {
			manager.OnSessionStart(e)
		}
		for _, e := range v.SessionEnd {
			manager.OnSessionEnd(e)
		}
		for _, e := range v.PostModelCall {
			manager.OnPostModelCall(e)
		}
		for _, e := range v.DoomLoopDetected {
			manager.OnDoomLoopDetected(e)
		}
		return manager
	default:
		log.Printf("[ResolveHooks] Ignoring CallModelInput.Hooks of unsupported type %T; expected *HooksManager or InlineHookConfig.", hooks)
		return nil
	}
}
