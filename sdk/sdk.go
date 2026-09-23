// Package sdk provides the public API surface for embedding tau as a Go library.
// It defines Session and Runtime interfaces and concrete implementations that
// wrap the agent/ package.
package sdk // import "github.com/losinggeneration/tau/sdk"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/agent/artifacts"
	"github.com/losinggeneration/tau/agent/compaction"
	"github.com/losinggeneration/tau/agent/plugins"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/ai"
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
func (s SessionState) Position() string { return s.position }

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
	// stack in order; empty entries are skipped. Per-session.
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

// MessageLister exposes the session's in-memory messages.
type MessageLister interface{ GetMessages() []Message }

// ForkMessageLister exposes user messages suitable for fork pickers.
type ForkMessageLister interface{ GetUserMessagesForForking() []Message }

// HTMLExporter exports a session as an HTML file.
type HTMLExporter interface {
	ExportHTML(ctx context.Context, path string) (string, error)
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
	// UI is the user interface for the runtime.
	UI ui.UI
	// SessionsDir is the directory for session storage.
	SessionsDir string
	// CredentialStore is the credential storage backend.
	CredentialStore store.CredentialStore
	// Settings is the merged settings (user + project). Used for model resolution.
	Settings *ai.Settings
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
	// ShareViewerURL returns the share viewer URL override from TAU_SHARE_VIEWER_URL.
	// Returns empty string if not set (defaults to https://pi.dev/session/).
	ShareViewerURL() string
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
	_ ForkMessageLister             = (*agentSession)(nil)
	_ HTMLExporter                  = (*agentSession)(nil)
	_ Namer                         = (*agentSession)(nil)
	_ LastAssistantTexter           = (*agentSession)(nil)
	_ Statser                       = (*agentSession)(nil)
	_ AttachmentManager             = (*agentSession)(nil)
	_ DisconnectedSessionEndEmitter = (*agentSession)(nil)
	_ ArtifactLoaderProvider        = (*runtime)(nil)
)

type agentSession struct {
	agent         *agent.Agent
	id            string
	cwd           string
	ui            ui.UI
	model         *ai.Model
	modelSpec     ai.ModelSpec
	seq           int64
	totalTokens   int
	mu            sync.Mutex
	cancelFn      context.CancelFunc
	closed        bool // true after first Close call
	sessionWriter *sessionpkg.Writer
	parentMsgID   *string // parent ID for the next message entry
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
	artifactLoader     ArtifactLoader

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
	toolAllowlist []string
	// toolDenylist removes tools from the session's tool set (CLI --exclude-tools/-xt).
	toolDenylist []string
	// approveProjectFiles pre-approves file operations within CWD (CLI --approve/-a).
	approveProjectFiles bool

	// sessionFileBeforeRename stores the session file path before slug derivation.
	// When empty, the session file has already been renamed (or no rename needed).
	sessionFileBeforeRename string

	// pendingHeader defers creating brand-new persisted session files until the
	// first user message. This avoids empty sessions from UI startup/teardown.
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

	// isPersistedFunc returns whether the session has a persisted backing store.
	// This is true unless NoSession was set without EphemeralExportPath.
	isPersistedFunc func() bool

	// compactionConfig holds auto-compaction settings for this session.
	compactionConfig compaction.Config
	// autoCompactionDefault remembers the configured auto-compaction settings when toggled off.
	autoCompactionDefault compaction.Config
	// autoRetryEnabled records the user's retry preference for UI/RPC state.
	autoRetryEnabled bool

	// logger receives session diagnostics. Defaults to discard.
	logger *slog.Logger
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

func (s *agentSession) Send(ctx context.Context, msg Message) (<-chan Event, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session closed")
	}
	// Check for slash-command invocation before processing as a normal message.
	// If the message starts with "/" and we have a command registry, route
	// the command through the registry instead of sending to the LLM.
	// Slash commands like /reload should interrupt any active run first.
	isSlashCommand := strings.HasPrefix(msg.Text, "/") && s.cmdRegistry != nil
	if s.cancelFn != nil {
		if isSlashCommand {
			// Interrupt the active run so the slash command can execute.
			s.cancelFn()
			s.agent.Abort()
		} else {
			s.mu.Unlock()
			return nil, fmt.Errorf("session already processing")
		}
	}
	if isSlashCommand {
		if handled, resolved := s.resolveArtifactCommand(msg); handled {
			msg = resolved
		} else {
			s.mu.Unlock()
			return s.executeSlashCommand(ctx, msg.Text)
		}
	}
	attachmentBlocks, err := s.attachmentBlocksLocked(msg.AttachmentIDs)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	msg.Images = append(msg.Images, attachmentBlocks...)
	if err := s.validateVisionInput(msg); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return s.sendPreparedMessageLocked(ctx, msg)
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
	for _, block := range msg.Images {
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
		if !s.agent.State().Capabilities.Vision {
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
	prompt, found := registry.Get(name)
	if !found {
		return false, msg
	}

	msg.Text = strings.ReplaceAll(string(prompt.Body), "{{args}}", args)
	return true, msg
}

func parseSlashCommand(line string) (name, args string) {
	line = strings.TrimPrefix(strings.TrimSpace(line), "/")
	parts := strings.SplitN(line, " ", 2)
	name = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return name, args
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
	ctx, cancel := context.WithCancel(ctx)
	s.cancelFn = cancel
	s.mu.Unlock()

	events := make(chan Event, 64)

	go func() {
		defer close(events)
		defer func() {
			s.mu.Lock()
			s.cancelFn = nil
			s.mu.Unlock()
		}()

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

		content := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: msg.Text}}
		content = append(content, msg.Images...)
		agentMsg := &agent.Message{
			Role:      agent.RoleUser,
			Content:   content,
			Timestamp: time.Now(),
		}

		if err := s.appendUserMessageToSession(msg); err != nil {
			s.logger.Warn("session write failed", "error", err)
		}

		unsubscribe := s.agent.Subscribe(func(event agent.Event, signal <-chan struct{}) {
			s.handleAgentEvent(events, event)
			// Tool results can push a running turn past the context-window
			// threshold between turns. Checking on message end lets compaction
			// fire mid-turn, and the agent loop rebases onto the replacement
			// summary before the next model call.
			if e, ok := event.(agent.MessageEnd); ok && e.Message != nil && e.Message.Role != agent.RoleUser {
				s.autoCompactIfDue(ctx)
			}
		})
		defer unsubscribe()

		// Auto-compaction check: trigger if context usage exceeds threshold.
		s.autoCompactIfDue(ctx)

		var promptErr error
		if msg.SystemAddendum != "" {
			promptErr = s.agent.Prompt(ctx, agentMsg, agent.WithPromptSystemAddendum(msg.SystemAddendum))
		} else {
			promptErr = s.agent.Prompt(ctx, agentMsg)
		}
		if promptErr != nil {
			s.recordProviderError(promptErr)
			s.emit(events, Event{
				Type:      "error",
				SessionID: s.id,
				Payload: map[string]any{
					"code":        "agent_error",
					"message":     promptErr.Error(),
					"recoverable": false,
				},
			})
		}

		s.emit(events, Event{
			Type:      "session_end",
			SessionID: s.id,
			Payload: map[string]any{
				"id":           s.id,
				"total_tokens": s.totalTokens,
				"cost_usd":     s.calculateCost(),
			},
		})
	}()

	return events, nil
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
	instructions := ""
	if len(customInstructions) > 0 {
		instructions = strings.TrimSpace(customInstructions[0])
	}
	// Fire `session_before_compact` (cancellable) so plugins can observe or
	// block the compaction attempt.
	if s.pluginManager != nil {
		beforePayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              "manual",
			"custom_instructions": instructions,
		})
		if blocked := s.pluginManager.DispatchEvent("session_before_compact", beforePayload, s.id); blocked {
			return fmt.Errorf("compaction blocked by plugin")
		}
	}

	// Fire `compaction_start` (informational) so plugins can observe the
	// moment compaction actually begins.
	if s.pluginManager != nil {
		startPayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              "manual",
			"custom_instructions": instructions,
		})
		s.pluginManager.DispatchEvent("compaction_start", startPayload, s.id)
	}
	// Frontends get the same event out of band. Raised outside the plugin-
	// manager guard: a session with no plugins still has a UI to inform.
	s.notify("compaction_start", map[string]any{
		"session_id":          s.id,
		"reason":              "manual",
		"custom_instructions": instructions,
	})

	// Build a summary from the current agent messages using the model.
	// Retry semantics: when the log's most recent compaction is a local
	// fallback, the live context is that degraded summary — a retry must
	// instead compact the same messages the failed attempt saw.
	state := s.agent.State()
	compactMessages := s.compactionInputMessages(state.Messages)
	var transcript strings.Builder
	for _, msg := range compactMessages {
		role := string(msg.Role)
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockText && block.Text != "" {
				fmt.Fprintf(&transcript, "[%s]: %s\n", role, block.Text)
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
		result := s.pluginManager.DispatchCompactionHook(transcriptStr, tokensBefore, s.id)
		summary = result.Summary
	}
	var summarizeErr error
	if summary == "" {
		summarizationPrompt := compaction.BuildSummarizationPrompt()
		var err error
		summary, err = s.callModelForSummary(ctx, summarizationPrompt, transcriptStr, instructions)
		if err != nil {
			// Keep the session usable with a naive summary, but never silently:
			// the warning and the compaction_end payload both record the failure.
			summarizeErr = err
			s.logger.Warn("compaction model call failed; using naive summary", "error", err)
			summary = s.buildNaiveSummary(compactMessages, instructions)
		}
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
		entry := sessionpkg.NewCompactionEntry(summary, *parentID, tokensBefore, 0, false)
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
	}

	// Replace agent messages with a single summary message to simulate
	// the effect of compaction on future context building.
	summaryMsg := &agent.Message{
		Role:      agent.RoleSystem,
		Content:   []ai.ContentBlock{{Type: ai.ContentBlockText, Text: "Session Summary: " + summary}},
		Timestamp: time.Now(),
	}
	s.agent.ReplaceMessages([]*agent.Message{summaryMsg})

	// Fire `compaction_end` (informational) so plugins can observe the
	// successful compaction outcome.
	if s.pluginManager != nil {
		afterPayload, _ := json.Marshal(map[string]any{
			"session_id":          s.id,
			"reason":              "manual",
			"custom_instructions": instructions,
		})
		s.pluginManager.DispatchEvent("compaction_end", afterPayload, s.id)
	}
	// The frontend copy carries the summary and the token count: §9 has the UI
	// commit a summary message when compaction ends, and it has no other way to
	// learn what the summary was. A model-call failure is flagged so frontends
	// can show that the summary is a local fallback rather than model output.
	endPayload := map[string]any{
		"session_id":          s.id,
		"reason":              "manual",
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
func (s *agentSession) summaryMaxTokens() int {
	s.mu.Lock()
	level := s.thinkingLevel
	s.mu.Unlock()
	if level == "" || level == agent.ThinkingOff || s.model == nil || !s.model.Reasoning {
		return summaryMaxOutputTokens
	}
	maxTokens := summaryMaxOutputTokens + ai.ThinkingBudgetForLevel(ai.ReasoningLevel(level), nil)
	if s.model.MaxTokens > 0 && s.model.MaxTokens < maxTokens {
		return s.model.MaxTokens
	}
	return maxTokens
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
	if len(entries) == 0 || entries[0].Type != sessionpkg.EntryTypeCompaction || !entries[0].Fallback {
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
func (s *agentSession) callModelForSummary(ctx context.Context, systemPrompt, transcript, customInstructions string) (string, error) {
	if s.model == nil {
		return "", fmt.Errorf("no model configured for summarization")
	}

	// Build the request using the AI client factory
	client, err := s.getClient()
	if err != nil {
		return "", fmt.Errorf("get AI client: %w", err)
	}

	instruction := "Generate a concise summary of the following conversation transcript. Preserve key decisions, open questions, files touched, and error context."
	if customInstructions != "" {
		instruction += " Additional focus: " + customInstructions
	}

	prompt := &ai.Context{
		SystemPrompt: systemPrompt,
		Messages: []ai.Message{
			{Role: "user", Content: instruction},
			{Role: "user", Content: trimTranscriptForSummary(transcript, s.model.ContextWindow)},
		},
	}

	opts := append(s.compactionCompletionOptions(ctx), ai.WithMaxTokens(s.summaryMaxTokens()))
	msg, err := client.Complete(ctx, s.model, prompt, opts...)
	if err != nil {
		return "", fmt.Errorf("model completion: %w", err)
	}
	// A length stop means the provider cut the response at the output cap
	// mid-generation. Accepting it persists a truncated fragment as the
	// summary, which then replaces the entire session context. Treat it as a
	// failed summarization so the caller falls back and a retry can succeed.
	if msg.StopReason == ai.StopReasonLength {
		return "", fmt.Errorf("summarization stopped at the output token limit (finish_reason=length); the summary would be truncated")
	}

	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockText {
			b.WriteString(block.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no text content in summarization response")
	}
	return b.String(), nil
}

// trimTranscriptForSummary keeps the most recent transcript characters that
// fit the model's context window (chars/4 estimate, minus the reserve for the
// summary output and prompt overhead). Compaction runs exactly when history
// is huge; an untrimmed transcript gets rejected by the provider and the
// summary silently degrades to the naive fallback.
// ponytail: tail-only trim drops the oldest history; chunked iterative
// summarization (pi-style) is the upgrade path if that loss matters.
func trimTranscriptForSummary(transcript string, contextWindow int) string {
	if contextWindow <= 0 {
		return transcript
	}
	budgetTokens := contextWindow - compaction.DefaultSettings.ReserveTokens
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
func (s *agentSession) compactionCompletionOptions(ctx context.Context) []ai.Option {
	opts := []ai.Option{ai.WithSessionID(s.id)}
	// Mirror buildAIOptions: with no level set, the OpenAI-compat layer sends
	// enable_thinking:false, which always-thinking models (zai glm-5.3) reject
	// (error 1210) — so forward the session's level like a normal prompt does.
	s.mu.Lock()
	level := s.thinkingLevel
	s.mu.Unlock()
	if level != "" && level != agent.ThinkingOff {
		opts = append(opts, ai.WithReasoning(ai.ReasoningLevel(level)))
	}
	if s.model == nil || s.credentialProvider == nil {
		return opts
	}
	providerKey := s.model.Provider
	if providerKey == "" {
		providerKey = s.modelSpec.Provider
	}
	if providerKey == "" {
		return opts
	}
	apiKey, err := s.credentialProvider(ctx, providerKey)
	if err != nil {
		return opts
	}
	if apiKey != "" {
		opts = append(opts, ai.WithAPIKey(apiKey))
	}
	return opts
}

// getClient returns an AI client for the current model.
func (s *agentSession) getClient() (ai.Client, error) {
	if s.model == nil {
		return nil, fmt.Errorf("no model configured")
	}

	if s.clientFactory != nil {
		return s.clientFactory(s.model)
	}

	p, ok := ai.Get(s.model.API)
	if !ok {
		return nil, fmt.Errorf("no provider for API: %s", s.model.API)
	}

	return p.NewClient(s.model)
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
	if s.compactionConfig.TriggerPct <= 0 || s.model == nil {
		return
	}
	cfg := s.compactionConfig
	if t := providerTriggerTokens(s.model.Provider); t > 0 {
		cfg.TriggerTokens = t
	}
	estimatedTokens := s.estimateSessionTokens(s.agent.State())
	if !compaction.ShouldCompact(estimatedTokens, s.model.ContextWindow, cfg) {
		return
	}
	if err := s.Compact(ctx); err != nil {
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
		if msg == nil {
			continue
		}
		compactionMessages = append(compactionMessages, &compaction.AgentMessage{
			Role:       compaction.MessageRole(msg.Role),
			Content:    msg.Content,
			Usage:      msg.Usage,
			StopReason: msg.StopReason,
		})
	}
	return compaction.EstimateContextTokens(compactionMessages).Tokens
}

func (s *agentSession) State() SessionState {
	s.mu.Lock()
	state := s.agent.State()
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

	var position string
	if writerPath != "" {
		if reader, err := sessionpkg.ReadFile(writerPath); err == nil {
			position = reader.CurrentPosition()
		}
	}

	return SessionState{
		id:          s.id,
		model:       modelID,
		cwd:         s.cwd,
		dataDir:     dataDir,
		isStreaming: state.IsStreaming,
		totalTokens: s.totalTokens,
		position:    position,
	}
}

// ID returns the session identifier.
func (s *agentSession) ID() string {
	return s.id
}

func (s *agentSession) Close(_ context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Fire `session_shutdown` on the plugin bus so extensions can observe
	// the session being torn down. The JSON stream counterpart is emitted
	// by the RPC server (or other transport) since Close is channel-agnostic.
	if s.pluginManager != nil {
		shutdownPayload, _ := json.Marshal(map[string]any{
			"session_id": s.id,
		})
		s.pluginManager.DispatchEvent("session_shutdown", shutdownPayload, s.id)
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

	// Cancel any in-flight send
	if s.cancelFn != nil {
		s.cancelFn()
	}

	// Close the session writer and release ownership.
	// This allows other processes to resume the session.
	s.closeWriter()
	if s.runtime != nil {
		s.runtime.releaseOpenSession(s.id)
	}
	s.removeUnsentDataDir()

	return nil
}

func (s *agentSession) removeUnsentDataDir() {
	s.mu.Lock()
	unsent := s.pendingHeader != nil && s.sessionWriter == nil
	dir := s.sessionDataDir
	s.mu.Unlock()
	if !unsent || dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.logger.Warn("session data cleanup failed", "error", err)
	}
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
	m, ok := ai.Models()[model]
	if !ok {
		return fmt.Errorf("session: SetModel: unknown model %q", model)
	}

	s.mu.Lock()
	s.model = m
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
	content := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: msg.Text}}
	content = append(content, msg.Images...)
	return &agent.Message{Role: agent.RoleUser, Content: content, Timestamp: time.Now()}
}

func sdkMessageFromAgent(msg *agent.Message) Message {
	out := Message{
		Role:       string(msg.Role),
		Content:    append([]ai.ContentBlock(nil), msg.Content...),
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		IsError:    msg.IsError,
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

func parseQueueMode(mode string) (agent.QueueMode, error) {
	switch agent.QueueMode(mode) {
	case agent.QueueModeOneAtATime, agent.QueueModeAll:
		return agent.QueueMode(mode), nil
	default:
		return "", fmt.Errorf("invalid queue mode %q", mode)
	}
}

func (s *agentSession) Steer(msg Message) error {
	if len(msg.AttachmentIDs) > 0 {
		blocks, err := s.attachments.ContentBlocks(msg.AttachmentIDs)
		if err != nil {
			return err
		}
		msg.Images = append(msg.Images, blocks...)
	}
	s.agent.Steer(agentMessageFromSDK(msg))
	s.recordQueueUpdate()
	return nil
}

func (s *agentSession) FollowUp(msg Message) error {
	if len(msg.AttachmentIDs) > 0 {
		blocks, err := s.attachments.ContentBlocks(msg.AttachmentIDs)
		if err != nil {
			return err
		}
		msg.Images = append(msg.Images, blocks...)
	}
	s.agent.FollowUp(agentMessageFromSDK(msg))
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

func (s *agentSession) CycleModel() (*ai.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Nothing to cycle through when no model is active yet. Callers
	// (the TUI's model-rotate shortcut) should open the /model picker
	// instead; CycleModel is a no-op in the degraded state.
	if s.model == nil {
		return nil, nil
	}

	models := ai.ListSupportedModels(s.model.Provider)
	if len(models) == 0 {
		models = ai.ListModels(s.model.Provider)
	}
	if len(models) == 0 {
		return nil, nil
	}
	idx := 0
	for i, m := range models {
		if m.ID == s.model.ID {
			idx = (i + 1) % len(models)
			break
		}
	}
	s.model = models[idx]
	s.agent.SetModel(s.model)
	return s.model, nil
}

func (s *agentSession) CycleThinkingLevel() string {
	levels := []agent.ThinkingLevel{agent.ThinkingOff, agent.ThinkingMinimal, agent.ThinkingLow, agent.ThinkingMedium, agent.ThinkingHigh, agent.ThinkingXHigh}
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *agentSession) AbortRetry() {}

func (s *agentSession) GetMessages() []Message {
	state := s.agent.State()
	messages := make([]Message, 0, len(state.Messages))
	for _, msg := range state.Messages {
		messages = append(messages, sdkMessageFromAgent(msg))
	}
	return messages
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

func (s *agentSession) ExportHTML(_ context.Context, path string) (string, error) {
	s.mu.Lock()
	writer := s.sessionWriter
	s.mu.Unlock()
	if writer == nil {
		return "", fmt.Errorf("session is not persisted")
	}
	writerPath := writer.Path()
	reader, err := sessionpkg.ReadFile(writerPath)
	if err != nil {
		return "", err
	}
	data, err := sessionpkg.RenderHTML(reader, sessionpkg.Options{})
	if err != nil {
		return "", err
	}
	if path == "" {
		path = strings.TrimSuffix(writerPath, filepath.Ext(writerPath)) + ".html"
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (s *agentSession) GetLastAssistantText() string { return s.getLastAssistantText() }

func (s *agentSession) GetSessionName() string { return s.getSessionName() }

func (s *agentSession) Stats() SessionStats {
	state := s.State()
	messages := s.GetMessages()
	contextWindow := 0
	if s.model != nil {
		contextWindow = s.model.ContextWindow
	}
	contextTokens := estimateContextTokens(s.agent.State().Messages)
	usedPct := 0.0
	if contextWindow > 0 {
		usedPct = float64(contextTokens) / float64(contextWindow) * 100
	}
	s.mu.Lock()
	thinkingLevel := string(s.thinkingLevel)
	autoCompaction := s.compactionConfig.TriggerPct > 0
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
	}
}

// closeWriter closes the session writer and releases the owner file.
// Callers must NOT hold s.mu; this method takes it to serialize against
// concurrent send/append paths that snapshot the writer pointer.
func (s *agentSession) closeWriter() {
	s.mu.Lock()
	writer := s.sessionWriter
	s.sessionWriter = nil
	s.mu.Unlock()
	if writer != nil {
		_ = writer.Close()
		if err := sessionpkg.ReleaseOwner(writer.Path()); err != nil {
			s.logger.Warn("session owner release failed", "error", err)
		}
	}
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
		return writer, nil
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

	return writer, nil
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
		if msg == nil {
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

// appendMessageToSession writes a message to the session file.
func (s *agentSession) appendMessageToSession(msg *agent.Message) {
	s.mu.Lock()
	writer := s.sessionWriter
	parentID := s.parentMsgID
	s.mu.Unlock()
	if writer == nil {
		return
	}

	// Always marshal the envelope to ensure valid {role, content} structure.
	// Even if content is nil, we need the role field present.
	content, err := marshalSessionMessageEnvelope(msg)
	if err != nil {
		content = []byte{}
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
		s.logger.Warn("session write failed", "error", err)
	}
	s.mu.Lock()
	if s.sessionWriter != nil {
		s.parentMsgID = &entry.ID
	}
	s.mu.Unlock()
}

func marshalSessionMessageEnvelope(msg *agent.Message) ([]byte, error) {
	envelope := map[string]any{
		"role":    msg.Role,
		"content": msg.Content,
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

	return json.Marshal(envelope)
}

// appendUserMessageToSession writes a user message to the session file.
func (s *agentSession) appendUserMessageToSession(msg Message) error {
	writer, err := s.ensureSessionWriter()
	s.mu.Lock()
	parentID := s.parentMsgID
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("ensure session writer: %w", err)
	}
	if writer == nil {
		return nil
	}

	isFirstMessage := parentID == nil

	content := []ai.ContentBlock{{Type: ai.ContentBlockText, Text: msg.Text}}
	content = append(content, msg.Images...)

	// Wrap content in {role, content} envelope to match session reader/exporter contract.
	envelope := map[string]any{
		"role":    agent.RoleUser,
		"content": content,
	}

	rawContent, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal user message content: %w", err)
	}

	entry := sessionpkg.NewMessageEntry(parentID, rawContent, nil)
	entry.ID = sessionpkg.NewID()
	if err := writer.Append(entry); err != nil {
		return fmt.Errorf("append user message: %w", err)
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

	return nil
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
		provider = s.model.API
	}

	entry := sessionpkg.UsageEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Session:      s.id,
		Provider:     provider,
		Model:        model,
		InputTokens:  usage.Input,
		OutputTokens: usage.Output,
		CostUSD:      0.0, // Cost calculation deferred - would need provider-specific pricing
	}

	if err := s.usageLogger.Log(entry); err != nil {
		s.logger.Warn("usage log write failed", "error", err)
	}
}

// calculateCost returns an estimate of the session cost based on accumulated usage.
// This is a simplified calculation - real cost would need provider-specific pricing.
func (s *agentSession) calculateCost() float64 {
	// Simple estimate using average pricing
	// In production, this would use provider-specific pricing from settings
	if s.model == nil {
		return 0.0
	}

	// Get usage from the agent's message history
	state := s.agent.State()
	var inputTokens, outputTokens int
	for _, msg := range state.Messages {
		inputTokens += msg.Usage.Input
		outputTokens += msg.Usage.Output
	}

	// Rough cost estimate (would be provider/model specific in production)
	// Using approximate Claude pricing as a placeholder
	estimatedCost := float64(inputTokens)*0.000003 + float64(outputTokens)*0.000015
	return estimatedCost
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
	return usage.TotalTokens > 0 || usage.Input > 0 || usage.Output > 0 || usage.CacheRead > 0 || usage.CacheWrite > 0
}

func normalizeSessionUsage(usage ai.Usage) ai.Usage {
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	}
	if usage.Cost.Total == 0 {
		usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	}
	return usage
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
		if e.Message != nil {
			msgID := s.getCurrentMessageID()
			var tokens int
			var usage map[string]any
			if hasUsage(e.Message.Usage) {
				usageValue := normalizeSessionUsage(e.Message.Usage)
				tokens = usageValue.TotalTokens
				usage = usagePayload(usageValue)

				// Update total tokens for session
				s.mu.Lock()
				s.totalTokens += usageValue.TotalTokens
				s.mu.Unlock()

				// Log usage to the usage log
				s.logUsage(usageValue, e.Message.Model)
			}
			s.emit(ch, Event{
				Type:      "message_end",
				SessionID: s.id,
				Payload: map[string]any{
					"message_id":    msgID,
					"role":          string(e.Message.Role),
					"content":       agentMessageText(e.Message),
					"tokens":        tokens,
					"finish_reason": string(e.Message.StopReason),
					"error_message": e.Message.ErrorMessage,
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

			// User prompts are persisted before entering the agent loop so the
			// session file records them even if provider setup fails. The loop also
			// emits message_end for those prompts; do not append them a second time.
			if e.Message.Role != agent.RoleUser {
				s.appendMessageToSession(e.Message)
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
		if e.PartialResult != nil {
			chunk = e.PartialResult.Content
		}
		s.emit(ch, Event{
			Type:      "tool_progress",
			SessionID: s.id,
			Payload: map[string]any{
				"tool_call_id": e.ToolCallID,
				"chunk":        chunk,
			},
		})
	case agent.ToolEnd:
		output := ""
		isErr := false
		details := any(nil)
		if e.Result != nil {
			output = e.Result.Content
			isErr = e.IsError
			details = e.Result.Details
		}
		s.emit(ch, Event{
			Type:      "tool_result",
			SessionID: s.id,
			Payload: map[string]any{
				"tool_call_id": e.ToolCallID,
				"output":       output,
				"is_error":     isErr,
				"details":      details,
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
	ui              ui.UI
	sessionsDir     string
	credStore       store.CredentialStore
	settings        *ai.Settings
	resolver        *ai.ModelResolver
	explicitModel   string
	pluginManager   *plugins.Manager
	commandRegistry commands.Registry
	// artifactLoader provides skills and context for session injection.
	artifactLoader ArtifactLoader
	// telemetry controls whether telemetry is reported.
	telemetry *bool
	// activeModel is the resolved model from settings/defaults/env.
	// It is nil if no model is configured (session creation will fail).
	activeModel *ai.Model
	// activeModelSpec is the resolved model spec for display/debugging.
	activeModelSpec ai.ModelSpec
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
	u := opts.UI
	if u == nil {
		u = ui.NewNoOpUI()
	}
	sessionsDir := opts.SessionsDir

	// Load settings with sensible defaults if not provided.
	// This matches the CLI behavior: load user settings (<configDir>/settings.json),
	// merge project settings (.tau/settings.json) with project taking precedence.
	settings := opts.Settings
	if settings == nil {
		userSettingsPath := filepath.Join(paths.ConfigDir(), "settings.json")
		userSettings, err := ai.LoadSettings(userSettingsPath)
		if err == nil && userSettings != nil {
			// Also try to load project settings from .tau/settings.json
			projectSettings, _ := ai.LoadSettings(".tau/settings.json")
			settings = ai.MergeSettings(userSettings, projectSettings)
		}
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
		if err := fileBackend.EnsureDir(); err == nil {
			credStore = fileBackend
		}
	}

	r := &runtime{
		ui:               u,
		sessionsDir:      sessionsDir,
		credStore:        credStore,
		settings:         settings,
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
		preloadSkills:    opts.PreloadSkills,
		logger:           logging.OrDiscard(opts.Logger),
		openSessions:     make(map[string]struct{}),
	}

	// Build compaction config from settings and runtime options.
	r.compactionConfig = r.buildCompactionConfig(opts.Compaction)

	// Register default providers (but defer model resolution to NewSession)
	r.setupDefaultsForRuntime()

	// Initialize model resolver if settings available
	if r.settings != nil {
		r.resolver = ai.NewModelResolver(r.settings)
	}

	return r, nil
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
func (r *runtime) setupDefaults() error {
	ai.Init() // Register default providers

	resolved, err := r.ResolveModel()
	if err != nil {
		if errors.Is(err, ErrNoModelConfigured) {
			r.activeModel = nil
			r.activeModelSpec = ai.ModelSpec{}
			return nil
		}
		return err
	}
	r.activeModel = resolved.Model
	r.activeModelSpec = resolved.Spec
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
func (r *runtime) buildCompactionConfig(opts *CompactionSettings) compaction.Config {
	cfg := compaction.DefaultConfig()

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
}

// ErrNoModelConfigured is returned by ResolveModel when no model can be
// resolved through the explicit / settings / env chain. Callers that build
// a session should treat this as recoverable: the user can pick a model
// interactively (e.g. the TUI's `/model` slash command) before sending.
var ErrNoModelConfigured = errors.New("no model configured: use /model to pick a provider and model")

// ResolveModel resolves the active model through the chain:
// 1. Explicit override (CLI or SDK option)
// 2. Settings default (user + project merged)
// 3. Env-var fallback (ANTHROPIC_API_KEY or OPENAI_API_KEY)
// 4. ErrNoModelConfigured if no model can be resolved
func (r *runtime) ResolveModel() (*ResolvedModel, error) {
	var modelID, providerKey string

	// 1. Explicit override
	if r.explicitModel != "" {
		spec, err := ai.ParseModelSpec(r.explicitModel)
		if err != nil {
			return nil, fmt.Errorf("parse explicit model %q: %w", r.explicitModel, err)
		}
		modelID = spec.Model
		providerKey = spec.Provider
	} else if r.resolver != nil {
		// 2. Settings default (resolve through ModelResolver)
		spec := r.resolver.ResolveDefaultModel()
		if spec.Provider != "" && spec.Model != "" {
			if providerKey == "" {
				providerKey = spec.Provider
			}
			modelID = spec.Model
		}
	}

	// 3. Env-var fallback (skip if IgnoreEnv is set)
	if modelID == "" && !r.noEnvCredentials {
		// Check ANTHROPIC_API_KEY
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
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
	model, err := ai.ResolveModelID(providerKey + "/" + modelID)
	if err != nil {
		return nil, fmt.Errorf("resolve model %s/%s: %w", providerKey, modelID, err)
	}

	// Get the API type for client creation
	api := model.API

	// Get provider for client factory
	p, ok := ai.Get(api)
	if !ok {
		return nil, fmt.Errorf("provider %q not registered for API %q", providerKey, api)
	}

	// Verify provider implements the correct API
	if p.API() != api {
		return nil, fmt.Errorf("provider %q API mismatch: expected %q, got %q", providerKey, api, p.API())
	}

	return &ResolvedModel{
		Model: model,
		Spec:  ai.ModelSpec{Provider: providerKey, Model: modelID},
	}, nil
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
		if r.resolver != nil {
			spec := r.resolver.ResolveDefaultModel()
			if spec.Provider != "" && spec.Model != "" {
				if providerKey == "" {
					providerKey = spec.Provider
				}
				modelID = spec.Model
			}
		}
		// Env fallback (skip if IgnoreEnv is set)
		if modelID == "" && !r.noEnvCredentials {
			if os.Getenv("ANTHROPIC_API_KEY") != "" {
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
	model, err := ai.ResolveModelID(providerKey + "/" + modelID)
	if err != nil {
		return nil, fmt.Errorf("resolve model %s/%s: %w", providerKey, modelID, err)
	}

	api := model.API

	p, ok := ai.Get(api)
	if !ok {
		return nil, fmt.Errorf("provider %q not registered for API %q", providerKey, api)
	}

	if p.API() != api {
		return nil, fmt.Errorf("provider %q API mismatch: expected %q, got %q", providerKey, api, p.API())
	}

	return &ResolvedModel{
		Model: model,
		Spec:  ai.ModelSpec{Provider: providerKey, Model: modelID},
	}, nil
}

func (r *runtime) restoreSessionModelAndThinking(opts SessionOptions) (modelSpec string, thinkingLevel string) {
	if opts.SessionID == "" || opts.Model != "" || r.sessionsDir == "" {
		return "", ""
	}
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return "", ""
	}
	reader, err := storage.Open(opts.SessionID)
	if err != nil {
		return "", ""
	}
	for _, entry := range reader.Entries() {
		switch entry.Type {
		case sessionpkg.EntryTypeModelChange:
			provider, model := stringPtrValue(entry.Provider), stringPtrValue(entry.Model)
			if provider != "" && model != "" {
				modelSpec = ai.ModelSpec{Provider: provider, Model: model}.String()
			}
		case sessionpkg.EntryTypeThinkingChange:
			if level := stringPtrValue(entry.Level); level != "" {
				thinkingLevel = level
			}
		}
	}
	return modelSpec, thinkingLevel
}

func (r *runtime) restoredModelCredentialAvailable(ctx context.Context, providerKey string, credStore store.CredentialStore) bool {
	if providerKey == "" || r.apiKey != "" {
		return true
	}
	if credStore != nil {
		cred, err := credStore.Get(ctx, providerKey)
		if err == nil && !cred.NeedsReauth {
			apiKey, keyErr := store.APIKeyFromCredential(providerKey, cred)
			if keyErr == nil && apiKey != "" {
				return true
			}
		}
	}
	return !r.noEnvCredentials && ai.GetEnvAPIKey(providerKey) != ""
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
	restoredModelSpec, restoredThinkingLevel := r.restoreSessionModelAndThinking(opts)

	// Honor SessionOptions.Model override if provided.
	// This allows per-session model selection without modifying runtime defaults.
	var activeModel *ai.Model
	var activeModelSpec ai.ModelSpec

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

	if activeModelSpec.Provider != "" && restoredModelSpec != "" && opts.Model == "" && !r.restoredModelCredentialAvailable(ctx, activeModelSpec.Provider, credStore) {
		activeModel = nil
	}
	if activeModel == nil {
		// Ensure model is resolved from runtime defaults
		if r.activeModel == nil {
			if err := r.setupDefaults(); err != nil {
				// Surface real errors; ignore the "no model configured" sentinel
				// so the TUI can boot and prompt the user to /model.
				if !errors.Is(err, ErrNoModelConfigured) {
					return nil, fmt.Errorf("resolve active model: %w", err)
				}
			}
		}
		activeModel = r.activeModel
		activeModelSpec = r.activeModelSpec
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
	// Uses a background context for credential reads to ensure refresh-on-read
	// operations are not canceled by the session/request context.
	sessionModel := activeModel
	sessionProviderKey := activeModelSpec.Provider
	// RuntimeOptions.Provider pins the credential provider when the model
	// specifier didn't already pin one. Honored only when the active model
	// is provider-agnostic.
	if sessionProviderKey == "" && r.provider != "" {
		sessionProviderKey = r.provider
	}

	credentialProvider := func(ctx context.Context, provider string) (string, error) {
		providerKey := provider
		if providerKey == "" {
			providerKey = sessionProviderKey
		}
		if providerKey == "" {
			return "", nil
		}

		// Precedence: CLI --api-key (r.apiKey) > credential store > env var.
		if r.apiKey != "" {
			return r.apiKey, nil
		}

		if credStore != nil {
			cred, err := credStore.Get(ctx, providerKey)
			if err != nil && !errors.Is(err, ai.ErrCredentialNotFound) {
				return "", fmt.Errorf("get credentials for %s: %w", providerKey, err)
			}
			if err == nil {
				if cred.NeedsReauth {
					return "", ai.ErrCredentialNeedsReauth{Provider: providerKey}
				}
				apiKey, err := store.APIKeyFromCredential(providerKey, cred)
				if err != nil {
					return "", err
				}
				if apiKey != "" {
					return apiKey, nil
				}
			}
		}

		if !r.noEnvCredentials {
			if envAPIKey := ai.GetEnvAPIKey(providerKey); envAPIKey != "" {
				envOverrideLoggedOncePtr.Do(func() {
					sessionLogger.Info("NOTICE: using API key from environment", "provider", providerKey)
				})
				return envAPIKey, nil
			}
		}

		return "", nil
	}

	clientFactory := func(m *ai.Model) (ai.Client, error) {
		if m == nil {
			return nil, fmt.Errorf("%w", ErrNoModelConfigured)
		}
		// Get provider for this model's API
		p, ok := ai.Get(m.API)
		if !ok {
			return nil, fmt.Errorf("no provider for API type: %s", m.API)
		}

		// Build client with credentials
		client, err := p.NewClient(m)
		if err != nil {
			return nil, err
		}

		// Some legacy clients accept a key only at construction time. Most calls now
		// receive ai.WithAPIKey from agent.WithAPIKeyProvider below, but keep this
		// setter path for clients that still consult local client state.
		providerKey := m.Provider
		if providerKey == "" {
			providerKey = sessionProviderKey
		}
		if apiKey, err := credentialProvider(context.Background(), providerKey); err == nil && apiKey != "" {
			if sc, ok := client.(APIKeySetter); ok {
				sc.SetAPIKey(apiKey)
			}
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

	agentCfg := agent.NewConfig(
		agent.WithModel(sessionModel),
		agent.WithClientFactory(clientFactory),
		agent.WithAPIKeyProvider(credentialProvider),
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
	if sessionThinkingLevel == "" {
		// Per-model override from settings, then the global defaultThinkingLevel.
		// Only fresh sessions reach here: opts/explicit/CLI overrides won above,
		// and resumes keep the level recorded in the session file.
		sessionThinkingLevel = r.settings.ThinkingLevelFor(activeModelSpec.String())
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
		toolCfg := buildToolConfig(r.settings)
		agentCfg.Tools = append(agentCfg.Tools, defaultCodingAgentTools(toolCfg)...)
	}

	loader := r.artifactLoader
	if loader == nil {
		loader = GlobalArtifactLoader
	}

	var addendumParts []string
	if !r.noContextFiles {
		if contextAddendum, err := buildContextFileAddendum(cwd); err == nil && contextAddendum != "" {
			addendumParts = append(addendumParts, contextAddendum)
		}
	}
	if loader != nil {
		if reg := loader.SkillRegistry(); reg != nil {
			if catalog := artifacts.FormatSkillsCatalog(reg.List()); catalog != "" {
				addendumParts = append(addendumParts, catalog)
			}
		}
	}

	// Inject preloaded skills (ID-based, not path-based).
	// PreloadSkills are resolved from the registry after discovery is complete.
	if loader != nil && len(r.preloadSkills) > 0 {
		if reg := loader.SkillRegistry(); reg != nil {
			for _, skillID := range r.preloadSkills {
				if skill, found := reg.Get(skillID); found {
					// Format as <skill> block per spec, with references note and body.
					skillBlock := fmt.Sprintf("<skill name=%q location=%q>\nReferences are relative to %s.\n\n%s</skill>",
						skill.ID, skill.SourcePath, skill.BaseDir, skill.Body)
					addendumParts = append(addendumParts, skillBlock)
				}
			}
		}
	}

	if len(addendumParts) > 0 {
		agentCfg.SystemAddendum = strings.Join(addendumParts, "\n\n")
	}

	// SYSTEM.md overrides the system prompt; APPEND_SYSTEM.md is appended.
	// Both lookups respect <cwd>/.tau first then the user config dir.
	if sys, sErr := loadSystemPrompt(paths.ConfigDir(), cwd); sErr == nil && sys != "" {
		agentCfg.SystemPrompt = sys
	}
	if appendSys, aErr := loadAppendSystemPrompt(paths.ConfigDir(), cwd); aErr == nil && appendSys != "" {
		if agentCfg.SystemAddendum != "" {
			agentCfg.SystemAddendum += "\n\n"
		}
		agentCfg.SystemAddendum += appendSys
	}

	// SessionOptions.SystemPrompt is the explicit CLI override and wins over
	// every file-loaded default. AppendSystemPrompts stack after the resolved
	// prompt in order; empty entries are skipped.
	if opts.SystemPrompt != "" {
		agentCfg.SystemPrompt = opts.SystemPrompt
	}
	for _, p := range opts.AppendSystemPrompts {
		if p == "" {
			continue
		}
		if agentCfg.SystemAddendum != "" {
			agentCfg.SystemAddendum += "\n\n"
		}
		agentCfg.SystemAddendum += p
	}

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

	// Wire plugin manager as EventDispatcher if available.
	// This dispatches agent events to all subscribed plugins per the extension spec.
	if r.pluginManager != nil {
		mgr := r.pluginManager
		agentCfg.EventDispatcher = func(name string, payload []byte, sessID string) agent.EventDispatchResult {
			responses := mgr.DispatchEventCollect(name, json.RawMessage(payload), sessID)
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
				tool := agent.Tool{
					Name:        pt.Name,
					Description: pt.Description,
					Parameters:  pt.Parameters,
					Execute: func(ctx agent.ToolContext) (*agent.ToolResult, error) {
						progressCh := make(chan plugins.ToolProgressEvent, 16)
						done := make(chan struct{})
						mgr.RegisterToolProgressSubscriber(ctx.ToolCallID, progressCh)
						defer func() {
							mgr.UnregisterToolProgressSubscriber(ctx.ToolCallID, progressCh)
							close(done)
						}()

						go func() {
							for {
								select {
								case event := <-progressCh:
									if ctx.OnUpdate != nil {
										ctx.OnUpdate(&agent.ToolResult{
											Content: event.Content,
											IsError: event.IsError,
										})
									}
								case <-done:
									return
								}
							}
						}()

						// Serialize arguments to JSON for the plugin handler
						argsJSON, err := json.Marshal(ctx.Args)
						if err != nil {
							return &agent.ToolResult{Content: fmt.Sprintf("error marshaling args: %v", err), IsError: true}, nil
						}
						result, isErr, err := mgr.ExecutePluginTool(ctx.Context, pt.Name, ctx.ToolCallID, string(argsJSON))
						if err != nil {
							return &agent.ToolResult{Content: fmt.Sprintf("plugin tool error: %v", err), IsError: true}, nil
						}
						return &agent.ToolResult{Content: result, IsError: isErr}, nil
					},
				}
				agentCfg.Tools = append(agentCfg.Tools, tool)
			}
		}
	}

	// Apply tool allow/deny filters (CLI --tools/-t and --exclude-tools/-xt).
	// Deny wins on conflict; an allowlist with no matches yields zero tools.
	if len(opts.ToolAllowlist) > 0 || len(opts.ToolDenylist) > 0 {
		agentCfg.Tools = filterToolsByName(agentCfg.Tools, opts.ToolAllowlist, opts.ToolDenylist)
	}

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
	if opts.SessionID != "" && r.sessionsDir != "" {
		storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
		if err == nil {
			reader, err := storage.Open(opts.SessionID)
			if err == nil {
				loadedTotalTokens = reader.TotalTokens().Total()
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
			// Fork creates a new session with parent reference
			sessionWriter, err = storage.Fork(opts.ForkFrom, atEntry)
			if err != nil {
				// Fork failed - continue without session writing
				sessionWriter = nil
			} else {
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
			if err == nil && len(history) > 0 {
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
		artifactLoader:              loader,
		sessionStartReason:          sessionStartReason,
		previousSessionFile:         previousSessionFile,
		thinkingLevel:               agent.ThinkingLevel(sessionThinkingLevel),
		systemPromptOverride:        opts.SystemPrompt,
		appendSystemPrompts:         opts.AppendSystemPrompts,
		toolAllowlist:               opts.ToolAllowlist,
		toolDenylist:                opts.ToolDenylist,
		approveProjectFiles:         opts.ApproveProjectFiles,
		sessionFileBeforeRename:     newSessionFilePath,
		pendingHeader:               pendingHeader,
		pendingSessionDir:           pendingSessionDir,
		sessionDataDir:              sessionDataDir,
		attachments:                 NewAttachmentStore(filepath.Join(attachmentDir, "attachments")),
		pendingInitialModelProvider: activeModelSpec.Provider,
		pendingInitialModel:         activeModelSpec.Model,
		pendingInitialThinking:      sessionThinkingLevel,
		compactionConfig:            r.compactionConfig,
		autoCompactionDefault:       r.compactionConfig,
		autoRetryEnabled:            true,
		totalTokens:                 loadedTotalTokens,
		isPersistedFunc:             opts.IsPersisted,
		logger:                      sessionLogger,
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
		r.pluginManager.DispatchEventCollect("session_start", startPayload, sessionID)
	}

	return s, nil
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
	allowSet := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		allowSet[name] = struct{}{}
	}
	out := make([]agent.Tool, 0, len(tools))
	for _, t := range tools {
		if _, blocked := denySet[t.Name]; blocked {
			continue
		}
		if len(allowSet) > 0 {
			if _, ok := allowSet[t.Name]; !ok {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func defaultCodingAgentTools(toolCfg agent.ToolConfig) []agent.Tool {
	all := agent.BuiltInToolsWithConfig(toolCfg)
	byName := make(map[string]agent.Tool, len(all))
	for _, tool := range all {
		byName[tool.Name] = tool
	}
	tools := make([]agent.Tool, 0, 4)
	for _, name := range []string{"read", "bash", "edit", "write"} {
		if tool, ok := byName[name]; ok {
			tools = append(tools, tool)
		}
	}
	return tools
}

func buildContextFileAddendum(cwd string) (string, error) {
	root, err := findContextWalkRoot(cwd)
	if err != nil {
		return "", err
	}

	var dirs []string
	for dir := cwd; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	slices.Reverse(dirs)

	var paths []string
	for _, dir := range dirs {
		// Per Pi spec: AGENTS.md wins over CLAUDE.md within each directory.
		agentsPath := filepath.Join(dir, "AGENTS.md")
		if _, err := os.Stat(agentsPath); err == nil {
			paths = append(paths, agentsPath)
			continue
		}
		claudePath := filepath.Join(dir, "CLAUDE.md")
		if _, err := os.Stat(claudePath); err == nil {
			paths = append(paths, claudePath)
		}
	}
	if len(paths) == 0 {
		return "", nil
	}

	var sections []string
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read context file %s: %w", path, err)
		}
		sections = append(sections, fmt.Sprintf("## Context: %s\n\n%s", path, strings.TrimSpace(string(data))))
	}

	return strings.Join(sections, "\n\n"), nil
}

// loadSystemPrompt reads SYSTEM.md from <cwd>/.tau first, then
// ~/.tau/agent. First-wins. The path used is reported alongside the body
// so callers can show provenance.
func loadSystemPrompt(homeDir, cwd string) (body string, err error) {
	if cwd == "" {
		cwd = "."
	}
	candidates := []string{
		filepath.Join(cwd, ".tau", "SYSTEM.md"),
		filepath.Join(homeDir, "SYSTEM.md"),
	}
	for _, path := range candidates {
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			return string(data), nil
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
	candidates := []string{
		filepath.Join(cwd, ".tau", "APPEND_SYSTEM.md"),
		filepath.Join(homeDir, "APPEND_SYSTEM.md"),
	}
	for _, path := range candidates {
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			return string(data), nil
		}
		if !os.IsNotExist(readErr) {
			return "", fmt.Errorf("read %s: %w", path, readErr)
		}
	}
	return "", nil
}

func findContextWalkRoot(cwd string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}

	for dir := absCWD; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if info.IsDir() || info.Mode().IsRegular() {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, nil
		}
	}
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
		branchEntries := entriesTo(sessionEntries(reader), atEntry)
		if len(branchEntries) == 0 {
			return nil, fmt.Errorf("fork entry %q not found in session %q", atEntry, reader.Header().ID)
		}
		for _, entry := range branchEntries {
			if !entry.Type.InContext() {
				continue
			}
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
			Role:         agent.MessageRole(sm.Role),
			Content:      sm.Content,
			ToolCallID:   sm.ToolCallID,
			ToolName:     sm.ToolName,
			IsError:      sm.IsError,
			Provider:     sm.Provider,
			API:          sm.API,
			Model:        sm.Model,
			StopReason:   ai.StopReason(sm.StopReason),
			ErrorMessage: sm.ErrorMessage,
			ResponseID:   sm.ResponseID,
			Timestamp:    sm.Timestamp,
		})
	}
	return messages
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

// Close releases runtime resources.
func (r *runtime) Close(_ context.Context) error {
	if r.pluginManager != nil {
		return r.pluginManager.Shutdown(context.Background())
	}
	return nil
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

func (r *runtime) PinSession(_ context.Context, id string) error {
	storage, err := sessionpkg.NewStorageAt(r.sessionsDir)
	if err != nil {
		return fmt.Errorf("open session storage: %w", err)
	}
	return storage.SetPinned(id, true)
}

func (r *runtime) UnpinSession(_ context.Context, id string) error {
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
	return r.setupDefaults()
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

// ShareViewerURL returns the share viewer URL override from TAU_SHARE_VIEWER_URL.
// Returns empty string if not set (defaults to https://pi.dev/session/).
func (r *runtime) ShareViewerURL() string {
	return ai.ShareViewerURL()
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
	if settings.Bash.Shell != "" {
		cfg.BashShell = settings.Bash.Shell
	}
	if settings.Bash.OutputCap > 0 {
		cfg.BashOutputCap = settings.Bash.OutputCap
	}
	if len(settings.Bash.EnvAllowlist) > 0 {
		cfg.BashEnvAllowlist = settings.Bash.EnvAllowlist
	}
	if len(settings.Bash.EnvExtra) > 0 {
		cfg.BashEnvExtra = settings.Bash.EnvExtra
	}
	if settings.Bash.CommandPrefix != "" {
		cfg.CommandPrefix = settings.Bash.CommandPrefix
	}
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
