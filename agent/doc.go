// Package agent provides a stateful agent runtime for LLM interactions.
//
// The package tracks the reference TypeScript implementation in the colocated
// `*.ts` sources and aims for API and behavioral parity where that translates
// cleanly into Go.
//
// Go-idiomatic parity helpers:
//   - `Complete(ctx, prompt, tools...)`
//   - `WithInitialMessages(...)`
//   - `WithStreamFn(...)`
//   - `Agent.SetModel`, `Agent.SetTools`, `Agent.ReplaceMessages`
//
// Intentional Go-specific additions that remain:
//   - `State.Error` alongside `State.ErrorMessage`
//   - `State.PendingToolIDs` plus `State.PendingToolCalls()` snapshot helper
//   - Built-in filesystem-oriented tools in `tools_builtin.go`
//
// The current documented parity deltas are:
//   - Pending tool tracking is stored as a Go map instead of a TypeScript set.
//   - In parallel tool execution mode, `ToolEnd` events are emitted in
//     completion order rather than assistant source order.
package agent // import "github.com/losinggeneration/tau/agent"
