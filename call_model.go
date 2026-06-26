package agent

// DefaultMaxTurns matches the upstream agent loop default: execute at most five
// tool turns unless the caller provides a smaller stop condition or larger cap.
const DefaultMaxTurns = 5
