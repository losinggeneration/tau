// Package sdk provides the public API surface for embedding tau as a Go library.
// It defines Session and Runtime interfaces and concrete implementations that
// wrap the agent/ package.
package sdk // import "github.com/losinggeneration/tau/sdk"

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/losinggeneration/tau/agent/codemode"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/compaction"
	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/agent/plugins"
	"github.com/losinggeneration/tau/agent/plugins/proto"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
	"github.com/losinggeneration/tau/ai/store"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/logging"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/ui"
)

// APIKeySetter is implemented by clients that accept an API key at runtime.
// This allows the SDK to inject credentials from the credential store into
// provider clients that were created without an API key.
// TransformContextFunc rewrites the message context before model conversion.
type TransformContextFunc = agent.TransformContextFunc

// APIKeySetter is implemented by clients that accept an API key at runtime.
type APIKeySetter interface {
	SetAPIKey(key string)
}

var (
	// envOverrideLoggedOncePtr gates the environment-credential notice. It is a
	// pointer so tests can reset the gate without racing with sync.Once internals.
	envOverrideLoggedOncePtr = &sync.Once{}

	// ErrSessionNotFound is returned when a caller attempts to resume a session
	// that does not exist in session storage.
	ErrSessionNotFound = errors.New("session not found")
	// ErrVisionNotSupported is returned when image input is sent to a model that
	// does not advertise vision capability.
	ErrVisionNotSupported = errors.New("vision input not supported by active model")
)

// Event mirrors the JSON event types from the modes/SDK spec — same set,
// same field names. cmd/tau --json is a thin marshaller from the channel
// onto JSONL.
type Event struct {
	// Type identifies the event (e.g. "session_start", "text_delta", "message_end").
	Type string `json:"type"`
	// SessionID is the session this event belongs to.
	SessionID string `json:"session_id,omitempty"`
	// Seq is a monotonic counter per-session, starting at 0.
	Seq int64 `json:"seq"`
	// Timestamp is RFC 3339 formatted.
	Timestamp string `json:"ts"`
	// Payload carries type-specific data.
	Payload any `json:"payload,omitempty"`
}

// Message is a message sent to a Session.
type Message struct {
	StructuredContent  json.RawMessage
	NestedCalls        []ai.NestedToolCall
	ToolUsage          *ai.Usage
	ExcludeFromContext bool
	// DurationMs is execution-only elapsed time, absent when execution never began.
	DurationMs *float64
	CustomType string
	Display    bool
	Details    any
	// Data identifies extension-originated custom messages.
	Data any
	// Role is the message role (user, assistant, tool, system).
	Role string
	// Text is the text content.
	Text string
	// Content preserves structured message blocks for replay and UI rendering.
	Content []ai.ContentBlock
	// Images are optional image content blocks.
	Images []ai.ContentBlock
	// AttachmentIDs references session-scoped attachments to include with this message.
	AttachmentIDs []string
	// ToolCallID identifies the tool call this message responds to, for tool messages.
	ToolCallID string
	// ToolName is the tool name for tool messages.
	ToolName string
	// IsError reports whether a tool message represents an error result.
	IsError bool
	// SystemAddendum is appended to the system prompt as additional context.
	// Used for skill content injection.
	SystemAddendum string
}

// SessionState holds the read-only state of a session.
type SessionState struct {
	id          string
	model       string
	cwd         string
	dataDir     string
	isStreaming bool
	totalTokens int
	// position is the current session cursor: the entry ID of the current head.
	// This is the ID of the most recent message or custom_message entry on
	// the active branch, respecting position_change entries per the session
	// tree model spec.
	position string

	// sessionPath, when set, resolves position lazily on Position(). The
	// cursor needs the whole session log parsed, which on a long session costs
	// hundreds of milliseconds and the file's size in garbage, and most State
	// callers only want the model or working directory.
	sessionPath string
}

// SessionID returns the session identifier.
func (s SessionState) SessionID() string { return s.id }

// Model returns the active model identifier.
func (s SessionState) Model() string { return s.model }

// CWD returns the session working directory.
func (s SessionState) CWD() string { return s.cwd }

// DataDir returns the session-owned directory for transient files.
func (s SessionState) DataDir() string { return s.dataDir }

// IsStreaming reports whether the session is currently processing.
func (s SessionState) IsStreaming() bool { return s.isStreaming }

// TotalTokens returns the cumulative token count.
func (s SessionState) TotalTokens() int { return s.totalTokens }

// Position returns the current session cursor: the entry ID of the
// current head on the active branch. Returns empty string if no messages.
func (s SessionState) Position() string {
	if s.position != "" || s.sessionPath == "" {
		return s.position
	}
	reader, err := sessionpkg.ReadFile(s.sessionPath)
	if err != nil {
		return ""
	}
	return reader.CurrentPosition()
}

// NewSessionState constructs a SessionState with the given core fields.
// Provided for tests and callers that need to surface session state outside
// the runtime (e.g. mock sessions).
func NewSessionState(id, model, cwd string) SessionState {
	return SessionState{
		id:    id,
		model: model,
		cwd:   cwd,
	}
}

// NewSessionStateWithDataDir constructs a SessionState with a session data directory.
func NewSessionStateWithDataDir(id, model, cwd, dataDir string) SessionState {
	return SessionState{
		id:      id,
		model:   model,
		cwd:     cwd,
		dataDir: dataDir,
	}
}

// SessionOptions configures a new session.
type SessionOptions struct {
	// Model is the model specifier ("<provider>/<model>", legacy three-segment form, or shortcut).
	Model string
	// CWD is the working directory for the session.
	CWD string
	// UI is the user interface. Defaults to NoOpUI.
	UI ui.UI
	// Store is the credential store. If nil, uses runtime's store.
	Store store.CredentialStore
	// Settings is the agent settings. If nil, uses runtime's settings.
	Settings *ai.Settings
	// SessionID is empty for a new session, non-empty to resume.
	SessionID string
	// ForkFrom is the session ID to fork from. When set, creates a new session
	// that inherits the parent session's message history and records the
	// parent relationship in the new session's header.
	ForkFrom string
	// ForkFromEntry is the optional parent entry ID to fork from within ForkFrom.
	ForkFromEntry string
	// NoSession makes the session ephemeral with no persisted JSONL output.
	NoSession bool
	// EphemeralExportPath is a path for exporting ephemeral sessions.
	// When set (along with NoSession), creates a temporary session file
	// for export purposes. The caller is responsible for cleaning up
	// this file after export.
	EphemeralExportPath string
	// ThinkingLevel sets the agent's reasoning/thinking level for the session.
	// Supported values: "off", "minimal", "low", "medium", "high", "xhigh".
	ThinkingLevel string
	// DisplayName sets the session display name. If empty, the slug is derived
	// from the first user message.
	DisplayName string

	// Logger receives diagnostic messages for this session. Nil inherits the
	// runtime logger; if neither is set, diagnostics are discarded.
	Logger *slog.Logger

	// SystemPrompt overrides the resolved system prompt entirely. When
	// non-empty, this replaces the resolved prompt (including SYSTEM.md).
	// Empty means "use the resolved default." Per-session because the
	// system prompt is conversation identity.
	SystemPrompt string
	// AppendSystemPrompts are additional system prompt fragments appended
	// after the resolved system prompt and any SYSTEM.md content. They
	// stack in order; empty entries are skipped. Nil discovers APPEND_SYSTEM.md;
	// a non-nil slice replaces discovery. Entries and SystemPrompt may name files.
	AppendSystemPrompts []string

	// ToolAllowlist restricts the session's tool set to the named tools.
	// When non-empty, only tools whose Name is in this list are exposed
	// to the model. Plugin tools, built-in tools, and any other tool
	// source are all filtered through this list. Empty means "no filter."
	ToolAllowlist []string
	// ToolDenylist removes the named tools from the session's tool set.
	// Applied after ToolAllowlist, so a tool in both is denied.
	// Empty means "no denials."
	ToolDenylist []string

	// ApproveProjectFiles, when true, pre-approves file operations within
	// the session's CWD. Tools that prompt for path confirmation skip the
	// prompt for paths inside CWD. Per-session because approval policy is
	// a conversation-level concern.
	ApproveProjectFiles bool

	// TransformContext is an optional hook called before message conversion.
	// Use this for context window management (pruning old messages) or
	// injecting context from external sources. The hook receives the current
	// messages and a cancellation signal. Return the original or transformed
	// messages. Errors are logged and the original messages are used.
	TransformContext TransformContextFunc

	// OnPayload is an optional hook called before each provider request.
	// The function receives the raw request payload and returns the (possibly
	// modified) payload to send. This can be used for logging, debugging,
	// or payload transformation.
	OnPayload func(payload any) any

	// OnResponse is an optional hook called after each provider response.
	// The function receives the full provider response including usage metrics.
	// This can be used for response logging or metrics collection.
	OnResponse func(response ai.ProviderResponse)

	// ValidateCWD validates the session's working directory exists.
	// If true and the CWD does not exist, session creation returns an error.
	// If false, missing CWD is allowed but tools may fail when accessing it.
	ValidateCWD bool

	// IsPersisted returns whether the session has a persisted backing store.
	// This is true unless NoSession was set without EphemeralExportPath.
	IsPersisted func() bool
}

// Session is the interface for an interactive session with the agent.
type Session interface {
	// Send sends a message and returns a channel of events.
	Send(ctx context.Context, msg Message) (<-chan Event, error)
	// SendText is an ergonomic shortcut that sends a text-only message.
	SendText(ctx context.Context, text string) (<-chan Event, error)
	// Interrupt cancels the in-flight assistant response.
	Interrupt() error
	// Compact triggers session compaction. Optional customInstructions focus
	// the generated summary, matching `/compact <instructions>`.
	Compact(ctx context.Context, customInstructions ...string) error
	// State returns the current session state.
	State() SessionState
	// ID returns the session identifier. Returns empty string for ephemeral sessions
	// without an export path.
	ID() string
	// IsPersisted returns whether the session has a persisted backing store.
	// Returns true unless NoSession was set without EphemeralExportPath.
	IsPersisted() bool
	// Close releases session resources.
	Close(ctx context.Context) error
	// SetThinkingLevel changes the thinking level mid-session.
	// The next prompt will use the new level for model reasoning.
	SetThinkingLevel(level string) error
	// SetModel changes the active model for future turns. The model
	// argument is the canonical "provider/model-id" string the session
	// uses in its State().Model() (e.g. "anthropic/claude-3-5-sonnet").
	// Implementations should validate the model exists in the registry
	// and return an error otherwise; the picker UI surfaces the error.
	SetModel(model string) error
	// SlashCommands returns the set of slash commands the session can
	// dispatch (built-ins plus any commands registered by extensions,
	// prompt templates, skills, and plugins at runtime). The returned
	// slice is a snapshot safe to iterate; the session may have a
	// larger or different set by the next call. UIs use this to surface
	// completion candidates; the session itself is the authoritative
	// dispatcher, so a UI does not need to (and should not) call these
	// handlers directly.
	SlashCommands() []SlashCommandInfo
}

// SlashCommandInfo is the description of a slash command the session can
// dispatch, surfaced to frontends for completion and listing. It mirrors
// commands.Info but only the fields frontends need; the session's
// registry carries the rest.
type SlashCommandInfo struct {
	// Name is the command identifier without the leading slash.
	Name string
	// Description is a one-line summary suitable for a completion popup.
	Description string
	// ArgumentHint shows expected arguments, e.g. "<file>" or "[--force]".
	ArgumentHint string
	// Source identifies where the command was registered from (builtin,
	// prompt, skill, plugin). Frontends may use it to group or filter
	// completion entries.
	Source string
}

// Steerer queues steering messages for the active agent turn.
type Steerer interface{ Steer(Message) error }

// FollowUpper queues follow-up messages after the active agent turn.
type FollowUpper interface{ FollowUp(Message) error }

// Queuer reports the current size of the agent's steering and
// follow-up message queues. The values mirror the agent's internal
// state and are safe to call from any goroutine (the agent's queue
// mutation methods take the same lock). Sessions that don't queue
// messages don't need to implement this — frontends should do a
// type assertion and treat a non-implementing session as "no queue".
type Queuer interface {
	QueueSizes() (steering, followUp int)
}

// ModelCycler cycles the active model for future turns.
type ModelCycler interface{ CycleModel() (*ai.Model, error) }

// ThinkingCycler cycles the active thinking level for future turns.
type ThinkingCycler interface{ CycleThinkingLevel() string }

// QueueModeSetter updates steering and follow-up queue behavior.
type QueueModeSetter interface {
	SetSteeringMode(mode string) error
	SetFollowUpMode(mode string) error
}

// RetryController exposes runtime retry toggles.
type RetryController interface {
	SetAutoRetryEnabled(enabled bool)
	AbortRetry()
}

// CompactionController exposes runtime compaction toggles.
type CompactionController interface{ SetAutoCompactionEnabled(enabled bool) }

type Compactor interface {
	Compact(ctx context.Context, customInstructions ...string) error
}

// ModelLister exposes credential-scoped available models with offline fallback.
type ModelLister interface {
	GetAvailableModels(context.Context) ([]*ai.Model, error)
}

// MessageLister exposes the session's in-memory messages.
type MessageLister interface{ GetMessages() []Message }

// HistoryEntry is one displayable entry of a session's persisted history.
type HistoryEntry struct {
	EntryID string
	Message Message
}

// HistoryPager reads a session's persisted display history a page at a time,
// newest pages first, so a frontend can drop old history from memory and load
// it back when the user scrolls to it.
type HistoryPager interface {
	// History returns up to limit entries of the active branch, oldest first,
	// ending just before the entry before, or at the current head when before
	// is empty. next is the before value for the preceding page, empty once
	// the start of the history is reached.
	History(before string, limit int) (entries []HistoryEntry, next string, err error)
}

// ContextEditor changes model-visible content without changing displayed history.
// A null replacement omits the target from context.
type ContextEditor interface {
	EditContext(ctx context.Context, entryID string, replacement json.RawMessage) error
}

// FullMessageLister exposes the entire persisted transcript on the active
// branch, ignoring compaction truncation: pre-compaction messages are included
// and each compaction summary appears in place. For UIs that want to display
// full history; the agent context stays compacted.
type FullMessageLister interface{ GetAllMessages() []Message }

// ForkMessageLister exposes user messages suitable for fork pickers.
type ForkMessageLister interface{ GetUserMessagesForForking() []Message }

// ForkEntry is a user or agent message that can serve as a fork point.
type ForkEntry struct {
	// EntryID is the session entry ID to pass as SessionOptions.ForkFromEntry.
	EntryID string
	// Role is "user" or "assistant".
	Role string
	// Text is the plain-text content for list display.
	Text string
}

// ForkEntryLister exposes the current branch's forkable messages with the
// entry IDs a fork picker needs.
type ForkEntryLister interface {
	GetForkEntries() ([]ForkEntry, error)
}

// Forgetter drops messages from the session's history. GetForgetEntries
// lists every message on the current branch, including pre-compaction ones
// and compaction summaries (Role "compaction"); Forget drops the given entry
// and everything after it.
type Forgetter interface {
	GetForgetEntries() ([]ForkEntry, error)
	Forget(entryID string) error
}

// Exporter writes a session to an HTML or JSONL file and returns its path.
type Exporter interface {
	ExportHTML(ctx context.Context, path string) (string, error)
	ExportJSONL(ctx context.Context, path string) (string, error)
}

// Namer exposes the current session display name.
type Namer interface{ GetSessionName() string }

// LastAssistantTexter exposes the latest assistant text for commands/UI.
type LastAssistantTexter interface{ GetLastAssistantText() string }

// DisconnectedSessionEndEmitter emits a terminal session_end event on disconnect.
type DisconnectedSessionEndEmitter interface{ EmitDisconnectedSessionEnd(chan<- Event) }

// ArtifactLoaderProvider exposes artifact registries to UI/RPC callers.
type ArtifactLoaderProvider interface{ ArtifactLoader() ArtifactLoader }

// AttachmentManager is implemented by sessions that can import session-scoped attachments.
type AttachmentManager interface {
	ImportAttachmentFile(path string) (Attachment, error)
	ImportAttachmentData(filename, mimeType, data string) (Attachment, error)
	ListAttachments() []Attachment
	DeleteAttachment(id string) error
}

func (s *agentSession) ImportAttachmentFile(path string) (Attachment, error) {
	record, err := s.attachments.ImportFile(path)
	if err != nil {
		return Attachment{}, err
	}
	if err := s.recordAttachment(record); err != nil {
		return Attachment{}, err
	}
	return record, nil
}

func (s *agentSession) ImportAttachmentData(filename, mimeType, data string) (Attachment, error) {
	record, err := s.attachments.ImportData(filename, mimeType, data)
	if err != nil {
		return Attachment{}, err
	}
	if err := s.recordAttachment(record); err != nil {
		return Attachment{}, err
	}
	return record, nil
}

func (s *agentSession) ListAttachments() []Attachment {
	return s.attachments.List()
}

func (s *agentSession) DeleteAttachment(id string) error {
	return s.attachments.Delete(id)
}

func (s *agentSession) recordAttachment(record Attachment) error {
	if !s.IsPersisted() {
		return nil
	}
	writer, err := s.ensureSessionWriter()
	if err != nil {
		return fmt.Errorf("ensure session writer: %w", err)
	}
	if writer == nil {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal attachment record: %w", err)
	}
	entry := sessionpkg.NewCustomEntry("attachment", data)
	if err := writer.Append(entry); err != nil {
		return fmt.Errorf("append attachment record: %w", err)
	}
	return nil
}

// SessionStats contains session statistics for UI/RPC callers.
type SessionStats struct {
	SessionID      string  `json:"session_id"`
	MessageCount   int     `json:"message_count"`
	TotalTokens    int     `json:"total_tokens"`
	ContextWindow  int     `json:"context_window"`
	ContextUsedPct float64 `json:"context_used_pct"`
	Model          string  `json:"model"`
	ThinkingLevel  string  `json:"thinking_level"`
	AutoCompaction bool    `json:"auto_compaction"`
	// Usage totals every billed call recorded in the session file, across all
	// branches, including summaries.
	Usage ai.Usage `json:"usage"`
}

// Statser exposes session statistics.
type Statser interface{ Stats() SessionStats }

// EventObserver receives session events that happen outside a Send.
//
// Send returns a channel scoped to one prompt, which is the right shape for
// everything that happens during a turn. Compaction is not one of those things:
// Compact is a standalone call, and auto-retry runs between turns, so neither
// has a channel to be reported on. Without this hook a frontend cannot observe
// either — compaction_start/compaction_end previously went to the plugin
// manager alone, so the TUI could not show a compaction indicator even in
// principle (app.md §9).
//
// The observer is called synchronously on the goroutine that raised the event,
// so implementations must not block. Passing nil clears it.
type EventObserver interface {
	SetEventObserver(fn func(Event))
}

// BranchSummarizer persists summaries for abandoned session branches.
type BranchSummarizer interface {
	SummarizeBranch(ctx context.Context, sessionID, branchTip string) error
}

// RuntimeOptions configures a new Runtime.
type RuntimeOptions struct {
	// ProjectTrust controls automatic project configuration when settings are loaded.
	ProjectTrust ai.ProjectTrustOptions
	// UI is the user interface for the runtime.
	UI ui.UI
	// SessionsDir is the directory for session storage.
	SessionsDir string
	// CredentialStore is the credential storage backend.
	CredentialStore store.CredentialStore
	// Settings is the merged settings (user + project). Used for model resolution.
	Settings *ai.Settings
	// ModelRegistry supplies runtime-local models.json overrides. Nil loads the configured file.
	ModelRegistry *ai.ModelRegistry
	// ExplicitModel is an explicit model override, resolved via the auth chain.
	ExplicitModel string
	// IgnoreEnv skips environment variable credential sources.
	IgnoreEnv bool
	// PluginManager is the plugin manager for extension loading.
	// If nil, plugins are not loaded.
	PluginManager *plugins.Manager
	// CommandRegistry is the command registry for slash-command handling.
	// If nil, slash commands are not available.
	CommandRegistry commands.Registry

	// ToolControl flags for CLI parity with pi reference:
	// NoTools disables all tools.
	NoTools bool
	// NoBuiltinTools disables only built-in tools (grep, find, ls).
	NoBuiltinTools bool

	// NoContextFiles disables AGENTS.md/CLAUDE.md discovery.
	NoContextFiles bool

	// Provider forces a specific provider key (e.g. "anthropic", "openai")
	// for credential resolution. When empty, the provider is derived from
	// ExplicitModel or settings. Honored only when the model specifier
	// didn't already pin a provider.
	Provider string

	// APIKey sets an explicit API key for the resolved provider.
	// Overrides the credential store and (when IgnoreEnv is
	// false) the environment-variable fallback. Empty means "use the
	// default resolution chain."
	APIKey string

	// SessionFlags controls session behavior.
	// ForkSourceID is the session ID to fork from.
	ForkSourceID string
	// ContinueSessionID is the session ID to continue (resumable latest).
	ContinueSessionID string
	// ResumeSessionID is an explicit session ID to resume.
	ResumeSessionID string
	// NoSession disables session saving (ephemeral sessions).
	NoSession bool

	// Models is an ordered list of model patterns used to resolve an explicit
	// model choice via exact, glob, or substring matching.
	Models []string

	// ThinkingLevel sets the thinking level (off, minimal, low, medium, high, xhigh).
	ThinkingLevel string

	// Verbose enables verbose logging.
	Verbose bool
	// Offline disables startup network operations.
	Offline bool

	// ArtifactLoader is the optional artifact loader for the runtime.
	// When set, it is used to inject skills and context into sessions.
	// This is preferred over the package-level GlobalArtifactLoader.
	ArtifactLoader ArtifactLoader

	// Compaction configures auto-compaction behavior.
	Compaction *CompactionSettings

	// PreloadSkills is a list of skill IDs to load into the session at startup.
	// Unlike path-based loading (--skill <path>), these are resolved by ID
	// from the skill registry after discovery is complete. Skills with
	// matching IDs are injected into the session's system addendum.
	PreloadSkills []string

	// Telemetry controls telemetry reporting.
	// If nil, defaults to checking TAU_TELEMETRY env var (enabled by default).
	Telemetry *bool

	// Logger receives runtime and session diagnostics. Nil discards so embedding
	// applications decide whether logs go to a file, stdout, stderr, or elsewhere.
	Logger *slog.Logger
}

// CompactionSettings configures auto-compaction behavior.
type CompactionSettings struct {
	// TriggerPct is the context window usage percentage (0-100) that triggers
	// auto-compaction. Defaults to 75. A value <= 0 disables auto-compaction
	// when Disabled is also true.
	TriggerPct int
	// PreserveRecent is the number of recent entries to keep uncompacted.
	// Defaults to 10.
	PreserveRecent int
	// Disabled disables auto-compaction when true.
	Disabled bool
}

// SessionInfo holds summary information about a session.
type SessionInfo struct {
	ID           string
	Name         string
	Filename     string
	CWD          string
	Created      string
	LastModified string
	Model        string
	Status       string
	MessageCount int
	Tokens       sessionpkg.TokenCounts
	ParentID     *string
	FirstPrompt  string
	Pinned       bool
	PinnedAt     string
}

// Runtime owns shared state (registries, settings). Sessions are short-lived;
// the runtime is long-lived.
type Runtime interface {
	// NewSession creates a new session.
	NewSession(ctx context.Context, opts SessionOptions) (Session, error)
	// Close releases runtime resources.
	Close(ctx context.Context) error
	// ListSessions returns a list of sessions.
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	// PinSession records whether a persisted session is pinned in workspace lists.
	PinSession(ctx context.Context, id string) error
	// UnpinSession removes a persisted session pin.
	UnpinSession(ctx context.Context, id string) error
	// ResumeSession resumes an existing session with the given ID.
	ResumeSession(ctx context.Context, id string, opts SessionOptions) (Session, error)
	// SessionID returns the current active session ID, or empty string if no session is active.
	// This is used by the plugin system to route Host.SessionAppend/SessionScan RPCs to the correct session.
	SessionID() string
	// SetupDefaults resolves and stores the active model. Returns error if no model is configured.
	// Call this before ActiveModel to ensure the model is resolved.
	SetupDefaults() error
	// ActiveModel returns the resolved active model, or nil if not yet resolved.
	// Call SetupDefaults first to ensure the model is resolved.
	ActiveModel() *ai.Model
	// ResolveModel runs the full resolution chain (explicit override → settings
	// → env-var fallback) and returns the resolved model and its spec without
	// caching it. Used by `tau show-model` to report what a fresh session
	// would resolve to.
	ResolveModel() (*ResolvedModel, error)
	// TelemetryEnabled returns whether telemetry reporting is enabled.
	// Uses RuntimeOptions.Telemetry if set, otherwise falls back to TAU_TELEMETRY env var.
	TelemetryEnabled() bool
	// PluginManager returns the plugin manager, or nil if not configured.
	PluginManager() *plugins.Manager
	// GetSessionStorage returns the session storage for direct access.
	GetSessionStorage() *sessionpkg.Storage
}

// agentSession wraps an agent.Agent to implement Session.
var (
	_ Session                       = (*agentSession)(nil)
	_ Steerer                       = (*agentSession)(nil)
	_ FollowUpper                   = (*agentSession)(nil)
	_ Queuer                        = (*agentSession)(nil)
	_ ModelCycler                   = (*agentSession)(nil)
	_ ThinkingCycler                = (*agentSession)(nil)
	_ QueueModeSetter               = (*agentSession)(nil)
	_ RetryController               = (*agentSession)(nil)
	_ CompactionController          = (*agentSession)(nil)
	_ MessageLister                 = (*agentSession)(nil)
	_ FullMessageLister             = (*agentSession)(nil)
	_ ForkMessageLister             = (*agentSession)(nil)
	_ ForkEntryLister               = (*agentSession)(nil)
	_ Forgetter                     = (*agentSession)(nil)
	_ Exporter                      = (*agentSession)(nil)
	_ Namer                         = (*agentSession)(nil)
	_ LastAssistantTexter           = (*agentSession)(nil)
	_ Statser                       = (*agentSession)(nil)
	_ AttachmentManager             = (*agentSession)(nil)
	_ DisconnectedSessionEndEmitter = (*agentSession)(nil)
	_ ArtifactLoaderProvider        = (*runtime)(nil)
)

type agentSession struct {
	agent                *agent.Agent
	mcp                  *mcp.Manager
	mcpStartup           sync.Once
	mcpErrors            []string
	mcpTools             []agent.Tool
	mcpListeners         map[int]func()
	mcpListenerSeq       int
	id                   string
	cwd                  string
	ui                   ui.UI
	model                *ai.Model
	modelSpec            ai.ModelSpec
	seq                  int64
	totalTokens          int
	usageTotals          ai.Usage
	mu                   sync.Mutex
	cancelFn             context.CancelFunc
	closed               bool // true after first Close call
	runDone              chan struct{}
	closeMu              sync.Mutex
	closeFinished        bool
	closeErr             error
	sessionWriter        *sessionpkg.Writer
	parentMsgID          *string // parent ID for the next message entry
	prePersistedMessage  *agent.Message
	prePersistedEntryID  string
	historyMu            sync.Mutex
	historyIndex         sessionpkg.Index
	pendingNextTurn      []*agent.Message
	pendingStartupCustom []*agent.Message
	emittingSettled      bool
	idleCustomDelivery   bool
	deferredRuns         []deferredRun
	// runtime back-references the owning Runtime so the session can clear
	// the runtime's current-session pointer on Close.
	runtime *runtime
	// eventObserver receives events raised outside a Send channel. See
	// EventObserver for why compaction needs one. Guarded by mu.
	eventObserver func(Event)
	// cmdRegistry is the optional command registry for slash-command handling.
	cmdRegistry commands.Registry
	// usageLogger is the usage log writer for this session.
	usageLogger *sessionpkg.UsageLogger
	// pluginManager is the optional plugin manager for compaction hook support.
	pluginManager *plugins.Manager
	// clientFactory builds authenticated clients for this session's model.
	clientFactory func(*ai.Model) (ai.Client, error)
	// credentialProvider resolves the active provider credential for direct SDK
	// model calls outside the agent loop, such as compaction summarization.
	credentialProvider func(context.Context, string) (string, error)
	requestCredentials agent.RequestCredentialsProvider
	// credStore holds the session's stored credentials, whose metadata can
	// limit the models an account offers.
	credStore        store.CredentialStore
	artifactLoader   ArtifactLoader
	providerCatalogs map[catalogCacheKey][]*ai.Model

	// sessionStartReason is the reason for session start, used in session_start events.
	sessionStartReason string

	// previousSessionFile is the path of the previous session file for resume/fork.
	previousSessionFile string

	// thinkingLevel is the current thinking level for this session.
	thinkingLevel agent.ThinkingLevel

	// systemPromptOverride replaces the resolved system prompt (CLI --system-prompt).
	systemPromptOverride string
	// appendSystemPrompts are appended after the resolved system prompt (CLI --append-system-prompt).
	appendSystemPrompts []string
	// toolAllowlist restricts the session's tool set (CLI --tools/-t). Empty means "no filter."
	toolAllowlist    []string
	pluginToolNames  []string
	defaultToolNames []string
	// toolDenylist removes tools from the session's tool set (CLI --exclude-tools/-xt).
	toolDenylist []string
	// approveProjectFiles pre-approves file operations within CWD (CLI --approve/-a).
	approveProjectFiles bool

	// sessionFileBeforeRename stores the session file path before slug derivation.
	// When empty, the session file has already been renamed (or no rename needed).
	sessionFileBeforeRename string

	// pendingHeader defers creating brand-new persisted session files until the
	// first user message. This avoids empty sessions from UI startup/teardown.
	sessionInitMu sync.Mutex
	pendingHeader *sessionpkg.Header
	// pendingSessionDir is the storage directory used when materializing pendingHeader.
	pendingSessionDir string
	// sessionDataDir is the session-owned directory for attachment bytes.
	sessionDataDir string
	attachments    *AttachmentStore
	// pendingInitialModelProvider/model/thinking are written immediately after
	// the header when a lazily-created session is first materialized.
	pendingInitialModelProvider string
	pendingInitialModel         string
	pendingInitialThinking      string
	pendingInitialTranscript    *agent.Message

	// isPersistedFunc returns whether the session has a persisted backing store.
	// This is true unless NoSession was set without EphemeralExportPath.
	isPersistedFunc func() bool

	// compactionConfig holds auto-compaction settings for this session.
	compactionConfig compaction.Config
	settings         *ai.Settings
	// autoCompactionDefault remembers the configured auto-compaction settings when toggled off.
	autoCompactionDefault compaction.Config
	// autoRetryEnabled records the user's retry preference for UI/RPC state.
	autoRetryEnabled bool
	retryCancel      context.CancelFunc

	// logger receives session diagnostics. Defaults to discard.
	logger *slog.Logger

	// routedModel is the physical model of the latest virtual-model route.
	routedModel *ai.Model
	// failedResponse is the failed request an automatic retry replaces.
	failedResponse *agent.Message
	// overflowRecoveryAttempted limits overflow recovery to one
	// compact-and-retry until a response succeeds or a new prompt starts.
	overflowRecoveryAttempted bool
	// virtualStates holds router state for sessions without a session file.
	virtualStates map[string]json.RawMessage
	// codemodeValues holds store() values for sessions without a session file.
	codemodeValues map[string]json.RawMessage
}

// modelIDForEvent returns the model id for the session_start event payload,
// or an empty string when no model has been resolved yet (e.g. the user
// has not yet picked one via /model).
func modelIDForEvent(m *ai.Model) string {
	if m == nil {
		return ""
	}
	return m.ID
}

type deferredRun struct {
	message Message
	events  chan Event
}

func (s *agentSession) Send(ctx context.Context, msg Message) (<-chan Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session closed")
	}
	if s.emittingSettled {
		prepared, err := s.prepareMessageLocked(msg)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		events := make(chan Event, 64)
		s.deferredRuns = append(s.deferredRuns, deferredRun{message: prepared, events: events})
		s.mu.Unlock()
		return events, nil
	}
	// Check for slash-command invocation before processing as a normal message.
	// If the message starts with "/" and we have a command registry, route
	// the command through the registry instead of sending to the LLM.
	// Slash commands like /reload should interrupt any active run first.
	if strings.TrimSpace(msg.Text) == "/retry" {
		if s.cancelFn != nil {
			s.mu.Unlock()
			return nil, ErrSessionBusy
		}
		return s.runLocked(ctx, s.agent.Continue), nil
	}
	isSlashCommand := strings.HasPrefix(msg.Text, "/") && s.cmdRegistry != nil
	if s.cancelFn != nil {
		if isSlashCommand {
			// Interrupt the active run so the slash command can execute.
			s.cancelFn()
			s.agent.Abort()
		} else {
			s.mu.Unlock()
			return nil, ErrSessionBusy
		}
	}
	if strings.HasPrefix(msg.Text, "/") {
		if handled, resolved := s.resolveArtifactCommand(msg); handled {
			msg = resolved
		} else if s.cmdRegistry != nil {
			s.mu.Unlock()
			return s.executeSlashCommand(ctx, msg.Text)
		}
	}
	prepared, err := s.prepareMessageLocked(msg)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return s.sendPreparedMessageLocked(ctx, prepared)
}

// ErrSessionBusy reports a prompt sent while the session is processing.
var ErrSessionBusy = errors.New("session already processing")

// CustomMessage is extension context delivered without user input processing.
type CustomMessage struct {
	CustomType string
	Content    []ai.ContentBlock
	Display    bool
	Details    any
	// ExcludeFromContext records the message without sending it to the model.
	ExcludeFromContext bool
}

// CustomMessageOptions controls whether delivery starts or joins a run.
// A nil TriggerTurn joins a busy run but does not start an idle run.
type CustomMessageOptions struct {
	TriggerTurn *bool
	DeliverAs   string
}

// CustomMessageSender delivers extension messages to a session.
type CustomMessageSender interface {
	SendCustomMessage(context.Context, CustomMessage, CustomMessageOptions) error
}

// SendCustomMessage delivers context immediately, at a safe boundary, or with a future run.
func (s *agentSession) SendCustomMessage(ctx context.Context, custom CustomMessage, options CustomMessageOptions) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send custom message: %w", err)
	}
	switch options.DeliverAs {
	case "", "steer", "followUp", "nextTurn":
	default:
		return fmt.Errorf("invalid custom delivery mode %q", options.DeliverAs)
	}
	content := slices.Clone(custom.Content)
	if content == nil {
		content = []ai.ContentBlock{}
	}
	msg := Message{Role: "custom", CustomType: custom.CustomType, Content: content, Display: custom.Display, Details: custom.Details, ExcludeFromContext: custom.ExcludeFromContext}
	if _, err := json.Marshal(agentMessageFromSDK(msg)); err != nil {
		return fmt.Errorf("encode custom message: %w", err)
	}
	s.mu.Lock()
	prepared, err := s.prepareMessageLocked(msg)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if s.idleCustomDelivery && options.DeliverAs != "nextTurn" {
		s.mu.Unlock()
		return ErrSessionBusy
	}
	message := agentMessageFromSDK(prepared)
	if options.DeliverAs == "nextTurn" {
		s.pendingNextTurn = append(s.pendingNextTurn, message)
		s.mu.Unlock()
		return nil
	}
	trigger := options.TriggerTurn != nil && *options.TriggerTurn
	if (trigger || options.TriggerTurn == nil) && s.emittingSettled {
		s.deferredRuns = append(s.deferredRuns, deferredRun{message: prepared})
		s.mu.Unlock()
		return nil
	}
	if s.cancelFn != nil {
		if options.TriggerTurn == nil || trigger {
			if options.DeliverAs == "followUp" {
				s.agent.FollowUp(message)
			} else {
				s.agent.Steer(message)
			}
			s.mu.Unlock()
			s.recordQueueUpdate()
			return nil
		}
		queued, err := s.agent.QueueCustomMessage(ctx, message)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !queued {
			s.pendingStartupCustom = append(s.pendingStartupCustom, message)
		}
		s.mu.Unlock()
		return nil
	}
	if trigger {
		// The run outlives the caller, which may be a plugin RPC request.
		events, err := s.sendPreparedMessageLocked(context.WithoutCancel(ctx), prepared)
		if err != nil {
			return err
		}
		go func() {
			for range events {
			}
		}()
		return nil
	}
	s.idleCustomDelivery = true
	deliveryCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancelFn = cancel
	s.runDone = done
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.cancelFn = nil
		s.runDone = nil
		s.idleCustomDelivery = false
		s.mu.Unlock()
		close(done)
	}()
	if _, err := s.ensureSessionWriter(); err != nil {
		return err
	}
	var persistenceErr error
	// Idle appends have no Send subscription to persist and publish their events.
	unsubscribe := s.agent.Subscribe(func(event agent.Event, _ <-chan struct{}) {
		switch e := event.(type) {
		case agent.MessageStart:
			if e.Message == message {
				s.notify("message_start", map[string]any{"role": "custom", "message": message})
			}
		case agent.MessageEnd:
			if e.Message == message {
				persistenceErr = s.persistMessage(message)
				s.notify("message_end", map[string]any{"role": "custom", "message": message})
			}
		}
	})
	defer unsubscribe()
	if err := s.agent.AppendCustomMessage(deliveryCtx, message); err != nil {
		return err
	}
	return persistenceErr
}

// BashExecutionCustomType tags custom messages that record user shell commands.
const BashExecutionCustomType = "bashExecution"

// ErrBashBlocked reports a user shell command vetoed by a user_bash handler.
var ErrBashBlocked = errors.New("user bash execution was blocked")

// BashOptions configures a user shell command.
type BashOptions struct {
	// ExcludeFromContext records the command for display without sending it
	// to the model, as the interactive !! prefix does.
	ExcludeFromContext bool
	// Dir overrides the session working directory.
	Dir string
	// Timeout bounds execution; zero means no limit.
	Timeout time.Duration
	// OnOutput receives raw output chunks from local execution. Result
	// overrides deliver their output only in the returned record.
	OnOutput func(chunk string)
}

// BashExecution is the session record of a user shell command.
type BashExecution struct {
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           int    `json:"exitCode"`
	CancelReason       string `json:"cancelReason,omitempty"`
	Cancelled          bool   `json:"cancelled,omitempty"`
	Truncated          bool   `json:"truncated,omitempty"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
	// DurationMs is shell execution time, absent when a hook supplied the result.
	DurationMs *float64 `json:"durationMs,omitempty"`
}

// BashExecutor runs user shell commands outside the model loop and records
// them in the session.
type BashExecutor interface {
	ExecuteBash(ctx context.Context, command string, options BashOptions) (BashExecution, error)
}

// ExecuteBash runs a user shell command through user_bash handlers and the
// configured shell operations, then records the result. Busy sessions receive
// the record at the next safe turn boundary so tool calls and results stay
// adjacent. Cancelled commands are still recorded.
func (s *agentSession) ExecuteBash(ctx context.Context, command string, options BashOptions) (BashExecution, error) {
	if err := ctx.Err(); err != nil {
		return BashExecution{}, fmt.Errorf("execute bash: %w", err)
	}
	if strings.TrimSpace(command) == "" {
		return BashExecution{}, errors.New("execute bash: command cannot be empty")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return BashExecution{}, errors.New("execute bash: session closed")
	}
	manager, id, settings, dir := s.pluginManager, s.id, s.settings, s.cwd
	s.mu.Unlock()
	if options.Dir != "" {
		dir = options.Dir
	}
	result, durationMs, err := runUserBash(ctx, manager, id, buildToolConfig(settings), command, dir, options)
	if err != nil {
		return BashExecution{}, err
	}
	execution := BashExecution{
		Command:            command,
		Output:             result.Output,
		ExitCode:           result.ExitCode,
		Cancelled:          result.CancelReason != "" && result.CancelReason != agent.BashCancelNone,
		Truncated:          result.Truncation != nil && result.Truncation.Truncated,
		FullOutputPath:     result.FullOutputPath,
		ExcludeFromContext: options.ExcludeFromContext,
		DurationMs:         durationMs,
	}
	if execution.Cancelled {
		execution.CancelReason = string(result.CancelReason)
	}
	err = s.SendCustomMessage(context.WithoutCancel(ctx), CustomMessage{
		CustomType:         BashExecutionCustomType,
		Content:            []ai.ContentBlock{{Type: ai.ContentBlockText, Text: bashExecutionText(execution)}},
		Display:            true,
		Details:            execution,
		ExcludeFromContext: options.ExcludeFromContext,
	}, CustomMessageOptions{TriggerTurn: new(false)})
	if err != nil {
		return execution, fmt.Errorf("record bash execution: %w", err)
	}
	return execution, nil
}

func runUserBash(ctx context.Context, manager *plugins.Manager, sessionID string, cfg agent.ToolConfig, command, dir string, options BashOptions) (agent.BashResult, *float64, error) {
	if manager != nil {
		payload, err := json.Marshal(map[string]any{"command": command, "cwd": dir, "excludeFromContext": options.ExcludeFromContext})
		if err != nil {
			return agent.BashResult{}, nil, fmt.Errorf("marshal user bash event: %w", err)
		}
		response := manager.DispatchEventResponse(ctx, "user_bash", payload, sessionID)
		if response.Block {
			if response.BlockReason != "" {
				return agent.BashResult{}, nil, fmt.Errorf("%w: %s", ErrBashBlocked, response.BlockReason)
			}
			return agent.BashResult{}, nil, ErrBashBlocked
		}
		if len(response.ResponseData) > 0 {
			result, err := agent.ParseUserBashOverride(response.ResponseData)
			return result, nil, err
		}
	}
	// Hooks may run long enough for the caller to cancel.
	if err := ctx.Err(); err != nil {
		return agent.BashResult{}, nil, fmt.Errorf("execute bash: %w", err)
	}
	started := time.Now()
	result, err := cfg.BashOperations().Run(ctx, agent.BashRequest{Command: agent.PrefixBashCommand(cfg.CommandPrefix, command), Dir: dir, Timeout: options.Timeout, OnOutput: options.OnOutput})
	durationMs := float64(time.Since(started)) / float64(time.Millisecond)
	if err != nil {
		return result, nil, fmt.Errorf("execute bash: %w", err)
	}
	return result, &durationMs, nil
}

// bashExecutionText is the model-facing form of a user shell command.
func bashExecutionText(execution BashExecution) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Ran `%s`\n", execution.Command)
	if execution.Output != "" {
		fmt.Fprintf(&text, "```\n%s\n```", execution.Output)
	} else {
		text.WriteString("(no output)")
	}
	if execution.Cancelled {
		text.WriteString("\n\n(command cancelled)")
	} else if execution.ExitCode != 0 {
		fmt.Fprintf(&text, "\n\nCommand exited with code %d", execution.ExitCode)
	}
	if execution.Truncated && execution.FullOutputPath != "" {
		fmt.Fprintf(&text, "\n\n[Output truncated. Full output: %s]", execution.FullOutputPath)
	}
	return text.String()
}

func (s *agentSession) prepareMessageLocked(msg Message) (Message, error) {
	if s.closed {
		return msg, errors.New("session closed")
	}
	attachmentBlocks, err := s.attachmentBlocksLocked(msg.AttachmentIDs)
	if err != nil {
		return msg, err
	}
	if msg.Content == nil {
		msg.Content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: msg.Text}}
	} else {
		msg.Content = slices.Clone(msg.Content)
	}
	msg.Content = append(msg.Content, msg.Images...)
	msg.Content = append(msg.Content, attachmentBlocks...)
	msg.Images = nil
	if s.model != nil && s.model.InputLimits != nil && s.model.InputLimits.Images != nil {
		limits := s.model.InputLimits.Images
		count := 0
		for i, block := range msg.Content {
			if block.Type != ai.ContentBlockImage {
				continue
			}
			count++
			resized, err := agent.ResizeImageBlock(block, limits.Resize)
			if err != nil {
				return msg, err
			}
			msg.Content[i] = resized
		}
		if limits.MaxPerMessage > 0 && count > limits.MaxPerMessage {
			return msg, fmt.Errorf("message exceeds model image limit %d", limits.MaxPerMessage)
		}
	}
	if err := s.validateVisionInput(msg); err != nil {
		return msg, err
	}
	return msg, nil
}

func (s *agentSession) attachmentBlocksLocked(ids []string) ([]ai.ContentBlock, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.attachments == nil {
		return nil, fmt.Errorf("attachment store is not configured")
	}
	return s.attachments.ContentBlocks(ids)
}

func (s *agentSession) validateVisionInput(msg Message) error {
	for _, block := range append(slices.Clone(msg.Content), msg.Images...) {
		if block.Type != ai.ContentBlockImage {
			continue
		}
		if s.model == nil {
			return fmt.Errorf("%w: no active model", ErrVisionNotSupported)
		}
		// The per-model registry entry is the source of truth for vision
		// support. The provider-level Capability is a secondary signal that
		// was historically used as a stand-in (and is still used to default
		// capabilities on the agent), but per-model entries take precedence:
		// a model that registers InputImage accepts images regardless of
		// whether its provider's baseline capability flag agrees.
		// See CodexCapabilities and models_generated.go for the gpt-5.5 case
		// that motivated this gate.
		if s.model.SupportsInput(ai.InputImage) {
			return nil
		}
		if len(s.model.Input) > 0 || !s.agent.State().Capabilities.Vision {
			return fmt.Errorf("%w: model %s", ErrVisionNotSupported, s.model.ID)
		}
		return nil
	}
	return nil
}

func (s *agentSession) resolveArtifactCommand(msg Message) (bool, Message) {
	if s.artifactLoader == nil {
		return false, msg
	}

	name, args := parseSlashCommand(msg.Text)
	if name == "" {
		return false, msg
	}

	if skillName, ok := strings.CutPrefix(name, "skill:"); ok {
		registry := s.artifactLoader.SkillRegistry()
		if registry == nil {
			return false, msg
		}
		skill, found := registry.Get(skillName)
		if !found {
			return false, msg
		}

		// Replace the slash command with the XML-wrapped skill body as the
		// user message; args (if any) are appended after a blank line. This
		// matches the TS reference _expandSkillCommand and the registered
		// /skill: handler output.
		msg.Text = artifacts.FormatSkillInvocation(skill, args)
		msg.SystemAddendum = ""
		return true, msg
	}

	registry := s.artifactLoader.PromptRegistry()
	if registry == nil {
		return false, msg
	}
	expanded, found := registry.Expand(name, args)
	if !found {
		return false, msg
	}
	msg.Text = expanded
	return true, msg
}

func parseSlashCommand(line string) (name, args string) {
	line = strings.TrimPrefix(line, "/")
	whitespace := func(r rune) bool { return unicode.IsSpace(r) && r != '\u0085' || r == '\ufeff' }
	if i := strings.IndexFunc(line, whitespace); i >= 0 {
		return line[:i], strings.TrimLeftFunc(line[i:], whitespace)
	}
	return line, ""
}

// providerTriggerTokens returns provider-specific token caps for
// auto-compaction. OpenAI's codex models bill at 2x rate above ~270K tokens
// even though the advertised window is larger, so compaction is capped below
// that line.
func providerTriggerTokens(provider string) int {
	if provider == ai.ProviderOpenAICodex {
		return 250_000
	}
	return 0
}

func (s *agentSession) sendPreparedMessageLocked(ctx context.Context, msg Message) (<-chan Event, error) {
	return s.runLocked(ctx, func(ctx context.Context) error { return s.runPreparedMessage(ctx, msg) }), nil
}

func (s *agentSession) runPreparedMessage(ctx context.Context, msg Message) error {
	agentMsg := agentMessageFromSDK(msg)

	entryID, err := s.appendUserMessageToSession(msg)
	if err != nil {
		return fmt.Errorf("persist prompt: %w", err)
	}
	s.mu.Lock()
	s.prePersistedMessage = agentMsg
	s.prePersistedEntryID = entryID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.prePersistedMessage = nil
		s.prePersistedEntryID = ""
		s.mu.Unlock()
	}()

	// Auto-compaction check: trigger if context usage exceeds threshold.
	s.autoCompactIfDue(ctx)
	s.waitMCPStartup(ctx)

	messages := []*agent.Message{agentMsg}
	if msg.SystemAddendum != "" {
		return s.agent.PromptMessages(ctx, messages, agent.WithPromptSystemAddendum(msg.SystemAddendum))
	}

	return s.agent.PromptMessages(ctx, messages)
}

// runLocked starts an agent run on a goroutine and streams its events. The
// caller holds s.mu; runLocked releases it.
func (s *agentSession) settingsSnapshot() *ai.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

func (s *agentSession) retryPolicy() ai.RetryPolicy {
	settings := s.settingsSnapshot()
	resolved := settings.ResolveRetry()
	policy := ai.RetryPolicy{MaxRetries: resolved.MaxRetries, BaseDelay: time.Duration(resolved.BaseDelayMs) * time.Millisecond, MaxDelay: time.Duration(resolved.MaxAgentDelayMs) * time.Millisecond}
	s.mu.Lock()
	enabled := s.autoRetryEnabled
	s.mu.Unlock()
	if !enabled || !resolved.Enabled {
		policy.MaxRetries = 0
	}
	return policy
}

func (s *agentSession) runWithRetry(ctx context.Context, events chan<- Event, run func(context.Context) error) (runErr error) {
	retryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.retryCancel = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.retryCancel = nil; s.mu.Unlock() }()
	policy := s.retryPolicy()
	retried := false
	defer func() {
		if retried {
			s.emit(events, Event{Type: "auto_retry_end", Payload: map[string]any{"success": runErr == nil}})
		}
	}()
	var latest *agent.Message
	produce := func(callCtx context.Context) (*ai.AssistantMessage, error) {
		latest = nil
		var previousAssistant *agent.Message
		for _, message := range slices.Backward(s.agent.Messages()) {
			if message.Role == agent.RoleAssistant {
				previousAssistant = message
				break
			}
		}
		if err := run(callCtx); err != nil {
			return nil, err
		}
		messages := s.agent.Messages()
		if len(messages) == 0 {
			return &ai.AssistantMessage{StopReason: ai.StopReasonStop}, nil
		}
		var message *agent.Message
		for _, candidate := range slices.Backward(messages) {
			if candidate.Role == agent.RoleAssistant {
				message = candidate
				break
			}
		}
		if message == nil || message == previousAssistant {
			return &ai.AssistantMessage{StopReason: ai.StopReasonStop}, nil
		}
		latest = message
		return &ai.AssistantMessage{StopReason: message.StopReason, ErrorMessage: message.ErrorMessage}, nil
	}
	onRetry := func(attempt int, delay time.Duration, message string) {
		retried = true
		if err := s.omitFailedAttempt(); err != nil {
			s.logger.Error("persist retry omission", "error", err)
			cancel()
			return
		}
		s.emit(events, Event{Type: "auto_retry_start", Payload: map[string]any{"attempt": attempt, "maxAttempts": policy.MaxRetries, "delayMs": delay.Milliseconds(), "errorMessage": message}})
		s.mu.Lock()
		s.failedResponse = latestAssistant(s.agent.Messages())
		s.mu.Unlock()
		run = s.agent.Continue
	}
	result, err := ai.RetryAssistantCall(retryCtx, produce, policy, onRetry)
	for err == nil && s.recoverFromOverflow(retryCtx, latest) {
		run = s.agent.Continue
		result, err = ai.RetryAssistantCall(retryCtx, produce, policy, onRetry)
	}
	if err == nil && result != nil {
		switch result.StopReason {
		case ai.StopReasonError:
			return fmt.Errorf("provider response failed: %s", result.ErrorMessage)
		case ai.StopReasonAborted:
			return fmt.Errorf("provider response aborted: %w", context.Canceled)
		}
	}
	return err
}

// recoverFromOverflow compacts after a response that overflowed the context
// window, or stopped short of its output limit, and reports whether the
// interrupted turn should be retried. Like upstream, a failed response gets one
// compact-and-retry; a completed one is compacted without a retry.
func (s *agentSession) recoverFromOverflow(ctx context.Context, message *agent.Message) bool {
	model := s.limitsModel()
	if message == nil || model == nil || s.compactionConfig.TriggerPct <= 0 {
		return false
	}
	// An overflow from another model says nothing about the current window.
	if message.Provider != model.Provider || message.Model != model.ID {
		return false
	}
	stop := string(message.StopReason)
	usage := ai.OverflowUsage{Input: message.Usage.Input, Output: message.Usage.Output, CacheHit: message.Usage.CacheRead}
	overflow := ai.IsContextOverflow(message.Provider, stop, message.ErrorMessage, usage, model.ContextWindow)
	if !overflow && !ai.IsRecoverableLength(stop, message.Usage.Output, model.MaxTokens) {
		return false
	}
	trigger := compactionTrigger{reason: compactionReasonOverflow, willRetry: message.StopReason != ai.StopReasonStop}
	if trigger.willRetry {
		s.mu.Lock()
		attempted := s.overflowRecoveryAttempted
		s.overflowRecoveryAttempted = true
		s.mu.Unlock()
		if attempted {
			errorMessage := "Truncated response recovery failed after one compact-and-retry attempt."
			if overflow {
				errorMessage = "Context overflow recovery failed after one compact-and-retry attempt. Try reducing context or switching to a larger-context model."
			}
			s.notify("compaction_end", map[string]any{"session_id": s.id, "reason": trigger.reason, "will_retry": false, "error_message": errorMessage})
			return false
		}
		if err := s.omitFailedAttempt(); err != nil {
			s.logger.Error("persist overflow retry omission", "error", err)
			return false
		}
	}
	if due, err := s.hasPersistedCompactionWork(model); err != nil || !due {
		if err != nil {
			s.logger.Warn("overflow compaction failed", "error", err)
		}
		return false
	}
	if err := s.compact(ctx, trigger, ""); err != nil {
		s.logger.Warn("overflow compaction failed", "error", err)
		return false
	}
	if trigger.willRetry {
		s.mu.Lock()
		s.failedResponse = message
		s.mu.Unlock()
	}
	return trigger.willRetry
}

func (s *agentSession) omitFailedAttempt() error {
	s.mu.Lock()
	writer := s.sessionWriter
	parent := s.parentMsgID
	s.mu.Unlock()
	if writer == nil || parent == nil {
		return nil
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return fmt.Errorf("read retry history: %w", err)
	}
	manager := sessionpkg.NewManager(reader.Header().CWD)
	manager.LoadEntries(reader)
	if _, err := manager.AppendContextEdit(*parent, json.RawMessage(`null`)); err != nil {
		return fmt.Errorf("omit retry attempt: %w", err)
	}
	entries := manager.Entries()
	if err := writer.Append(entries[len(entries)-1]); err != nil {
		return fmt.Errorf("persist retry edit: %w", err)
	}
	id := entries[len(entries)-1].ID
	s.mu.Lock()
	s.parentMsgID = &id
	s.mu.Unlock()
	return nil
}

func (s *agentSession) flushStartupCustom(ctx context.Context) {
	for {
		s.mu.Lock()
		pending := s.pendingStartupCustom
		s.pendingStartupCustom = nil
		s.mu.Unlock()
		if len(pending) == 0 {
			return
		}
		// Accepted context survives cancellation of the provider run.
		for _, message := range pending {
			if err := s.agent.AppendCustomMessage(context.WithoutCancel(ctx), message); err != nil {
				s.logger.Error("append startup context", "error", err)
			}
		}
	}
}

func (s *agentSession) runLocked(ctx context.Context, run func(context.Context) error) <-chan Event {
	ctx, cancel := context.WithCancel(ctx)
	s.cancelFn = cancel
	done := make(chan struct{})
	s.runDone = done
	s.mu.Unlock()

	events := make(chan Event, 64)

	go func() {
		defer close(done)
		defer close(events)
		defer cancel()

		s.emit(events, Event{
			Type:      "session_start",
			SessionID: s.id,
			Payload: map[string]any{
				"id":                  s.id,
				"cwd":                 s.cwd,
				"model":               modelIDForEvent(s.model),
				"reason":              s.sessionStartReason,
				"previousSessionFile": s.previousSessionFile,
			},
		})

		currentEvents := events
		unsubscribe := s.agent.Subscribe(func(event agent.Event, signal <-chan struct{}) {
			if _, ok := event.(agent.AgentStart); ok {
				s.flushStartupCustom(ctx)
			}
			s.handleAgentEvent(currentEvents, event)
			// Tool results can push a running turn past the context-window
			// threshold between turns. Checking on message end lets compaction
			// fire mid-turn, and the agent loop rebases onto the replacement
			// summary before the next model call.
			// A failed or truncated response is left to overflow recovery
			// after the run, which must omit it before compacting.
			if e, ok := event.(agent.MessageEnd); ok && e.Message != nil && e.Message.Role != agent.RoleUser && e.Message.StopReason != ai.StopReasonError && e.Message.StopReason != ai.StopReasonLength {
				s.autoCompactIfDue(ctx)
			}
		})
		defer unsubscribe()

		var runErr error
		for {
			runErr = s.runWithRetry(ctx, currentEvents, run)
			if err := runErr; err != nil {
				s.recordProviderError(err)
				s.emit(currentEvents, Event{
					Type:      "error",
					SessionID: s.id,
					Payload: map[string]any{
						"code":        "agent_error",
						"message":     err.Error(),
						"recoverable": false,
					},
				})
			}

			s.flushStartupCustom(ctx)
			if currentEvents != events {
				close(currentEvents)
				currentEvents = events
			}
			s.mu.Lock()
			if runErr != nil || len(s.deferredRuns) == 0 || s.closed || ctx.Err() != nil {
				for _, pending := range s.deferredRuns {
					if pending.events != nil {
						close(pending.events)
					}
				}
				s.deferredRuns = nil
				s.mu.Unlock()
				break
			}
			next := s.deferredRuns[0]
			s.deferredRuns = s.deferredRuns[1:]
			s.mu.Unlock()
			currentEvents = events
			if next.events != nil {
				currentEvents = next.events
			}
			run = func(ctx context.Context) error { return s.runPreparedMessage(ctx, next.message) }
		}

		s.emit(events, Event{
			Type:      "session_end",
			SessionID: s.id,
			Payload: map[string]any{
				"id":           s.id,
				"aborted":      ctx.Err() != nil || errors.Is(runErr, context.Canceled),
				"total_tokens": s.totalTokens,
				"cost_usd":     s.calculateCost(),
			},
		})
		for {
			s.flushStartupCustom(ctx)
			s.mu.Lock()
			if len(s.pendingStartupCustom) != 0 {
				s.mu.Unlock()
				continue
			}
			unsubscribe()
			s.cancelFn = nil
			s.runDone = nil
			s.mu.Unlock()
			break
		}
	}()

	return events
}

// SendText is an ergonomic shortcut that sends a text-only message.
func (s *agentSession) SendText(ctx context.Context, text string) (<-chan Event, error) {
	return s.Send(ctx, Message{Role: "user", Text: text})
}

func (s *agentSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelFn != nil {
		s.cancelFn()
	}
	s.agent.Abort()
	return nil
}

func (s *agentSession) Compact(ctx context.Context, customInstructions ...string) error {
	var instructions string
	if len(customInstructions) > 0 {
		instructions = strings.TrimSpace(customInstructions[0])
	}
	return s.compact(ctx, compactionTrigger{reason: compactionReasonManual}, instructions)
}

const (
	compactionReasonManual    = "manual"
	compactionReasonThreshold = "threshold"
	compactionReasonOverflow  = "overflow"
)

// compactionTrigger says why a compaction runs and whether the interrupted
// turn is retried after it.
type compactionTrigger struct {
	reason    string
	willRetry bool
}

// compact runs one compaction. Once compaction_start is raised, a failure
// still raises compaction_end, flagged aborted or carrying the error, so a
// frontend never keeps a compaction indicator for an attempt that ended.
func (s *agentSession) compact(ctx context.Context, trigger compactionTrigger, instructions string) (err error) {
	reason := trigger.reason
	var started bool
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("compact: %w", errors.Join(err, ctx.Err(), context.Cause(ctx)))
		}
		if err == nil || !started {
			return
		}
		payload := map[string]any{"session_id": s.id, "reason": reason, "will_retry": false, "custom_instructions": instructions}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			payload["aborted"] = true
		} else {
			payload["error_message"] = map[string]string{
				compactionReasonOverflow:  "Context overflow recovery failed: ",
				compactionReasonThreshold: "Auto-compaction failed: ",
			}[reason] + err.Error()
		}
		s.notify("compaction_end", payload)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Fire `session_before_compact` (cancellable) so plugins can observe or
	// block the compaction attempt.
	if s.pluginManager != nil {
		beforePayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              reason,
			"will_retry":          trigger.willRetry,
			"custom_instructions": instructions,
		})
		if blocked := s.pluginManager.DispatchEvent(ctx, "session_before_compact", beforePayload, s.id); blocked {
			return fmt.Errorf("compaction blocked by plugin")
		}
	}

	// Fire `compaction_start` (informational) so plugins can observe the
	// moment compaction actually begins.
	if s.pluginManager != nil {
		startPayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              reason,
			"custom_instructions": instructions,
		})
		s.pluginManager.DispatchEvent(ctx, "compaction_start", startPayload, s.id)
	}
	// Frontends get the same event out of band. Raised outside the plugin-
	// manager guard: a session with no plugins still has a UI to inform.
	started = true
	s.notify("compaction_start", map[string]any{
		"session_id":          s.id,
		"reason":              reason,
		"custom_instructions": instructions,
	})

	// Build a summary from the current agent messages using the model.
	// Retry semantics: when the log's most recent compaction is a local
	// fallback, the live context is that degraded summary — a retry must
	// instead compact the same messages the failed attempt saw.
	state := s.agent.State()
	compactMessages := s.compactionInputMessages(state.Messages)
	var keptMessages []*agent.Message
	var firstKeptID string
	settings := s.settingsSnapshot()
	limits, err := settings.ResolveCompaction(s.limitsModel())
	if err != nil {
		return err
	}
	{
		s.mu.Lock()
		writer := s.sessionWriter
		s.mu.Unlock()
		if writer != nil {
			reader, err := sessionpkg.ReadFile(writer.Path())
			if err != nil {
				return fmt.Errorf("read compaction history: %w", err)
			}
			entries := reader.ContextEntriesUnwound()
			preparation := prepareSessionCompaction(entries, limits)
			if preparation == nil && len(entries) > 0 {
				if branch := reader.ActiveBranch(); len(branch) > 0 {
					if last := reader.FindByID(branch[len(branch)-1]); last != nil && last.Type == sessionpkg.EntryTypeCompaction {
						return errors.New("already compacted")
					}
				}
				return errors.New("nothing to compact (session too small)")
			}
			if preparation != nil {
				firstKeptID = preparation.FirstKeptID
				var toSummarize []*sessionpkg.Message
				var toKeep []*sessionpkg.Message
				keeping := false
				for _, entry := range entries {
					keeping = keeping || entry.ID == firstKeptID
					if message := entry.ToAgentMessage(); message != nil {
						if keeping {
							toKeep = append(toKeep, message)
						} else {
							toSummarize = append(toSummarize, message)
						}
					}
				}
				keptMessages = sessionMessagesToAgentMessages(toKeep)
				compactMessages = sessionMessagesToAgentMessages(toSummarize)
			}
		}
	}
	compactMessages = slices.DeleteFunc(slices.Clone(compactMessages), func(message *agent.Message) bool {
		return message == nil || message.ExcludeFromContext
	})
	var transcript strings.Builder
	for _, msg := range compactMessages {
		role := string(msg.Role)
		for _, block := range msg.Content {
			switch block.Type {
			case ai.ContentBlockText:
				if block.Text != "" {
					fmt.Fprintf(&transcript, "[%s]: %s\n", role, block.Text)
				}
			case ai.ContentBlockToolCall:
				if block.Name != "" {
					fmt.Fprintf(&transcript, "[%s tool call]: %s(%s)\n", role, block.Name, block.Arguments)
				}
			}
		}
	}

	transcriptStr := transcript.String()

	// Try the plugin compaction hook first when plugins are available; fall
	// through to the built-in model-driven summarization otherwise.
	var summary string
	if s.pluginManager != nil {
		tokensBefore := int64(0)
		for _, msg := range compactMessages {
			tokensBefore += int64(msg.Usage.TotalTokens)
		}
		result := s.pluginManager.DispatchCompactionHook(ctx, transcriptStr, tokensBefore, s.id)
		summary = strings.TrimSpace(result.Summary)
	}
	var summarizeErr error
	if summary == "" {
		summarizationPrompt := compaction.BuildSummarizationPrompt()
		var err error
		summary, err = s.callModelForSummary(ctx, summarizationPrompt, transcriptStr, instructions)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Keep the session usable with a naive summary, but never silently:
			// the warning and the compaction_end payload both record the failure.
			summarizeErr = err
			s.logger.Warn("compaction model call failed; using naive summary", "error", err)
			summary = s.buildNaiveSummary(compactMessages, instructions)
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	// Counted once, before the messages are replaced: the entry, the event, and
	// any frontend indicator all have to quote the same figure.
	tokensBefore := 0
	for _, msg := range compactMessages {
		tokensBefore += msg.Usage.TotalTokens
	}

	// Write compaction entry to session
	s.mu.Lock()
	writer := s.sessionWriter
	parentID := s.parentMsgID
	s.mu.Unlock()
	if writer != nil && parentID != nil {
		keptID := *parentID
		if firstKeptID != "" {
			keptID = firstKeptID
		}
		entry := sessionpkg.NewCompactionEntry(summary, keptID, tokensBefore, 0, false)
		entry.ParentID = parentID
		entry.ID = sessionpkg.NewID()
		if summarizeErr != nil {
			// The session log must show this compaction degraded to a local
			// summary; without the flag a silent fallback is indistinguishable
			// from a model-driven one when reading the file later.
			entry.Fallback = true
			entry.FallbackError = summarizeErr.Error()
		}
		if err := writer.Append(entry); err != nil {
			return fmt.Errorf("append compaction entry: %w", err)
		}
		s.mu.Lock()
		s.parentMsgID = &entry.ID
		s.mu.Unlock()
	}

	// Replace agent messages with a single summary message to simulate
	// the effect of compaction on future context building.
	summaryMsg := &agent.Message{
		Role:      agent.RoleSystem,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary: " + summary}},
		Timestamp: time.Now(),
	}
	var replacement []*agent.Message
	for _, message := range compactMessages {
		if message != nil && message.Role == agent.RoleSystem && !message.ExcludeFromContext {
			replacement = append(replacement, message)
		}
	}
	replacement = append(replacement, summaryMsg)
	s.agent.ReplaceMessages(collapseSystemBaselines(append(replacement, keptMessages...)))

	// Fire `compaction_end` (informational) so plugins can observe the
	// successful compaction outcome.
	if s.pluginManager != nil {
		afterPayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              reason,
			"will_retry":          trigger.willRetry,
			"custom_instructions": instructions,
		})
		s.pluginManager.DispatchEvent(ctx, "compaction_end", afterPayload, s.id)
	}
	// The frontend copy carries the summary and the token count: §9 has the UI
	// commit a summary message when compaction ends, and it has no other way to
	// learn what the summary was. A model-call failure is flagged so frontends
	// can show that the summary is a local fallback rather than model output.
	endPayload := map[string]any{
		"session_id":          s.id,
		"reason":              reason,
		"will_retry":          trigger.willRetry,
		"summary":             summary,
		"tokens_before":       tokensBefore,
		"custom_instructions": instructions,
	}
	if summarizeErr != nil {
		endPayload["fallback"] = true
		endPayload["fallback_error"] = summarizeErr.Error()
	}
	s.notify("compaction_end", endPayload)

	return nil
}

// summaryMaxOutputTokens caps the summarization request's *visible* output.
// A summary never needs the model's full output budget (often 64K+), and
// providers that validate input+output against the context window reject
// such requests.
const summaryMaxOutputTokens = 8192

// summaryMaxTokens returns the summarization request's output cap. Thinking
// models count reasoning against the same cap as visible text (zai glm-5.3:
// max_tokens covers reasoning_content + content), so a thinking level adds
// its budget on top of the summary budget — otherwise the model reasons away
// the cap and the visible summary collapses to a fragment cut at the length
// limit. Normal prompts get the same treatment implicitly by falling back to
// the model's full MaxTokens; the compact path sets an explicit cap, so the
// adjustment has to be explicit too.
func (s *agentSession) summaryMaxTokens(model *ai.Model, level agent.ThinkingLevel) int {
	if level == "" || level == agent.ThinkingOff || model == nil || !model.Reasoning {
		return summaryMaxOutputTokens
	}
	maxTokens := summaryMaxOutputTokens + ai.ThinkingBudgetForLevel(ai.ReasoningLevel(level), s.settingsSnapshot().ResolveThinkingBudgets())
	if model.MaxTokens > 0 && model.MaxTokens < maxTokens {
		return model.MaxTokens
	}
	return maxTokens
}

// prepareSessionCompaction runs compaction preparation over persisted context
// entries. It returns nil when no conversation message would be summarized.
func prepareSessionCompaction(entries []sessionpkg.Entry, limits ai.CompactionSettings) *compaction.Preparation {
	scanner := make([]compaction.ScannerEntry, 0, len(entries))
	for _, entry := range entries {
		scannerEntry := compaction.ScannerEntry{ID: entry.ID, Type: string(entry.Type), ParentID: entry.ParentID, Message: entry.Message}
		if entry.FirstKeptID != nil {
			scannerEntry.FirstKeptID = *entry.FirstKeptID
		}
		if entry.Summary != nil {
			scannerEntry.Summary = *entry.Summary
		}
		scanner = append(scanner, scannerEntry)
	}
	preparation := compaction.PrepareCompaction(scanner, compaction.Settings{Enabled: true, ReserveTokens: limits.ReserveTokens, KeepRecentTokens: limits.KeepRecentTokens})
	if preparation == nil || !slices.ContainsFunc(append(slices.Clone(preparation.MessagesToSummarize), preparation.TurnPrefixMessages...), func(message *compaction.AgentMessage) bool {
		return message != nil && message.Role != compaction.MessageRole(agent.RoleSystem)
	}) {
		return nil
	}
	return preparation
}

// hasPersistedCompactionWork reports whether automatic compaction has
// anything to summarize. Like upstream, a session whose persisted history
// yields no preparation (for example, one that was just compacted) is
// skipped instead of compacting its retained tail again. Sessions without a
// file keep summarizing their live history.
func (s *agentSession) hasPersistedCompactionWork(model *ai.Model) (bool, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return true, nil
	}
	limits, err := s.settingsSnapshot().ResolveCompaction(model)
	if err != nil {
		return false, err
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return false, fmt.Errorf("read compaction history: %w", err)
	}
	return prepareSessionCompaction(reader.ContextEntriesUnwound(), limits) != nil, nil
}

// compactionInputMessages selects the message set to compact. When the
// session log's most recent compaction is a local fallback, the live context
// holds that degraded summary; the retry must instead compact the same
// messages the failed attempt saw, unwinding the fallback (and any earlier
// trailing fallbacks) from the persisted log. Falls back to the live message
// list when there is no session file to read.
func (s *agentSession) compactionInputMessages(live []*agent.Message) []*agent.Message {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return live
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return live
	}
	entries := reader.ContextEntries()
	compactionIndex := slices.IndexFunc(entries, func(entry sessionpkg.Entry) bool { return entry.Type == sessionpkg.EntryTypeCompaction })
	if compactionIndex < 0 || !entries[compactionIndex].Fallback {
		return live
	}
	unwound := agentMessagesFromUnwoundContext(reader)
	if len(unwound) == 0 {
		return live
	}
	return unwound
}

// agentMessagesFromUnwoundContext converts the fallback-unwound context
// entries to agent messages for a compaction retry.
func agentMessagesFromUnwoundContext(reader *sessionpkg.Reader) []*agent.Message {
	var sessionMsgs []*sessionpkg.Message
	for _, entry := range reader.ContextEntriesUnwound() {
		if msg := entry.ToAgentMessage(); msg != nil {
			sessionMsgs = append(sessionMsgs, msg)
		}
	}
	return sessionMessagesToAgentMessages(sessionMsgs)
}

// callModelForSummary calls the model to generate a summary using the compaction prompt.
func (s *agentSession) callModelForSummary(ctx context.Context, systemPrompt, transcript, customInstructions string) (summary string, err error) {
	defer func() {
		if ctx.Err() != nil {
			summary = ""
			err = fmt.Errorf("summarization cancelled: %w", errors.Join(err, ctx.Err(), context.Cause(ctx)))
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.model == nil {
		return "", fmt.Errorf("no model configured for summarization")
	}
	// Route a virtual model first: summaries size their input and output from
	// the model they get.
	model, level, err := s.directModel(ctx)
	if err != nil {
		return "", fmt.Errorf("summarization model: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Build the request using the AI client factory
	client, err := s.getClient(model)
	if err != nil {
		return "", fmt.Errorf("get AI client: %w", err)
	}

	instruction := "Generate a concise summary of the following conversation transcript. Preserve key decisions, open questions, files touched, and error context."
	if customInstructions != "" {
		instruction += " Additional focus: " + customInstructions
	}

	settings := s.settingsSnapshot()
	limits, err := settings.ResolveCompaction(model)
	if err != nil {
		return "", err
	}
	prompt := &ai.Context{
		SystemPrompt: systemPrompt,
		Messages: []ai.Message{
			{Role: "user", Content: instruction},
			{Role: "user", Content: trimTranscriptForSummaryWithReserve(transcript, model.ContextWindow, limits.ReserveTokens)},
		},
	}

	opts, err := s.compactionCompletionOptions(ctx, model, level)
	if err != nil {
		return "", fmt.Errorf("summary request options: %w", err)
	}
	opts = append(opts, ai.WithMaxTokens(ai.ClampMaxTokensToContext(model, prompt, s.summaryMaxTokens(model, level))))
	retryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	enabled := s.autoRetryEnabled
	s.retryCancel = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.retryCancel = nil; s.mu.Unlock() }()
	policy := s.retryPolicy()
	if !enabled {
		policy.MaxRetries = 0
	}
	retried := false
	defer func() {
		if retried {
			s.notify("summarization_retry_finished", nil)
		}
	}()
	msg, err := ai.RetryAssistantCall(retryCtx, func(callCtx context.Context) (*ai.AssistantMessage, error) {
		if retried {
			s.notify("summarization_retry_attempt_start", map[string]any{"source": "compaction"})
		}
		message, err := client.Complete(callCtx, model, prompt, opts...)
		if message != nil && hasUsage(message.Usage) {
			if usageErr := s.recordSummaryUsage(message.Usage); usageErr != nil {
				return message, errors.Join(err, usageErr)
			}
		}
		return message, err
	}, policy, func(attempt int, delay time.Duration, message string) {
		retried = true
		s.notify("summarization_retry_scheduled", map[string]any{"attempt": attempt, "maxAttempts": policy.MaxRetries, "delayMs": delay.Milliseconds(), "errorMessage": message})
	})
	if err != nil {
		return "", fmt.Errorf("model completion: %w", err)
	}
	// A length stop means the provider cut the response at the output cap
	// mid-generation. Accepting it persists a truncated fragment as the
	// summary, which then replaces the entire session context. Treat it as a
	// failed summarization so the caller falls back and a retry can succeed.
	if msg == nil {
		return "", errors.New("summarization returned no assistant message")
	}
	if msg.StopReason == ai.StopReasonAborted {
		return "", fmt.Errorf("summarization aborted: %s: %w", msg.ErrorMessage, context.Canceled)
	}
	if msg.StopReason == ai.StopReasonError {
		return "", fmt.Errorf("summarization failed: %s", msg.ErrorMessage)
	}
	if msg.StopReason == ai.StopReasonLength {
		return "", fmt.Errorf("summarization stopped at the output token limit (finish_reason=length); the summary would be truncated")
	}

	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockToolCall {
			return "", errors.New("summarization attempted to call a tool")
		}
		if block.Type == ai.ContentBlockText {
			b.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", fmt.Errorf("no text content in summarization response")
	}
	return b.String(), nil
}

func (s *agentSession) recordSummaryUsage(usage ai.Usage) error {
	usage = sessionpkg.NormalizeUsage(usage)
	s.mu.Lock()
	s.totalTokens += usage.TotalTokens
	addUsage(&s.usageTotals, usage)
	parentID := s.parentMsgID
	s.mu.Unlock()
	s.logUsage(usage, s.model.ID)
	writer, err := s.ensureSessionWriter()
	if err != nil {
		return fmt.Errorf("persist summary usage: %w", err)
	}
	if writer == nil {
		return nil
	}
	entry, err := sessionpkg.NewSummaryUsageEntry(parentID, usage)
	if err != nil {
		return err
	}
	if err := writer.Append(entry); err != nil {
		return fmt.Errorf("append summary usage: %w", err)
	}
	return nil
}

func recordedSessionUsage(reader *sessionpkg.Reader) (ai.Usage, error) {
	var total ai.Usage
	for _, entry := range reader.Entries() {
		var usage ai.Usage
		if message := entry.ToAgentMessage(); message != nil {
			usage = message.Usage
		} else if entry.Type == sessionpkg.EntryTypeCustom && entry.CustomType != nil && *entry.CustomType == sessionpkg.SummaryUsageType {
			if err := json.Unmarshal(entry.Data, &usage); err != nil {
				return total, fmt.Errorf("decode summary usage entry %s: %w", entry.ID, err)
			}
		}
		addUsage(&total, usage)
	}
	return total, nil
}

func addUsage(total *ai.Usage, usage ai.Usage) {
	usage = sessionpkg.NormalizeUsage(usage)
	total.Input += usage.Input
	total.Output += usage.Output
	total.CacheRead += usage.CacheRead
	total.CacheWrite += usage.CacheWrite
	total.CacheWrite1h += usage.CacheWrite1h
	total.TotalTokens += usage.TotalTokens
	total.Cost.Input += usage.Cost.Input
	total.Cost.Output += usage.Cost.Output
	total.Cost.CacheRead += usage.Cost.CacheRead
	total.Cost.CacheWrite += usage.Cost.CacheWrite
	total.Cost.Total += usage.Cost.Total
}

// trimTranscriptForSummary keeps the most recent transcript characters that
// fit the model's context window (chars/4 estimate, minus the reserve for the
// summary output and prompt overhead). Compaction runs exactly when history
// is huge; an untrimmed transcript gets rejected by the provider and the
// summary silently degrades to the naive fallback.
// ponytail: tail-only trim drops the oldest history; chunked iterative
// summarization (pi-style) is the upgrade path if that loss matters.
func trimTranscriptForSummaryWithReserve(transcript string, contextWindow, reserve int) string {
	if contextWindow <= 0 {
		return transcript
	}
	budgetTokens := contextWindow - reserve
	if budgetTokens < 1024 {
		budgetTokens = 1024
	}
	maxChars := budgetTokens * 4
	if len(transcript) <= maxChars {
		return transcript
	}
	cut := len(transcript) - maxChars
	// Advance to a rune boundary (≤3 bytes) so the trim never splits a
	// UTF-8 sequence. Line alignment would be nicer but a single huge
	// message can overshoot the entire budget.
	for cut < len(transcript) && !utf8.RuneStart(transcript[cut]) {
		cut++
	}
	return "[...older transcript truncated to fit the context window...]\n" + transcript[cut:]
}

// compactionCompletionOptions mirrors the authentication/session options the
// agent loop applies to normal prompts. Compact calls the provider client
// directly rather than going through Agent.Prompt, so without this path real
// providers see an unauthenticated summary request, fail immediately, and the
// caller falls back to the naive instant summary.
func (s *agentSession) compactionCompletionOptions(ctx context.Context, model *ai.Model, level agent.ThinkingLevel) ([]ai.Option, error) {
	opts := append(s.runtime.requestOptions(s.settingsSnapshot()), ai.WithSessionID(s.id))
	// Mirror buildAIOptions: with no level set, the OpenAI-compat layer sends
	// enable_thinking:false, which always-thinking models (zai glm-5.3) reject
	// (error 1210) — so forward the session's level like a normal prompt does.
	if level != "" && level != agent.ThinkingOff {
		opts = append(opts, ai.WithReasoning(ai.ReasoningLevel(level)))
	}
	if budgets := s.settingsSnapshot().ResolveThinkingBudgets(); budgets != nil {
		opts = append(opts, ai.WithThinkingBudgets(budgets))
	}
	if model == nil || (s.credentialProvider == nil && s.requestCredentials == nil) {
		return opts, nil
	}
	providerKey := model.Provider
	if providerKey == "" {
		providerKey = s.modelSpec.Provider
	}
	if providerKey == "" {
		return opts, nil
	}

	if s.requestCredentials != nil {
		key, env, err := s.requestCredentials(ctx, providerKey)
		if err != nil {
			return nil, fmt.Errorf("summary credentials for %s: %w", providerKey, err)
		}
		if env != nil {
			opts = append(opts, ai.WithProviderEnv(env))
		}
		if key != "" {
			opts = append(opts, ai.WithAPIKey(key))
		}
		return opts, nil
	}

	apiKey, err := s.credentialProvider(ctx, providerKey)
	if err != nil {
		return nil, fmt.Errorf("summary credentials for %s: %w", providerKey, err)
	}
	if apiKey != "" {
		opts = append(opts, ai.WithAPIKey(apiKey))
	}
	return opts, nil
}

// getClient returns an AI client for the current model.
func (s *agentSession) getClient(model *ai.Model) (ai.Client, error) {
	if model == nil {
		return nil, fmt.Errorf("no model configured")
	}

	if s.clientFactory != nil {
		return s.clientFactory(model)
	}

	return ai.GetClient(model)
}

// buildNaiveSummary creates a fallback summary when model-driven summarization is not available.
func (s *agentSession) buildNaiveSummary(messages []*agent.Message, customInstructions string) string {
	var summary strings.Builder
	if customInstructions != "" {
		summary.WriteString("Custom focus: ")
		summary.WriteString(customInstructions)
		summary.WriteString(".\n")
	}

	priorSummaries := preservedSessionSummaries(messages)
	if len(priorSummaries) > 0 {
		summary.WriteString("Prior context:\n")
		for _, prior := range priorSummaries {
			summary.WriteString("- ")
			summary.WriteString(prior)
			summary.WriteString("\n")
		}
	}

	recent := recentConversationHighlights(messages, 8)
	if len(recent) > 0 {
		summary.WriteString("Recent conversation:\n")
		for _, line := range recent {
			summary.WriteString("- ")
			summary.WriteString(line)
			summary.WriteString("\n")
		}
	}

	out := strings.TrimSpace(summary.String())
	if out == "" {
		return "Session compacted."
	}
	return out
}

func preservedSessionSummaries(messages []*agent.Message) []string {
	seen := make(map[string]bool)
	var out []string
	for _, msg := range messages {
		if msg == nil || msg.Role != agent.RoleSystem {
			continue
		}
		text := strings.TrimSpace(agentMessageText(msg))
		if !strings.HasPrefix(text, "Session Summary:") {
			continue
		}
		text = strings.TrimPrefix(text, "Session Summary:")
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

func recentConversationHighlights(messages []*agent.Message, limit int) []string {
	if limit <= 0 {
		return nil
	}
	out := make([]string, 0, limit)
	for i := len(messages) - 1; i >= 0 && len(out) < limit; i-- {
		msg := messages[i]
		if msg == nil || msg.Role == agent.RoleSystem || msg.Role == agent.RoleTool {
			continue
		}
		text := compactWhitespace(agentMessageText(msg))
		if text == "" {
			continue
		}
		line := fmt.Sprintf("%s: %s", msg.Role, truncateText(text, 500))
		out = append(out, line)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func compactWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncateText(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

// autoCompactIfDue runs auto-compaction when the estimated context usage
// crosses the configured threshold. Called between turns (before prompting)
// and after mid-turn message events, because a tool result can push a
// running turn past the window between turns.
func (s *agentSession) autoCompactIfDue(ctx context.Context) {
	model := s.limitsModel()
	if s.compactionConfig.TriggerPct <= 0 || model == nil {
		return
	}
	cfg := s.compactionConfig
	if t := providerTriggerTokens(model.Provider); t > 0 {
		cfg.TriggerTokens = t
	}
	if settings := s.settingsSnapshot(); settings != nil {
		limits, err := settings.ResolveCompaction(model)
		if err != nil {
			s.logger.Warn("invalid compaction settings", "error", err)
			return
		}
		if limits.ReserveTokens != 16384 || len(settings.Compaction.ModelOverrides) > 0 {
			cfg.TriggerTokens = max(1, model.ContextWindow-limits.ReserveTokens)
			cfg.TriggerPct = 100
		}
	}
	estimatedTokens := s.estimateSessionTokens(s.agent.State())
	if !compaction.ShouldCompact(estimatedTokens, model.ContextWindow, cfg) {
		return
	}
	if due, err := s.hasPersistedCompactionWork(model); err != nil || !due {
		if err != nil {
			s.logger.Warn("auto-compaction failed", "error", err)
		}
		return
	}
	if err := s.compact(ctx, compactionTrigger{reason: compactionReasonThreshold}, ""); err != nil {
		s.logger.Warn("auto-compaction failed", "error", err)
	}
}

// estimateSessionTokens estimates the total token count from agent state.
// This uses the per-message token counts from provider responses.
func (s *agentSession) estimateSessionTokens(state agent.State) int {
	return estimateContextTokens(state.Messages)
}

func estimateContextTokens(messages []*agent.Message) int {
	compactionMessages := make([]*compaction.AgentMessage, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || msg.ExcludeFromContext {
			continue
		}
		compactionMessages = append(compactionMessages, &compaction.AgentMessage{
			Role:       compaction.MessageRole(msg.Role),
			Content:    msg.Content,
			Sections:   msg.Sections,
			Usage:      msg.Usage,
			StopReason: msg.StopReason,
		})
	}
	return compaction.EstimateContextTokens(compactionMessages).Tokens
}

func (s *agentSession) State() SessionState {
	s.mu.Lock()
	state := s.agent.State()
	processing := state.IsStreaming || s.runDone != nil
	// s.model may be nil when the session was created with no configured
	// model (the TUI recovery path where the user picks a model via /model
	// before sending). Treat it as an empty string in the public state.
	modelID := ""
	if s.model != nil {
		modelID = s.model.ID
		if s.model.Provider != "" {
			modelID = s.model.Provider + "/" + s.model.ID
		}
	}

	// Snapshot the session writer path under the lock so Close() racing
	// us can't null s.sessionWriter between the nil check and the read.
	var writerPath string
	if s.sessionWriter != nil {
		writerPath = s.sessionWriter.Path()
	}
	dataDir := s.sessionDataDir
	s.mu.Unlock()

	return SessionState{
		id:          s.id,
		model:       modelID,
		cwd:         s.cwd,
		dataDir:     dataDir,
		isStreaming: processing,
		totalTokens: s.totalTokens,
		sessionPath: writerPath,
	}
}

// ID returns the session identifier.
func (s *agentSession) ID() string {
	return s.id
}

func (s *agentSession) Close(ctx context.Context) error {
	return s.close(ctx, 2*time.Second)
}

func (s *agentSession) close(ctx context.Context, cleanupGrace time.Duration) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closeFinished {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	cancel := s.cancelFn
	done := s.runDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var waitErr error
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			// Keep the writer owned until admitted work has stopped using it.
			waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(ctx), cleanupGrace)
			defer cancelWait()
			select {
			case <-done:
			case <-waitCtx.Done():
				return fmt.Errorf("session run still running: %w", errors.Join(context.Cause(ctx), waitCtx.Err()))
			}
		}
		if ctx.Err() != nil {
			waitErr = fmt.Errorf("wait for session shutdown: %w", context.Cause(ctx))
		}
	}

	// Fire `session_shutdown` on the plugin bus so extensions can observe
	// the session being torn down. The JSON stream counterpart is emitted
	// by the RPC server (or other transport) since Close is channel-agnostic.
	var err error
	if s.pluginManager != nil {
		shutdownPayload, marshalErr := json.Marshal(map[string]any{"session_id": s.id})
		if marshalErr != nil {
			err = fmt.Errorf("encode session shutdown: %w", marshalErr)
		} else {
			s.pluginManager.DispatchEvent(ctx, "session_shutdown", shutdownPayload, s.id)
		}
	}

	// Clear the runtime's active-session pointer so subsequent plugin
	// Host.SessionSendUserMessage calls don't dispatch into a closed session.
	if s.runtime != nil {
		s.runtime.mu.Lock()
		if s.runtime.currentSessionID == s.id {
			s.runtime.currentSession = nil
		}
		s.runtime.mu.Unlock()
	}

	if s.mcp != nil {
		err = errors.Join(err, s.mcp.Close())
	}

	// Close the session writer and release ownership.
	// This allows other processes to resume the session.
	err = errors.Join(err, s.closeWriter())
	if s.runtime != nil {
		s.runtime.releaseOpenSession(s.id)
		s.runtime.forgetLiveSession(s)
	}
	err = errors.Join(err, s.removeUnsentDataDir())
	s.mu.Lock()
	s.closeFinished = true
	s.closeErr = err
	s.mu.Unlock()
	return errors.Join(waitErr, err)
}

func (s *agentSession) removeUnsentDataDir() error {
	s.mu.Lock()
	unsent := s.pendingHeader != nil && s.sessionWriter == nil
	dir := s.sessionDataDir
	s.mu.Unlock()
	if !unsent || dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove unsent session data: %w", err)
	}
	return nil
}

// IsPersisted returns whether the session has a persisted backing store.
// Returns true unless NoSession was set without EphemeralExportPath.
// If a custom IsPersistedFunc was provided in SessionOptions, that is used.
func (s *agentSession) IsPersisted() bool {
	if s.isPersistedFunc != nil {
		return s.isPersistedFunc()
	}
	// Default: a session is persisted if it has, or will lazily create, a
	// session writer (i.e., it was not created with NoSession and no export path).
	return s.sessionWriter != nil || s.pendingHeader != nil
}

func pluginAgentTool(mgr *plugins.Manager, pt plugins.ToolHandlerDesc) agent.Tool {
	var outputSchema map[string]any
	var namespace *agent.ToolNamespace
	var namespaceErr error
	if len(pt.Namespace) > 0 {
		namespaceErr = json.Unmarshal(pt.Namespace, &namespace)
	}
	var schemaErr error
	if len(pt.OutputSchema) > 0 {
		schemaErr = json.Unmarshal(pt.OutputSchema, &outputSchema)
	}
	var reach func(context.Context, map[string]any) ([]string, error)
	if pt.HasReachesTools {
		reach = func(ctx context.Context, args map[string]any) ([]string, error) {
			return mgr.ReachPluginTools(ctx, pt.Name, args)
		}
	}
	return agent.Tool{
		ReachTools:          reach,
		Namespace:           namespace,
		DefaultActive:       pt.DefaultActive,
		OutputSchema:        outputSchema,
		Exposure:            agent.ToolExposure(pt.Exposure),
		ConstrainedSampling: pt.ConstrainedSampling,
		Name:                pt.Name,
		Description:         pt.Description,
		Parameters:          pt.Parameters,
		Execute: func(ctx agent.ToolContext) (*agent.ToolResult, error) {
			switch agent.ToolExposure(pt.Exposure) {
			case "", agent.ToolDirect, agent.ToolModelOnly, agent.ToolCodemode, agent.ToolDeferred, agent.ToolHidden:
			default:
				return nil, fmt.Errorf("invalid plugin tool exposure %q", pt.Exposure)
			}
			if namespaceErr != nil {
				return nil, fmt.Errorf("invalid plugin namespace: %w", namespaceErr)
			}
			if schemaErr != nil {
				return nil, fmt.Errorf("invalid plugin output schema: %w", schemaErr)
			}
			progressCh := make(chan plugins.ToolProgressEvent, 16)
			done := make(chan struct{})
			workerDone := make(chan struct{})
			mgr.RegisterToolProgressSubscriber(ctx.ToolCallID, progressCh)
			defer func() {
				mgr.UnregisterToolProgressSubscriber(ctx.ToolCallID, progressCh)
				close(done)
				<-workerDone
			}()

			go func() {
				defer close(workerDone)
				publish := func(event plugins.ToolProgressEvent) {
					if ctx.Context.Err() == nil && ctx.OnUpdate != nil {
						ctx.OnUpdate(&agent.ToolResult{
							Content:     event.Content,
							NestedCalls: slices.Clone(event.NestedCalls),
							IsError:     event.IsError,
						})
					}
				}
				for {
					select {
					case event := <-progressCh:
						publish(event)
					case <-done:
						for {
							select {
							case event := <-progressCh:
								publish(event)
							default:
								return
							}
						}
					}
				}
			}()

			// Serialize arguments to JSON for the plugin handler
			argsJSON, err := json.Marshal(ctx.Args)
			if err != nil {
				return nil, fmt.Errorf("marshal plugin args: %w", err)
			}
			result, err := mgr.ExecutePluginToolResult(ctx.Context, pt.Name, ctx.ToolCallID, string(argsJSON))
			if err != nil {
				return nil, fmt.Errorf("plugin tool error: %w", err)
			}
			return decodePluginToolResult(result)
		},
	}
}

func decodePluginToolResult(result *proto.ToolResult) (*agent.ToolResult, error) {
	converted := &agent.ToolResult{Content: result.Output, IsError: result.IsError}
	if result.NestedCalls != "" {
		if err := json.Unmarshal([]byte(result.NestedCalls), &converted.NestedCalls); err != nil {
			return nil, fmt.Errorf("decode plugin nested calls: %w", err)
		}
		if len(converted.NestedCalls) > 128 {
			converted.NestedCalls = slices.Clone(converted.NestedCalls[len(converted.NestedCalls)-128:])
		}
	}
	if result.Usage != "" {
		if err := json.Unmarshal([]byte(result.Usage), &converted.Usage); err != nil {
			return nil, fmt.Errorf("decode plugin usage: %w", err)
		}
	}
	if result.Rejection != "" {
		converted.Content = result.Rejection
		converted.IsError = true
		converted.Rejection = errors.New(result.Rejection)
	} else {
		if result.Content != "" {
			var content []ai.ContentBlock
			if err := json.Unmarshal([]byte(result.Content), &content); err != nil {
				return nil, fmt.Errorf("decode plugin content: %w", err)
			}
			converted.ContentBlocks = content
		}
		if result.StructuredContent != "" {
			if !json.Valid([]byte(result.StructuredContent)) {
				return nil, fmt.Errorf("invalid plugin structured content")
			}
			converted.StructuredContent = json.RawMessage(result.StructuredContent)
		}
	}
	return converted, nil
}

// ToolRegistryReloader refreshes extension tool registrations on an idle session.
type ToolRegistryReloader interface {
	ReloadToolRegistry(context.Context) error
}

func (s *agentSession) ReloadToolRegistry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	if s.runDone != nil || s.agent.State().IsStreaming {
		return errors.New("cannot reload tools while session is processing")
	}
	if s.pluginManager == nil || s.runtime.noTools {
		return nil
	}
	return s.reloadToolRegistryLocked()
}

func (s *agentSession) reloadToolRegistryLocked() error {
	tools := slices.DeleteFunc(slices.Clone(s.agent.State().Tools), func(tool agent.Tool) bool {
		return slices.Contains(s.pluginToolNames, tool.Name)
	})
	var nextNames []string
	for _, registered := range s.pluginManager.Tools() {
		nextNames = append(nextNames, registered.Name)
		candidate := pluginAgentTool(s.pluginManager, registered)
		selected := selectSessionTools([]agent.Tool{candidate}, s.toolAllowlist, s.toolDenylist)
		for _, tool := range selected {
			if index := slices.IndexFunc(tools, func(existing agent.Tool) bool { return existing.Name == tool.Name }); index >= 0 {
				tools[index] = tool
			} else {
				tools = append(tools, tool)
			}
		}
	}
	s.pluginToolNames = nextNames
	s.agent.SetTools(tools...)
	s.runtime.syncPluginVirtualModels()
	return nil
}

// SystemPromptReloader rebuilds an idle session's system prompt from disk.
type SystemPromptReloader interface {
	ReloadSystemPrompt(context.Context) error
}

// ReloadSystemPrompt re-reads SYSTEM.md, the context files, and the skills
// catalog. A changed prompt is recorded as a later baseline that
// collapseSystemBaselines folds into the first one on every restore.
func (s *agentSession) ReloadSystemPrompt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	if s.runDone != nil || s.agent.State().IsStreaming {
		return errors.New("cannot reload system prompt while session is processing")
	}
	prompt, err := s.runtime.buildSystemPrompt(s.cwd, s.artifactLoader, s.systemPromptOverride, s.appendSystemPrompts)
	if err != nil {
		return err
	}
	return s.reloadSystemPromptLocked(prompt)
}

func (s *agentSession) reloadSystemPromptLocked(prompt string) error {
	messages := s.agent.Messages()
	if first := slices.IndexFunc(messages, isInitialSystemBaseline); first >= 0 && agentMessageText(messages[first]) == prompt {
		return nil
	}
	content := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: prompt}}
	if pending := s.pendingInitialTranscript; pending != nil {
		baseline := *pending
		baseline.Content = content
		s.pendingInitialTranscript = &baseline
		if i := slices.Index(messages, pending); i >= 0 {
			messages[i] = &baseline
			s.agent.ReplaceMessages(messages)
			return nil
		}
	}
	replacement := &agent.Message{Role: agent.RoleSystem, Timestamp: time.UnixMilli(0), Content: content}
	if s.sessionWriter != nil && s.pendingInitialTranscript == nil {
		data, err := marshalSessionMessageEnvelope(replacement)
		if err != nil {
			return fmt.Errorf("marshal system prompt: %w", err)
		}
		entry := sessionpkg.NewMessageEntry(s.parentMsgID, data, nil)
		if err := s.sessionWriter.Append(entry); err != nil {
			return fmt.Errorf("append system prompt: %w", err)
		}
		s.parentMsgID = &entry.ID
	}
	s.agent.ReplaceMessages(collapseSystemBaselines(append(messages, replacement)))
	return nil
}

// collapseSystemBaselines applies system prompt reloads: the last baseline
// with text supplies the first baseline's text, keeping the prompt ahead of
// compaction summaries, and the later baselines keep only their declarations.
func collapseSystemBaselines(messages []*agent.Message) []*agent.Message {
	var indexes []int
	for i, message := range messages {
		if isInitialSystemBaseline(message) && !message.ExcludeFromContext {
			indexes = append(indexes, i)
		}
	}
	if len(indexes) < 2 {
		return messages
	}
	var content []ai.ContentBlock
	for _, i := range indexes {
		if len(messages[i].Content) > 0 {
			content = messages[i].Content
		}
	}
	out := slices.Clone(messages)
	for n, i := range indexes {
		collapsed := *messages[i]
		collapsed.Content = nil
		if n == 0 {
			collapsed.Content = content
		}
		out[i] = &collapsed
	}
	return out
}

// SettingsReloader updates an idle session after settings files are reloaded.
type SettingsReloader interface {
	ReloadSettings(context.Context, *ai.Settings) error
}

func (r *runtime) requestOptions(settings *ai.Settings) []ai.Option {
	opts := settings.ResolveRetry().Provider.RequestOptions()
	if r != nil && r.noEnvCredentials {
		opts = append(opts, ai.WithoutEnvCredentials())
	}
	return opts
}

// SettingsProvider reports the settings a session currently applies.
type SettingsProvider interface {
	Settings() *ai.Settings
}

// Settings returns the settings the session last applied.
func (s *agentSession) Settings() *ai.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// ReloadSettings activates newly added defaults without undoing session tool changes.
func (s *agentSession) ReloadSettings(ctx context.Context, settings *ai.Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	if s.runDone != nil || s.agent.State().IsStreaming {
		return errors.New("cannot reload settings while session is processing")
	}
	return s.reloadSettingsLocked(settings)
}

func (s *agentSession) reloadSettingsLocked(settings *ai.Settings) error {
	limits, err := settings.ResolveCompaction(s.model)
	if err != nil {
		return err
	}
	previousLimits, err := s.settings.ResolveCompaction(s.model)
	if err != nil {
		return err
	}
	steering, followUp, err := settingsQueueModes(settings)
	if err != nil {
		return err
	}
	transport, err := settings.ResolveTransport()
	if err != nil {
		return err
	}
	if s.settings == nil || settings == nil || s.settings.SteeringMode != settings.SteeringMode {
		s.agent.SetSteeringMode(steering)
	}
	if s.settings == nil || settings == nil || s.settings.FollowUpMode != settings.FollowUpMode {
		s.agent.SetFollowUpMode(followUp)
	}
	s.settings = settings
	s.runtime.replaceSettings(settings)
	if s.pluginManager != nil {
		s.pluginManager.SetSettings(settings)
	}
	s.agent.SetThinkingBudgets(settings.ResolveThinkingBudgets())
	s.agent.SetRequestOptions(s.runtime.requestOptions(settings)...)
	s.agent.SetTransport(transport)
	if !s.runtime.noTools && !s.runtime.noBuiltinTools {
		tools := s.agent.State().Tools
		for _, builtin := range agent.BuiltInToolsWithConfig(buildToolConfig(settings)) {
			if slices.Contains(s.pluginToolNames, builtin.Name) {
				continue
			}
			if index := slices.IndexFunc(tools, func(tool agent.Tool) bool { return tool.Name == builtin.Name }); index >= 0 {
				tools[index] = builtin
			}
		}
		s.agent.SetTools(tools...)
	}
	if limits.Enabled != previousLimits.Enabled {
		if limits.Enabled {
			s.compactionConfig = s.autoCompactionDefault
			if s.compactionConfig.TriggerPct <= 0 {
				s.compactionConfig = compaction.DefaultConfig()
			}
		} else {
			s.compactionConfig.TriggerPct = 0
		}
	}
	next := resolveDefaultToolSelection(settings)
	previous := s.defaultToolNames
	s.defaultToolNames = slices.Clone(next)
	if s.toolAllowlist != nil && !toolSelectionHasModifiers(s.toolAllowlist) {
		return nil
	}
	if s.runtime.noTools || s.runtime.noBuiltinTools {
		return nil
	}
	next = applyToolModifiers(next, s.toolAllowlist)
	previous = applyToolModifiers(previous, s.toolAllowlist)
	if len(next) == 0 {
		return nil
	}
	tools := s.agent.State().Tools
	for _, tool := range filterToolsByName(agent.BuiltInToolsWithConfig(buildToolConfig(settings)), next, s.toolDenylist) {
		if !slices.Contains(previous, tool.Name) && !slices.ContainsFunc(tools, func(t agent.Tool) bool { return t.Name == tool.Name }) {
			tools = append(tools, tool)
		}
	}
	s.agent.SetTools(tools...)
	return nil
}

// SetThinkingLevel changes the thinking level mid-session.
// The new level takes effect on the next call to Send.
func (s *agentSession) SetThinkingLevel(level string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.thinkingLevel = agent.ThinkingLevel(level)
	s.agent.SetThinkingLevel(s.thinkingLevel)
	return nil
}

// SetModel switches the active model for future turns. The argument is
// the canonical "provider/model-id" string (matching State().Model()).
// The model must exist in the global ai.Models() registry; the agent's
// underlying model is updated immediately. In-flight turns are
// unaffected, so a model change mid-stream is observed from the next
// prompt onward.
func (s *agentSession) SetModel(model string) error {
	if model == "" {
		return fmt.Errorf("session: SetModel: empty model id")
	}
	m, err := s.runtime.resolveChatModel(model)
	if err != nil {
		return fmt.Errorf("session: SetModel: %w", err)
	}

	s.mu.Lock()
	s.model = m
	s.routedModel = nil
	s.modelSpec = ai.ModelSpec{Provider: m.Provider, Model: m.ID}
	s.pendingInitialModelProvider = m.Provider
	s.pendingInitialModel = m.ID
	writer := s.sessionWriter
	s.agent.SetModel(m)
	s.mu.Unlock()

	// If the session has already been materialized, persist the model change so
	// future resume/replay uses the same active model. Lazy sessions only update
	// the pending initial model above, avoiding an empty session file just because
	// the user opened the picker before sending the first prompt.
	if writer != nil {
		if err := writer.Append(sessionpkg.NewModelChangeEntry(m.Provider, m.ID)); err != nil {
			return fmt.Errorf("append model change: %w", err)
		}
	}

	return nil
}

// SlashCommands returns a snapshot of the session's command registry
// suitable for surfacing in a frontend's completion UI. The result is a
// copy: callers may iterate it freely without holding the registry's
// lock, and the session's underlying registry may be mutated by
// plugin reloads in between calls.
func (s *agentSession) SlashCommands() []SlashCommandInfo {
	if s.cmdRegistry == nil {
		return nil
	}
	infos := s.cmdRegistry.List()
	out := make([]SlashCommandInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, SlashCommandInfo{
			Name:         info.Name,
			Description:  info.Description,
			ArgumentHint: info.ArgumentHint,
			Source:       info.Source.String(),
		})
	}
	return out
}

func agentMessageFromSDK(msg Message) *agent.Message {
	content := slices.Clone(msg.Content)
	if content == nil {
		content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: msg.Text}}
	}
	content = append(content, msg.Images...)
	role := agent.RoleUser
	if msg.Role == string(agent.RoleCustom) {
		role = agent.RoleCustom
	}
	return &agent.Message{CustomType: msg.CustomType, Display: msg.Display, Details: msg.Details, ExcludeFromContext: msg.ExcludeFromContext, Role: role, Data: msg.Data, Content: content, Timestamp: time.Now()}
}

func sdkMessageFromAgent(msg *agent.Message) Message {
	out := Message{
		ExcludeFromContext: msg.ExcludeFromContext, CustomType: msg.CustomType, Display: msg.Display, Details: msg.Details,
		Data:       msg.Data,
		Role:       string(msg.Role),
		Content:    append([]ai.ContentBlock(nil), msg.Content...),
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		IsError:    msg.IsError,
	}
	out.StructuredContent = slices.Clone(msg.StructuredContent)
	out.NestedCalls = slices.Clone(msg.NestedCalls)
	if msg.ToolUsage != nil {
		out.ToolUsage = new(*msg.ToolUsage)
	}

	if msg.DurationMs != nil {
		out.DurationMs = new(*msg.DurationMs)
	}
	for _, block := range msg.Content {
		switch block.Type {
		case ai.ContentBlockText:
			out.Text += block.Text
		case ai.ContentBlockImage:
			out.Images = append(out.Images, block)
		case ai.ContentBlockThinking:
			if block.Thinking != "" {
				out.Text += block.Thinking
			}
		}
	}
	return out
}

func settingsQueueModes(settings *ai.Settings) (agent.QueueMode, agent.QueueMode, error) {
	steering, followUp := agent.QueueModeOneAtATime, agent.QueueModeOneAtATime
	if settings == nil {
		return steering, followUp, nil
	}
	var err error
	if settings.SteeringMode != "" {
		steering, err = parseQueueMode(settings.SteeringMode)
		if err != nil {
			return "", "", fmt.Errorf("steering mode: %w", err)
		}
	}
	if settings.FollowUpMode != "" {
		followUp, err = parseQueueMode(settings.FollowUpMode)
		if err != nil {
			return "", "", fmt.Errorf("follow-up mode: %w", err)
		}
	}
	return steering, followUp, nil
}

func parseQueueMode(mode string) (agent.QueueMode, error) {
	switch agent.QueueMode(mode) {
	case agent.QueueModeOneAtATime, agent.QueueModeAll:
		return agent.QueueMode(mode), nil
	default:
		return "", fmt.Errorf("invalid queue mode %q", mode)
	}
}

func (s *agentSession) Steer(msg Message) error {
	s.mu.Lock()
	prepared, err := s.prepareMessageLocked(msg)
	if err == nil {
		s.agent.Steer(agentMessageFromSDK(prepared))
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.recordQueueUpdate()
	return nil
}

func (s *agentSession) FollowUp(msg Message) error {
	s.mu.Lock()
	prepared, err := s.prepareMessageLocked(msg)
	if err == nil {
		s.agent.FollowUp(agentMessageFromSDK(prepared))
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.recordQueueUpdate()
	return nil
}

// recordQueueUpdate writes a queue_update entry to the session log so
// downstream consumers (export, replay, TUI) can observe steering/follow-up
// queue mutations. No-op when the session has no writer (e.g. ephemeral mode).
func (s *agentSession) recordQueueUpdate() {
	writer, err := s.ensureSessionWriter()
	if err != nil || writer == nil {
		if err != nil {
			s.logger.Warn("session write failed", "error", err)
		}
		return
	}
	steering, followUp := s.agent.QueueSizes()
	entry := sessionpkg.NewQueueUpdateEntry(steering, followUp)
	entry.ID = sessionpkg.NewID()
	if err := writer.Append(entry); err != nil {
		// Best-effort: queue_update is informational; failure to write it
		// must not break the queue mutation the caller just requested.
		return
	}
}

// QueueSizes reports the current size of the agent's steering and
// follow-up message queues. The TUI polls this on every relevant
// state change (after Steer/FollowUp/Send completes, on message
// boundaries) to keep the pending-messages region in sync. The agent
// already serializes queue access internally so no extra lock is
// needed here.
func (s *agentSession) QueueSizes() (steering, followUp int) {
	return s.agent.QueueSizes()
}

func (s *agentSession) GetAvailableModels(ctx context.Context) ([]*ai.Model, error) {
	models, err := s.runtime.modelRegistry.Models()
	if err != nil {
		return nil, fmt.Errorf("list local model catalog: %w", err)
	}
	// Sessions converse with chat models; image and classifier entries are
	// for operation-specific calls.
	models = slices.DeleteFunc(models, func(model *ai.Model) bool { return model.Type != "" && model.Type != "chat" })
	if models, err = s.filterCopilotModels(ctx, models); err != nil {
		return nil, err
	}
	if s.credentialProvider == nil {
		return models, nil
	}
	for _, provider := range []string{ai.ProviderOpenRouter, ai.ProviderRadius} {
		key, err := s.credentialProvider(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("%s catalog credential: %w", provider, err)
		}
		if key == "" {
			continue
		}
		fetch := func(ctx context.Context, catalog []*ai.Model, baseURL string) ([]*ai.Model, error) {
			return ai.PersonalizeOpenRouterCatalog(ctx, http.DefaultClient, catalog, baseURL, key)
		}
		if provider == ai.ProviderRadius {
			// Radius org owners can disable models, so the account catalog
			// replaces the shipped Radius entries; those only cover the time
			// before any catalog has been fetched or cached.
			fetch = func(ctx context.Context, _ []*ai.Model, baseURL string) ([]*ai.Model, error) {
				return ai.FetchRadiusCatalog(ctx, http.DefaultClient, baseURL, key)
			}
		}
		if models, err = s.personalizeCatalog(ctx, models, provider, key, s.catalogBaseURL(provider), fetch); err != nil {
			return nil, err
		}
	}
	// llama.cpp models exist only on a configured server, which may need no key.
	server := s.catalogBaseURL(ai.ProviderLlamaCpp)
	if server == "" {
		server = os.Getenv("LLAMA_BASE_URL")
	}
	if strings.TrimSpace(server) == "" {
		return models, nil
	}
	key, err := s.credentialProvider(ctx, ai.ProviderLlamaCpp)
	if err != nil {
		return nil, fmt.Errorf("%s catalog credential: %w", ai.ProviderLlamaCpp, err)
	}
	models, err = s.personalizeCatalog(ctx, models, ai.ProviderLlamaCpp, key, server, func(ctx context.Context, _ []*ai.Model, baseURL string) ([]*ai.Model, error) {
		return ai.FetchLlamaCatalog(ctx, http.DefaultClient, baseURL, key)
	})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(models, func(model *ai.Model) bool { return model.Type != "" && model.Type != "chat" }), nil
}

// filterCopilotModels keeps only the GitHub Copilot models the signed-in
// account recorded as available at sign-in or refresh.
func (s *agentSession) filterCopilotModels(ctx context.Context, models []*ai.Model) ([]*ai.Model, error) {
	if s.credStore == nil {
		return models, nil
	}
	cred, err := s.credStore.Get(ctx, ai.ProviderGitHubCopilot)
	if errors.Is(err, ai.ErrCredentialNotFound) {
		return models, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s catalog credential: %w", ai.ProviderGitHubCopilot, err)
	}
	available, ok := oauth.CopilotAvailableModels(cred.Extra)
	if cred.Type != ai.CredentialTypeOAuth || !ok {
		return models, nil
	}
	return slices.DeleteFunc(models, func(model *ai.Model) bool {
		return model.Provider == ai.ProviderGitHubCopilot && !slices.Contains(available, model.ID)
	}), nil
}

// catalogBaseURL is the endpoint whose catalog applies to provider: the
// session model's when it belongs to provider, otherwise the configured one.
func (s *agentSession) catalogBaseURL(provider string) string {
	var baseURL string
	if registered, ok := s.runtime.modelRegistry.Provider(provider); ok {
		baseURL = registered.BaseURL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.model != nil && s.model.Provider == provider && s.model.BaseURL != "" {
		baseURL = s.model.BaseURL
	}
	return baseURL
}

type catalogCacheKey struct {
	provider string
	hash     [32]byte
}

// personalizeCatalog replaces provider's entries in models with the catalog
// fetch returns for the session's key and endpoint. A failed fetch falls back
// to the last catalog for the same key and endpoint, in memory or on disk,
// and then to the shipped entries.
func (s *agentSession) personalizeCatalog(ctx context.Context, models []*ai.Model, provider, key, baseURL string, fetch func(context.Context, []*ai.Model, string) ([]*ai.Model, error)) ([]*ai.Model, error) {
	var catalog []*ai.Model
	var others []*ai.Model
	for _, model := range models {
		if model.Provider == provider {
			catalog = append(catalog, model)
		} else {
			others = append(others, model)
		}
	}
	cacheKey := catalogCacheKey{provider: provider, hash: sha256.Sum256([]byte(baseURL + "\x00" + key))}
	personalized, err := fetch(ctx, catalog, baseURL)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.logger.Warn(provider+" catalog unavailable; using offline catalog", "error", err)
		s.mu.Lock()
		cached, ok := s.providerCatalogs[cacheKey]
		s.mu.Unlock()
		if !ok {
			data, readErr := os.ReadFile(catalogCachePath(cacheKey))
			if readErr == nil {
				readErr = json.Unmarshal(data, &cached)
				ok = readErr == nil && cached != nil && !slices.ContainsFunc(cached, func(model *ai.Model) bool { return model == nil || model.Provider != provider })
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				s.logger.Warn("read offline "+provider+" catalog", "error", readErr)
			}
		}
		if ok {
			return append(others, slices.Clone(cached)...), nil
		}
		return models, nil
	}
	s.mu.Lock()
	if s.providerCatalogs == nil {
		s.providerCatalogs = map[catalogCacheKey][]*ai.Model{}
	}
	s.providerCatalogs[cacheKey] = slices.Clone(personalized)
	s.mu.Unlock()
	if err := saveCatalog(cacheKey, personalized); err != nil {
		s.logger.Warn("save offline "+provider+" catalog", "error", err)
	}
	return append(others, personalized...), nil
}

func catalogCachePath(key catalogCacheKey) string {
	return filepath.Join(paths.ConfigDir(), "cache", key.provider, fmt.Sprintf("%x.json", key.hash))
}

func saveCatalog(key catalogCacheKey, models []*ai.Model) (err error) {
	data, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	path := catalogCachePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create catalog cache: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return fmt.Errorf("create catalog file: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove catalog temporary file: %w", removeErr))
		}
	}()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write catalog: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace catalog cache: %w", err)
	}
	return nil
}

func (s *agentSession) CycleModel() (*ai.Model, error) {
	s.mu.Lock()
	if s.model == nil {
		s.mu.Unlock()
		return nil, nil
	}
	current := *s.model
	s.mu.Unlock()
	catalog, err := s.runtime.modelRegistry.Models()
	if err != nil {
		return nil, fmt.Errorf("cycle model catalog: %w", err)
	}
	models := slices.DeleteFunc(catalog, func(model *ai.Model) bool {
		return model.Provider != current.Provider || (model.Type != "" && model.Type != "chat")
	})
	if len(models) == 0 {
		return nil, nil
	}
	index := 0
	for i, model := range models {
		if model.ID == current.ID {
			index = (i + 1) % len(models)
			break
		}
	}
	next := models[index]
	if err := s.SetModel(next.Provider + "/" + next.ID); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *agentSession) CycleThinkingLevel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	levels := ai.GetSupportedThinkingLevels(s.model)
	idx := 0
	for i, level := range levels {
		if s.thinkingLevel == level {
			idx = (i + 1) % len(levels)
			break
		}
	}
	s.thinkingLevel = levels[idx]
	s.agent.SetThinkingLevel(s.thinkingLevel)
	return string(s.thinkingLevel)
}

func (s *agentSession) SetSteeringMode(mode string) error {
	parsed, err := parseQueueMode(mode)
	if err != nil {
		return err
	}
	s.agent.SetSteeringMode(parsed)
	return nil
}

func (s *agentSession) SetFollowUpMode(mode string) error {
	parsed, err := parseQueueMode(mode)
	if err != nil {
		return err
	}
	s.agent.SetFollowUpMode(parsed)
	return nil
}

func (s *agentSession) SetAutoCompactionEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		if s.autoCompactionDefault.TriggerPct <= 0 {
			s.autoCompactionDefault = compaction.DefaultConfig()
		}
		s.compactionConfig = s.autoCompactionDefault
		return
	}
	s.compactionConfig.TriggerPct = 0
}

func (s *agentSession) SetAutoRetryEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoRetryEnabled = enabled
}

func (s *agentSession) AbortRetry() {
	s.mu.Lock()
	cancel := s.retryCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func isInitialSystemBaseline(message *agent.Message) bool {
	return message != nil && message.Role == agent.RoleSystem && message.Timestamp.Equal(time.UnixMilli(0))
}

func (s *agentSession) GetMessages() []Message {
	state := s.agent.State()
	messages := make([]Message, 0, len(state.Messages))
	for _, msg := range state.Messages {
		if isInitialSystemBaseline(msg) {
			continue
		}
		messages = append(messages, sdkMessageFromAgent(msg))
	}
	return messages
}

// GetAllMessages returns the full persisted transcript, ignoring compaction
// truncation. Falls back to the live message list when there is no session
// file to read (ephemeral sessions), so callers get the best available view.
func (s *agentSession) GetAllMessages() []Message {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return s.GetMessages()
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return s.GetMessages()
	}
	msgs := sessionMessagesToAgentMessages(reader.AllMessages())
	out := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		if isInitialSystemBaseline(msg) {
			continue
		}
		out = append(out, sdkMessageFromAgent(msg))
	}
	return out
}

// History pages the persisted display history through an offset index of the
// session file, so each page reads only its own entries.
func (s *agentSession) History(before string, limit int) ([]HistoryEntry, string, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	head := ""
	if s.parentMsgID != nil {
		head = *s.parentMsgID
	}
	s.mu.Unlock()
	if writer == nil || limit <= 0 {
		return nil, "", nil
	}
	s.historyMu.Lock()
	entries, next, err := s.historyIndex.History(writer.Path(), head, before, limit)
	s.historyMu.Unlock()
	if err != nil {
		return nil, "", fmt.Errorf("read session history: %w", err)
	}
	out := make([]HistoryEntry, 0, len(entries))
	for i := range entries {
		msg := entries[i].ToAgentMessage()
		if msg == nil {
			continue
		}
		agentMsg := sessionMessagesToAgentMessages([]*sessionpkg.Message{msg})[0]
		if isInitialSystemBaseline(agentMsg) {
			continue
		}
		out = append(out, HistoryEntry{EntryID: entries[i].ID, Message: sdkMessageFromAgent(agentMsg)})
	}
	return out, next, nil
}

func (s *agentSession) GetUserMessagesForForking() []Message {
	all := s.GetMessages()
	users := make([]Message, 0, len(all))
	for _, msg := range all {
		if msg.Role == string(agent.RoleUser) {
			users = append(users, msg)
		}
	}
	return users
}

// GetForkEntries returns the user and assistant messages on the session's
// current branch, oldest first, with the entry IDs needed to fork at them.
// A session that has not been persisted yet has no entries.
func (s *agentSession) GetForkEntries() ([]ForkEntry, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return nil, nil
	}
	writerPath := writer.Path()
	reader, err := sessionpkg.ReadFile(writerPath)
	if err != nil {
		return nil, fmt.Errorf("read session for fork entries: %w", err)
	}
	branch := entriesTo(sessionEntries(reader), reader.CurrentPosition())
	out := make([]ForkEntry, 0, len(branch))
	for _, entry := range branch {
		if !entry.Type.InContext() {
			continue
		}
		msg := entry.ToAgentMessage()
		if msg == nil {
			continue
		}
		role := string(msg.Role)
		if role != string(agent.RoleUser) && role != string(agent.RoleAssistant) {
			continue
		}
		out = append(out, ForkEntry{EntryID: entry.ID, Role: role, Text: forkEntryText(msg.Content)})
	}
	return out, nil
}

// GetForgetEntries returns the entries /forget can drop from, oldest first.
func (s *agentSession) GetForgetEntries() ([]ForkEntry, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return nil, nil
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return nil, fmt.Errorf("read session for forget entries: %w", err)
	}
	var out []ForkEntry
	for _, entry := range reader.AllEntries() {
		if entry.Type == sessionpkg.EntryTypeCompaction && entry.Summary != nil {
			out = append(out, ForkEntry{EntryID: entry.ID, Role: string(sessionpkg.EntryTypeCompaction), Text: *entry.Summary})
			continue
		}
		msg := entry.ToAgentMessage()
		if msg == nil {
			continue
		}
		role := string(msg.Role)
		if role != string(agent.RoleUser) && role != string(agent.RoleAssistant) {
			continue
		}
		out = append(out, ForkEntry{EntryID: entry.ID, Role: role, Text: forkEntryText(msg.Content)})
	}
	return out, nil
}

// Forget marks entryID and every later entry on the current branch deleted,
// then reloads the agent context from what remains.
func (s *agentSession) Forget(entryID string) error {
	if s.agent.State().IsStreaming {
		return fmt.Errorf("cannot forget while the agent is running")
	}
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return fmt.Errorf("session is not persisted")
	}
	reader, err := sessionpkg.ReadFile(writer.Path())
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	ids, parent, err := reader.ForgetSet(entryID)
	if err != nil {
		return err
	}
	if err := writer.MarkDeleted(ids); err != nil {
		return fmt.Errorf("mark entries deleted: %w", err)
	}
	if parent != "" {
		if err := writer.Append(sessionpkg.NewPositionChangeEntry(parent)); err != nil {
			return fmt.Errorf("append position change: %w", err)
		}
	}
	if reader, err = sessionpkg.ReadFile(writer.Path()); err != nil {
		return fmt.Errorf("reread session: %w", err)
	}
	messages, err := agentMessagesFromSessionReader(reader, "")
	if err != nil {
		return err
	}
	if s.runtime != nil && !s.runtime.noTools && !s.runtime.noBuiltinTools {
		tools := restoreDeclaredBuiltinTools(s.agent.State().Tools, messages, s.settings, s.toolAllowlist, s.toolDenylist)
		s.agent.SetTools(tools...)
	}
	s.agent.RestoreMessages(messages)
	s.mu.Lock()
	s.parentMsgID = nil
	if pos := reader.CurrentPosition(); pos != "" {
		s.parentMsgID = &pos
	}
	s.mu.Unlock()
	return nil
}

// EditContext appends a validated, branch-local context replacement.
func (s *agentSession) EditContext(ctx context.Context, entryID string, replacement json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return fmt.Errorf("session closed")
	}
	if s.cancelFn != nil || s.agent.State().IsStreaming {
		return fmt.Errorf("cannot edit context while the agent is running")
	}
	if s.sessionWriter == nil {
		return fmt.Errorf("session is not persisted")
	}
	reader, err := sessionpkg.ReadFile(s.sessionWriter.Path())
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	manager := sessionpkg.NewManager(reader.Header().CWD)
	manager.LoadEntries(reader)
	id, err := manager.AppendContextEdit(entryID, replacement)
	if err != nil {
		return fmt.Errorf("edit context: %w", err)
	}
	entries := manager.Entries()
	entry := entries[len(entries)-1]
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.sessionWriter.Append(entry); err != nil {
		return fmt.Errorf("append context edit: %w", err)
	}
	reader, err = sessionpkg.ReadFile(s.sessionWriter.Path())
	if err != nil {
		return fmt.Errorf("reload context: %w", err)
	}
	messages, err := agentMessagesFromSessionReader(reader, "")
	if err != nil {
		return fmt.Errorf("restore context: %w", err)
	}
	if s.runtime != nil && !s.runtime.noTools && !s.runtime.noBuiltinTools {
		tools := restoreDeclaredBuiltinTools(s.agent.State().Tools, messages, s.settings, s.toolAllowlist, s.toolDenylist)
		s.agent.SetTools(tools...)
	}
	s.agent.RestoreMessages(messages)
	s.parentMsgID = &id
	return nil
}

func forkEntryText(blocks []ai.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ai.ContentBlockText:
			parts = append(parts, block.Text)
		case ai.ContentBlockToolCall:
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("tool: %s %s", block.Name, block.Arguments)))
		}
	}
	return strings.Join(parts, "\n")
}

func (s *agentSession) ExportHTML(_ context.Context, path string) (string, error) {
	reader, err := s.exportReader()
	if err != nil {
		return "", err
	}
	data, err := sessionpkg.RenderHTML(reader, sessionpkg.Options{})
	if err != nil {
		return "", err
	}
	if path == "" {
		path = "tau-session-" + strings.TrimSuffix(filepath.Base(reader.Path()), ".jsonl") + ".html"
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ExportJSONL writes the header and active branch to path, defaulting to a
// timestamped file in the working directory.
func (s *agentSession) ExportJSONL(_ context.Context, path string) (string, error) {
	reader, err := s.exportReader()
	if err != nil {
		return "", err
	}
	data, err := reader.BranchJSONL()
	if err != nil {
		return "", err
	}
	if path == "" {
		path = "session-" + strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15:04:05.000Z")) + ".jsonl"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (s *agentSession) exportReader() (*sessionpkg.Reader, error) {
	s.mu.Lock()
	writer, pending := s.sessionWriter, s.pendingHeader != nil
	s.mu.Unlock()
	if pending && writer == nil {
		return nil, errors.New("nothing to export yet - start a conversation first")
	}
	if writer == nil {
		return nil, errors.New("cannot export an in-memory session")
	}
	return sessionpkg.ReadFile(writer.Path())
}

func (s *agentSession) GetLastAssistantText() string { return s.getLastAssistantText() }

func (s *agentSession) GetSessionName() string { return s.getSessionName() }

func (s *agentSession) Stats() SessionStats {
	state := s.State()
	messages := s.GetMessages()
	contextWindow := 0
	if model := s.limitsModel(); model != nil {
		contextWindow = model.ContextWindow
	}
	contextTokens := estimateContextTokens(s.agent.State().Messages)
	usedPct := 0.0
	if contextWindow > 0 {
		usedPct = float64(contextTokens) / float64(contextWindow) * 100
	}
	s.mu.Lock()
	thinkingLevel := string(s.thinkingLevel)
	autoCompaction := s.compactionConfig.TriggerPct > 0
	usage := s.usageTotals
	s.mu.Unlock()
	return SessionStats{
		SessionID:      state.SessionID(),
		MessageCount:   len(messages),
		TotalTokens:    contextTokens,
		ContextWindow:  contextWindow,
		ContextUsedPct: usedPct,
		Model:          state.Model(),
		ThinkingLevel:  thinkingLevel,
		AutoCompaction: autoCompaction,
		Usage:          usage,
	}
}

// closeWriter closes the session writer and releases the owner file.
// Callers must NOT hold s.mu; this method takes it to serialize against
// concurrent send/append paths that snapshot the writer pointer.
func (s *agentSession) closeWriter() error {
	s.mu.Lock()
	writer := s.sessionWriter
	s.sessionWriter = nil
	s.mu.Unlock()
	if writer != nil {
		if err := writer.Close(); err != nil {
			return fmt.Errorf("close session writer: %w", err)
		}
	}
	return nil
}

// EmitDisconnectedSessionEnd emits a session_end event with disconnected reason.
// This is called by the RPC server when the client disconnects without sending shutdown.
func (s *agentSession) EmitDisconnectedSessionEnd(events chan<- Event) {
	s.mu.Lock()
	e := Event{
		Type:      "session_end",
		SessionID: s.id,
		Seq:       s.seq,
		Timestamp: time.Now().Format(time.RFC3339),
		Payload: map[string]any{
			"id":           s.id,
			"total_tokens": s.totalTokens,
			"cost_usd":     0.0,
			"reason":       "disconnected",
		},
	}
	s.seq++
	s.mu.Unlock()
	events <- e
}

func (s *agentSession) emit(ch chan<- Event, e Event) {
	s.mu.Lock()
	e.Seq = s.seq
	s.seq++
	e.Timestamp = time.Now().Format(time.RFC3339)
	s.mu.Unlock()
	ch <- e
}

// SetEventObserver installs the out-of-band event observer (EventObserver).
func (s *agentSession) SetEventObserver(fn func(Event)) {
	s.mu.Lock()
	s.eventObserver = fn
	s.mu.Unlock()
}

// notify raises an event to the out-of-band observer, stamping it with the same
// sequence and timestamp a Send-channel event would carry so a frontend can
// order the two together. No-ops when nothing is observing.
func (s *agentSession) notify(eventType string, payload any) {
	s.mu.Lock()
	observer := s.eventObserver
	if observer == nil {
		s.mu.Unlock()
		return
	}
	e := Event{
		Type:      eventType,
		SessionID: s.id,
		Seq:       s.seq,
		Timestamp: time.Now().Format(time.RFC3339),
		Payload:   payload,
	}
	s.seq++
	s.mu.Unlock()
	observer(e)
}

func (s *agentSession) ensureSessionWriter() (*sessionpkg.Writer, error) {
	s.sessionInitMu.Lock()
	defer s.sessionInitMu.Unlock()
	s.mu.Lock()
	writer := s.sessionWriter
	pendingHeader := s.pendingHeader
	pendingSessionDir := s.pendingSessionDir
	pendingModelProvider := s.pendingInitialModelProvider
	pendingModel := s.pendingInitialModel
	pendingThinking := s.pendingInitialThinking
	if writer == nil && pendingHeader != nil && pendingSessionDir != "" {
		storage, err := sessionpkg.NewStorageAt(pendingSessionDir)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("open session storage: %w", err)
		}
		writer, err = storage.Create(*pendingHeader)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("create session file: %w", err)
		}
		s.sessionWriter = writer
		s.sessionFileBeforeRename = writer.Path()
		s.pendingHeader = nil
		s.pendingSessionDir = ""
	}
	s.mu.Unlock()

	if writer == nil {
		return nil, nil
	}
	if pendingHeader == nil {
		return writer, s.persistInitialTranscript()
	}
	if pendingModelProvider != "" || pendingModel != "" {
		if err := writer.Append(sessionpkg.NewModelChangeEntry(pendingModelProvider, pendingModel)); err != nil {
			return nil, fmt.Errorf("append initial model change: %w", err)
		}
	}
	if pendingThinking != "" {
		if err := writer.Append(sessionpkg.NewThinkingLevelChangeEntry(pendingThinking)); err != nil {
			return nil, fmt.Errorf("append initial thinking level change: %w", err)
		}
	}
	s.mu.Lock()
	if s.pendingHeader == nil {
		s.pendingInitialModelProvider = ""
		s.pendingInitialModel = ""
		s.pendingInitialThinking = ""
	}
	s.mu.Unlock()

	return writer, s.persistInitialTranscript()
}

func (s *agentSession) persistInitialTranscript() error {
	s.mu.Lock()
	baseline := s.pendingInitialTranscript
	s.mu.Unlock()
	if baseline == nil {
		return nil
	}
	if err := s.persistMessage(baseline); err != nil {
		return fmt.Errorf("persist initial transcript: %w", err)
	}
	s.mu.Lock()
	s.pendingInitialTranscript = nil
	s.mu.Unlock()
	return nil
}

// recordProviderError appends an error entry to the session log when a
// provider request fails. Without it, API rejections (e.g. zai 1214 "The
// messages parameter is illegal") surface only as a transient UI event, and
// the request shape that was rejected is unrecoverable after the fact.
// User aborts are not failures and are not recorded.
func (s *agentSession) recordProviderError(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	writer, werr := s.ensureSessionWriter()
	if werr != nil {
		s.logger.Warn("session write failed", "error", werr)
		return
	}
	if writer == nil {
		return
	}

	state := s.agent.State()
	summary := errorRequestSummary(state.Messages, state.SystemPrompt)
	if s.model != nil {
		summary["api"] = s.model.API
	}
	s.mu.Lock()
	thinking := string(s.thinkingLevel)
	parentID := s.parentMsgID
	s.mu.Unlock()
	if thinking != "" {
		summary["thinkingLevel"] = thinking
	}
	data, merr := json.Marshal(summary)
	if merr != nil {
		s.logger.Warn("marshal error request summary", "error", merr)
		data = nil
	}

	var provider, modelID string
	if s.model != nil {
		provider, modelID = s.model.Provider, s.model.ID
	}
	entry := sessionpkg.NewErrorEntry(provider, modelID, err.Error(), data)
	if parentID != nil {
		entry.ParentID = parentID
	}
	if aerr := writer.Append(entry); aerr != nil {
		s.logger.Warn("session write failed", "error", aerr)
	}
}

// errorRequestSummary captures the shape of the request that failed: the
// message sequence with per-message block kinds and sizes. Message contents
// already live in the session's message entries; this records the structure
// providers actually validate (role order, tool-call pairing, empty content)
// so a rejection can be diagnosed from the session log alone.
func errorRequestSummary(messages []*agent.Message, systemPrompt string) map[string]any {
	const maxMessages = 200
	msgs := make([]map[string]any, 0, min(len(messages), maxMessages))
	for i, msg := range messages {
		if i >= maxMessages {
			break
		}
		if msg == nil || msg.Role == agent.RoleSystem {
			continue
		}
		m := map[string]any{"role": string(msg.Role)}
		var blocks []string
		for _, b := range msg.Content {
			switch b.Type {
			case ai.ContentBlockText:
				blocks = append(blocks, fmt.Sprintf("text:%d", len(b.Text)))
			case ai.ContentBlockThinking:
				blocks = append(blocks, fmt.Sprintf("thinking:%d", len(b.Thinking)))
			case ai.ContentBlockToolCall:
				blocks = append(blocks, "toolCall:"+b.Name)
			case ai.ContentBlockImage:
				blocks = append(blocks, "image")
			}
		}
		if len(blocks) > 0 {
			m["blocks"] = blocks
		}
		if msg.ToolCallID != "" {
			m["toolCallId"] = msg.ToolCallID
			m["toolName"] = msg.ToolName
		}
		if msg.IsError {
			m["isError"] = true
		}
		if msg.ErrorMessage != "" {
			m["errorMessage"] = msg.ErrorMessage
		}
		msgs = append(msgs, m)
	}
	out := map[string]any{
		"messages":          msgs,
		"systemPromptChars": len(systemPrompt),
	}
	if len(messages) > maxMessages {
		out["messagesTruncated"] = true
	}
	return out
}

// appendMessageToSession writes a message to the session file and returns its
// entry ID, empty when nothing was written.
func (s *agentSession) appendMessageToSession(msg *agent.Message) string {
	id, err := s.persistMessageEntry(msg)
	if err != nil {
		s.logger.Error("session write failed", "error", err)
	}
	return id
}

func (s *agentSession) persistMessage(msg *agent.Message) error {
	_, err := s.persistMessageEntry(msg)
	return err
}

func (s *agentSession) persistMessageEntry(msg *agent.Message) (string, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	parentID := s.parentMsgID
	s.mu.Unlock()
	if writer == nil {
		return "", nil
	}

	// Always marshal the envelope to ensure valid {role, content} structure.
	// Even if content is nil, we need the role field present.
	content, err := marshalSessionMessageEnvelope(msg)
	if err != nil {
		return "", fmt.Errorf("marshal message: %w", err)
	}

	var tokens *sessionpkg.TokenCounts
	if msg.Usage.TotalTokens > 0 || msg.Usage.Input > 0 || msg.Usage.Output > 0 || msg.Usage.CacheRead > 0 || msg.Usage.CacheWrite > 0 {
		tokens = &sessionpkg.TokenCounts{
			Input:       msg.Usage.Input,
			Output:      msg.Usage.Output,
			CachedInput: msg.Usage.CacheRead + msg.Usage.CacheWrite,
		}
	}

	entry := sessionpkg.NewMessageEntry(parentID, content, tokens)
	if err := writer.Append(entry); err != nil {
		return "", fmt.Errorf("append message: %w", err)
	}
	s.mu.Lock()
	if s.sessionWriter != nil {
		s.parentMsgID = &entry.ID
	}
	s.mu.Unlock()
	return entry.ID, nil
}

func marshalSessionMessageEnvelope(msg *agent.Message) ([]byte, error) {
	envelope := map[string]any{
		"customType": msg.CustomType, "display": msg.Display, "details": msg.Details, "timestamp": msg.Timestamp,
		"thinkingLevel": msg.ThinkingLevel,
		"data":          msg.Data,
		"toolUsage":     msg.ToolUsage, "nestedCalls": msg.NestedCalls,
		"sections": msg.Sections, "toolsAdded": msg.ToolsAdded, "toolsRemoved": msg.ToolsRemoved,
		"excludeFromContext": msg.ExcludeFromContext,
		"role":               msg.Role,
		"content":            msg.Content,
		"usage":              msg.Usage,
	}
	if len(msg.StructuredContent) > 0 {
		envelope["structuredContent"] = msg.StructuredContent
	}

	if msg.ToolCallID != "" {
		envelope["toolCallId"] = msg.ToolCallID
		envelope["tool_call_id"] = msg.ToolCallID
	}
	if msg.ToolName != "" {
		envelope["toolName"] = msg.ToolName
		envelope["tool_name"] = msg.ToolName
	}
	if msg.IsError {
		envelope["isError"] = msg.IsError
		envelope["is_error"] = msg.IsError
	}
	if msg.Provider != "" {
		envelope["provider"] = msg.Provider
	}
	if msg.API != "" {
		envelope["api"] = msg.API
	}
	if msg.Model != "" {
		envelope["model"] = msg.Model
	}
	if msg.StopReason != "" {
		envelope["stopReason"] = msg.StopReason
		envelope["stop_reason"] = msg.StopReason
	}
	if msg.ErrorMessage != "" {
		envelope["errorMessage"] = msg.ErrorMessage
		envelope["error_message"] = msg.ErrorMessage
	}
	if msg.ResponseID != "" {
		envelope["responseId"] = msg.ResponseID
		envelope["response_id"] = msg.ResponseID
	}
	if msg.ProviderThinkingLevel != "" {
		envelope["providerThinkingLevel"] = msg.ProviderThinkingLevel
	}

	if len(msg.Diagnostics) > 0 {
		envelope["diagnostics"] = msg.Diagnostics
	}
	if msg.DurationMs != nil {
		envelope["durationMs"] = *msg.DurationMs
	}
	return json.Marshal(envelope)
}

// appendUserMessageToSession writes a user message to the session file.
// appendUserMessageToSession writes a prompt to the session file and returns its
// entry ID, empty when nothing was written.
func (s *agentSession) appendUserMessageToSession(msg Message) (string, error) {
	writer, err := s.ensureSessionWriter()
	s.mu.Lock()
	parentID := s.parentMsgID
	isFirstMessage := s.sessionFileBeforeRename != ""
	s.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("ensure session writer: %w", err)
	}
	if writer == nil {
		return "", nil
	}

	rawContent, err := marshalSessionMessageEnvelope(agentMessageFromSDK(msg))
	if err != nil {
		return "", fmt.Errorf("marshal user message content: %w", err)
	}

	entry := sessionpkg.NewMessageEntry(parentID, rawContent, nil)
	entry.ID = sessionpkg.NewID()
	if err := writer.Append(entry); err != nil {
		return "", fmt.Errorf("append user message: %w", err)
	}

	// Update parentMsgID under the lock so subsequent sends chain to this one.
	// Close() may have nulled the writer between our local snapshot and here;
	// if so, the next send will see nil and skip the write cleanly.
	s.mu.Lock()
	if s.sessionWriter != nil {
		s.parentMsgID = &entry.ID
	}
	s.mu.Unlock()

	// After first user message, derive slug from content if header name is UUID-based
	if isFirstMessage {
		if err := s.maybeDeriveSlugFromFirstMessage(msg.Text); err != nil {
			s.logger.Warn("slug derivation failed", "error", err)
		}
	}

	return entry.ID, nil
}

// maybeDeriveSlugFromFirstMessage updates the session filename by deriving
// a slug from the first user message when the session file doesn't have a slug yet.
// Per the spec: slug is derived from first 40 chars of the first user message,
// lowercase non-alphanumeric -> "-".
func (s *agentSession) maybeDeriveSlugFromFirstMessage(firstContent string) error {
	s.mu.Lock()
	writer := s.sessionWriter
	sessionFileBeforeRename := s.sessionFileBeforeRename
	s.mu.Unlock()
	if writer == nil {
		return nil
	}

	// Check if we've already done slug derivation for this session
	if sessionFileBeforeRename == "" {
		return nil // Already renamed or no rename needed
	}

	path := writer.Path()

	// Derive slug from first 40 chars of content
	slug := slugFromMessageContent(firstContent)
	if slug == "" {
		// No usable content for slug; mark as done and keep current filename
		s.mu.Lock()
		if s.sessionWriter != nil {
			s.sessionFileBeforeRename = ""
		}
		s.mu.Unlock()
		return nil
	}

	// Rename the session file using the derived slug
	newPath, err := sessionpkg.RenameSession(path, slug)
	if err != nil {
		return err
	}

	// Update the writer to use the new path
	newWriter, err := sessionpkg.NewWriter(newPath)
	if err != nil {
		return err
	}
	var closeErr error
	s.mu.Lock()
	if s.sessionWriter != nil {
		closeErr = s.sessionWriter.Close()
		s.sessionWriter = newWriter
		s.sessionFileBeforeRename = ""
	} else {
		closeErr = newWriter.Close()
	}
	s.mu.Unlock()
	if closeErr != nil {
		return fmt.Errorf("close session writer during slug rename: %w", closeErr)
	}

	return nil
}

// slugFromMessageContent derives a filesystem-safe slug from message content.
func slugFromMessageContent(content string) string {
	if content == "" {
		return ""
	}

	// Get first 40 chars
	if len(content) > 40 {
		content = content[:40]
	}

	// Convert to slug-safe characters
	var buf []byte
	for _, r := range content {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			buf = append(buf, byte(r))
		} else if r >= 'A' && r <= 'Z' {
			buf = append(buf, byte(r+'a'-'A'))
		} else if r == ' ' || r == '-' || r == '_' {
			buf = append(buf, '-')
		}
	}

	result := string(buf)
	// Trim leading/trailing dashes
	for len(result) > 0 && result[0] == '-' {
		result = result[1:]
	}
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}

	// Collapse multiple dashes
	var collapsed []byte
	prevDash := false
	for _, r := range result {
		if r == '-' {
			if !prevDash {
				collapsed = append(collapsed, byte(r))
				prevDash = true
			}
		} else {
			collapsed = append(collapsed, byte(r))
			prevDash = false
		}
	}

	return string(collapsed)
}

// logUsage logs a usage entry to the cumulative usage log.
func (s *agentSession) logUsage(usage ai.Usage, model string) {
	if s.usageLogger == nil {
		logger, err := sessionpkg.GetUsageLogger()
		if err != nil {
			s.logger.Warn("usage logger init failed", "error", err)
			return
		}
		s.usageLogger = logger
	}

	// Extract provider from the model spec if available
	provider := ""
	if s.model != nil {
		provider = s.model.Provider
	}

	entry := sessionpkg.UsageEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Session:      s.id,
		Provider:     provider,
		Model:        model,
		InputTokens:  usage.Input,
		OutputTokens: usage.Output,
		CostUSD:      usage.Cost.Total,
	}

	if err := s.usageLogger.Log(entry); err != nil {
		s.logger.Warn("usage log write failed", "error", err)
	}
}

// calculateCost totals recorded provider costs without repricing previous turns.
func (s *agentSession) calculateCost() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageTotals.Cost.Total
}

func agentMessageText(msg *agent.Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockText && block.Text != "" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func hasUsage(usage ai.Usage) bool {
	return usage.TotalTokens > 0 || usage.Input > 0 || usage.Output > 0 || usage.CacheRead > 0 || usage.CacheWrite > 0 ||
		usage.Cost.Total > 0 || usage.Cost.Input > 0 || usage.Cost.Output > 0 || usage.Cost.CacheRead > 0 || usage.Cost.CacheWrite > 0
}

func usagePayload(usage ai.Usage) map[string]any {
	payload := map[string]any{
		"input":       usage.Input,
		"output":      usage.Output,
		"cacheRead":   usage.CacheRead,
		"cacheWrite":  usage.CacheWrite,
		"totalTokens": usage.TotalTokens,
		// Legacy snake_case aliases are kept for JSON/event clients that still
		// read the older SDK shape.
		"input_tokens":       usage.Input,
		"output_tokens":      usage.Output,
		"cache_read_tokens":  usage.CacheRead,
		"cache_write_tokens": usage.CacheWrite,
		"total_tokens":       usage.TotalTokens,
	}
	if usage.Cost.Input > 0 || usage.Cost.Output > 0 || usage.Cost.CacheRead > 0 || usage.Cost.CacheWrite > 0 || usage.Cost.Total > 0 {
		payload["cost"] = map[string]any{
			"input":      usage.Cost.Input,
			"output":     usage.Cost.Output,
			"cacheRead":  usage.Cost.CacheRead,
			"cacheWrite": usage.Cost.CacheWrite,
			"total":      usage.Cost.Total,
		}
		payload["cost_usd"] = usage.Cost.Total
	}
	return payload
}

func (s *agentSession) handleAgentEvent(ch chan<- Event, event agent.Event) {
	switch e := event.(type) {
	case agent.AgentStart:
		s.emit(ch, Event{
			Type:      "agent_start",
			SessionID: s.id,
			Payload:   map[string]any{},
		})
	case agent.AgentEnd:
		s.emit(ch, Event{
			Type:      "agent_end",
			SessionID: s.id,
			Payload:   map[string]any{},
		})
	case agent.TurnStart:
		s.emit(ch, Event{
			Type:      "turn_start",
			SessionID: s.id,
			Payload:   map[string]any{},
		})
	case agent.TurnEnd:
		// Informational; payload is empty since the followup message/tool summary
		// is already carried by the message_end / tool_result events.
		s.emit(ch, Event{
			Type:      "turn_end",
			SessionID: s.id,
			Payload:   map[string]any{},
		})
	case agent.UsageLimitWait:
		s.emit(ch, Event{
			Type:      "usage_limit_wait",
			SessionID: s.id,
			Payload: map[string]any{
				"provider":  e.Provider,
				"resets_at": e.ResetsAt.Format(time.RFC3339),
			},
		})
	case agent.Paused:
		s.emit(ch, Event{
			Type:      "paused",
			SessionID: s.id,
			Payload:   map[string]any{"target": e.Target},
		})
	case agent.MessageStart:
		if e.Message != nil {
			var parentID string
			if len(s.agent.State().Messages) > 0 {
				parentID = fmt.Sprintf("msg-%d", len(s.agent.State().Messages)-1)
			}
			s.emit(ch, Event{
				Type:      "message_start",
				SessionID: s.id,
				Payload: map[string]any{
					"message_id": fmt.Sprintf("msg-%d", len(s.agent.State().Messages)),
					"role":       string(e.Message.Role),
					"parent_id":  parentID,
				},
			})
		}
	case agent.MessageUpdate:
		// The agent emits MessageUpdate for every streaming delta.
		// Inspect the underlying AI event to determine the delta type.
		switch aiEvt := e.AssistantMessageEvent.(type) {
		case ai.TextDelta:
			s.emit(ch, Event{
				Type:      "text_delta",
				SessionID: s.id,
				Payload: map[string]any{
					"message_id": s.getCurrentMessageID(),
					"delta":      aiEvt.Delta,
				},
			})
		case ai.ThinkingDelta:
			s.emit(ch, Event{
				Type:      "thinking_delta",
				SessionID: s.id,
				Payload: map[string]any{
					"message_id": s.getCurrentMessageID(),
					"delta":      aiEvt.Delta,
				},
			})
		}
	case agent.MessageEnd:
		s.mu.Lock()
		if e.Message != nil && (e.Message.Role == agent.RoleUser || (e.Message.Role == agent.RoleAssistant && e.Message.StopReason != ai.StopReasonError && e.Message.StopReason != ai.StopReasonLength)) {
			s.overflowRecoveryAttempted = false
		}
		s.pendingNextTurn = slices.DeleteFunc(s.pendingNextTurn, func(pending *agent.Message) bool { return pending == e.Message })
		s.mu.Unlock()
		if e.Message != nil {
			msgID := s.getCurrentMessageID()
			var tokens int
			var usage map[string]any
			if hasUsage(e.Message.Usage) {
				usageValue := sessionpkg.NormalizeUsage(e.Message.Usage)
				tokens = usageValue.TotalTokens
				usage = usagePayload(usageValue)

				// Update total tokens for session
				s.mu.Lock()
				s.totalTokens += usageValue.TotalTokens
				addUsage(&s.usageTotals, usageValue)
				s.mu.Unlock()

				// Log usage to the usage log
				s.logUsage(usageValue, e.Message.Model)
			}
			// Only the triggering prompt is persisted before admission. Consumed
			// steering and follow-up messages must be written at their boundary.
			// Messages are written before message_end so the event can name
			// their entry: frontends page history back in from that point.
			s.mu.Lock()
			persisted := e.Message == s.prePersistedMessage
			entryID := ""
			if persisted {
				entryID = s.prePersistedEntryID
			}
			s.mu.Unlock()
			if !persisted {
				entryID = s.appendMessageToSession(e.Message)
			}
			s.emit(ch, Event{
				Type:      "message_end",
				SessionID: s.id,
				Payload: map[string]any{
					"message_id":    msgID,
					"entry_id":      entryID,
					"role":          string(e.Message.Role),
					"content":       agentMessageText(e.Message),
					"tokens":        tokens,
					"finish_reason": string(e.Message.StopReason),
					"error_message": e.Message.ErrorMessage,
					"durationMs":    e.Message.DurationMs,
				},
			})
			if usage != nil {
				s.mu.Lock()
				sessionTotal := s.totalTokens
				s.mu.Unlock()
				s.emit(ch, Event{
					Type:      "usage",
					SessionID: s.id,
					Payload: map[string]any{
						"this_call":     usage,
						"session_total": sessionTotal,
					},
				})
			}

		}
	case agent.ToolStart:
		inputJSON, _ := json.Marshal(e.Args)
		s.emit(ch, Event{
			Type:      "tool_call",
			SessionID: s.id,
			Payload: map[string]any{
				"tool_call_id": e.ToolCallID,
				"message_id":   s.getCurrentMessageID(),
				"name":         e.Name,
				"input":        string(inputJSON),
			},
		})
	case agent.ToolUpdate:
		// Emit tool_progress for partial results
		chunk := ""
		var nestedCalls []agent.NestedToolCall
		if e.PartialResult != nil {
			chunk = e.PartialResult.Content
			nestedCalls = slices.Clone(e.PartialResult.NestedCalls)
		}
		s.emit(ch, Event{
			Type:      "tool_progress",
			SessionID: s.id,
			Payload: map[string]any{
				"tool_call_id": e.ToolCallID,
				"chunk":        chunk,
				"nestedCalls":  nestedCalls,
			},
		})
	case agent.ToolEnd:
		output := ""
		isErr := false
		details := any(nil)
		var nestedCalls []agent.NestedToolCall
		var toolUsage *ai.Usage
		if e.Result != nil {
			output = e.Result.Content
			isErr = e.IsError
			details = e.Result.Details
			nestedCalls = slices.Clone(e.Result.NestedCalls)
			if e.Result.Usage != nil {
				toolUsage = new(*e.Result.Usage)
			}
		}
		var content []ai.ContentBlock
		var structuredContent json.RawMessage
		if e.Result != nil {
			content = slices.Clone(e.Result.NormalizedContent())
			structuredContent = slices.Clone(e.Result.StructuredContent)
		}

		s.emit(ch, Event{
			Type:      "tool_result",
			SessionID: s.id,
			Payload: map[string]any{
				"tool_call_id":       e.ToolCallID,
				"content":            content,
				"structuredContent":  structuredContent,
				"output":             output,
				"is_error":           isErr,
				"details":            details,
				"durationMs":         e.DurationMs,
				"excludeFromContext": e.Result != nil && e.Result.ExcludeFromContext,
				"nestedCalls":        nestedCalls,
				"toolUsage":          toolUsage,
			},
		})
	}
}

// getCurrentMessageID returns an identifier for the current message being processed.
func (s *agentSession) getCurrentMessageID() string {
	state := s.agent.State()
	return fmt.Sprintf("msg-%d", len(state.Messages))
}

// executeSlashCommand routes a slash command through the command registry.
// It returns a channel of events containing the command result.
func (s *agentSession) executeSlashCommand(ctx context.Context, line string) (<-chan Event, error) {
	events := make(chan Event, 4)

	// Emit session_start for the command invocation
	s.emit(events, Event{
		Type:      "session_start",
		SessionID: s.id,
		Payload: map[string]any{
			"id":                  s.id,
			"cwd":                 s.cwd,
			"model":               modelIDForEvent(s.model),
			"reason":              s.sessionStartReason,
			"previousSessionFile": s.previousSessionFile,
		},
	})

	// Parse and execute the slash command.
	uiAdapter := &commandUIAdapter{session: s}
	result, err := s.cmdRegistry.ParseAndExecute(ctx, line, uiAdapter, s)

	name, _ := parseSlashCommand(line)
	if name == "" {
		name = line
	}

	// Emit command execution result as a dedicated command_result event. Slash
	// commands are not model tool invocations, so rendering them as a
	// tool_result produced confusing "Completed tool" rows in frontends.
	content, isError := commandResultToToolResult(result, err)

	s.emit(events, Event{
		Type:      "command_result",
		SessionID: s.id,
		Payload: map[string]any{
			"command":  name,
			"output":   content,
			"is_error": isError,
		},
	})

	// Emit session_end
	s.emit(events, Event{
		Type:      "session_end",
		SessionID: s.id,
		Payload: map[string]any{
			"id":           s.id,
			"total_tokens": s.totalTokens,
			"cost_usd":     0.0,
		},
	})

	close(events)
	return events, nil
}

// getLastAssistantText returns the text content of the most recent assistant message.
// It iterates through messages in reverse to find the last assistant message with content.
func (s *agentSession) getLastAssistantText() string {
	state := s.agent.State()
	// Iterate in reverse to find the last assistant message
	for i := len(state.Messages) - 1; i >= 0; i-- {
		msg := state.Messages[i]
		if msg.Role != agent.RoleAssistant {
			continue
		}
		// Skip aborted messages with no content
		if msg.StopReason == ai.StopReasonAborted && len(msg.Content) == 0 {
			continue
		}
		// Extract text content
		var text string
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockText {
				text += block.Text
			}
		}
		if text = strings.TrimSpace(text); text != "" {
			return text
		}
	}
	return ""
}

// pausedBy returns the flag file in dir that names the session by id or by
// display name, or the requested model as provider=<provider> or model=<id>,
// or "" when none does. The name is only resolved when dir holds flags, since
// it reads the session file. A missing dir means no flags.
func pausedBy(dir, id string, name func() string, model *ai.Model) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read pause dir: %w", err)
	}
	if len(entries) == 0 {
		return "", nil
	}
	flags := make(map[string]bool, len(entries))
	for _, entry := range entries {
		flags[entry.Name()] = true
	}
	if id != "" && flags[id] {
		return id, nil
	}
	if n := name(); n != "" && flags[n] {
		return n, nil
	}
	if model != nil {
		for _, flag := range []string{"provider=" + model.Provider, "model=" + model.ID} {
			if flags[flag] {
				return flag, nil
			}
		}
	}
	return "", nil
}

// getSessionName returns the session display name from the session file.
func (s *agentSession) getSessionName() string {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return ""
	}
	// Open the session file to read the display name
	path := writer.Path()
	r, err := sessionpkg.ReadFile(path)
	if err != nil {
		return ""
	}
	return r.DisplayName()
}

// commandUIAdapter adapts sdk.UI to commands.CommandUI.
type commandUIAdapter struct {
	session *agentSession
}

func (a *commandUIAdapter) Confirm(ctx context.Context, title, message string) (bool, error) {
	if a.session.ui == nil {
		return false, fmt.Errorf("no UI configured for confirmation")
	}
	return a.session.ui.Confirm(ctx, title, message)
}

func (a *commandUIAdapter) Select(ctx context.Context, title string, options []string) (int, error) {
	if a.session.ui == nil {
		return 0, fmt.Errorf("no UI configured for selection")
	}
	return a.session.ui.Select(ctx, title, options)
}

func (a *commandUIAdapter) Input(ctx context.Context, title, placeholder string) (string, error) {
	if a.session.ui == nil {
		return "", fmt.Errorf("no UI configured for input")
	}
	return a.session.ui.Input(ctx, title, placeholder)
}

// commandResultToToolResult converts a command execution result to a tool_result event.
func commandResultToToolResult(result commands.Result, err error) (content string, isError bool) {
	if err != nil {
		content = fmt.Sprintf("command error: %v", err)
		isError = true
	} else {
		content = result.Output
		isError = result.IsError
	}
	return content, isError
}

// runtime implements Runtime.
type runtime struct {
	// mcpTransport replaces MCP transports in tests.
	mcpTransport mcp.TransportFactory
	// pluginVirtualModels are the virtual models plugins last registered.
	pluginVirtualMu     sync.Mutex
	pluginVirtualModels []ai.VirtualModelDefinition
	ui                  ui.UI
	sessionsDir         string
	credStore           store.CredentialStore
	// settingsMu guards settings, resolver, and compactionConfig, which an
	// idle session's settings reload replaces for later sessions.
	settingsMu        sync.RWMutex
	settings          *ai.Settings
	resolver          *ai.ModelResolver
	compactionOptions *CompactionSettings
	modelRegistry     *ai.ModelRegistry
	explicitModel     string
	pluginManager     *plugins.Manager
	commandRegistry   commands.Registry
	// artifactLoader provides skills and context for session injection.
	artifactLoader ArtifactLoader
	// telemetry controls whether telemetry is reported.
	telemetry *bool
	// activeModel is the resolved model from settings/defaults/env.
	// It is nil if no model is configured (session creation will fail).
	activeModel *ai.Model
	// activeModelSpec is the resolved model spec for display/debugging.
	activeModelSpec ai.ModelSpec
	// activeThinkingLevel is the ":level" suffix of the enabledModels
	// pattern that chose activeModel, if any.
	activeThinkingLevel string
	// currentSessionID is the ID of the currently active session.
	// Used by the plugin system to route Host.SessionAppend/SessionScan RPCs to the correct session.
	currentSessionID  string
	currentSessionCWD string
	// currentSession is the active session; used to route Host.SessionSendUserMessage.
	currentSession Session
	// openSessions holds every live session keyed by ID. A session must have
	// a single live instance per process: a second open would interleave two
	// writers into one JSONL file and duplicate message IDs.
	openSessions map[string]struct{}
	// liveSessions holds every session NewSession returned that has not
	// closed, so Close can shut them down before the plugins they use.
	liveSessions map[*agentSession]struct{}
	closed       bool
	mu           sync.Mutex
	// noTools disables all tools including built-in tools.
	noTools bool
	// noBuiltinTools disables only built-in tools (grep, find, ls, read, write, edit, bash).
	noBuiltinTools bool
	// noContextFiles disables AGENTS.md/CLAUDE.md discovery.
	noContextFiles bool
	// noEnvCredentials disables environment variable credential sources.
	noEnvCredentials bool
	// provider pins the provider key for credential resolution (CLI --provider).
	provider string
	// apiKey is an explicit API key override (CLI --api-key). Wins over
	// the credential store and the env-var fallback.
	apiKey string
	// thinkingLevel sets the thinking level (off, minimal, low, medium, high, xhigh).
	thinkingLevel string
	// preloadSkills are skill IDs to inject into sessions at startup.
	preloadSkills []string
	// compactionConfig holds auto-compaction settings.
	compactionConfig compaction.Config
	// logger receives runtime diagnostics. Defaults to discard.
	logger *slog.Logger
}

func trackRuntimeSession(opts SessionOptions) bool {
	return !opts.NoSession && opts.EphemeralExportPath == ""
}

func (r *runtime) claimOpenSession(id string) error {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.openSessions == nil {
		r.openSessions = make(map[string]struct{})
	}
	if _, ok := r.openSessions[id]; ok {
		return fmt.Errorf("session %q is already open in this process", id)
	}
	r.openSessions[id] = struct{}{}
	return nil
}

func (r *runtime) releaseOpenSession(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.openSessions, id)
	r.mu.Unlock()
}

func (r *runtime) trackLiveSession(s *agentSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if r.liveSessions == nil {
		r.liveSessions = make(map[*agentSession]struct{})
	}
	r.liveSessions[s] = struct{}{}
	return nil
}

func (r *runtime) forgetLiveSession(s *agentSession) {
	r.mu.Lock()
	delete(r.liveSessions, s)
	r.mu.Unlock()
}

// ArtifactLoader is the optional artifact loader that the SDK exposes.
// It is nil unless initialized by cmd/tau during its lifecycle setup.
// When non-nil, Runtime.NewSession can use it to inject skills/prompts/themes.
// The interface uses concrete types from agent/artifacts package.
type ArtifactLoader interface {
	SkillRegistry() *artifacts.SkillRegistry
	PromptRegistry() *artifacts.PromptRegistry
	ThemeRegistry() *artifacts.ThemeRegistry
	// Reload re-discovers and reloads all artifact registries.
	Reload(ctx context.Context) error
}

// GlobalArtifactLoader is the optional artifact loader that the SDK exposes.
// It is nil unless initialized by cmd/tau during its lifecycle setup.
var GlobalArtifactLoader ArtifactLoader

// NewRuntime creates a new Runtime with the given options.
// If options are not provided (nil struct), sensible defaults are used:
// - UI: NoOpUI
// - SessionsDir: ~/.tau/agent/sessions
// - CredentialStore: file backend at ~/.tau/agent/auth.json
// - Settings: loaded from ~/.tau/agent/settings.json + project .tau/settings.json
func NewRuntime(opts RuntimeOptions) (Runtime, error) {
	registry := opts.ModelRegistry
	if registry == nil {
		var err error
		registry, err = ai.LoadModelRegistry(filepath.Join(paths.ConfigDir(), "models.json"))
		if err != nil {
			return nil, fmt.Errorf("load model registry: %w", err)
		}
	}
	u := opts.UI
	if u == nil {
		u = ui.NewNoOpUI()
	}
	sessionsDir := opts.SessionsDir

	// Load settings with sensible defaults if not provided.
	// Global settings and directory trust precede protected project settings.
	settings := opts.Settings
	if settings == nil {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve runtime working directory: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve runtime home: %w", err)
		}
		trustOptions := opts.ProjectTrust
		if trustOptions.Prompt == nil {
			trustOptions.Prompt = func(ctx context.Context, cwd string) (*bool, error) {
				return ai.PromptProjectTrust(ctx, ai.ProjectTrustStore{Path: filepath.Join(paths.ConfigDir(), "trust.json")}, cwd, u.Select)
			}
		}
		settings, err = ai.LoadTrustedSettings(context.Background(), paths.ConfigDir(), cwd, home, trustOptions)
		if err != nil {
			return nil, err
		}
	}

	if err := settings.ApplyHTTPProxySettings(); err != nil {
		return nil, err
	}

	// Sessions default: TAU_SESSION_DIR, then sessionDir from settings, then
	// <configDir>/sessions. Matches the CLI resolution chain.
	if sessionsDir == "" {
		var configured string
		if settings != nil {
			configured = settings.SessionDir
		}
		sessionsDir = paths.Dir("TAU_SESSION_DIR", configured, filepath.Join(paths.ConfigDir(), "sessions"))
	}

	// Initialize credential store with file backend if not provided
	credStore := opts.CredentialStore
	if credStore == nil {
		authPath := filepath.Join(paths.ConfigDir(), "auth.json")
		fileBackend := store.NewFileBackend(authPath, store.Options{RefreshFuncs: store.OAuthProviderRefreshFuncs()})
		if err := fileBackend.EnsureDir(); err != nil {
			return nil, fmt.Errorf("ensure credential directory: %w", err)
		}
		credStore = fileBackend
	}

	r := &runtime{
		ui:               u,
		sessionsDir:      sessionsDir,
		credStore:        credStore,
		settings:         settings,
		modelRegistry:    registry,
		explicitModel:    opts.ExplicitModel,
		pluginManager:    opts.PluginManager,
		commandRegistry:  opts.CommandRegistry,
		artifactLoader:   opts.ArtifactLoader,
		telemetry:        opts.Telemetry,
		noTools:          opts.NoTools,
		noBuiltinTools:   opts.NoBuiltinTools,
		noContextFiles:   opts.NoContextFiles,
		noEnvCredentials: opts.IgnoreEnv,
		provider:         opts.Provider,
		apiKey:           opts.APIKey,
		thinkingLevel:    opts.ThinkingLevel,
		preloadSkills:    slices.Clone(opts.PreloadSkills),
		logger:           logging.OrDiscard(opts.Logger),
		openSessions:     make(map[string]struct{}),
	}

	r.compactionOptions = opts.Compaction
	r.replaceSettings(settings)

	// Register default providers (but defer model resolution to NewSession)
	r.setupDefaultsForRuntime()

	return r, nil
}

// replaceSettings installs settings for sessions created afterwards.
func (r *runtime) replaceSettings(settings *ai.Settings) {
	compactionConfig := r.buildCompactionConfig(settings, r.compactionOptions)
	var resolver *ai.ModelResolver
	if settings != nil {
		resolver = ai.NewModelResolver(settings)
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	r.settings = settings
	r.resolver = resolver
	r.compactionConfig = compactionConfig
}

// settingsState returns the settings, model resolver, and compaction
// configuration new sessions start from.
func (r *runtime) settingsState() (*ai.Settings, *ai.ModelResolver, compaction.Config) {
	r.settingsMu.RLock()
	defer r.settingsMu.RUnlock()
	return r.settings, r.resolver, r.compactionConfig
}

// setupDefaults resolves and stores the active model from settings/defaults/env.
// It also registers default providers. This is called during NewSession to
// ensure the real resolved provider/model is used.
//
// When no model is configured, setupDefaults leaves r.activeModel nil rather
// than failing. Session creation proceeds; the user can pick a model via the
// TUI's `/model` slash command before sending. The first Send will then fail
// with the agent loop's "model is required" error if the user hasn't picked
// a model yet.
func (r *runtime) setupDefaults(ctx context.Context) error {
	ai.Init() // Register default providers

	resolved, err := r.resolveModel(ctx)
	if err != nil {
		if errors.Is(err, ErrNoModelConfigured) {
			r.activeModel = nil
			r.activeModelSpec = ai.ModelSpec{}
			r.activeThinkingLevel = ""
			return nil
		}
		return err
	}
	r.activeModel = resolved.Model
	r.activeModelSpec = resolved.Spec
	r.activeThinkingLevel = resolved.ThinkingLevel
	return nil
}

// setupDefaultsForRuntime is called during NewRuntime to register providers
// without resolving the active model. Model resolution is deferred to NewSession
// so that callers can set up the runtime first.
func (r *runtime) setupDefaultsForRuntime() {
	ai.Init() // Register default providers
}

// buildCompactionConfig builds the auto-compaction trigger config from
// runtime options and the user-configured defaults. The settings.json flat
// schema (compaction.enabled / reserveTokens / keepRecentTokens) feeds the
// lower-level agent/compaction package via DefaultSettings; runtime options
// override on a per-field basis.
//
// The runtime SDK CompactionSettings still uses TriggerPct/PreserveRecent
// (the percentage-based auto-compaction policy) for explicit runtime
// overrides; user-level settings.json only carries the Pi-style fields.
func (r *runtime) buildCompactionConfig(settings *ai.Settings, opts *CompactionSettings) compaction.Config {
	cfg := compaction.DefaultConfig()
	limits, err := settings.ResolveCompaction(nil)
	if err != nil {
		r.logger.Warn("invalid compaction settings", "error", err)
	} else if !limits.Enabled {
		cfg.TriggerPct = 0
	}

	// Runtime options override defaults.
	if opts != nil {
		if opts.Disabled {
			cfg.TriggerPct = 0
		}
		if opts.TriggerPct > 0 {
			cfg.TriggerPct = opts.TriggerPct
		}
		if opts.PreserveRecent > 0 {
			cfg.PreserveRecent = opts.PreserveRecent
		}
	}

	return cfg
}

// ResolvedModel holds the resolved model and its spec.
type ResolvedModel struct {
	Model *ai.Model
	Spec  ai.ModelSpec
	// ThinkingLevel is the ":level" suffix of the enabledModels pattern that
	// selected the model, or empty.
	ThinkingLevel string
}

// ErrNoModelConfigured is returned by ResolveModel when no model can be
// resolved through the explicit / settings / env chain. Callers that build
// a session should treat this as recoverable: the user can pick a model
// interactively (e.g. the TUI's `/model` slash command) before sending.
var ErrNoModelConfigured = errors.New("no model configured: use /model to pick a provider and model")

// ResolveModel resolves the active model through the chain:
// 1. Explicit override (CLI or SDK option)
// 2. Settings default (user + project merged)
// 3. Env-var fallback (Anthropic credentials or OPENAI_API_KEY)
// 4. ErrNoModelConfigured if no model can be resolved
func (r *runtime) ResolveModel() (*ResolvedModel, error) {
	return r.resolveModel(context.Background())
}

func (r *runtime) resolveModel(ctx context.Context) (*ResolvedModel, error) {
	var modelID, providerKey, thinkingLevel string

	// 1. Explicit override
	if r.explicitModel != "" {
		spec, err := ai.ParseModelSpec(r.explicitModel)
		if err != nil {
			return nil, fmt.Errorf("parse explicit model %q: %w", r.explicitModel, err)
		}
		modelID = spec.Model
		providerKey = spec.Provider
	} else if settings, resolver, _ := r.settingsState(); resolver != nil {
		// 2. Settings default (resolve through ModelResolver), limited to
		// enabledModels when that scope is set.
		spec := resolver.ResolveDefaultModel()
		scoped, err := r.scopedStartupModel(ctx, settings, spec)
		if err != nil {
			return nil, err
		}
		if scoped != nil {
			spec, thinkingLevel = scoped.Model, scoped.ThinkingLevel
		}
		if spec.Provider != "" && spec.Model != "" {
			if providerKey == "" {
				providerKey = spec.Provider
			}
			modelID = spec.Model
		}
	}

	// 3. Env-var fallback (skip if IgnoreEnv is set)
	if modelID == "" && !r.noEnvCredentials {
		// Check Anthropic authentication.
		if ai.HasEnvCredentials(ai.ProviderAnthropic) {
			providerKey = "anthropic"
			modelID = "claude-sonnet-4-6"
		}
		// Check OPENAI_API_KEY
		if modelID == "" && os.Getenv("OPENAI_API_KEY") != "" {
			providerKey = ai.ProviderOpenAI
			modelID = "gpt-4o"
		}
	}

	// 4. No model configured
	if modelID == "" {
		return nil, fmt.Errorf("%w", ErrNoModelConfigured)
	}

	// Resolve the model from registry
	model, err := r.resolveChatModel(providerKey + "/" + modelID)
	if err != nil {
		return nil, fmt.Errorf("resolve model %s/%s: %w", providerKey, modelID, err)
	}

	if err := checkModelProvider(model, providerKey); err != nil {
		return nil, err
	}

	return &ResolvedModel{
		Model:         model,
		Spec:          ai.ModelSpec{Provider: providerKey, Model: modelID},
		ThinkingLevel: thinkingLevel,
	}, nil
}

// scopedStartupModel picks the startup model from enabledModels as upstream
// does: the saved default when it is in scope, otherwise the first scoped
// model with credentials. It returns nil when no scope applies.
func (r *runtime) scopedStartupModel(ctx context.Context, settings *ai.Settings, saved ai.ModelSpec) (*ai.ScopedModel, error) {
	if settings == nil || len(settings.EnabledModels) == 0 {
		return nil, nil
	}
	models, err := r.modelRegistry.Models()
	if err != nil {
		return nil, fmt.Errorf("list models for enabledModels: %w", err)
	}
	authenticated := map[string]bool{}
	var keys []string
	for _, model := range models {
		if model.Type != "" && model.Type != "chat" {
			continue
		}
		ok, seen := authenticated[model.Provider]
		if !seen {
			ok = r.restoredModelCredentialAvailable(ctx, model.Provider, r.credStore)
			authenticated[model.Provider] = ok
		}
		if ok {
			keys = append(keys, model.Provider+"/"+model.ID)
		}
	}
	scope := ai.ExpandModelScope(settings.EnabledModels, keys)
	if len(scope) == 0 {
		return nil, nil
	}
	for _, scoped := range scope {
		if scoped.Model == saved {
			return &scoped, nil
		}
	}
	return &scope[0], nil
}

// checkModelProvider verifies a physical model has a provider for its API.
// Virtual models are routed to a physical model per request instead.
func checkModelProvider(model *ai.Model, providerKey string) error {
	if ai.IsVirtualModel(model) {
		return nil
	}
	p, ok := ai.Get(model.API)
	if !ok {
		return fmt.Errorf("provider %q not registered for API %q", providerKey, model.API)
	}
	if p.API() != model.API {
		return fmt.Errorf("provider %q API mismatch: expected %q, got %q", providerKey, model.API, p.API())
	}
	return nil
}

func (r *runtime) resolveChatModel(spec string) (*ai.Model, error) {
	model, err := r.modelRegistry.ResolveModel(spec)
	if err != nil {
		return nil, fmt.Errorf("resolve chat model: %w", err)
	}
	if model.Type != "" && model.Type != "chat" {
		return nil, fmt.Errorf("model %s is %s, not chat", spec, model.Type)
	}
	return model, nil
}

// resolveModelWithOverride resolves a specific model string, ignoring runtime defaults.
// This is used for per-session model overrides via SessionOptions.Model.
func (r *runtime) resolveModelWithOverride(modelSpec string) (*ResolvedModel, error) {
	spec, err := ai.ParseModelSpec(modelSpec)
	if err != nil {
		return nil, fmt.Errorf("parse model spec %q: %w", modelSpec, err)
	}

	modelID := spec.Model
	providerKey := spec.Provider

	// If model wasn't specified (empty), try resolver or env fallback
	if modelID == "" {
		if _, resolver, _ := r.settingsState(); resolver != nil {
			spec := resolver.ResolveDefaultModel()
			if spec.Provider != "" && spec.Model != "" {
				if providerKey == "" {
					providerKey = spec.Provider
				}
				modelID = spec.Model
			}
		}
		// Env fallback (skip if IgnoreEnv is set)
		if modelID == "" && !r.noEnvCredentials {
			if ai.HasEnvCredentials(ai.ProviderAnthropic) {
				providerKey = ai.ProviderAnthropic
				modelID = "claude-sonnet-4-6"
			} else if os.Getenv("OPENAI_API_KEY") != "" {
				providerKey = ai.ProviderOpenAI
				modelID = "gpt-4o"
			}
		}
	}

	if modelID == "" {
		return nil, fmt.Errorf("no model configured and no default available")
	}

	// Resolve the model from registry
	model, err := r.resolveChatModel(providerKey + "/" + modelID)
	if err != nil {
		return nil, fmt.Errorf("resolve model %s/%s: %w", providerKey, modelID, err)
	}

	if err := checkModelProvider(model, providerKey); err != nil {
		return nil, err
	}

	return &ResolvedModel{
		Model: model,
		Spec:  ai.ModelSpec{Provider: providerKey, Model: modelID},
	}, nil
}

// restoreSessionModelAndThinking returns the recorded selection, the model
// of the latest response after it, and the thinking level. A virtual
// selection that is no longer registered falls back to the answering model.
func (r *runtime) restoreSessionModelAndThinking(opts SessionOptions) (modelSpec, answeredSpec, thinkingLevel string) {
	if opts.SessionID == "" || opts.Model != "" || r.sessionsDir == "" {
		return "", "", ""
	}
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return "", "", ""
	}
	reader, err := storage.Open(opts.SessionID)
	if err != nil {
		return "", "", ""
	}
	for _, entry := range reader.Entries() {
		switch entry.Type {
		case sessionpkg.EntryTypeMessage:
			if message := entry.ToAgentMessage(); message != nil && message.Role == "assistant" && message.Provider != "" && message.Model != "" && message.API != ai.VirtualModelAPI {
				answeredSpec = ai.ModelSpec{Provider: message.Provider, Model: message.Model}.String()
			}
		case sessionpkg.EntryTypeModelChange:
			provider, model := stringPtrValue(entry.Provider), stringPtrValue(entry.Model)
			if provider != "" && model != "" {
				modelSpec = ai.ModelSpec{Provider: provider, Model: model}.String()
				answeredSpec = ""
			}
		case sessionpkg.EntryTypeThinkingChange:
			if level := stringPtrValue(entry.Level); level != "" {
				thinkingLevel = level
			}
		}
	}
	return modelSpec, answeredSpec, thinkingLevel
}

func (r *runtime) restoredModelCredentialAvailable(ctx context.Context, providerKey string, credStore store.CredentialStore) bool {
	if providerKey == "" || r.apiKey != "" {
		return true
	}
	if credStore != nil {
		cred, err := credStore.Get(ctx, providerKey)
		if err == nil && !cred.NeedsReauth {
			if cred.Type == ai.CredentialTypeAPIKey && (cred.APIKey != "" || (!r.noEnvCredentials && ai.ProviderEnvHasCredentials(providerKey, cred.Env))) {
				return true
			}
			apiKey, keyErr := store.APIKeyFromCredential(ctx, providerKey, cred)
			if keyErr == nil && apiKey != "" {
				return true
			}
		}
	}
	if provider, ok := r.modelRegistry.Provider(providerKey); ok && provider.APIKey != "" {
		return true
	}
	return !r.noEnvCredentials && ai.HasEnvCredentials(providerKey)
}

func stringPtrValue(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}

// NewSession creates a new session using the resolved provider/model from the
// runtime's settings. It uses the credential store for API keys and creates a
// real agent that communicates with the configured provider.
// If opts.SessionID is non-empty and the session file exists, the session
// history is loaded and appended to the agent's transcript.
// Session options (Store, Settings, UI) override runtime defaults when provided.
func (r *runtime) NewSession(ctx context.Context, opts SessionOptions) (ret Session, err error) {
	// codemodeSession is set once the session exists; codemode only calls into it while running.
	var codemodeSession *agentSession
	settings, _, compactionCfg := r.settingsState()
	if err := ValidateToolSelection(opts.ToolAllowlist); err != nil {
		return nil, fmt.Errorf("invalid tools option: %w", err)
	}
	r.syncPluginVirtualModels()
	restoredModelSpec, answeredModelSpec, restoredThinkingLevel := r.restoreSessionModelAndThinking(opts)

	// Honor SessionOptions.Model override if provided.
	// This allows per-session model selection without modifying runtime defaults.
	var (
		activeModel     *ai.Model
		activeModelSpec ai.ModelSpec
		fromDefaults    bool
	)

	if opts.Model != "" {
		// Resolve the explicitly specified model for this session
		resolved, err := r.resolveModelWithOverride(opts.Model)
		if err != nil {
			return nil, fmt.Errorf("resolve model %q: %w", opts.Model, err)
		}
		activeModel = resolved.Model
		activeModelSpec = resolved.Spec
	} else if restoredModelSpec != "" {
		resolved, err := r.resolveModelWithOverride(restoredModelSpec)
		if err != nil && answeredModelSpec != "" {
			resolved, err = r.resolveModelWithOverride(answeredModelSpec)
		}
		if err == nil {
			activeModel = resolved.Model
			activeModelSpec = resolved.Spec
		} else {
			// If the recorded model is no longer available, fall back to the
			// runtime default rather than making the session unopenable.
			activeModel = nil
		}
	}
	// Use session-specific store or fall back to runtime store
	credStore := opts.Store
	if credStore == nil {
		credStore = r.credStore
	}

	if activeModelSpec.Provider != "" && restoredModelSpec != "" && opts.Model == "" && !ai.IsVirtualModel(activeModel) && !r.restoredModelCredentialAvailable(ctx, activeModelSpec.Provider, credStore) {
		activeModel = nil
	}
	if activeModel == nil {
		// Ensure model is resolved from runtime defaults
		if r.activeModel == nil {
			if err := r.setupDefaults(ctx); err != nil {
				// Surface real errors; ignore the "no model configured" sentinel
				// so the TUI can boot and prompt the user to /model.
				if !errors.Is(err, ErrNoModelConfigured) {
					return nil, fmt.Errorf("resolve active model: %w", err)
				}
			}
		}
		activeModel = r.activeModel
		activeModelSpec = r.activeModelSpec
		fromDefaults = true
	}

	// Use session-specific UI or fall back to runtime UI
	ui := opts.UI
	if ui == nil {
		ui = r.ui
	}

	// Use session-specific logger or fall back to runtime logger. Nil discards.
	sessionLogger := opts.Logger
	if sessionLogger == nil {
		sessionLogger = r.logger
	}
	sessionLogger = logging.OrDiscard(sessionLogger)

	// Determine CWD early so it can be used across session setup.
	cwd := opts.CWD
	if cwd == "" {
		cwd = "."
	}

	// Validate CWD if requested.
	if opts.ValidateCWD {
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("session CWD %q does not exist or is not a directory", cwd)
		}
	}

	// Create a client factory that uses the credential store for API keys
	// and resolves them per request (including refresh-on-read).
	sessionModel := activeModel
	sessionProviderKey := activeModelSpec.Provider
	// RuntimeOptions.Provider pins the credential provider when the model
	// specifier didn't already pin one. Honored only when the active model
	// is provider-agnostic.
	if sessionProviderKey == "" && r.provider != "" {
		sessionProviderKey = r.provider
	}

	requestCredentials := func(ctx context.Context, provider string) (key string, scopedEnv map[string]string, err error) {
		if err := context.Cause(ctx); err != nil {
			return "", nil, err
		}
		providerKey := provider
		if providerKey == "" {
			providerKey = sessionProviderKey
		}
		if providerKey == "" {
			return "", nil, nil
		}

		// Precedence: explicit key > stored API key or OAuth > models.json > environment.
		if r.apiKey != "" {
			return r.apiKey, nil, nil
		}

		if credStore != nil {
			cred, err := credStore.Get(ctx, providerKey)
			if cause := context.Cause(ctx); cause != nil {
				return "", nil, errors.Join(cause, err)
			}
			if err != nil && !errors.Is(err, ai.ErrCredentialNotFound) {
				return "", nil, fmt.Errorf("get credentials for %s: %w", providerKey, err)
			}
			if err == nil {
				cred.Env = maps.Clone(cred.Env)
				scopedEnv = cred.Env
				if cred.NeedsReauth {
					return "", nil, ai.ErrCredentialNeedsReauth{Provider: providerKey}
				}
				apiKey, err := store.APIKeyFromCredential(ctx, providerKey, cred)
				if err != nil {
					return "", nil, err
				}
				if apiKey != "" {
					return apiKey, scopedEnv, nil
				}
			}
		}

		if key, err := r.modelRegistry.ResolveAPIKeyWithEnv(ctx, providerKey, scopedEnv); err != nil {
			return "", nil, err
		} else if key != "" {
			return key, scopedEnv, nil
		}

		if !r.noEnvCredentials {
			if len(ai.ProviderEnvHeaders(providerKey, scopedEnv)) != 0 {
				return "", scopedEnv, nil
			}
			if envAPIKey := ai.ProviderEnvAPIKey(providerKey, scopedEnv); envAPIKey != "" {
				envOverrideLoggedOncePtr.Do(func() {
					sessionLogger.Info("NOTICE: using API key from environment", "provider", providerKey)
				})
				return envAPIKey, scopedEnv, nil
			}
		}

		return "", scopedEnv, nil
	}

	credentialProvider := func(ctx context.Context, provider string) (string, error) {
		key, _, err := requestCredentials(ctx, provider)
		return key, err
	}

	clientFactory := func(m *ai.Model) (ai.Client, error) {
		if m == nil {
			return nil, fmt.Errorf("%w", ErrNoModelConfigured)
		}
		if ai.IsVirtualModel(m) {
			return unroutedClient{model: m}, nil
		}
		client, err := ai.GetClient(m)
		if err != nil {
			return nil, err
		}

		providerKey := m.Provider
		if providerKey == "" {
			providerKey = sessionProviderKey
		}

		// Pass transport-level options for 401 retry and credential refresh.
		if refreshable, ok := client.(interface {
			SetTransportOption(opts ai.TransportOption)
		}); ok && credStore != nil {
			refreshable.SetTransportOption(ai.TransportOption{
				RefreshCreds: credentialProvider,
				Provider:     providerKey,
			})
		}

		return client, nil
	}

	steeringMode, followUpMode, err := settingsQueueModes(settings)
	if err != nil {
		return nil, err
	}
	transport, err := settings.ResolveTransport()
	if err != nil {
		return nil, err
	}
	agentCfg := agent.NewConfig(
		agent.WithSteeringMode(steeringMode),
		agent.WithFollowUpMode(followUpMode),
		agent.WithModel(sessionModel),
		agent.WithThinkingBudgets(settings.ResolveThinkingBudgets()),
		agent.WithRequestOptions(r.requestOptions(settings)...),
		agent.WithTransport(transport),
		agent.WithClientFactory(clientFactory),
		agent.WithAPIKeyProvider(credentialProvider),
		agent.WithRequestCredentials(requestCredentials),
	)

	// Honor the thinking level from SessionOptions or RuntimeOptions if set.
	// SessionOptions takes precedence over RuntimeOptions. Coding-agent sessions
	// default to medium reasoning; the lower-level agent package intentionally
	// keeps its independent default at off.
	sessionThinkingLevel := opts.ThinkingLevel
	if sessionThinkingLevel == "" && restoredThinkingLevel != "" {
		sessionThinkingLevel = restoredThinkingLevel
	}
	if sessionThinkingLevel == "" && r.thinkingLevel != "" {
		sessionThinkingLevel = r.thinkingLevel
	}
	if sessionThinkingLevel == "" && fromDefaults {
		sessionThinkingLevel = r.activeThinkingLevel
	}
	if sessionThinkingLevel == "" {
		// Per-model override from settings, then the global defaultThinkingLevel.
		// Only fresh sessions reach here: opts/explicit/CLI overrides won above,
		// and resumes keep the level recorded in the session file.
		sessionThinkingLevel = settings.ThinkingLevelFor(activeModelSpec.String())
	}
	if sessionThinkingLevel == "" {
		sessionThinkingLevel = string(agent.ThinkingMedium)
	}
	agentCfg.ThinkingLevel = agent.ThinkingLevel(sessionThinkingLevel)

	// Wire session hooks into the agent config if provided.
	if opts.TransformContext != nil {
		agentCfg.TransformContext = opts.TransformContext
	}
	if opts.OnPayload != nil {
		agentCfg.OnPayload = opts.OnPayload
	}
	if opts.OnResponse != nil {
		agentCfg.OnResponse = opts.OnResponse
	}

	// Wire built-in tools into the agent's tool registry if enabled.
	// These are the core file tools: read, write, edit, bash, grep, find, ls.
	// They are attached on the normal session path for parity with the spec.
	if !r.noTools && !r.noBuiltinTools {
		toolCfg := buildToolConfig(settings)
		selection := resolveDefaultToolSelection(settings)
		if opts.ToolAllowlist != nil {
			if toolSelectionHasModifiers(opts.ToolAllowlist) {
				selection = applyToolModifiers(selection, opts.ToolAllowlist)
			} else {
				selection = opts.ToolAllowlist
			}
		}
		var tools []agent.Tool
		if len(selection) > 0 {
			all := agent.BuiltInToolsWithConfig(toolCfg)
			for _, name := range selection {
				for _, tool := range filterToolsByName(all, []string{name}, nil) {
					if !slices.ContainsFunc(tools, func(t agent.Tool) bool { return t.Name == tool.Name }) {
						tools = append(tools, tool)
					}
				}
			}
			if !slices.Contains(opts.ToolDenylist, toolSearchName) {
				// The closure runs only after the session exists.
				tool := toolSearchTool(func(tc agent.ToolContext) (*agent.ToolResult, error) { return codemodeSession.searchAndLoadTools(tc) })
				if slices.Contains(selection, toolSearchName) {
					tool.DefaultActive = nil
				}
				tools = append(tools, tool)
			}
			// Codemode is registered inactive unless selected by name. Its store
			// belongs to the session created below.
			if !slices.Contains(opts.ToolDenylist, codemode.ToolName) {
				tool := codemode.NewTool(&codemode.Store{
					Load: func(ctx context.Context) (map[string]json.RawMessage, error) {
						return codemodeSession.loadCodemodeStore(ctx)
					},
					Save: func(ctx context.Context, writes codemode.StoreWrites) error {
						return codemodeSession.saveCodemodeStore(ctx, writes)
					},
				})
				tool.PrepareLoadout = func(ctx context.Context, loadout agent.ToolLoadout) (agent.ToolLoadoutChanges, error) {
					if err := ctx.Err(); err != nil {
						return agent.ToolLoadoutChanges{}, err
					}
					codemodeSession.mu.Lock()
					settings := codemodeSession.settings.Codemode
					codemodeSession.mu.Unlock()
					budget := 3000
					if settings.InlineBudget != nil && *settings.InlineBudget >= 0 {
						budget = *settings.InlineBudget
					}
					return codemode.PrepareLoadout(loadout, settings.Mode, budget), nil
				}

				if slices.Contains(selection, codemode.ToolName) {
					tool.DefaultActive = nil
				}
				tools = append(tools, tool)
			}
		}
		agentCfg.Tools = append(agentCfg.Tools, tools...)
	}

	loader := r.artifactLoader
	if loader == nil {
		loader = GlobalArtifactLoader
	}

	resolvedPrompt, promptErr := r.buildSystemPrompt(cwd, loader, opts.SystemPrompt, opts.AppendSystemPrompts)
	if promptErr != nil {
		return nil, fmt.Errorf("build system prompt: %w", promptErr)
	}
	agentCfg.SystemPrompt = resolvedPrompt

	sessionID := opts.SessionID
	requestedSessionID := opts.SessionID
	if sessionID == "" && opts.ForkFrom == "" {
		sessionID = uuid.Must(uuid.NewV7()).String()
	}
	trackingOpenSession := false
	defer func() {
		if err != nil && trackingOpenSession {
			r.releaseOpenSession(sessionID)
		}
	}()
	if sessionID != "" && trackRuntimeSession(opts) {
		if err := r.claimOpenSession(sessionID); err != nil {
			return nil, err
		}
		trackingOpenSession = true
	}
	sessionDataDir := ""
	if r.sessionsDir != "" && (!opts.NoSession || opts.EphemeralExportPath != "") {
		sessionDataDir = filepath.Join(r.sessionsDir, ".data", sessionID)
	}
	agentCfg.SessionID = sessionID

	var hookSession *agentSession
	agentCfg.PrepareRequest = func(ctx context.Context, request agent.PrepareRequestContext) (*agent.AgentRequestUpdate, error) {
		return hookSession.prepareVirtualRequest(ctx, request)
	}
	pauseDir := filepath.Join(paths.ConfigDir(), "paused")
	agentCfg.PausedBy = func(model *ai.Model) string {
		target, err := pausedBy(pauseDir, hookSession.id, hookSession.getSessionName, model)
		if err != nil {
			hookSession.logger.Error("check pause flags", "dir", pauseDir, "error", err)
		}
		return target
	}
	agentCfg.GetStartupMessages = func(ctx context.Context) ([]*agent.Message, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hookSession.mu.Lock()
		defer hookSession.mu.Unlock()
		return slices.Clone(hookSession.pendingNextTurn), nil
	}
	// Wire plugin manager as EventDispatcher if available.
	// This dispatches agent events to all subscribed plugins per the extension spec.
	if r.pluginManager != nil {
		mgr := r.pluginManager
		agentCfg.EventDispatcher = func(ctx context.Context, name string, payload []byte, sessID string) agent.EventDispatchResult {
			if name == "agent_settled" && hookSession != nil {
				hookSession.mu.Lock()
				hookSession.emittingSettled = true
				hookSession.mu.Unlock()
				defer func() { hookSession.mu.Lock(); hookSession.emittingSettled = false; hookSession.mu.Unlock() }()
			}
			responses := mgr.DispatchEventCollect(ctx, name, json.RawMessage(payload), sessID)
			result := agent.EventDispatchResult{}
			for _, resp := range responses {
				result.Responses = append(result.Responses, agent.EventDispatchResponse{
					Block:        resp.Block,
					BlockReason:  resp.BlockReason,
					ResponseData: resp.ResponseData,
				})
				// Block only for cancellable events (tool_call,
				// session_before_fork/switch/tree/compact).
				if resp.Block && plugins.IsCancellableEvent(name) {
					result.Blocked = true
				}
			}
			return result
		}

		// Wire plugin tools into the agent's tool registry.
		// Plugin tools become model-callable from the normal agent tool path.
		if !r.noTools {
			pluginTools := mgr.Tools()
			for _, pt := range pluginTools {
				tool := pluginAgentTool(mgr, pt)
				agentCfg.Tools = append(agentCfg.Tools, tool)
			}
		}
	}

	agentCfg.Tools = selectSessionTools(agentCfg.Tools, opts.ToolAllowlist, opts.ToolDenylist)

	// Load session history if resuming or forking
	if opts.SessionID != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err != nil {
			return nil, fmt.Errorf("open session storage: %w", err)
		}
		if _, err := storage.Open(opts.SessionID); err != nil {
			if strings.Contains(err.Error(), "not found") {
				return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, opts.SessionID)
			}
			return nil, fmt.Errorf("validate session %q: %w", opts.SessionID, err)
		}
	}

	if opts.ForkFrom != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err != nil {
			return nil, fmt.Errorf("open session storage: %w", err)
		}
		// Validate that the parent session exists before forking
		if _, err := storage.Open(opts.ForkFrom); err != nil {
			return nil, fmt.Errorf("parent session %q not found: %w", opts.ForkFrom, err)
		}
	}

	var initialMessages []*agent.Message
	var sessionWriter *sessionpkg.Writer
	var resumedParentMsgID *string
	// Track if we created a new session file that might need slug derivation
	var newSessionFilePath string
	var pendingHeader *sessionpkg.Header
	var pendingSessionDir string

	// Load total tokens from the session file if resuming an existing session.
	// This is needed because totalTokens starts at 0 and only increases during
	// message processing, so we must restore the persisted count on resume.
	var loadedTotalTokens int
	var loadedUsage ai.Usage
	if opts.SessionID != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err == nil {
			reader, err := storage.Open(opts.SessionID)
			if err == nil {
				loadedTotalTokens = reader.TotalTokens().Total()
				usage, err := recordedSessionUsage(reader)
				if err != nil {
					return nil, err
				}
				loadedUsage = usage
				loadedTotalTokens = max(loadedTotalTokens, usage.TotalTokens)
			}
		}
	}

	// Ephemeral mode: skip all session persistence unless we have an ephemeral export path.
	// When EphemeralExportPath is set, create a temporary session file for export.
	if opts.NoSession && opts.EphemeralExportPath == "" {
		goto agentCreation
	}

	// Handle ephemeral export: create a temporary session file for export purposes.
	if opts.EphemeralExportPath != "" {
		storage, err := sessionpkg.NewStorageAt(opts.EphemeralExportPath)
		if err == nil {
			header := sessionpkg.NewHeader(cwd, nil, nil, opts.DisplayName)
			sessionWriter, err = storage.Create(header)
			if err != nil {
				// Failed to create temp session - continue without writing
				sessionWriter = nil
			}
		}
		goto agentCreation
	}

	// Handle fork: create a new session that inherits from a parent
	if opts.ForkFrom != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err == nil {
			// Load parent history for initial messages
			history, err := loadSessionHistoryAt(storage, opts.ForkFrom, opts.ForkFromEntry)
			if err != nil {
				return nil, err
			}
			if len(history) > 0 {
				initialMessages = history
			}
			var atEntry *string
			if opts.ForkFromEntry != "" {
				atEntry = &opts.ForkFromEntry
			}
			// Fork creates a new session with parent reference; the fork
			// copies the inherited entries into the child file, so chain
			// new messages onto the copied branch's last entry.
			sessionWriter, err = storage.Fork(opts.ForkFrom, atEntry)
			if err != nil {
				return nil, fmt.Errorf("fork session %q: %w", opts.ForkFrom, err)
			} else {
				if reader, posErr := sessionpkg.ReadFile(sessionWriter.Path()); posErr == nil {
					loadedTotalTokens = reader.TotalTokens().Total()
					usage, err := recordedSessionUsage(reader)
					if err != nil {
						return nil, errors.Join(err, sessionWriter.Close())
					}
					loadedUsage = usage
					loadedTotalTokens = max(loadedTotalTokens, usage.TotalTokens)
					if pos := reader.CurrentPosition(); pos != "" {
						resumedParentMsgID = &pos
					}
				}
				header, headerErr := sessionpkg.ReadHeader(sessionWriter.Path())
				if headerErr == nil {
					sessionID = header.ID
					agentCfg.SessionID = sessionID
					if !trackingOpenSession && trackRuntimeSession(opts) {
						if err := r.claimOpenSession(sessionID); err != nil {
							if closeErr := sessionWriter.Close(); closeErr != nil {
								return nil, errors.Join(err, fmt.Errorf("close fork writer: %w", closeErr))
							}
							return nil, err
						}
						trackingOpenSession = true
					}
				}
				// Forks may need slug derivation too
				newSessionFilePath = sessionWriter.Path()
			}
		}
	} else if sessionID != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err == nil {
			history, historyPosition, err := loadSessionHistoryWithPosition(storage, sessionID)
			if err == nil {
				initialMessages = history
				if historyPosition != "" {
					resumedParentMsgID = &historyPosition
				}
				// Resume the existing session for appending
				sessionWriter, err = storage.Resume(sessionID)
				if err != nil {
					return nil, fmt.Errorf("resume session %q: %w", sessionID, err)
				}
			} else if requestedSessionID == "" {
				// This is a fresh session with an auto-generated ID. Defer creating
				// the on-disk session file until the first message so launching and
				// closing the TUI does not leave empty sessions behind.
				header := sessionpkg.NewHeader(cwd, nil, nil, opts.DisplayName)
				header.ID = sessionID
				pendingHeader = &header
				pendingSessionDir = r.sessionsDir
			} else if opts.DisplayName != "" {
				// Display name provided; no slug derivation needed
				newSessionFilePath = ""
			}
		}
	}

	if len(initialMessages) > 0 {
		agentCfg.InitialMessages = initialMessages
	}

	// Label for ephemeral (NoSession) skip target.
agentCreation:

	// A newly-created persisted coding-agent session records the initial model
	// and thinking level as explicit entries, matching Pi's replay contract.
	// Resumed sessions already contain their historical setting changes and must
	// not receive duplicate initial entries.
	if sessionWriter != nil && newSessionFilePath != "" {
		if activeModelSpec.Provider != "" || activeModelSpec.Model != "" {
			if err := sessionWriter.Append(sessionpkg.NewModelChangeEntry(activeModelSpec.Provider, activeModelSpec.Model)); err != nil {
				return nil, fmt.Errorf("append initial model change: %w", err)
			}
		}
		if sessionThinkingLevel != "" {
			if err := sessionWriter.Append(sessionpkg.NewThinkingLevelChangeEntry(sessionThinkingLevel)); err != nil {
				return nil, fmt.Errorf("append initial thinking level change: %w", err)
			}
		}
	}

	agentCfg.SessionCWD = cwd

	// Get provider capabilities for tool execution.
	// The activeModel is resolved with a specific provider, so we can
	// retrieve its capabilities to pass to tools.
	var capabilities ai.Capabilities
	if sessionModel != nil {
		if capProvider, ok := ai.Get(sessionModel.API); ok {
			if capClient, ok := capProvider.(ai.CapableProvider); ok {
				capabilities = capClient.Capabilities()
			}
		}
	}
	// Capability overrides live in models.json under
	// providers.<id>.modelOverrides; the model-registry applies them when
	// the model is resolved.
	agentCfg.Capabilities = capabilities

	if !r.noTools && !r.noBuiltinTools {
		agentCfg.Tools = restoreDeclaredBuiltinTools(agentCfg.Tools, agentCfg.InitialMessages, settings, opts.ToolAllowlist, opts.ToolDenylist)
	}

	a, err := agent.New(agentCfg)
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	// Update the runtime's current session ID so the plugin system
	// can route Host.SessionAppend/SessionScan RPCs to this session.
	r.mu.Lock()
	r.currentSessionID = sessionID
	r.currentSessionCWD = cwd
	if r.pluginManager != nil {
		r.pluginManager.SetSessionIDProvider(func() string { return r.currentSessionID })
		r.pluginManager.SetSessionCWDProvider(func() string { return r.currentSessionCWD })
		r.pluginManager.SetCompactor(func(ctx context.Context, instructions string) error {
			r.mu.Lock()
			sess := r.currentSession
			r.mu.Unlock()
			if sess == nil {
				return fmt.Errorf("no active session")
			}
			if instructions == "" {
				return sess.Compact(ctx)
			}
			return sess.Compact(ctx, instructions)
		})
		r.pluginManager.SetToolDiscovery(func(ctx context.Context, req *proto.ToolDiscoveryRequest) (json.RawMessage, error) {
			r.mu.Lock()
			sess := r.currentSession
			r.mu.Unlock()
			loadout, ok := sess.(ToolLoadout)
			if !ok {
				return nil, errors.New("no active tool session")
			}
			return discoverSessionTools(ctx, loadout, req)
		})
		r.pluginManager.SetCustomMessageSender(func(ctx context.Context, raw json.RawMessage, trigger *bool, deliverAs string) error {
			var wire struct {
				CustomType string          `json:"customType"`
				Content    json.RawMessage `json:"content"`
				Display    bool            `json:"display"`
				Details    any             `json:"details"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				return fmt.Errorf("decode custom message: %w", err)
			}
			var content []ai.ContentBlock
			if len(wire.Content) > 0 && string(wire.Content) != "null" {
				if err := json.Unmarshal(wire.Content, &content); err != nil {
					var text string
					if err := json.Unmarshal(wire.Content, &text); err != nil {
						return fmt.Errorf("decode custom content: %w", err)
					}
					content = []ai.ContentBlock{{Type: ai.ContentBlockText, Text: text}}
				}
			}
			r.mu.Lock()
			sess := r.currentSession
			r.mu.Unlock()
			sender, ok := sess.(CustomMessageSender)
			if !ok {
				return errors.New("no active custom message session")
			}
			return sender.SendCustomMessage(ctx, CustomMessage{CustomType: wire.CustomType, Content: content, Display: wire.Display, Details: wire.Details}, CustomMessageOptions{TriggerTurn: trigger, DeliverAs: deliverAs})
		})
		r.pluginManager.SetMessageSender(func(text, deliverAs string) error {
			r.mu.Lock()
			sess := r.currentSession
			r.mu.Unlock()
			if sess == nil {
				return fmt.Errorf("no active session")
			}
			as, ok := sess.(*agentSession)
			if !ok {
				return fmt.Errorf("active session is not agentSession: %T", sess)
			}
			switch deliverAs {
			case "", "steer", "followUp":
			default:
				return fmt.Errorf("invalid deliver_as %q", deliverAs)
			}
			if deliverAs == "" {
				// Fire-and-forget: Send returns an event channel; drain in
				// a goroutine so plugin calls don't block until the turn
				// completes. Errors are returned synchronously.
				as.mu.Lock()
				busy := as.cancelFn != nil
				as.mu.Unlock()
				if busy {
					return fmt.Errorf("deliver_as is required while the session is busy")
				}
				ctx := context.Background()
				events, err := as.Send(ctx, Message{Role: "user", Text: text})
				if err != nil {
					return err
				}
				if events != nil {
					go func() {
						for range events {
						}
					}()
				}
				return nil
			}
			if deliverAs == "followUp" {
				return as.FollowUp(Message{Role: "user", Text: text})
			}
			return as.Steer(Message{Role: "user", Text: text})
		})
	}
	r.mu.Unlock()

	// Determine session start reason and previous file for session_start events.
	var sessionStartReason string
	var previousSessionFile string
	if opts.SessionID != "" && opts.ForkFrom == "" {
		// Resuming an existing session
		sessionStartReason = "resume"
		previousSessionFile = opts.SessionID
	} else if opts.ForkFrom != "" {
		// Forking from an existing session
		sessionStartReason = "fork"
		previousSessionFile = opts.ForkFrom
	} else {
		// New session
		sessionStartReason = "new"
	}

	// A no-session run has no session data directory. Keep an in-memory-ish store
	// root in temp so direct SDK attachment calls still fail or work predictably.
	attachmentDir := sessionDataDir
	if attachmentDir == "" {
		attachmentDir = filepath.Join(os.TempDir(), "tau-attachments", sessionID)
	}

	s := &agentSession{
		agent:                       a,
		id:                          sessionID,
		cwd:                         cwd,
		ui:                          ui,
		model:                       sessionModel,
		modelSpec:                   activeModelSpec,
		sessionWriter:               sessionWriter,
		parentMsgID:                 resumedParentMsgID,
		cmdRegistry:                 r.commandRegistry,
		pluginManager:               r.pluginManager,
		runtime:                     r,
		clientFactory:               clientFactory,
		credentialProvider:          credentialProvider,
		requestCredentials:          requestCredentials,
		credStore:                   credStore,
		artifactLoader:              loader,
		sessionStartReason:          sessionStartReason,
		previousSessionFile:         previousSessionFile,
		thinkingLevel:               agent.ThinkingLevel(sessionThinkingLevel),
		systemPromptOverride:        opts.SystemPrompt,
		appendSystemPrompts:         slices.Clone(opts.AppendSystemPrompts),
		toolAllowlist:               slices.Clone(opts.ToolAllowlist),
		defaultToolNames:            resolveDefaultToolSelection(settings),
		toolDenylist:                slices.Clone(opts.ToolDenylist),
		approveProjectFiles:         opts.ApproveProjectFiles,
		sessionFileBeforeRename:     newSessionFilePath,
		pendingHeader:               pendingHeader,
		pendingSessionDir:           pendingSessionDir,
		sessionDataDir:              sessionDataDir,
		attachments:                 NewAttachmentStore(filepath.Join(attachmentDir, "attachments")),
		pendingInitialModelProvider: activeModelSpec.Provider,
		pendingInitialModel:         activeModelSpec.Model,
		pendingInitialThinking:      sessionThinkingLevel,
		settings:                    settings,
		compactionConfig:            compactionCfg,
		autoCompactionDefault:       compactionCfg,
		autoRetryEnabled:            true,
		totalTokens:                 loadedTotalTokens,
		usageTotals:                 loadedUsage,
		isPersistedFunc:             opts.IsPersisted,
		logger:                      sessionLogger,
	}
	codemodeSession = s

	if resumedParentMsgID == nil && len(initialMessages) == 0 {
		messages := a.Messages()
		if len(messages) > 0 && messages[0].Role == agent.RoleSystem {
			s.pendingInitialTranscript = messages[0]
		}
	}

	hookSession = s
	if !r.noTools {
		s.startMCP(settings.ProjectResourcesTrustedAt(cwd))
	}
	if r.pluginManager != nil {
		for _, registered := range r.pluginManager.Tools() {
			s.pluginToolNames = append(s.pluginToolNames, registered.Name)
		}
	}

	// Expose the new session to the plugin system before session_start so Host
	// calls made by session_start handlers route to this session.
	r.mu.Lock()
	r.currentSession = s
	r.mu.Unlock()

	// pi-equivalent: dispatch session_start to subscribed plugins so they can
	// restore state from the session transcript via SessionScan. This mirrors
	// the session_start event pi extensions receive on every session (new /
	// resume / fork). The frontend session_start event continues to be
	// emitted per Send; this plugin dispatch fires once per session
	// construction.
	if r.pluginManager != nil {
		modelID := modelIDForEvent(sessionModel)
		startPayload, _ := json.Marshal(map[string]any{
			"id":                  sessionID,
			"cwd":                 cwd,
			"model":               modelID,
			"reason":              sessionStartReason,
			"previousSessionFile": previousSessionFile,
		})
		r.pluginManager.DispatchEventCollect(ctx, "session_start", startPayload, sessionID)
	}
	if err := r.trackLiveSession(s); err != nil {
		return nil, errors.Join(err, s.Close(ctx))
	}

	return s, nil
}

func restoreDeclaredBuiltinTools(tools []agent.Tool, messages []*agent.Message, settings *ai.Settings, allow, deny []string) []agent.Tool {
	baseline := ai.CurrentSystemMessage(agent.ConvertMessagesToAI(messages))
	if baseline == nil {
		return tools
	}
	candidates := filterToolsByName(agent.BuiltInToolsWithConfig(buildToolConfig(settings)), allow, deny)
	for _, declaration := range baseline.ToolsAdded {
		if slices.ContainsFunc(tools, func(tool agent.Tool) bool { return tool.Name == declaration.Name }) {
			continue
		}
		for _, candidate := range candidates {
			if candidate.Name == declaration.Name {
				tools = append(tools, candidate)
				break
			}
		}
	}
	return tools
}

func selectSessionTools(tools []agent.Tool, selection, exclusions []string) []agent.Tool {
	allow := selection
	deny := slices.Clone(exclusions)
	if toolSelectionHasModifiers(allow) {
		// Modifiers change initial activation, not which extension tools may register.
		for _, entry := range allow {
			if name, ok := strings.CutPrefix(entry, "-"); ok {
				deny = append(deny, name)
			}
			if name, ok := strings.CutPrefix(entry, "+"); ok {
				deny = slices.DeleteFunc(deny, func(n string) bool { return n == name && !slices.Contains(exclusions, n) })
			}
		}
		allow = nil
	}
	if allow != nil || len(deny) > 0 {
		tools = filterToolsByName(tools, allow, deny)
		if allow != nil && len(allow) == 0 {
			tools = nil
		}
	}

	if toolSelectionHasModifiers(selection) {
		for _, selection := range selection {
			if name, ok := strings.CutPrefix(selection, "+"); ok {
				for i, tool := range tools {
					if matchesToolName(tool.Name, map[string]struct{}{name: {}}) {
						tools[i].DefaultActive = new(true)
					}
				}
			}
		}
	}

	return tools
}

// filterToolsByName restricts tools to the allowlist (when non-empty) and
// drops any name in the denylist. Deny wins on conflict. The order of the
// remaining tools matches the input order.
func filterToolsByName(tools []agent.Tool, allow, deny []string) []agent.Tool {
	if len(allow) == 0 && len(deny) == 0 {
		return tools
	}
	denySet := make(map[string]struct{}, len(deny))
	for _, name := range deny {
		denySet[name] = struct{}{}
	}
	allowSet := make(map[string]struct{})
	hasExplicit := false
	for _, name := range allow {
		if !strings.HasPrefix(name, "+") && !strings.HasPrefix(name, "-") {
			hasExplicit = true
		}
	}
	if !hasExplicit {
		for _, tool := range tools {
			allowSet[tool.Name] = struct{}{}
		}
	}
	for _, name := range allow {
		if removed, ok := strings.CutPrefix(name, "-"); ok {
			delete(allowSet, removed)
		} else {
			allowSet[strings.TrimPrefix(name, "+")] = struct{}{}
		}
	}

	out := make([]agent.Tool, 0, len(tools))
	for _, t := range tools {
		if matchesToolName(t.Name, denySet) {
			continue
		}
		if len(allow) > 0 {
			if !matchesToolName(t.Name, allowSet) {
				continue
			}
		}
		for _, selection := range allow {
			if strings.HasPrefix(selection, "-") {
				continue
			}
			if matchesToolName(t.Name, map[string]struct{}{strings.TrimPrefix(selection, "+"): {}}) {
				t.DefaultActive = new(true)
			}
		}
		out = append(out, t)
	}
	return out
}

// ValidateToolSelection checks SDK and CLI tool lists. Settings may mix names and modifiers.
func ValidateToolSelection(entries []string) error {
	if !toolSelectionHasModifiers(entries) {
		return nil
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "+") && !strings.HasPrefix(entry, "-") {
			return errors.New("tool names cannot be mixed with +name or -name entries")
		}
		if strings.Contains(entry, "*") {
			return fmt.Errorf("+name and -name entries take exact tool names, not patterns: %s", entry)
		}
	}
	return nil
}

func toolSelectionHasModifiers(entries []string) bool {
	return slices.ContainsFunc(entries, func(entry string) bool { return strings.HasPrefix(entry, "+") || strings.HasPrefix(entry, "-") })
}

func applyToolModifiers(base, entries []string) []string {
	tools := slices.Clone(base)
	for _, entry := range entries {
		if name, ok := strings.CutPrefix(entry, "-"); ok {
			tools = slices.DeleteFunc(tools, func(n string) bool { return n == name })
		} else if name, ok := strings.CutPrefix(entry, "+"); ok && name != "" && !slices.Contains(tools, name) {
			tools = append(tools, name)
		}
	}
	return tools
}

func resolveDefaultToolSelection(settings *ai.Settings) []string {
	defaults := []string{"read", "bash", "edit", "write"}
	if settings == nil || settings.DefaultTools == nil {
		return defaults
	}
	entries := settings.DefaultTools
	plain := []string{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "+") && !strings.HasPrefix(entry, "-") {
			plain = append(plain, entry)
		}
	}
	if len(entries) > 0 && len(plain) == 0 {
		plain = defaults
	}
	return applyToolModifiers(plain, entries)
}

func matchesToolName(name string, patterns map[string]struct{}) bool {
	for pattern := range patterns {
		if pattern == name {
			return true
		}
		// A malformed glob matches nothing, like a name that is not registered.
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	return false
}

// buildContextFileAddendum follows upstream loadProjectContextFiles: the agent
// directory's context file, then one file per directory from the filesystem
// root down to cwd. Unreadable files are skipped and reported in err.
func buildContextFileAddendum(agentDir, cwd string) (addendum string, err error) {
	if cwd == "" {
		cwd = "."
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}

	var (
		files    []contextFile
		ancestor []contextFile
		errs     []error
		seen     = map[string]bool{}
	)
	if agentDir != "" {
		if file, ok := loadContextFileFromDir(agentDir, &errs); ok {
			files = append(files, file)
			seen[file.path] = true
		}
	}
	shadowed := findShadowedContextFile(cwd, &errs)
	for dir := cwd; ; {
		file, ok := loadContextFileFromDir(dir, &errs)
		isShadowed := ok && shadowed != "" && filepath.Join(canonicalPath(dir), filepath.Base(file.path)) == shadowed
		if ok && !isShadowed && !seen[file.path] {
			ancestor = append(ancestor, file)
			seen[file.path] = true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	slices.Reverse(ancestor)
	files = append(files, ancestor...)

	sections := make([]string, 0, len(files))
	for _, file := range files {
		sections = append(sections, fmt.Sprintf("## Context: %s\n\n%s", file.path, strings.TrimSpace(file.content)))
	}
	return strings.Join(sections, "\n\n"), errors.Join(errs...)
}

type contextFile struct{ path, content string }

func loadContextFileFromDir(dir string, errs *[]error) (contextFile, bool) {
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				*errs = append(*errs, fmt.Errorf("stat context file %s: %w", path, err))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("read context file %s: %w", path, err))
			continue
		}
		return contextFile{path: path, content: strings.TrimPrefix(string(data), "\ufeff")}, true
	}
	return contextFile{}, false
}

// findShadowedContextFile returns the main repository's context file that a
// linked worktree nested inside it shadows, canonicalized, or "" when none is.
func findShadowedContextFile(cwd string, errs *[]error) string {
	repoDir, commonGitDir, ok := findGitPaths(cwd, errs)
	if !ok {
		return ""
	}
	commonGitDir = canonicalPath(commonGitDir)
	worktreeRoot := canonicalPath(repoDir)
	mainRoot := filepath.Dir(commonGitDir)
	if !strings.HasPrefix(worktreeRoot, mainRoot+string(filepath.Separator)) || canonicalPath(filepath.Join(mainRoot, ".git")) != commonGitDir {
		return ""
	}
	file, ok := loadContextFileFromDir(worktreeRoot, errs)
	if !ok {
		return ""
	}
	return filepath.Join(mainRoot, filepath.Base(file.path))
}

// findGitPaths mirrors upstream's footer helper: the nearest repository or
// worktree root and its common git directory.
func findGitPaths(cwd string, errs *[]error) (repoDir, commonGitDir string, ok bool) {
	for dir := cwd; ; {
		gitPath := filepath.Join(dir, ".git")
		info, statErr := os.Stat(gitPath)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			*errs = append(*errs, fmt.Errorf("stat %s: %w", gitPath, statErr))
			return "", "", false
		}
		if statErr == nil {
			if info.IsDir() {
				return dir, gitPath, contextGitHeadExists(gitPath, errs)
			}
			data, err := os.ReadFile(gitPath)
			if err != nil {
				*errs = append(*errs, fmt.Errorf("read %s: %w", gitPath, err))
				return "", "", false
			}
			target, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
			if !found {
				return "", "", false
			}
			gitDir := target
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(dir, gitDir)
			}
			if !contextGitHeadExists(gitDir, errs) {
				return "", "", false
			}
			commonGitDir = gitDir
			if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
				commonGitDir = strings.TrimSpace(string(common))
				if !filepath.IsAbs(commonGitDir) {
					commonGitDir = filepath.Join(gitDir, commonGitDir)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				*errs = append(*errs, fmt.Errorf("read %s: %w", filepath.Join(gitDir, "commondir"), err))
				return "", "", false
			}
			return dir, commonGitDir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

func contextGitHeadExists(gitDir string, errs *[]error) bool {
	path := filepath.Join(gitDir, "HEAD")
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		*errs = append(*errs, fmt.Errorf("stat %s: %w", path, err))
	}
	return err == nil
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// buildSystemPrompt resolves the session system prompt from SYSTEM.md, the
// context files, the skills catalog, preloaded skills, APPEND_SYSTEM.md, and
// the explicit overrides, reading every source from disk on each call.
func (r *runtime) buildSystemPrompt(cwd string, loader ArtifactLoader, override string, appends []string) (string, error) {
	var skills *artifacts.SkillRegistry
	if loader != nil {
		skills = loader.SkillRegistry()
	}
	return r.buildSystemPromptWithSkills(cwd, skills, override, appends)
}

func (r *runtime) buildSystemPromptWithSkills(cwd string, skills *artifacts.SkillRegistry, override string, appends []string) (string, error) {
	return r.buildSystemPromptWithPolicy(cwd, skills, override, appends, r.settings)
}

func (r *runtime) buildSystemPromptWithPolicy(cwd string, skills *artifacts.SkillRegistry, override string, appends []string, settings *ai.Settings) (string, error) {
	addendumParts := []string{fmt.Sprintf("Treat %s as the project root. Start project file discovery and searches there, and keep them scoped to it unless the user directs you elsewhere or the task explicitly requires external files.", cwd)}
	if !r.noContextFiles {
		contextAddendum, err := buildContextFileAddendum(paths.ConfigDir(), cwd)
		if err != nil {
			return "", fmt.Errorf("load context files: %w", err)
		}
		if contextAddendum != "" {
			addendumParts = append(addendumParts, contextAddendum)
		}
	}
	if skills != nil {
		if catalog := artifacts.FormatSkillsCatalog(skills.List()); catalog != "" {
			addendumParts = append(addendumParts, catalog)
		}
		for _, skillID := range r.preloadSkills {
			if skill, found := skills.Get(skillID); found {
				addendumParts = append(addendumParts, fmt.Sprintf("<skill name=%q location=%q>\nReferences are relative to %s.\n\n%s</skill>", skill.ID, skill.SourcePath, skill.BaseDir, skill.Body))
			}
		}
	}
	promptCWD := cwd
	if !settings.ProjectResourcesTrustedAt(cwd) {
		promptCWD = ""
	}
	if appends == nil {
		appendSys, err := loadPromptFile(paths.ConfigDir(), promptCWD, "APPEND_SYSTEM.md")
		if err != nil {
			return "", err
		}
		if appendSys != "" {
			addendumParts = append(addendumParts, appendSys)
		}
	} else {
		for _, input := range appends {
			if body := r.resolvePromptInput(input); body != "" {
				addendumParts = append(addendumParts, body)
			}
		}
	}
	var prompt string
	if override != "" {
		prompt = r.resolvePromptInput(override)
	} else {
		var err error
		prompt, err = loadPromptFile(paths.ConfigDir(), promptCWD, "SYSTEM.md")
		if err != nil {
			return "", err
		}
	}

	if prompt != "" {
		prompt += "\n\n"
	}
	return prompt + strings.Join(addendumParts, "\n\n"), nil
}

func (r *runtime) resolvePromptInput(input string) string {
	if input == "" {
		return ""
	}
	body, err := os.ReadFile(input)
	if err == nil {
		return strings.TrimPrefix(string(body), "\ufeff")
	}
	if !os.IsNotExist(err) && r.logger != nil {
		r.logger.Warn("could not read prompt file; using literal input", "path", input, "error", err)
	}
	return input
}

// loadSystemPrompt reads SYSTEM.md from <cwd>/.tau first, then
// ~/.tau/agent. First-wins. The path used is reported alongside the body
// so callers can show provenance.
func loadSystemPrompt(homeDir, cwd string) (body string, err error) {
	if cwd == "" {
		cwd = "."
	}
	return loadPromptFile(homeDir, cwd, "SYSTEM.md")
}

func loadPromptFile(homeDir, cwd, name string) (string, error) {
	candidates := []string{filepath.Join(homeDir, name)}
	if cwd != "" {
		candidates = append([]string{filepath.Join(cwd, ".tau", name)}, candidates...)
	}
	for _, path := range candidates {
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			return strings.TrimPrefix(string(data), "\ufeff"), nil
		}
		if !os.IsNotExist(readErr) {
			return "", fmt.Errorf("read %s: %w", path, readErr)
		}
	}
	return "", nil
}

// loadAppendSystemPrompt reads APPEND_SYSTEM.md from the same two locations.
// Empty body means "nothing to append".
func loadAppendSystemPrompt(homeDir, cwd string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	return loadPromptFile(homeDir, cwd, "APPEND_SYSTEM.md")
}

// loadSessionHistory loads messages from a session file for re-injection into the agent.
func loadSessionHistory(storage *sessionpkg.Storage, sessionID string) ([]*agent.Message, error) {
	messages, _, err := loadSessionHistoryWithPosition(storage, sessionID)
	return messages, err
}

func loadSessionHistoryWithPosition(storage *sessionpkg.Storage, sessionID string) ([]*agent.Message, string, error) {
	reader, err := storage.Open(sessionID)
	if err != nil {
		return nil, "", err
	}
	messages, err := agentMessagesFromSessionReader(reader, "")
	if err != nil {
		return nil, "", err
	}
	return messages, reader.CurrentPosition(), nil
}

func loadSessionHistoryAt(storage *sessionpkg.Storage, sessionID, atEntry string) ([]*agent.Message, error) {
	reader, err := storage.Open(sessionID)
	if err != nil {
		return nil, err
	}
	return agentMessagesFromSessionReader(reader, atEntry)
}

func agentMessagesFromSessionReader(reader *sessionpkg.Reader, atEntry string) ([]*agent.Message, error) {
	var sessionMsgs []*sessionpkg.Message
	if atEntry == "" {
		sessionMsgs = reader.ToAgentMessages()
	} else {
		manager := sessionpkg.NewManager(reader.Header().CWD)
		manager.LoadEntries(reader)
		if err := manager.SetCurrentPosition(atEntry); err != nil {
			return nil, fmt.Errorf("select session position: %w", err)
		}
		for _, entry := range manager.ContextEntries() {
			if msg := entry.ToAgentMessage(); msg != nil {
				sessionMsgs = append(sessionMsgs, msg)
			}
		}
	}
	return sessionMessagesToAgentMessages(sessionMsgs), nil
}

func sessionMessagesToAgentMessages(sessionMsgs []*sessionpkg.Message) []*agent.Message {
	messages := make([]*agent.Message, 0, len(sessionMsgs))
	for _, sm := range sessionMsgs {
		messages = append(messages, &agent.Message{
			CustomType: sm.CustomType, Display: sm.Display, Details: sm.Details,
			Sections: sm.Sections, ToolsAdded: sm.ToolsAdded, ToolsRemoved: sm.ToolsRemoved,
			Data:              sm.Data,
			StructuredContent: sm.StructuredContent, ToolUsage: sm.ToolUsage, NestedCalls: sm.NestedCalls,
			ExcludeFromContext:    sm.ExcludeFromContext,
			Role:                  agent.MessageRole(sm.Role),
			Usage:                 sm.Usage,
			Content:               sm.Content,
			ToolCallID:            sm.ToolCallID,
			ToolName:              sm.ToolName,
			IsError:               sm.IsError,
			Provider:              sm.Provider,
			API:                   sm.API,
			Model:                 sm.Model,
			StopReason:            ai.StopReason(sm.StopReason),
			ErrorMessage:          sm.ErrorMessage,
			ResponseID:            sm.ResponseID,
			ProviderThinkingLevel: sm.ProviderThinkingLevel,
			Diagnostics:           sm.Diagnostics,
			DurationMs:            sm.DurationMs,
			ThinkingLevel:         sm.ThinkingLevel,
			Timestamp:             sm.Timestamp,
		})
	}
	return collapseSystemBaselines(messages)
}

func sessionEntries(reader *sessionpkg.Reader) []sessionpkg.Entry {
	entries := reader.Entries()
	out := make([]sessionpkg.Entry, len(entries))
	copy(out, entries)
	return out
}

func entriesTo(entries []sessionpkg.Entry, entryID string) []sessionpkg.Entry {
	byID := make(map[string]sessionpkg.Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	current, ok := byID[entryID]
	if !ok {
		return nil
	}
	var reversed []sessionpkg.Entry
	seen := make(map[string]bool)
	for !seen[current.ID] {

		seen[current.ID] = true
		reversed = append(reversed, current)
		if current.ParentID == nil || *current.ParentID == "" {
			break
		}
		next, ok := byID[*current.ParentID]
		if !ok {
			break
		}
		current = next
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// Close closes every open session, then shuts down plugins. Later NewSession
// calls fail.
func (r *runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	sessions := slices.Collect(maps.Keys(r.liveSessions))
	r.mu.Unlock()
	var err error
	for _, s := range sessions {
		if closeErr := s.Close(ctx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close session %s: %w", s.id, closeErr))
		}
	}
	if r.pluginManager != nil {
		err = errors.Join(err, r.pluginManager.Shutdown(ctx))
	}
	return err
}

// SessionID returns the current active session ID, or empty string if no session is active.
func (r *runtime) SessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentSessionID
}

// ListSessions returns a list of sessions from storage.
func (r *runtime) ListSessions(_ context.Context) ([]SessionInfo, error) {
	if r.sessionsDir == "" {
		return nil, nil
	}

	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return nil, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get cwd: %w", err)
	}
	summaries, err := storage.ListCWD(cwd)
	if err != nil {
		return nil, err
	}

	infos := make([]SessionInfo, len(summaries))
	for i, s := range summaries {
		infos[i] = SessionInfo{
			ID:           s.ID,
			Name:         s.Name,
			Filename:     s.Filename,
			CWD:          s.CWD,
			Created:      s.Created,
			LastModified: s.LastModified,
			Model:        s.Model,
			Status:       s.Status,
			MessageCount: s.MessageCount,
			Tokens:       s.Tokens,
			ParentID:     s.ParentID,
			FirstPrompt:  s.FirstPrompt,
			Pinned:       s.Pinned,
			PinnedAt:     s.PinnedAt,
		}
	}
	return infos, nil
}

func (r *runtime) PinSession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pin session: %w", err)
	}
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return fmt.Errorf("open session storage: %w", err)
	}
	return storage.SetPinned(id, true)
}

func (r *runtime) UnpinSession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unpin session: %w", err)
	}
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return fmt.Errorf("open session storage: %w", err)
	}
	return storage.SetPinned(id, false)
}

// ResumeSession resumes an existing session with the given ID.
func (r *runtime) ResumeSession(ctx context.Context, id string, opts SessionOptions) (Session, error) {
	opts.SessionID = id
	return r.NewSession(ctx, opts)
}

// setupDefaults resolves and stores the active model. Exposed for capability checking.
func (r *runtime) SetupDefaults() error {
	return r.setupDefaults(context.Background())
}

// ActiveModel returns the resolved active model, or nil if not yet resolved.
func (r *runtime) ActiveModel() *ai.Model {
	return r.activeModel
}

// TelemetryEnabled returns whether telemetry reporting is enabled.
// Uses RuntimeOptions.Telemetry if set, otherwise falls back to TAU_TELEMETRY env var.
func (r *runtime) TelemetryEnabled() bool {
	if r.telemetry != nil {
		return *r.telemetry
	}
	return ai.TelemetryEnabled()
}

// PluginManager returns the plugin manager.
func (r *runtime) PluginManager() *plugins.Manager {
	return r.pluginManager
}

// ArtifactLoader returns the runtime artifact loader.
func (r *runtime) ArtifactLoader() ArtifactLoader {
	return r.artifactLoader
}

// GetSessionStorage returns the session storage for direct access.
// It creates the storage on first access. Returns nil if storage cannot be created.
func (r *runtime) GetSessionStorage() *sessionpkg.Storage {
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return nil
	}
	return storage
}

// buildToolConfig constructs an agent.ToolConfig from settings.
func buildToolConfig(settings *ai.Settings) agent.ToolConfig {
	cfg := agent.ToolConfig{}
	if settings == nil {
		return cfg
	}
	cfg.BashShell = cmp.Or(settings.Bash.Shell, expandShellPath(settings.ShellPath))
	if settings.Bash.OutputCap > 0 {
		cfg.BashOutputCap = settings.Bash.OutputCap
	}
	if len(settings.Bash.EnvAllowlist) > 0 {
		cfg.BashEnvAllowlist = settings.Bash.EnvAllowlist
	}
	if len(settings.Bash.EnvExtra) > 0 {
		cfg.BashEnvExtra = settings.Bash.EnvExtra
	}
	cfg.CommandPrefix = cmp.Or(settings.Bash.CommandPrefix, settings.ShellCommandPrefix)
	if settings.Bash.StreamWindowMs > 0 {
		cfg.BashStreamWindow = time.Duration(settings.Bash.StreamWindowMs) * time.Millisecond
	}
	if settings.Bash.KillGraceMs > 0 {
		cfg.BashKillGrace = time.Duration(settings.Bash.KillGraceMs) * time.Millisecond
	}
	if settings.Read.LineCap > 0 {
		cfg.ReadLineCap = settings.Read.LineCap
	}
	if settings.Images.AutoResize != nil {
		cfg.AutoResizeImages = *settings.Images.AutoResize
		cfg.AutoResizeImagesSet = true
	}
	if settings.Images.MaxWidth > 0 {
		cfg.MaxImageWidth = settings.Images.MaxWidth
	}
	if settings.Images.MaxHeight > 0 {
		cfg.MaxImageHeight = settings.Images.MaxHeight
	}
	cfg.BlockImages = settings.Images.BlockImages
	return cfg
}

// expandShellPath expands a leading ~ in the shellPath setting.
func expandShellPath(shell string) string {
	if shell != "~" && !strings.HasPrefix(shell, "~/") && !strings.HasPrefix(shell, `~\`) {
		return shell
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return shell
	}
	return filepath.Join(home, shell[1:])
}

// ToolLoadout exposes registered metadata, activation, and cancellable discovery.
type ToolLoadout interface {
	RegisteredTools() []agent.Tool
	ActiveTools() []string
	SetActiveTools(context.Context, ...string) error
	SearchTools(context.Context, string, string, int) ([]agent.Tool, error)
	DescribeTool(string) *agent.Tool
	DescribeNamespace(string) *agent.ToolNamespace
}

func (s *agentSession) RegisteredTools() []agent.Tool { return s.agent.State().Tools }
func (s *agentSession) ActiveTools() []string         { return s.agent.ActiveTools() }
func (s *agentSession) SetActiveTools(ctx context.Context, names ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	return s.agent.SetActiveTools(names...)
}

func (s *agentSession) SearchTools(ctx context.Context, query, namespace string, limit int) ([]agent.Tool, error) {
	return agent.SearchTools(ctx, s.RegisteredTools(), query, namespace, limit)
}

func (s *agentSession) DescribeTool(name string) *agent.Tool {
	return agent.DescribeTool(s.RegisteredTools(), name)
}

func (s *agentSession) DescribeNamespace(name string) *agent.ToolNamespace {
	return agent.DescribeNamespace(s.RegisteredTools(), name)
}

func discoverSessionTools(ctx context.Context, loadout ToolLoadout, req *proto.ToolDiscoveryRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	describe := func(tool agent.Tool) map[string]any {
		return map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters, "outputSchema": tool.OutputSchema, "exposure": tool.Exposure, "namespace": tool.Namespace, "defaultActive": tool.DefaultActive}
	}
	var result any
	switch req.Operation {
	case "registered", "search":
		tools := loadout.RegisteredTools()
		if req.Operation == "search" {
			var err error
			tools, err = loadout.SearchTools(ctx, req.Query, req.Namespace, int(req.Limit))
			if err != nil {
				return nil, err
			}
		}
		descriptions := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			descriptions = append(descriptions, describe(tool))
		}
		result = descriptions
	case "active":
		result = loadout.ActiveTools()
	case "activate":
		if err := loadout.SetActiveTools(ctx, req.Names...); err != nil {
			return nil, err
		}
		result = loadout.ActiveTools()
	case "tool":
		if tool := loadout.DescribeTool(req.Query); tool != nil {
			result = describe(*tool)
		}
	case "namespace":
		result = loadout.DescribeNamespace(req.Query)
	default:
		return nil, errors.New("unknown tool discovery operation")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode tool discovery: %w", err)
	}
	return data, nil
}

// ResourceReloader admits prepared resources while the session is idle.
type ResourceReloader interface {
	ReloadResources(context.Context, *ai.Settings, *artifacts.PreparedReload, *plugins.PreparedReload, string) error
}

func (s *agentSession) ReloadResources(ctx context.Context, settings *ai.Settings, prepared *artifacts.PreparedReload, preparedPlugins *plugins.PreparedReload, name string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if s.closed {
		return errors.New("session closed")
	}
	if s.runDone != nil || s.agent.State().IsStreaming {
		return errors.New("cannot reload resources while session is processing")
	}
	if _, err := settings.ResolveCompaction(s.model); err != nil {
		return err
	}
	if _, err := s.settings.ResolveCompaction(s.model); err != nil {
		return err
	}
	if _, _, err := settingsQueueModes(settings); err != nil {
		return err
	}
	if _, err := settings.ResolveTransport(); err != nil {
		return err
	}
	var skills *artifacts.SkillRegistry
	if prepared != nil {
		skills = prepared.SkillRegistry()
	} else if s.artifactLoader != nil {
		skills = s.artifactLoader.SkillRegistry()
	}
	prompt, err := s.runtime.buildSystemPromptWithPolicy(s.cwd, skills, s.systemPromptOverride, s.appendSystemPrompts, settings)
	if err != nil {
		return err
	}
	admitted := false
	admit := func() error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err := s.reloadSystemPromptLocked(prompt); err != nil {
			return err
		}
		admitted = true
		return nil
	}
	var reloadErr error
	if preparedPlugins != nil {
		reloadErr = preparedPlugins.ReloadWithAdmission(ctx, name, admit)
	} else {
		reloadErr = admit()
	}
	if !admitted {
		return reloadErr
	}
	if prepared != nil {
		prepared.Commit()
	}
	reloadErr = errors.Join(reloadErr, s.reloadSettingsLocked(settings))
	if s.pluginManager != nil && !s.runtime.noTools {
		reloadErr = errors.Join(reloadErr, s.reloadToolRegistryLocked())
	}
	return reloadErr
}
