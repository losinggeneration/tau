// Package ai provides the core model, message, tool, and client types used by
// tau.
//
// # Public Parity Surface
//
// The `ai` package aims for behavioral and surface parity with
// `pi-mono/packages/ai` unless a divergence is explicitly documented as
// intentional. The following summarizes the parity relationship.
//
// ## Directly Mirrored Exports
//
// The following are re-exported from the top-level `ai` package with the same
// capability as the pi-mono `index.ts` re-exports:
//
//   - Provider constants and model metadata access: `ProviderOpenAI`,
//     `ProviderAnthropic`, `ProviderGoogle`, `ProviderMistral`, `ProviderAzure`,
//     `ProviderGoogleVertex`, `ProviderOpenAICodex`, and the `Model` type with
//     full metadata (ID, provider, API, capabilities, pricing).
//   - Core message and content types: `Message`, `Context`, `ContentBlock`,
//     `ToolCall`, `Tool`, `AssistantMessage`, `Usage`, `Cost`.
//   - Stream protocol: `StreamEvent`, `Start`, `Done`, `StreamError`,
//     `TextStart`, `TextDelta`, `TextEnd`, `ThinkingStart`, `ThinkingDelta`,
//     `ThinkingEnd`, `ToolCallStart`, `ToolCallDelta`, `ToolCallEnd`.
//   - Validation: `ValidateToolCall`, `ValidateToolArguments`, `ValidationError`,
//     `ValidationResult`.
//   - Overflow detection: `IsContextOverflow`.
//   - Partial JSON parsing: `PartialJSONParser`, `ExtractValidJSON`.
//   - Utilities: `SanitizeSurrogates`, `ShortHash`.
//   - Faux provider: `FauxProvider` for deterministic in-process testing.
//   - OAuth: `oauth.Provider` and the concrete OAuth provider implementations.
//   - Top-level stream helpers: `Stream`, `Complete`, `StreamSimple`,
//     `CompleteSimple`.
//   - Thinking budget helpers: `ClampReasoning`, `ClampReasoningForModel`,
//     `ThinkingBudgetForLevel`, `AdjustMaxTokensForThinking`,
//     `AdjustMaxTokensForThinkingForModel`, `DefaultThinkingBudgets`.
//
// ## Go-Idiomatic Additions
//
// The following are Go-specific additions not present in pi-mono:
//
//   - `Model.Output []OutputType`: Go-only capability annotation for model
//     output modality tracking.
//   - `Tool.Parameters json.RawMessage` and `ToolCall.Arguments json.RawMessage`:
//     Go uses typed JSON bytes for type safety; wire serialization is identical
//     to pi-mono's `Record<string, any>`.
//   - `TransformContext`, `SerializeContext`, `DeserializeContext`,
//     `CloneContext`: Go additions for agent context serialization and replay.
//     pi-mono has no direct equivalent.
//   - `GetEnvAPIKey(provider)`: Go-specific env-var resolution helper with the
//     same priority ordering as pi-mono's `getEnvApiKey`.
//
// ## Intentional Divergences
//
// The following are documented Go divergences from pi-mono:
//
//   - Registry strategy: Go exposes both the TS-style full generated registry
//     (`ListProviders`, `ListModels`, `GetModel`) and explicit support-taxonomy
//     helpers (`ListSupportedProviders`, `ListSupportedModels`,
//     `GetSupportedModel`, `ProviderSupport`, `ModelSupport`) for callers that
//     need a narrower defended support surface.
//   - Timestamp semantics: `ai.Message.Timestamp` and
//     `ai.AssistantMessage.Timestamp` are Unix milliseconds at the public
//     boundary, not seconds.
//   - `Usage`/`Cost` zero-value JSON: zero-value `Usage` fields and nested
//     `Cost` fields are preserved in JSON output with the full stable shape.
//   - OAuth helpers: the `oauth` package includes both provider primitives and
//     convenience helpers that refresh expired tokens on lookup
//     (`CredentialsStore.GetAPIKey`, `GetOAuthAPIKey`, `ResolveOAuthAPIKey`).
//   - Tool schema validation: Go uses `gojsonschema` instead of Ajv; error
//     messages differ but validation behavior is equivalent.
//
// ## Utility Surface
//
// The `ai` package public utility surface corresponds to pi-mono's exported
// utility modules:
//
//   - `event-stream`: represented by the stream event protocol types above.
//   - `json-parse`: `PartialJSONParser` and `ExtractValidJSON` in `ai/util.go`.
//   - `validation`: `ValidateToolCall` and `ValidateToolArguments` in
//     `ai/tools.go`.
//   - `overflow`: `IsContextOverflow` in `ai/overflow.go`.
//   - `hash`: `ShortHash` in `ai/util.go`.
//   - `sanitize-unicode`: `SanitizeSurrogates` in `ai/util.go`.
//   - `typebox-helpers`: no direct Go counterpart; Go uses `json.RawMessage`
//     for schema-carrying fields without a separate schema-type layer.
//
// Public support is intentionally narrower than the set of provider constants,
// internal adapters, or in-repo model metadata. The defended support surface is
// the subset that is implemented, registered, reachable through GetClient,
// exposed through the public model lookup helpers where applicable, and covered
// by targeted tests. Callers that need the current support taxonomy should use
// ProviderSupport, ListProviderSupport, ModelSupport, or ListModelSupport
// instead of inferring support from internal registry breadth alone.
//
// The generated registry is still available through GetAvailableModel,
// ListAvailableModels, and ListAvailableProviders for callers that need to
// inspect models outside the defended support surface.
package ai // import "github.com/losinggeneration/tau/ai"
