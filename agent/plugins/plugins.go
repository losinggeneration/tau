// Package plugins implements the extension/plugin system for tau using hashicorp/go-plugin.
// Plugins are subprocesses that communicate with tau over gRPC.
package plugins // import "github.com/losinggeneration/tau/agent/plugins"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/commands"
	"github.com/losinggeneration/tau/logging"
	"github.com/losinggeneration/tau/piext"
	"github.com/losinggeneration/tau/ui"
)

// Version is the current tau version.
const Version = "0.1.0"

// SupportedAPIVersions contains the set of API versions tau supports.
var SupportedAPIVersions = []string{"1"}

// DefaultDrainTimeout is the default drain timeout for plugin shutdown.
const DefaultDrainTimeout = 5 * time.Second

// DefaultFailureConfig holds the default failure handling configuration.
var DefaultFailureConfig = FailureConfig{
	MaxAttempts:   3,
	BackoffBase:   1 * time.Second,
	BackoffFactor: 2,
}

// FailureConfig controls retry behavior on plugin failure.
type FailureConfig struct {
	MaxAttempts   int           // number of retry attempts (default 3)
	BackoffBase   time.Duration // initial backoff duration (default 1s)
	BackoffFactor float64       // backoff multiplier (default 2)
}

// DefaultRPCConfig holds default RPC timeout configurations.
type DefaultRPCConfig struct {
	Initialize  time.Duration
	OnEvent     time.Duration
	ExecuteTool time.Duration
	ExecuteCmd  time.Duration
}

// DefaultRPCTimeouts are the default RPC timeouts.
var DefaultRPCTimeouts = DefaultRPCConfig{
	Initialize:  10 * time.Second,
	OnEvent:     5 * time.Second,
	ExecuteTool: 30 * time.Second,
	ExecuteCmd:  30 * time.Second,
}

// ToolRegistration represents a tool registered by a plugin.
type ToolRegistration struct {
	HasReachesTools     bool
	Namespace           json.RawMessage
	DefaultActive       *bool
	Exposure            string
	OutputSchema        json.RawMessage
	ConstrainedSampling json.RawMessage
	Name                string
	Description         string
	Parameters          map[string]string // JSON schema as string map
}

// CommandRegistration represents a command registered by a plugin.
type CommandRegistration struct {
	Name         string
	Description  string
	ArgumentHint string
}

// CLIFlag represents a CLI flag registered by a plugin.
type CLIFlag struct {
	Name         string
	Description  string
	Required     bool
	DefaultValue string
}

// Registrations holds all the items a plugin registers.
type Registrations struct {
	Tools              []ToolRegistration
	Commands           []CommandRegistration
	EventSubscriptions []string
	CLIFlags           []CLIFlag
	ToolPrefix         string
	VirtualModels      []VirtualModelRegistration
}

// VirtualModelRegistration is a catalog model the plugin routes per request.
type VirtualModelRegistration struct {
	Provider       string
	ID             string
	Name           string
	ThinkingLevels []string
	Input          []ai.InputType
	ContextWindow  int
	MaxTokens      int
}

// PluginInfo holds information about a loaded plugin.
type PluginInfo struct {
	configSnapshot json.RawMessage
	configCaptured bool
	Name           string
	Path           string
	ManifestPath   string
	Manifest       *Manifest // parsed tau-package.toml, if found
	Hash           string
	APIVersion     string
	Registrations  Registrations
	Cleanup        func()

	// grpcClient is the gRPC client for calling Shutdown.
	grpcClient proto.PluginClient

	// failureCount tracks consecutive load failures (reset on success).
	failureCount int

	// deregistered is true when retries are exhausted and plugin was removed.
	deregistered bool
}

// Manager manages plugin lifecycle and communication.
type Manager struct {
	mu sync.RWMutex

	// Loaded plugins keyed by name.
	plugins map[string]*PluginInfo

	// Plugin binaries and their hashes.
	binaries map[string]string // path -> hash

	// manifestPaths maps binary paths to their manifest paths (if any).
	manifestPaths    map[string]string // path -> manifestPath
	packageBinaries  map[string]bool
	packageManifests map[string]*Manifest

	// Configuration.
	configDir string

	// Host service implementation.
	host *HostService

	// Event dispatch.
	eventHandlers map[string][]EventHandler

	// Tool handlers.
	toolHandlers map[string]ToolHandler

	// Command handlers.
	commandHandlers map[string]CommandHandler

	// RPC timeouts per plugin (can be customized per manifest).
	rpcTimeouts map[string]DefaultRPCConfig

	// Plugin state directories.
	stateDir string

	// UI interface for headless routing.
	ui UI
	// uiStatus and uiWidgets hold the latest value a plugin pushed for each
	// status/widget key. They exist because the UI is attached late: plugins
	// load (and receive session_start) before the interactive TUI exists, so
	// a plugin that renders an indicator from an early event would push it
	// into a placeholder no-op UI and lose it permanently. Status and widgets
	// are keyed state rather than transient events, so SetUI replays the
	// current values into a newly attached UI. Both are guarded by mu.
	uiStatus  []keyedStatus
	uiWidgets []keyedWidget

	// failureConfig holds retry configuration per plugin path.
	failureConfig map[string]FailureConfig

	// sessionIDProvider returns the current session ID (may be nil).
	sessionIDProvider func() string

	// sessionAccessor handles session read/write for Host service RPCs.
	sessionAccessor SessionAccessor
	toolDiscovery   func(context.Context, *proto.ToolDiscoveryRequest) (json.RawMessage, error)

	// commandRegistry is the command registry for plugin commands.
	commandRegistry *commands.InMemoryRegistry

	// settings holds the merged plugin configuration.
	settings *ai.Settings

	// gRPC server for the Host service (served to plugins).
	grpcServer *grpc.Server

	// CLI flags holds resolved CLI flag values per plugin.
	// These are injected into the config namespace so Host.GetConfig returns them.
	cliFlags         map[string]map[string]string // pluginName -> key -> value
	cliFlagOverrides map[string]map[string]string

	progressMu          sync.RWMutex
	progressSubscribers map[string][]chan<- ToolProgressEvent

	// hostMu guards hostSockets. Kept separate from mu so socket setup
	// (which involves net.Listen and file IO) does not need m.mu and can
	// run while m.mu is held by the plugin loader.
	hostMu      sync.Mutex
	hostSockets map[string]*hostSocket

	// cleanups tracks plugin cleanup hooks so Shutdown can join them.
	cleanupMu    sync.Mutex
	cleanupErr   error
	cleanups     sync.WaitGroup
	cleanupGrace time.Duration

	// sessionCWDProvider returns the current session CWD (may be nil).
	sessionCWDProvider func() string

	// messageSender routes plugin sendUserMessage calls to the active
	// session's Send/Steer/FollowUp methods. May be nil.
	// deliverAs: "" -> Send (normal); "steer" -> Steer; "followUp" -> FollowUp.
	messageSender              func(text string, deliverAs string) error
	customMessageSender        func(context.Context, json.RawMessage, *bool, string) error
	sessionCustomMessageSender func(context.Context, string, json.RawMessage, *bool, string) error

	// compactor routes plugin SessionCompact calls to the active session's
	// Compact. May be nil.
	compactor func(ctx context.Context, instructions string) error

	// loadFn is the function used by AttemptLoadWithRetry to load a plugin.
	// Defaults to m.Load. Tests can override to avoid launching subprocesses.
	loadFn        func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error)
	connectPlugin PluginConnector

	// extensionPaths are additional paths to load extensions from (via --extension CLI flag).
	extensionPaths []string

	// pendingProviderRegistrations are queued provider registrations for bind-time flush.
	pendingProviderRegistrations []ProviderRegistration

	// activeProviders holds committed provider registrations.
	activeProviders map[string]ProviderConfig

	// toolRefreshCallbacks are called after tool registry refresh.
	toolRefreshCallbacks []ToolRefreshCallback

	// logger receives plugin-system diagnostics. Defaults to discard so library
	// use never writes to stdout/stderr unless the application opts in.
	logger *slog.Logger

	shimSetup     *shimSetup
	shimClosed    bool
	shimInstallFn piext.InstallFn
	shimStatusFn  piext.StatusFn
}

// ToolProgressEvent represents a tool progress update from a plugin.
type ToolProgressEvent struct {
	NestedCalls []ai.NestedToolCall
	ToolCallID  string
	Content     string
	IsError     bool
}

// ManagerOption configures a plugin Manager.
type ManagerOption func(*Manager)

// PluginConnector opens a plugin connection and returns nonnil, idempotent cleanup.
// On error, the connector must release its own resources.
// Initialization and registration remain the manager's responsibility.
type PluginConnector func(ctx context.Context, path string, manifest *Manifest, manifestPath, hostSocket string) (proto.PluginClient, func(), error)

// WithPluginConnector replaces subprocess startup, allowing in-process transports.
func WithPluginConnector(connect PluginConnector) ManagerOption {
	return func(m *Manager) {
		if connect != nil {
			m.connectPlugin = connect
		}
	}
}

// WithLogger configures the plugin manager diagnostic logger. Nil discards.
func WithLogger(logger *slog.Logger) ManagerOption {
	return func(m *Manager) {
		m.logger = logging.OrDiscard(logger)
	}
}

// WithShimInstall configures the pi-ext-shim dependency installer. Tests use this to avoid npm.
func WithShimInstall(install piext.InstallFn) ManagerOption {
	return func(m *Manager) {
		m.shimInstallFn = install
	}
}

// WithShimStatus configures pi-ext-shim setup status reporting.
func WithShimStatus(status piext.StatusFn) ManagerOption {
	return func(m *Manager) {
		m.shimStatusFn = status
	}
}

// This is implemented by ui.UI (which uses ui.NotifyLevel) and
// wraps the call to pass int(level) for compatibility.
type UI interface {
	Confirm(ctx context.Context, title, message string) (bool, error)
	Select(ctx context.Context, title string, options []string) (int, error)
	Input(ctx context.Context, title, placeholder string) (string, error)
	Notify(level int, message string)
	SetStatus(key, message string)
	SetWidget(key string, lines []string)
	SetTitle(title string)
	SetEditorText(text string)
	SetToolsExpanded(expanded bool)
}

// uiAdapter wraps ui.UI to implement plugins.UI.
type uiAdapter struct {
	ui ui.UI
}

func (a *uiAdapter) Confirm(ctx context.Context, title, message string) (bool, error) {
	return a.ui.Confirm(ctx, title, message)
}

func (a *uiAdapter) Select(ctx context.Context, title string, options []string) (int, error) {
	return a.ui.Select(ctx, title, options)
}

func (a *uiAdapter) Input(ctx context.Context, title, placeholder string) (string, error) {
	return a.ui.Input(ctx, title, placeholder)
}

func (a *uiAdapter) Notify(level int, message string) {
	a.ui.Notify(ui.NotifyLevel(level), message)
}

func (a *uiAdapter) SetStatus(key, message string) {
	a.ui.SetStatus(key, message)
}

func (a *uiAdapter) SetWidget(key string, lines []string) {
	a.ui.SetWidget(key, lines)
}

func (a *uiAdapter) SetTitle(title string) {
	a.ui.SetTitle(title)
}

func (a *uiAdapter) SetEditorText(text string) {
	a.ui.SetEditorText(text)
}

func (a *uiAdapter) SetToolsExpanded(expanded bool) {
	a.ui.SetToolsExpanded(expanded)
}

// SessionAccessor is implemented by the component that owns session storage.
// It is called by the Host service to fulfill plugin requests for session access.
type SessionAccessor interface {
	// AppendEntry appends a custom or custom_message entry to the active session.
	// Returns the new entry ID or an error.
	AppendEntry(ctx context.Context, sessionID string, entryType string, payload []byte, parentID *string) (string, error)
	// ScanEntries returns entries from the active session matching the filters.
	// If entryType is empty, returns all entry types.
	// If customType is non-empty, filters custom/custom_message entries by custom_type.
	ScanEntries(ctx context.Context, sessionID string, entryType string, customType string) ([]SessionEntry, error)
	// GetLabel returns the label for an entry, or empty string if none.
	GetLabel(ctx context.Context, sessionID string, entryID string) (string, error)
	// SetLabel sets or clears a label on an entry.
	// Pass empty string to clear the label.
	// Returns the new label entry ID or an error.
	SetLabel(ctx context.Context, sessionID string, entryID string, label string) (string, error)
	// GetTree returns the session tree structure.
	GetTree(ctx context.Context, sessionID string) ([]SessionTreeNode, error)
	// GetChildren returns direct children of an entry.
	GetChildren(ctx context.Context, sessionID string, parentID string) ([]SessionTreeNode, error)
	// GetHeader returns the session header (id, name, cwd, timestamp).
	GetHeader(ctx context.Context, sessionID string) (SessionHeader, error)
	// GetSessionName returns the session display name.
	GetSessionName(ctx context.Context, sessionID string) (string, error)
	// Branch moves the current position to a different entry, enabling new entries
	// to be children of that entry. This is equivalent to TS's SessionManager.branch().
	Branch(ctx context.Context, sessionID string, branchFromID string) error
	// GetBranch returns all entries from the given entry back to root,
	// in chronological order. Used for path navigation and branch summaries.
	GetBranch(ctx context.Context, sessionID string, fromID string) ([]SessionEntry, error)
}

// SessionEntry represents a session entry for plugin consumption.
type SessionEntry struct {
	ID         string
	EntryType  string
	CustomType string
	Payload    []byte
	ParentID   *string
	Label      string // Resolved label for this entry, if any (from label entries)
}

// SessionTreeNode represents a node in the session tree for plugins.
type SessionTreeNode struct {
	Entry          SessionEntry
	Children       []SessionTreeNode
	Label          string
	LabelTimestamp string
}

// SessionHeader represents session metadata for plugins.
type SessionHeader struct {
	ID        string
	Name      string
	CWD       string
	Timestamp string
}

// EventHandler is called when an event is dispatched.
// The pluginName field identifies which plugin registered this handler.
type EventHandler struct {
	PluginName string
	Handler    func(ctx context.Context, name string, payload json.RawMessage, sessionID string) EventResponse
}

// ToolHandler executes a tool call.
type ToolHandler func(ctx context.Context, toolName, toolCallID, arguments string) (string, bool, error)

// CommandHandler executes a command.
type CommandHandler func(ctx context.Context, name, arguments string) (string, bool, error)

// EventResponse is returned by event handlers.
type EventResponse struct {
	Block       bool
	BlockReason string
	// ResponseData carries event-specific result data as raw JSON (e.g.
	// {"systemPrompt": "..."} for before_agent_start system-prompt rewrites).
	ResponseData []byte
	// Compaction is set by handlers of compaction_request events.
	// When non-nil, the compaction entry should use this data.
	Compaction *proto.CompactionHookResult
}

// HandshakeResult holds the results of a successful plugin handshake.
type HandshakeResult struct {
	Name          string
	APIVersion    string
	Registrations Registrations
	ManifestPath  string
	Manifest      *Manifest
	Hash          string
	Close         func() error // releases plugin resources
}

type pluginStartup struct {
	grpcClient    proto.PluginClient
	apiVersion    string
	registrations Registrations
	cleanup       func()
}

func (m *Manager) connectPluginProcess(ctx context.Context, path string, manifest *Manifest, manifestPath, hostSocket string) (proto.PluginClient, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd, err := buildPluginCmd(path, manifest, manifestPath, hostSocket)
	if err != nil {
		return nil, nil, fmt.Errorf("build plugin command: %w", err)
	}

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  1,
			MagicCookieKey:   "TAU_PLUGIN_API_VERSION",
			MagicCookieValue: "1",
		},
		Plugins:          tauPluginSet(),
		Cmd:              cmd,
		AutoMTLS:         true,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           newPluginLogger(m.logger),
	})

	cleanup := func() {
		client.Kill()
	}

	conn, err := client.Client()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("plugin client: %w", err)
	}

	grpcConn, err := GetPluginConn(conn)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("plugin does not use gRPC protocol: %w", err)
	}
	grpcClient := proto.NewPluginClient(grpcConn)

	return grpcClient, cleanup, nil
}

func (m *Manager) startPlugin(ctx context.Context, path string, manifest *Manifest, manifestPath string, timeouts DefaultRPCConfig) (*pluginStartup, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	pluginName := filepath.Base(path)
	hostSocket, err := m.prepareHostSocket(ctx, pluginName)
	if err != nil {
		return nil, fmt.Errorf("prepare host socket: %w", err)
	}

	grpcClient, closeConnection, err := m.connectPlugin(ctx, path, manifest, manifestPath, hostSocket)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("connect plugin: %w", err), context.Cause(ctx), m.runCleanup(ctx, pluginName, closeConnection), m.closeHostSocketLocked(pluginName))
	}
	if grpcClient == nil || closeConnection == nil {
		return nil, errors.Join(errors.New("plugin connector requires a client and cleanup"), m.runCleanup(ctx, pluginName, closeConnection), m.closeHostSocketLocked(pluginName))
	}
	fail := func(err error) error {
		return errors.Join(err, m.runCleanup(ctx, pluginName, closeConnection), m.closeHostSocketLocked(pluginName))
	}

	cwd := ""
	if m.sessionCWDProvider != nil {
		cwd = m.sessionCWDProvider()
	}
	if cwd == "" {
		var getwdErr error
		cwd, getwdErr = os.Getwd()
		if getwdErr != nil {
			cwd = ""
		}
	}

	initCtx, cancel := context.WithTimeout(ctx, timeouts.Initialize)
	defer cancel()

	sessionID := ""
	if m.sessionIDProvider != nil {
		sessionID = m.sessionIDProvider()
	}

	resp, err := grpcClient.Initialize(initCtx, &proto.InitRequest{
		Version:      Version,
		SessionId:    sessionID,
		Cwd:          cwd,
		ManifestPath: manifestPath,
	})
	if err != nil {
		return nil, fail(errors.Join(fmt.Errorf("initialize: %w", err), context.Cause(initCtx)))
	}
	if err := context.Cause(initCtx); err != nil {
		return nil, fail(err)
	}
	if resp == nil {
		return nil, fail(errors.New("initialize returned no response"))
	}
	if resp.Error != "" {
		return nil, fail(fmt.Errorf("plugin init error: %s", resp.Error))
	}
	if !isAPIVersionSupported(resp.ApiVersion) {
		return nil, fail(fmt.Errorf("unsupported api version: %s (supported: %v)", resp.ApiVersion, SupportedAPIVersions))
	}

	regCtx, regCancel := context.WithTimeout(ctx, 5*time.Second)
	defer regCancel()

	regs, err := grpcClient.ListRegistrations(regCtx, &proto.ListRegistrationsRequest{})
	if err != nil {
		return nil, fail(errors.Join(fmt.Errorf("list registrations: %w", err), context.Cause(regCtx)))
	}

	if err := context.Cause(regCtx); err != nil {
		return nil, fail(err)
	}

	if regs == nil {
		return nil, fail(errors.New("registrations returned no response"))
	}

	for _, entry := range regs.Tools {
		if entry == nil {
			return nil, fail(errors.New("nil tool registration"))
		}
	}
	for _, entry := range regs.Commands {
		if entry == nil {
			return nil, fail(errors.New("nil command registration"))
		}
	}
	for _, entry := range regs.CliFlags {
		if entry == nil {
			return nil, fail(errors.New("nil CLI flag registration"))
		}
	}
	for _, entry := range regs.VirtualModels {
		if entry == nil {
			return nil, fail(errors.New("nil virtual model registration"))
		}
	}
	return &pluginStartup{
		grpcClient: grpcClient,
		apiVersion: resp.ApiVersion,
		registrations: Registrations{
			Tools:              convertToolRegistrations(regs.Tools),
			Commands:           convertCommandRegistrations(regs.Commands),
			EventSubscriptions: regs.EventSubscriptions,
			CLIFlags:           convertCLIFlags(regs.CliFlags),
			ToolPrefix:         regs.ToolPrefix,
			VirtualModels:      convertVirtualModelRegistrations(regs.VirtualModels),
		},
		cleanup: closeConnection,
	}, nil
}

// Handshake runs the plugin handshake protocol: starts the plugin subprocess,
// performs gRPC handshake, initializes, and lists registrations.
// It does not register the plugin into the Manager; use Load for that.
func (m *Manager) Handshake(ctx context.Context, path string) (*HandshakeResult, error) {
	hash, manifest, manifestPath, err := m.handshakeMetadata(path)
	if err != nil {
		return nil, fmt.Errorf("load plugin metadata: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shimClosed {
		return nil, errors.New("plugin manager is shut down")
	}

	if _, exists := m.plugins[filepath.Base(path)]; exists {
		return nil, fmt.Errorf("plugin %s: %w", filepath.Base(path), errPluginAlreadyLoaded)
	}
	timeouts := DefaultRPCTimeouts
	if manifest != nil {
		shimErr := m.waitForShimReady(ctx, manifest)
		if shimErr != nil {
			return nil, shimErr
		}
		timeouts = manifestTimeouts(manifest)
	}
	started, err := m.startPlugin(context.WithValue(ctx, stagedHostContextKey{}, true), path, manifest, manifestPath, timeouts)
	if err != nil {
		return nil, err
	}

	m.hostMu.Lock()
	probeSocket := m.hostSockets[filepath.Base(path)]
	m.hostMu.Unlock()
	closeHandshake := sync.OnceValue(func() error {
		cleanupErr := m.runCleanup(context.WithoutCancel(ctx), filepath.Base(path), started.cleanup)
		m.hostMu.Lock()
		defer m.hostMu.Unlock()
		if m.hostSockets[filepath.Base(path)] == probeSocket {
			delete(m.hostSockets, filepath.Base(path))
		}
		if probeSocket != nil {
			cleanupErr = errors.Join(cleanupErr, probeSocket.close())
		}
		return cleanupErr
	})

	return &HandshakeResult{
		Name:          filepath.Base(path),
		APIVersion:    started.apiVersion,
		Registrations: started.registrations,
		ManifestPath:  manifestPath,
		Manifest:      manifest,
		Hash:          hash,
		Close:         closeHandshake,
	}, nil
}

// handshakeMetadata extracts the metadata needed for a plugin handshake
// without actually loading the plugin into the manager.
func (m *Manager) handshakeMetadata(path string) (hash string, manifest *Manifest, manifestPath string, err error) {
	m.mu.RLock()
	manifestPath = m.manifestPaths[path]
	manifest = m.packageManifests[manifestPath]
	m.mu.RUnlock()
	if manifest == nil {
		manifestPath = filepath.Join(filepath.Dir(path), "tau-package.toml")
		// Directory-style plugins keep the manifest inside the plugin dir itself.
		if info, statErr := os.Stat(filepath.Join(path, "tau-package.toml")); statErr == nil && !info.IsDir() {
			manifestPath = filepath.Join(path, "tau-package.toml")
		}
		_, err = os.ReadFile(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				// No manifest is fine for loose plugins.
				manifestPath = ""
			} else {
				return "", nil, "", fmt.Errorf("read manifest: %w", err)
			}
		} else {
			var parseErr error
			m.mu.RLock()
			manifest, parseErr = m.packageManifest(manifestPath)
			m.mu.RUnlock()
			if parseErr != nil {
				return "", nil, "", fmt.Errorf("parse plugin manifest: %w", parseErr)
			}
		}
	}

	hash, err = m.pluginHash(path, manifest, manifestPath)
	if err != nil {
		return "", nil, "", err
	}
	return hash, manifest, manifestPath, nil
}

// GetPluginConn extracts the gRPC client connection from a ClientProtocol.
// This works with go-plugin's gRPC implementation.
func GetPluginConn(cp plugin.ClientProtocol) (*grpc.ClientConn, error) {
	// Use reflection to get the underlying grpc.ClientConn from the GRPCClient.
	// The GRPCClient type is not exported, so we need to use reflection.
	v := reflect.ValueOf(cp)
	if v.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("client protocol is not a pointer")
	}

	// Look for the Conn field
	field := v.Elem().FieldByName("Conn")
	if !field.IsValid() {
		return nil, fmt.Errorf("client protocol has no Conn field")
	}

	conn, ok := field.Interface().(*grpc.ClientConn)
	if !ok {
		return nil, fmt.Errorf("conn field is not *grpc.ClientConn")
	}

	return conn, nil
}

// TerminalInput represents raw terminal input forwarded to interested plugins.
type TerminalInput struct {
	Key  string
	Text string
	Mod  string
}

// DispatchTerminalInput forwards raw terminal input to loaded plugins. It returns
// true if any plugin asks to consume the input.
func (m *Manager) DispatchTerminalInput(input TerminalInput) bool {
	m.mu.RLock()
	clients := make([]proto.PluginClient, 0, len(m.plugins))
	for _, info := range m.plugins {
		if info.grpcClient != nil {
			clients = append(clients, info.grpcClient)
		}
	}
	m.mu.RUnlock()

	consume := false
	for _, client := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		resp, err := client.OnTerminalInput(ctx, &proto.TerminalInput{Key: input.Key, Text: input.Text, Mod: input.Mod})
		cancel()
		if err != nil {
			m.logger.Debug("plugin terminal input handler failed", "error", err)
			continue
		}
		consume = consume || resp.Consume
	}
	return consume
}

// MessageSenderConfigured reports whether a message-sender closure has been
// wired into the manager via SetMessageSender. Exposed for in-process tests.
func (m *Manager) MessageSenderConfigured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.messageSender != nil
}

// HostSocketPath returns the Host service socket path for a loaded plugin.
// Exposed for tests and diagnostics.
func (m *Manager) HostSocketPath(pluginName string) string {
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	if hs := m.hostSockets[pluginName]; hs != nil {
		return hs.path
	}
	return ""
}

// NewManager creates a new plugin manager.
// ui can be any ui.UI implementation; it will be adapted to plugins.UI internally.
// commandRegistry may be nil if command registration is not needed.
// opts allow configuration of optional features like progress streaming.
func NewManager(configDir string, ui ui.UI, commandRegistry *commands.InMemoryRegistry, opts ...ManagerOption) *Manager {
	// Wrap ui.UI in an adapter if provided
	var adaptedUI UI
	if ui != nil {
		adaptedUI = &uiAdapter{ui: ui}
	}

	m := &Manager{
		plugins:             make(map[string]*PluginInfo),
		binaries:            make(map[string]string),
		manifestPaths:       make(map[string]string),
		packageManifests:    make(map[string]*Manifest),
		configDir:           configDir,
		ui:                  adaptedUI,
		eventHandlers:       make(map[string][]EventHandler),
		toolHandlers:        make(map[string]ToolHandler),
		commandHandlers:     make(map[string]CommandHandler),
		rpcTimeouts:         make(map[string]DefaultRPCConfig),
		failureConfig:       make(map[string]FailureConfig),
		stateDir:            filepath.Join(configDir, "plugin-state"),
		commandRegistry:     commandRegistry,
		settings:            &ai.Settings{},
		cliFlags:            make(map[string]map[string]string),
		progressSubscribers: make(map[string][]chan<- ToolProgressEvent),
		hostSockets:         make(map[string]*hostSocket),
		cleanupGrace:        GracePeriod,
		logger:              logging.Discard(),
		shimInstallFn:       piext.NPMInstall,
	}

	// Default loadFn delegates to Load. Tests override to avoid subprocess overhead.
	m.loadFn = m.Load
	m.connectPlugin = m.connectPluginProcess

	// Apply options.
	for _, opt := range opts {
		opt(m)
	}

	// Create the Host service and gRPC server.
	m.host = NewHostService(m)
	m.grpcServer = grpc.NewServer()
	proto.RegisterHostServer(m.grpcServer, m.host)

	return m
}

// keyedStatus and keyedWidget are the retained UI state, kept in the order the
// keys were first set so a replay renders deterministically.
type (
	keyedStatus struct {
		key     string
		message string
	}
	keyedWidget struct {
		key   string
		lines []string
	}
)

// SetUI updates the UI implementation used for Host.UI* RPCs and replays the
// status and widget state plugins pushed before this UI was attached.
//
// The replay is what makes a late-attached UI correct: the interactive TUI
// installs itself only after the session exists, while session_start reaches
// plugins during session creation. Without it, an indicator rendered from an
// early event stays invisible until the plugin happens to redraw it.
func (m *Manager) SetUI(impl ui.UI) {
	m.mu.Lock()
	if impl == nil {
		m.ui = nil
		m.mu.Unlock()
		return
	}
	adapted := &uiAdapter{ui: impl}
	m.ui = adapted
	statuses := append([]keyedStatus(nil), m.uiStatus...)
	widgets := append([]keyedWidget(nil), m.uiWidgets...)
	m.mu.Unlock()

	// Replayed outside the lock: a UI implementation may call back into the
	// manager while handling these.
	for _, status := range statuses {
		adapted.SetStatus(status.key, status.message)
	}
	for _, widget := range widgets {
		adapted.SetWidget(widget.key, widget.lines)
	}
}

// uiSetStatus retains the status for key and forwards it to the attached UI.
// An empty message is how a plugin clears its indicator, so the key is dropped
// rather than retained as an empty replay.
func (m *Manager) uiSetStatus(key, message string) {
	m.mu.Lock()
	idx := slices.IndexFunc(m.uiStatus, func(s keyedStatus) bool { return s.key == key })
	switch {
	case message == "" && idx >= 0:
		m.uiStatus = slices.Delete(m.uiStatus, idx, idx+1)
	case message == "":
	case idx >= 0:
		m.uiStatus[idx].message = message
	default:
		m.uiStatus = append(m.uiStatus, keyedStatus{key: key, message: message})
	}
	impl := m.ui
	m.mu.Unlock()

	if impl != nil {
		impl.SetStatus(key, message)
	}
}

// uiSetWidget retains the lines for key and forwards them to the attached UI.
// No lines means the plugin removed its widget, so nothing is retained.
func (m *Manager) uiSetWidget(key string, lines []string) {
	m.mu.Lock()
	idx := slices.IndexFunc(m.uiWidgets, func(w keyedWidget) bool { return w.key == key })
	switch {
	case len(lines) == 0 && idx >= 0:
		m.uiWidgets = slices.Delete(m.uiWidgets, idx, idx+1)
	case len(lines) == 0:
	case idx >= 0:
		m.uiWidgets[idx].lines = slices.Clone(lines)
	default:
		m.uiWidgets = append(m.uiWidgets, keyedWidget{key: key, lines: slices.Clone(lines)})
	}
	impl := m.ui
	m.mu.Unlock()

	if impl != nil {
		impl.SetWidget(key, lines)
	}
}

// Discover scans for plugins in the configured directories and loads them.
func (m *Manager) Discover(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := m.scanExtensionCandidates(); err != nil {
		return err
	}

	// Scan for plugins in installed packages.
	if err := m.scanConfiguredPackages(); err != nil {
		return fmt.Errorf("discover configured packages: %w", err)
	}

	m.filterConfiguredExtensions()
	if err := context.Cause(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	m.prepareShimIfNeeded(ctx)
	m.mu.Unlock()

	// Auto-load all discovered plugins.
	if err := m.AutoLoad(ctx); err != nil {
		m.logger.Warn("plugin auto-load failed", "error", err)
		return err
	}

	return nil
}

// AutoLoad loads all discovered plugin binaries.
func (m *Manager) AutoLoad(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shimClosed {
		return errors.New("plugin manager is shut down")
	}

	return m.autoLoadLocked(ctx)
}

// autoLoadLocked loads all discovered plugin binaries. Caller must hold m.mu.
func (m *Manager) autoLoadLocked(ctx context.Context) error {
	type pluginToLoad struct {
		path         string
		manifestPath string
	}

	var toLoad []pluginToLoad
	// Skip plugins that are already loaded.
	for path := range m.binaries {
		if _, exists := m.plugins[filepath.Base(path)]; exists {
			continue
		}
		toLoad = append(toLoad, pluginToLoad{path: path, manifestPath: m.manifestPaths[path]})
	}

	m.mu.Unlock()
	defer m.mu.Lock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(toLoad))
	for _, item := range toLoad {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.loadFn(ctx, item.path, item.manifestPath); err != nil {
				m.logger.Warn("plugin load failed", "path", item.path, "error", err)
				errCh <- fmt.Errorf("load plugin %s: %w", item.path, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// getTimeouts returns the effective RPC timeouts for this plugin.
// It merges manifest-defined overrides onto DefaultRPCTimeouts.
func (p *PluginInfo) getTimeouts() DefaultRPCConfig {
	if p.Manifest == nil {
		return DefaultRPCTimeouts
	}
	return DefaultRPCConfig{
		Initialize:  p.Manifest.getTimeout("init", DefaultRPCTimeouts.Initialize),
		OnEvent:     p.Manifest.getTimeout("on_event", DefaultRPCTimeouts.OnEvent),
		ExecuteTool: p.Manifest.getTimeout("exec", DefaultRPCTimeouts.ExecuteTool),
		ExecuteCmd:  p.Manifest.getTimeout("on_cmd", DefaultRPCTimeouts.ExecuteCmd),
	}
}

var errPluginAlreadyLoaded = errors.New("plugin is already loaded; use reload to replace it")

// loadLocked loads a single plugin. Caller must hold m.mu.
func (m *Manager) loadLocked(ctx context.Context, path string, manifestPath string) (*PluginInfo, error) {
	if _, exists := m.plugins[filepath.Base(path)]; exists {
		return nil, fmt.Errorf("plugin %s: %w", filepath.Base(path), errPluginAlreadyLoaded)
	}

	info, err := m.startCandidateLocked(context.WithValue(ctx, stagedHostContextKey{}, true), path, manifestPath)
	if err != nil {
		return nil, err
	}
	m.plugins[info.Name] = info
	m.binaries[path] = info.Hash
	if info.Manifest != nil {
		m.failureConfig[path] = info.Manifest.getFailureConfig()
	}
	m.registerPlugin(info)
	m.hostMu.Lock()
	if socket := m.hostSockets[info.Name]; socket != nil && socket.staging != nil {
		socket.staging.Store(false)
	}
	m.hostMu.Unlock()
	return info, nil
}

func (m *Manager) startCandidateLocked(ctx context.Context, path string, manifestPath string) (*PluginInfo, error) {
	if m.shimClosed {
		return nil, errors.New("plugin manager is shut down")
	}
	var manifest *Manifest
	var err error
	if manifestPath != "" {
		manifest, err = m.packageManifest(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("parse plugin manifest: %w", err)
		}
	}

	if manifest != nil {
		if err := m.waitForShimReady(ctx, manifest); err != nil {
			return nil, err
		}
	}

	hash, err := m.pluginHash(path, manifest, manifestPath)
	if err != nil {
		return nil, err
	}

	if manifest != nil && manifest.ConfigSchema != nil && manifest.ConfigSchema.Schema != "" {
		if err := validatePluginSettings(m.settings, filepath.Base(path), manifest.ConfigSchema.Schema); err != nil {
			return nil, fmt.Errorf("config validation: %w", err)
		}
	}

	timeouts := DefaultRPCTimeouts
	if manifest != nil {
		timeouts = manifestTimeouts(manifest)
	}
	started, err := m.startPlugin(ctx, path, manifest, manifestPath, timeouts)
	if err != nil {
		return nil, err
	}

	var config json.RawMessage
	if m.settings != nil {
		config = slices.Clone(m.settings.Plugins[filepath.Base(path)])
	}
	info := &PluginInfo{
		configSnapshot: config, configCaptured: true,
		Name:          filepath.Base(path),
		Path:          path,
		ManifestPath:  manifestPath,
		Manifest:      manifest,
		Hash:          hash,
		APIVersion:    started.apiVersion,
		Registrations: started.registrations,
		grpcClient:    started.grpcClient,
		Cleanup:       started.cleanup,
	}

	return info, nil
}

func manifestTimeouts(manifest *Manifest) DefaultRPCConfig {
	return DefaultRPCConfig{
		Initialize:  manifest.getTimeout("init", DefaultRPCTimeouts.Initialize),
		OnEvent:     manifest.getTimeout("on_event", DefaultRPCTimeouts.OnEvent),
		ExecuteTool: manifest.getTimeout("exec", DefaultRPCTimeouts.ExecuteTool),
		ExecuteCmd:  manifest.getTimeout("on_cmd", DefaultRPCTimeouts.ExecuteCmd),
	}
}

func (m *Manager) pluginHash(path string, manifest *Manifest, manifestPath string) (string, error) {
	root := filepath.Dir(manifestPath)
	if manifestPath == "" {
		root = filepath.Dir(path)
	}
	hash, err := pluginDiscoveryHash(root, manifestPath, manifest, path, m.computeHash)
	if err != nil {
		return "", fmt.Errorf("compute plugin hash: %w", err)
	}
	return hash, nil
}

// Load launches and initializes a plugin at the given path.
// The manifestPath is optional; if provided, it will be parsed for configuration.
func (m *Manager) Load(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Determine manifest path.
	var manifest string
	if len(manifestPath) > 0 {
		manifest = manifestPath[0]
	}

	return m.loadLocked(ctx, path, manifest)
}

// isAPIVersionSupported checks if the given API version is supported.
func isAPIVersionSupported(version string) bool {
	for _, v := range SupportedAPIVersions {
		if v == version {
			return true
		}
	}
	return false
}

// AttemptLoadWithRetry attempts to load a plugin with bounded retry and backoff.
// Returns an error after all attempts are exhausted or the caller cancels.
func (m *Manager) AttemptLoadWithRetry(ctx context.Context, path string, manifestPath string) (*PluginInfo, error) {
	m.mu.RLock()
	if m.shimClosed {
		m.mu.RUnlock()
		return nil, errors.New("plugin manager is shut down")
	}

	existing := m.plugins[filepath.Base(path)]
	config := m.failureConfig[path]
	m.mu.RUnlock()
	if existing != nil {
		return nil, fmt.Errorf("plugin %s: %w", filepath.Base(path), errPluginAlreadyLoaded)
	}
	if config.MaxAttempts == 0 {
		config = DefaultFailureConfig
	}

	var lastErr error
	for attempt := 0; attempt < config.MaxAttempts; attempt++ {
		if err := context.Cause(ctx); err != nil {
			return nil, fmt.Errorf("plugin load retry canceled: %w", errors.Join(lastErr, err))
		}
		m.mu.RLock()
		closed := m.shimClosed
		m.mu.RUnlock()
		if closed {
			return nil, errors.Join(lastErr, errors.New("plugin manager is shut down"))
		}

		if attempt > 0 {
			// Calculate backoff: base * factor^attempt
			backoff := float64(config.BackoffBase) * pow(config.BackoffFactor, float64(attempt-1))
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("plugin load retry canceled: %w", errors.Join(lastErr, context.Cause(ctx)))
			case <-time.After(time.Duration(backoff)):
			}
			m.logger.Debug("plugin load retrying after backoff", "attempt", attempt+1, "max_attempts", config.MaxAttempts)
		}

		info, err := m.loadFn(ctx, path, manifestPath)
		if err == nil {
			// Success — reset failure count for this plugin.
			m.mu.Lock()
			if existing, ok := m.plugins[info.Name]; ok {
				existing.failureCount = 0
				existing.deregistered = false
			}
			m.mu.Unlock()
			return info, nil
		}

		if errors.Is(err, errPluginAlreadyLoaded) {
			return nil, errors.Join(lastErr, err)
		}

		lastErr = errors.Join(lastErr, err)
		m.logger.Warn("plugin load attempt failed", "attempt", attempt+1, "max_attempts", config.MaxAttempts, "error", err)

		// Clean up failed load attempt resources.
		m.mu.Lock()
		if info := m.plugins[filepath.Base(path)]; info != nil && info.Cleanup != nil {
			if err := m.releasePluginLocked(ctx, info.Name, info); err != nil {
				lastErr = errors.Join(lastErr, err)
				m.logger.Warn("plugin cleanup failed", "plugin", info.Name, "error", err)
			}
			delete(m.plugins, filepath.Base(path))
		}
		m.mu.Unlock()
	}

	m.deregister(filepath.Base(path), fmt.Sprintf("load failed after %d attempts: %v", config.MaxAttempts, lastErr))
	return nil, fmt.Errorf("plugin load failed after %d attempts: %w", config.MaxAttempts, lastErr)
}

// pow calculates base^exp as a float64.
func pow(base, exp float64) float64 {
	result := 1.0
	for i := 0; i < int(exp); i++ {
		result *= base
	}
	return result
}

// deregister removes a plugin from the manager after retry exhaustion.
func (m *Manager) deregister(name string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if info, ok := m.plugins[name]; ok {
		m.unregisterPluginLocked(info)
		delete(m.plugins, name)
		m.logger.Warn("plugin deregistered", "plugin", name, "reason", reason)
	}
}

// ReattemptDeregistered stages retries before replacing deregistered entries.
func (m *Manager) ReattemptDeregistered(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shimClosed {
		return errors.New("plugin manager is shut down")
	}

	var retryErr error
	for _, name := range slices.Sorted(maps.Keys(m.plugins)) {
		previous := m.plugins[name]
		if !previous.deregistered {
			continue
		}
		if previous.ManifestPath != "" {
			manifest, err := m.packageManifest(previous.ManifestPath)
			if err != nil {
				retryErr = errors.Join(retryErr, err)
				continue
			}
			if err := m.waitForShimReady(ctx, manifest); err != nil {
				retryErr = errors.Join(retryErr, err)
				continue
			}
			if m.plugins[name] != previous {
				continue
			}
		}
		candidate, err := m.stageReloadPluginLocked(ctx, previous)
		if err != nil {
			retryErr = errors.Join(retryErr, fmt.Errorf("retry plugin %s: %w", name, err))
			continue
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(retryErr, err, m.discardStagedPlugin(ctx, name, candidate))
		}
		retryErr = errors.Join(retryErr, m.replacePluginLocked(ctx, name, previous, candidate))
	}
	return retryErr
}

// convertToolRegistrations converts proto registrations to internal type.
func convertToolRegistrations(tools []*proto.ToolRegistration) []ToolRegistration {
	result := make([]ToolRegistration, 0, len(tools))
	for _, t := range tools {
		result = append(result, ToolRegistration{
			HasReachesTools:     t.HasReachesTools,
			Namespace:           json.RawMessage(t.Namespace),
			DefaultActive:       t.DefaultActive,
			Name:                t.Name,
			Description:         t.Description,
			Parameters:          t.Parameters,
			Exposure:            t.Exposure,
			OutputSchema:        json.RawMessage(t.OutputSchema),
			ConstrainedSampling: json.RawMessage(t.ConstrainedSampling),
		})
	}
	return result
}

// convertCommandRegistrations converts proto registrations.
func convertCommandRegistrations(cmds []*proto.CommandRegistration) []CommandRegistration {
	result := make([]CommandRegistration, 0, len(cmds))
	for _, c := range cmds {
		result = append(result, CommandRegistration{
			Name:         c.Name,
			Description:  c.Description,
			ArgumentHint: c.ArgumentHint,
		})
	}
	return result
}

// convertCLIFlags converts proto CLI flags.
func convertCLIFlags(flags []*proto.CLIFlag) []CLIFlag {
	result := make([]CLIFlag, 0, len(flags))
	for _, f := range flags {
		result = append(result, CLIFlag{
			Name:         f.Name,
			Description:  f.Description,
			Required:     f.Required,
			DefaultValue: f.DefaultValue,
		})
	}
	return result
}

func convertVirtualModelRegistrations(models []*proto.VirtualModelRegistration) []VirtualModelRegistration {
	result := make([]VirtualModelRegistration, 0, len(models))
	for _, model := range models {
		input := make([]ai.InputType, 0, len(model.Input))
		for _, kind := range model.Input {
			input = append(input, ai.InputType(kind))
		}
		result = append(result, VirtualModelRegistration{Provider: model.Provider, ID: model.Id, Name: model.Name, ThinkingLevels: slices.Clone(model.ThinkingLevels), Input: input, ContextWindow: int(model.ContextWindow), MaxTokens: int(model.MaxTokens)})
	}
	return result
}

// VirtualModels returns loaded plugins' virtual models. Their routers call
// the registering plugin, which must still be loaded.
func (m *Manager) VirtualModels() []ai.VirtualModelDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var definitions []ai.VirtualModelDefinition
	for _, name := range slices.Sorted(maps.Keys(m.plugins)) {
		for _, model := range m.plugins[name].Registrations.VirtualModels {
			definitions = append(definitions, ai.VirtualModelDefinition{
				Provider: model.Provider, ID: model.ID, Name: model.Name, ThinkingLevels: slices.Clone(model.ThinkingLevels), Input: slices.Clone(model.Input), ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens,
				Route: func(ctx context.Context, request ai.ModelRouteRequest) (ai.ModelRoute, error) {
					return m.routeModel(ctx, name, model, request)
				},
			})
		}
	}
	return definitions
}

func (m *Manager) routeModel(ctx context.Context, pluginName string, model VirtualModelRegistration, request ai.ModelRouteRequest) (ai.ModelRoute, error) {
	m.mu.RLock()
	info, ok := m.plugins[pluginName]
	var client proto.PluginClient
	var timeouts DefaultRPCConfig
	if ok {
		client, timeouts = info.grpcClient, info.getTimeouts()
	}
	m.mu.RUnlock()
	if client == nil {
		return ai.ModelRoute{}, fmt.Errorf("plugin %s is not loaded", pluginName)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return ai.ModelRoute{}, fmt.Errorf("encode route request: %w", err)
	}
	routeCtx, cancel := context.WithTimeout(ctx, timeouts.ExecuteTool)
	defer cancel()
	response, err := client.RouteModel(routeCtx, &proto.ModelRouteRequest{Provider: model.Provider, ModelId: model.ID, RequestJson: string(encoded)})
	if err != nil {
		return ai.ModelRoute{}, fmt.Errorf("plugin %s route model: %w", pluginName, err)
	}
	if response.Error != "" {
		return ai.ModelRoute{}, fmt.Errorf("plugin %s route model: %s", pluginName, response.Error)
	}
	route := ai.ModelRoute{Model: &ai.Model{Provider: response.Provider, ID: response.ModelId}, ThinkingLevel: response.ThinkingLevel}
	if response.StateJson != "" {
		if !json.Valid([]byte(response.StateJson)) {
			return ai.ModelRoute{}, fmt.Errorf("plugin %s returned router state that is not JSON", pluginName)
		}
		route.State = json.RawMessage(response.StateJson)
	}
	return route, nil
}

// registerPlugin registers a plugin's tools, commands, event subscriptions, and CLI flags
// into the manager's internal maps and the command registry.
func (m *Manager) registerPlugin(info *PluginInfo) {
	pluginName := info.Name

	// Apply tool prefix if specified.
	toolPrefix := info.Registrations.ToolPrefix

	// Register tools.
	for _, tool := range info.Registrations.Tools {
		fullName := toolPrefix + tool.Name
		m.toolHandlers[fullName] = func(ctx context.Context, toolName, toolCallID, arguments string) (string, bool, error) {
			return m.executeTool(ctx, pluginName, tool.Name, toolCallID, arguments)
		}
	}

	// Register commands. ToolPrefix applies only to model-callable tools;
	// slash commands keep their registered command names so a plugin command
	// named "hello" is invoked as /hello, not /<tool-prefix>hello.
	for _, cmd := range info.Registrations.Commands {
		commandName := cmd.Name
		m.commandHandlers[commandName] = func(ctx context.Context, name, arguments string) (string, bool, error) {
			return m.executeCommand(ctx, pluginName, name, arguments)
		}

		// Also register in the command registry if available.
		if m.commandRegistry != nil {
			cmdInfo := commands.Info{
				Name:         commandName,
				Description:  cmd.Description,
				ArgumentHint: cmd.ArgumentHint,
				Source:       commands.Plugin,
				SourcePath:   info.Path,
			}
			handler := func(ctx context.Context, args string, ui commands.CommandUI, sess commands.CommandSession) (commands.Result, error) {
				output, isError, err := m.executeCommand(ctx, pluginName, commandName, args)
				return commands.Result{Output: output, IsError: isError}, err
			}
			if err := m.commandRegistry.Register(cmdInfo, handler); err != nil {
				m.logger.Warn("plugin command already registered", "command", commandName, "error", err)
			}
		}
	}

	// Register event subscriptions.
	for _, eventName := range info.Registrations.EventSubscriptions {
		m.eventHandlers[eventName] = append(m.eventHandlers[eventName], EventHandler{
			PluginName: pluginName,
			Handler: func(ctx context.Context, name string, payload json.RawMessage, sessionID string) EventResponse {
				return m.handleEvent(ctx, pluginName, eventName, payload, sessionID)
			},
		})
	}

	// Initialize CLI flags with their default values.
	// These are injected into the config namespace so Host.GetConfig returns them.
	flags := make(map[string]string)
	for _, flag := range info.Registrations.CLIFlags {
		flags[flag.Name] = flag.DefaultValue
	}
	for key, value := range m.cliFlagOverrides[pluginName] {
		flags[key] = value
	}
	m.cliFlags[pluginName] = flags
}

// executeTool executes a tool call on a plugin.
func (m *Manager) executeTool(ctx context.Context, pluginName, toolName, toolCallID, arguments string) (string, bool, error) {
	result, err := m.executeToolResult(ctx, pluginName, toolName, toolCallID, arguments)
	if err != nil {
		return "", true, err
	}
	if result.Rejection != "" {
		return "", true, fmt.Errorf("plugin execution rejected: %s", result.Rejection)
	}
	if result.IsError {
		return result.Output, true, fmt.Errorf("%s", result.ErrorMessage)
	}
	return result.Output, false, nil
}

func (m *Manager) executeToolResult(ctx context.Context, pluginName, toolName, toolCallID, arguments string) (*proto.ToolResult, error) {
	return m.executeSessionToolResult(ctx, "", pluginName, toolName, toolCallID, arguments)
}

func (m *Manager) executeSessionToolResult(ctx context.Context, sessionID, pluginName, toolName, toolCallID, arguments string) (*proto.ToolResult, error) {
	m.mu.RLock()
	info, ok := m.plugins[pluginName]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("plugin not found: %s", pluginName)
	}
	client := info.grpcClient
	timeouts := info.getTimeouts()
	m.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("plugin %s: no gRPC client", pluginName)
	}
	execCtx, cancel := context.WithTimeout(ctx, timeouts.ExecuteTool)
	defer cancel()
	result, err := client.ExecuteTool(execCtx, &proto.ToolCall{ToolName: toolName, ToolCallId: toolCallID, Arguments: arguments, SessionId: sessionID})
	if err != nil {
		return nil, fmt.Errorf("execute tool: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("plugin returned no tool result")
	}
	return result, nil
}

// executeCommand executes a command on a plugin.
func (m *Manager) executeCommand(ctx context.Context, pluginName, name, arguments string) (string, bool, error) {
	m.mu.RLock()
	info, ok := m.plugins[pluginName]
	if !ok {
		m.mu.RUnlock()
		return "", false, fmt.Errorf("plugin not found: %s", pluginName)
	}
	grpcClient := info.grpcClient
	m.mu.RUnlock()

	if grpcClient == nil {
		return "", false, fmt.Errorf("plugin %s: no gRPC client", pluginName)
	}

	timeouts := info.getTimeouts()
	execCtx, cancel := context.WithTimeout(ctx, timeouts.ExecuteCmd)
	defer cancel()

	sessionID := ""
	if m.sessionIDProvider != nil {
		sessionID = m.sessionIDProvider()
	}

	result, err := grpcClient.ExecuteCommand(execCtx, &proto.CommandInvocation{
		CommandName: name,
		Arguments:   arguments,
		SessionId:   sessionID,
	})
	if err != nil {
		return "", true, fmt.Errorf("execute command: %w", err)
	}

	if result.IsError {
		return result.Output, true, fmt.Errorf("%s", result.ErrorMessage)
	}
	return result.Output, false, nil
}

// handleEvent dispatches an event to a plugin.
func (m *Manager) handleEvent(ctx context.Context, pluginName, eventName string, payload json.RawMessage, sessionID string) EventResponse {
	m.mu.RLock()
	info, ok := m.plugins[pluginName]
	if !ok {
		m.mu.RUnlock()
		return EventResponse{}
	}
	grpcClient := info.grpcClient
	m.mu.RUnlock()

	if grpcClient == nil {
		return EventResponse{}
	}

	timeouts := info.getTimeouts()
	execCtx, cancel := context.WithTimeout(ctx, timeouts.OnEvent)
	defer cancel()

	payloadStr := ""
	if payload != nil {
		payloadStr = string(payload)
	}

	resp, err := grpcClient.OnEvent(execCtx, &proto.Event{
		Name:      eventName,
		Payload:   payloadStr,
		SessionId: sessionID,
		Timestamp: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		m.logger.Warn("plugin event handler failed", "event", eventName, "plugin", pluginName, "error", err)
		if eventName == "user_bash" {
			return EventResponse{Block: true, BlockReason: fmt.Sprintf("user bash handler failed: %v", err)}
		}
		return EventResponse{}
	}

	return EventResponse{
		Block:        resp.Block,
		BlockReason:  resp.BlockReason,
		ResponseData: []byte(resp.ResponseData),
	}
}

// unregisterPluginLocked removes a plugin's registrations from the manager.
// Caller must hold m.mu.
func (m *Manager) unregisterPluginLocked(info *PluginInfo) {
	if m.commandRegistry != nil && info.Path != "" {
		m.commandRegistry.ClearSource(info.Path)
	}
	toolPrefix := info.Registrations.ToolPrefix

	// Remove tools.
	for _, tool := range info.Registrations.Tools {
		fullName := toolPrefix + tool.Name
		delete(m.toolHandlers, fullName)
	}

	// Remove commands from internal map. ToolPrefix applies only to tools;
	// commands are registered under their slash-command name.
	for _, cmd := range info.Registrations.Commands {
		delete(m.commandHandlers, cmd.Name)
	}

	// Remove event subscriptions (only this plugin's handlers).
	for _, eventName := range info.Registrations.EventSubscriptions {
		// Filter out handlers belonging to this plugin.
		var remaining []EventHandler
		for _, h := range m.eventHandlers[eventName] {
			if h.PluginName != info.Name {
				remaining = append(remaining, h)
			}
		}
		if len(remaining) > 0 {
			m.eventHandlers[eventName] = remaining
		} else {
			delete(m.eventHandlers, eventName)
		}
	}

	// Remove CLI flags.
	delete(m.cliFlags, info.Name)
}

// Reload re-hashes all plugins and restarts those whose hash changed,
// and re-attempts loading of any plugins that were previously deregistered.
func (m *Manager) Reload(ctx context.Context) error {
	return m.ReloadNamed(ctx, "")
}

// ReloadNamed rehashes plugins and restarts those matching name, or all when empty.
// A named plugin is restarted even when its hash is unchanged.
func (m *Manager) ReloadNamed(ctx context.Context, name string) error {
	prepared, err := m.PrepareReload(ctx, m.getSettings())
	if err != nil {
		return err
	}
	return prepared.Reload(ctx, name)
}

// reloadCandidates requires m.mu to be held throughout staging and admission.
func (m *Manager) reloadCandidates(ctx context.Context, name string, changedConfig map[string]bool, admit func() error) (bool, error) {
	var reloadErr error
	names := slices.Sorted(maps.Keys(m.plugins))
	staged := make(map[string]*stagedPlugin)
	replacements := make(map[string]string)
	for _, path := range slices.Sorted(maps.Keys(m.binaries)) {
		pluginName := filepath.Base(path)
		if _, exists := replacements[pluginName]; !exists {
			replacements[pluginName] = path
		}
	}
	for _, pluginName := range names {
		info := m.plugins[pluginName]
		if name != "" && pluginName != name {
			continue
		}
		hash, exists := m.binaries[info.Path]
		candidateInfo := info
		if !exists {
			if replacementPath := replacements[pluginName]; replacementPath != "" {
				moved := *info
				moved.Path = replacementPath
				moved.ManifestPath = m.manifestPaths[replacementPath]
				candidateInfo = &moved
				hash, exists = m.binaries[replacementPath]
			}
		}
		if !exists || (!info.deregistered && hash == info.Hash && candidateInfo.Path == info.Path && name == "" && !changedConfig[pluginName]) {
			continue
		}
		candidate, err := m.stageReloadPluginLocked(ctx, candidateInfo)
		if err != nil {
			for candidateName, item := range staged {
				err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
			}
			return false, fmt.Errorf("stage plugin %s: %w", pluginName, err)
		}
		staged[pluginName] = candidate
	}

	newPlugins := make(map[string]*stagedPlugin)
	for _, path := range slices.Sorted(maps.Keys(m.binaries)) {
		pluginName := filepath.Base(path)
		if name != "" && name != pluginName {
			continue
		}
		if _, exists := m.plugins[pluginName]; exists {
			continue
		}
		if _, exists := newPlugins[pluginName]; exists {
			continue
		}
		candidate, err := m.stagePluginLocked(ctx, &PluginInfo{Name: pluginName, Path: path})
		if err != nil {
			for candidateName, item := range staged {
				err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
			}
			for candidateName, item := range newPlugins {
				err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
			}
			return false, fmt.Errorf("stage new plugin %s: %w", pluginName, err)
		}
		newPlugins[pluginName] = candidate
	}

	if err := context.Cause(ctx); err != nil {
		for candidateName, item := range staged {
			err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
		}
		for candidateName, item := range newPlugins {
			err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
		}
		return false, err
	}

	if admit != nil {
		if err := admit(); err != nil {
			for candidateName, item := range staged {
				err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
			}
			for candidateName, item := range newPlugins {
				err = errors.Join(err, m.discardStagedPlugin(ctx, candidateName, item))
			}
			return false, err
		}
	}

	// Check each loaded plugin for hash changes or force restart.
	for _, pluginName := range names {
		info := m.plugins[pluginName]
		// If name is specified and doesn't match, skip.
		if name != "" && pluginName != name {
			continue
		}

		if candidate := staged[pluginName]; candidate != nil {
			reloadErr = errors.Join(reloadErr, m.replacePluginLocked(ctx, pluginName, info, candidate))
			continue
		}
		_, exists := m.binaries[info.Path]
		if !exists {
			// Binary no longer exists, shut down and remove.
			m.logger.Info("plugin binary no longer found; shutting down", "plugin", pluginName)
			if err := m.shutdownPluginLocked(ctx, pluginName, info, DefaultDrainTimeout); err != nil {
				reloadErr = errors.Join(reloadErr, err)
				m.logger.Warn("plugin shutdown failed", "plugin", pluginName, "error", err)
			}
			m.unregisterPluginLocked(info)
			delete(m.plugins, pluginName)
			continue
		}

	}

	for pluginName, candidate := range newPlugins {
		m.installStagedPluginLocked(pluginName, candidate)
	}
	return true, reloadErr
}

// PluginShutdowner allows a plugin to be shut down gracefully.
type PluginShutdowner interface {
	ShutdownPlugin(ctx context.Context, name string, drainTimeout time.Duration) error
}

// Shutdown gracefully shuts down all plugins via their Shutdown RPC.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.stopShimSetupLocked(ctx)
	for _, name := range slices.Sorted(maps.Keys(m.plugins)) {
		info := m.plugins[name]
		err = errors.Join(err, m.shutdownPluginLocked(ctx, name, info, DefaultDrainTimeout))
		m.unregisterPluginLocked(info)
		delete(m.plugins, name)
	}
	return errors.Join(err, m.waitCleanups(ctx))
}

// GracePeriod is the time to wait after SIGTERM before sending SIGKILL.
const GracePeriod = 2 * time.Second

// shutdownPluginLocked calls the Shutdown RPC on a plugin.
// Caller must hold m.mu.
func (m *Manager) shutdownPluginLocked(ctx context.Context, name string, info *PluginInfo, drainTimeout time.Duration) error {
	m.hostMu.Lock()
	if socket := m.hostSockets[name]; socket != nil && socket.retired != nil {
		socket.retired.Store(true)
	}
	m.hostMu.Unlock()
	if info.grpcClient == nil {
		return m.releasePluginLocked(ctx, name, info)
	}

	// Attempt cooperative shutdown via Shutdown RPC.
	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	resp, err := info.grpcClient.Shutdown(drainCtx, &proto.ShutdownRequest{
		DrainTimeoutSeconds: int32(drainTimeout.Seconds()),
	})
	err = shutdownResponseError(name, resp, err)
	if err != nil {
		err = errors.Join(err, context.Cause(drainCtx))
	}
	// Cancel the drain context to stop waiting.
	cancel()

	if releaseErr := m.releasePluginLocked(ctx, name, info); releaseErr != nil {
		return errors.Join(err, releaseErr)
	}
	if err != nil {
		return err
	}
	m.logger.Debug("plugin shut down gracefully", "plugin", name)
	return nil
}

// releasePluginLocked kills the plugin process and closes its Host socket.
func (m *Manager) releasePluginLocked(ctx context.Context, name string, info *PluginInfo) error {
	return errors.Join(m.runCleanup(ctx, name, info.Cleanup), m.closeHostSocketLocked(name))
}

func shutdownResponseError(name string, resp *proto.ShutdownResponse, err error) error {
	if err != nil {
		return fmt.Errorf("shutdown plugin %s: %w", name, err)
	}
	if resp == nil {
		return fmt.Errorf("shutdown plugin %s: empty response", name)
	}
	if resp.Error != "" {
		return fmt.Errorf("shutdown plugin %s: %s", name, resp.Error)
	}
	if !resp.Graceful {
		return fmt.Errorf("shutdown plugin %s: drain did not finish gracefully", name)
	}
	return nil
}

// ShutdownPlugin gracefully shuts down a specific plugin by name.
func (m *Manager) ShutdownPlugin(ctx context.Context, name string, drainTimeout time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin not found: %s", name)
	}

	err := m.shutdownPluginLocked(ctx, name, info, drainTimeout)
	m.unregisterPluginLocked(info)
	delete(m.plugins, name)
	return err
}

// CancellableEvents is the set of event names whose handlers may return
// CancellableEvents lists plugin event names for which a handler returning
// Block=true aborts the originating operation. The first handler to block
// wins; the rest are short-circuited. The cancellable hooks are session
// fork/compact and tool_call. Switch and tree hooks are not yet wired
// because the corresponding operations are not implemented; they should
// be re-added here when the operations are added.
var CancellableEvents = map[string]bool{
	"tool_call":              true,
	"user_bash":              true,
	"session_before_fork":    true,
	"session_before_compact": true,
}

// IsCancellableEvent reports whether a plugin returning Block=true for
// the named event should abort the originating operation.
func IsCancellableEvent(name string) bool { return CancellableEvents[name] }

// DispatchEvent sends an event to all subscribed plugins.
// Returns true if any plugin returned a block verdict for a cancellable
// event (see CancellableEvents).
func (m *Manager) DispatchEvent(ctx context.Context, name string, payload json.RawMessage, sessionID string) bool {
	return m.DispatchEventResponse(ctx, name, payload, sessionID).Block
}

// DispatchEventResponse sends an event to all subscribed plugins and returns
// the aggregated response. For cancellable events, the first block verdict is
// propagated. For compaction_request, the first non-nil Compaction result is
// propagated.
func (m *Manager) DispatchEventResponse(ctx context.Context, name string, payload json.RawMessage, sessionID string) EventResponse {
	responses := m.DispatchEventCollect(ctx, name, payload, sessionID)

	resp := EventResponse{}
	for _, r := range responses {
		// Propagate block for cancellable events (tool_call,
		// session_before_fork, session_before_compact). The first block wins.
		if r.Block && IsCancellableEvent(name) {
			resp.Block = true
			resp.BlockReason = r.BlockReason
			break
		}

		if name == "user_bash" && len(r.ResponseData) > 0 {
			if len(resp.ResponseData) > 0 {
				return EventResponse{Block: true, BlockReason: "multiple user bash overrides"}
			}
			resp.ResponseData = r.ResponseData
		}
		// Propagate compaction result for compaction_request events.
		if name == "compaction_request" && r.Compaction != nil && resp.Compaction == nil {
			resp.Compaction = r.Compaction
		}
	}
	return resp
}

// ProjectTrustDecision asks project_trust subscribers in dispatch order. The
// first yes/no answer wins. Undecided, failed and malformed answers fall
// through to the next handler, and remember asks the caller to save the answer.
func (m *Manager) ProjectTrustDecision(ctx context.Context, cwd string) (decision *bool, remember bool, err error) {
	payload, err := json.Marshal(map[string]string{"cwd": cwd})
	if err != nil {
		return nil, false, fmt.Errorf("marshal project_trust payload: %w", err)
	}
	m.mu.RLock()
	handlers := slices.Clone(m.eventHandlers["project_trust"])
	m.mu.RUnlock()
	for _, handler := range handlers {
		if err := context.Cause(ctx); err != nil {
			return nil, false, err
		}
		response := handler.Handler(ctx, "project_trust", payload, "")
		if len(response.ResponseData) == 0 {
			continue
		}
		var result struct {
			Trusted  string `json:"trusted"`
			Remember bool   `json:"remember"`
		}
		if err := json.Unmarshal(response.ResponseData, &result); err != nil {
			m.logger.Warn("parse project_trust response", "plugin", handler.PluginName, "error", err)
			continue
		}
		switch result.Trusted {
		case "yes", "no":
			if err := context.Cause(ctx); err != nil {
				return nil, false, err
			}
			return new(result.Trusted == "yes"), result.Remember, nil
		case "", "undecided":
		default:
			m.logger.Warn("invalid project_trust decision", "plugin", handler.PluginName, "trusted", result.Trusted)
		}
	}
	return nil, false, context.Cause(ctx)
}

// DispatchEventCollect sends an event to all subscribed plugins and returns
// every handler's response in dispatch (registration) order. Use this for
// events whose per-plugin responses matter individually, e.g. chaining
// before_agent_start system-prompt rewrites.
func (m *Manager) DispatchEventCollect(ctx context.Context, name string, payload json.RawMessage, sessionID string) []EventResponse {
	m.mu.RLock()
	handlers := slices.Clone(m.eventHandlers[name])
	m.mu.RUnlock()

	responses := make([]EventResponse, 0, len(handlers))
	for _, handler := range handlers {
		response := handler.Handler(ctx, name, payload, sessionID)
		responses = append(responses, response)
		var fields map[string]json.RawMessage
		var update map[string]json.RawMessage
		if len(response.ResponseData) == 0 {
			continue
		}
		if err := json.Unmarshal(response.ResponseData, &update); err != nil {
			m.logger.Warn("parse mutable event response", "event", name, "error", err)
			continue
		}
		var keys []string
		switch name {
		case "before_agent_start":
			keys = []string{"systemPrompt"}
		case "context", "context_with_system":
			keys = []string{"messages"}
		case "input":
			var action string
			if raw := update["action"]; raw != nil {
				if err := json.Unmarshal(raw, &action); err != nil {
					m.logger.Warn("parse input action", "error", err)
					continue
				}
			}
			if action == "handled" {
				return responses
			}
			if action == "transform" {
				keys = []string{"text", "images"}
			}
		}
		if len(keys) == 0 {
			continue
		}
		if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
			m.logger.Warn("parse mutable event payload", "event", name, "error", err)
			continue
		}
		for _, key := range keys {
			if value := update[key]; value != nil {
				fields[key] = value
			}
		}
		next, err := json.Marshal(fields)
		if err != nil {
			m.logger.Warn("marshal mutable event payload", "event", name, "error", err)
			continue
		}
		payload = next
	}
	return responses
}

// DispatchCompactionHook dispatches a compaction_request event to all subscribed
// plugins. Returns the first successful result, or an empty result if no plugin
// handled the event.
func (m *Manager) DispatchCompactionHook(ctx context.Context, transcript string, tokensBefore int64, sessionID string) proto.CompactionHookResult {
	// Build the compaction request payload.
	payload := struct {
		Transcript   string `json:"transcript"`
		TokensBefore int64  `json:"tokens_before"`
	}{transcript, tokensBefore}
	payloadBytes, _ := json.Marshal(payload)

	resp := m.DispatchEventResponse(ctx, "compaction_request", payloadBytes, sessionID)
	if resp.Compaction != nil {
		return *resp.Compaction
	}
	return proto.CompactionHookResult{}
}

// RegisterEventHandler registers an event handler for the given event name.
// This is exposed for testing and internal use.
func (m *Manager) RegisterEventHandler(name string, handler EventHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventHandlers[name] = append(m.eventHandlers[name], handler)
}

// ToolHandlerDesc describes a plugin tool for registration in the agent.
type ToolHandlerDesc struct {
	HasReachesTools     bool
	Namespace           json.RawMessage
	DefaultActive       *bool
	Exposure            string
	OutputSchema        json.RawMessage
	ConstrainedSampling json.RawMessage
	Name                string
	Description         string
	Parameters          map[string]any
	Handler             ToolHandler
}

// Tools returns all plugin tools as ToolHandlerDesc for agent registration.
func (m *Manager) Tools() []ToolHandlerDesc {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []ToolHandlerDesc
	for name, handler := range m.toolHandlers {
		// Find the plugin that registered this tool
		var desc ToolHandlerDesc
		desc.Name = name
		desc.Handler = handler

		// Look through registrations to get description/parameters
		for _, info := range m.plugins {
			for _, tool := range info.Registrations.Tools {
				fullName := info.Registrations.ToolPrefix + tool.Name
				if fullName == name {
					desc.Description = tool.Description
					desc.Parameters = parseToolParameters(tool.Parameters)
					desc.Namespace = tool.Namespace
					desc.DefaultActive = tool.DefaultActive
					desc.HasReachesTools = tool.HasReachesTools
					desc.Exposure = tool.Exposure
					desc.OutputSchema = tool.OutputSchema
					desc.ConstrainedSampling = tool.ConstrainedSampling
					break
				}
			}
		}

		result = append(result, desc)
	}
	return result
}

func parseToolParameters(parameters map[string]string) map[string]any {
	if parameters == nil {
		return nil
	}

	parsed := make(map[string]any, len(parameters))
	for k, v := range parameters {
		var value any
		if err := json.Unmarshal([]byte(v), &value); err == nil {
			parsed[k] = value
			continue
		}
		parsed[k] = v
	}
	return parsed
}

// ExecutePluginTool executes a named plugin tool with the given arguments.
// Returns the result string, whether it's an error, and any error.
func (m *Manager) ExecutePluginTool(ctx context.Context, toolName, toolCallID, arguments string) (string, bool, error) {
	m.mu.RLock()
	handler, ok := m.toolHandlers[toolName]
	m.mu.RUnlock()

	if !ok {
		return "", false, fmt.Errorf("plugin tool not found: %s", toolName)
	}

	return handler(ctx, toolName, toolCallID, arguments)
}

// RegisterToolProgressSubscriber registers a sink for progress events emitted
// by a specific in-flight plugin tool call.
func (m *Manager) RegisterToolProgressSubscriber(toolCallID string, ch chan<- ToolProgressEvent) {
	if toolCallID == "" || ch == nil {
		return
	}

	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	m.progressSubscribers[toolCallID] = append(m.progressSubscribers[toolCallID], ch)
}

// UnregisterToolProgressSubscriber removes a previously registered progress sink.
func (m *Manager) UnregisterToolProgressSubscriber(toolCallID string, ch chan<- ToolProgressEvent) {
	if toolCallID == "" || ch == nil {
		return
	}

	m.progressMu.Lock()
	defer m.progressMu.Unlock()

	subs := m.progressSubscribers[toolCallID]
	if len(subs) == 0 {
		return
	}

	filtered := subs[:0]
	for _, sub := range subs {
		if sub != ch {
			filtered = append(filtered, sub)
		}
	}

	if len(filtered) == 0 {
		delete(m.progressSubscribers, toolCallID)
		return
	}

	m.progressSubscribers[toolCallID] = filtered
}

func (m *Manager) broadcastToolProgress(event ToolProgressEvent) {
	m.progressMu.RLock()
	defer m.progressMu.RUnlock()

	for _, sub := range m.progressSubscribers[event.ToolCallID] {
		select {
		case sub <- event:
		default:
		}
	}
}

// ExecutePluginToolResult retains structured plugin output while supporting legacy handlers.
func (m *Manager) ExecutePluginToolResult(ctx context.Context, toolName, toolCallID, arguments string) (*proto.ToolResult, error) {
	return m.ExecutePluginToolResultForSession(ctx, "", toolName, toolCallID, arguments)
}

// ExecutePluginToolResultForSession dispatches with the originating session's
// identity. Callers must capture it from the owning session, not the active UI.
func (m *Manager) ExecutePluginToolResultForSession(ctx context.Context, sessionID, toolName, toolCallID, arguments string) (*proto.ToolResult, error) {
	m.mu.RLock()
	var pluginName, localName string
	for name, info := range m.plugins {
		for _, tool := range info.Registrations.Tools {
			if info.Registrations.ToolPrefix+tool.Name == toolName && info.grpcClient != nil {
				pluginName = name
				localName = tool.Name
				break
			}
		}
	}
	m.mu.RUnlock()
	if pluginName != "" {
		return m.executeSessionToolResult(ctx, sessionID, pluginName, localName, toolCallID, arguments)
	}
	output, isError, err := m.ExecutePluginTool(ctx, toolName, toolCallID, arguments)
	if err != nil {
		return nil, err
	}
	return &proto.ToolResult{Output: output, IsError: isError}, nil
}

// ReachPluginTools evaluates a plugin's reach callback using its local tool name.
func (m *Manager) ReachPluginTools(ctx context.Context, name string, args map[string]any) ([]string, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode reach arguments: %w", err)
	}
	m.mu.RLock()
	var client proto.PluginClient
	var local string
	var timeout time.Duration
	for _, info := range m.plugins {
		for _, tool := range info.Registrations.Tools {
			if info.Registrations.ToolPrefix+tool.Name == name && tool.HasReachesTools {
				client = info.grpcClient
				local = tool.Name
				timeout = info.getTimeouts().ExecuteTool
			}
		}
	}
	m.mu.RUnlock()
	if client == nil {
		return nil, errors.New("reach callback unavailable")
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := client.ReachTools(execCtx, &proto.ToolCall{ToolName: local, Arguments: string(data)})
	if err != nil {
		return nil, fmt.Errorf("plugin reach callback: %w", err)
	}
	if result == nil {
		return nil, errors.New("plugin returned no reach result")
	}
	return result.Names, nil
}

type stagedPlugin struct {
	info   *PluginInfo
	socket *hostSocket
}

func (m *Manager) discardStagedPlugin(ctx context.Context, name string, candidate *stagedPlugin) error {
	err := m.runCleanup(ctx, name, candidate.info.Cleanup)
	if candidate.socket != nil {
		err = errors.Join(err, candidate.socket.close())
	}
	return err
}

func (m *Manager) stagePluginLocked(ctx context.Context, previous *PluginInfo) (*stagedPlugin, error) {
	name := previous.Name
	m.hostMu.Lock()
	oldSocket := m.hostSockets[name]
	delete(m.hostSockets, name)
	m.hostMu.Unlock()
	manifestPath, discovered := m.manifestPaths[previous.Path]
	if !discovered {
		manifestPath = previous.ManifestPath
	}
	replacement, err := m.startCandidateLocked(context.WithValue(ctx, stagedHostContextKey{}, true), previous.Path, manifestPath)
	m.hostMu.Lock()
	newSocket := m.hostSockets[name]
	if oldSocket != nil {
		m.hostSockets[name] = oldSocket
	} else {
		delete(m.hostSockets, name)
	}
	m.hostMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &stagedPlugin{info: replacement, socket: newSocket}, nil
}

func (m *Manager) replacePluginLocked(ctx context.Context, name string, previous *PluginInfo, candidate *stagedPlugin) error {
	shutdownErr := m.shutdownPluginLocked(ctx, name, previous, DefaultDrainTimeout)
	m.unregisterPluginLocked(previous)
	m.installStagedPluginLocked(name, candidate)
	return shutdownErr
}

func (m *Manager) installStagedPluginLocked(name string, candidate *stagedPlugin) {
	replacement := candidate.info
	newSocket := candidate.socket
	m.hostMu.Lock()
	if newSocket != nil {
		m.hostSockets[name] = newSocket
	}
	m.hostMu.Unlock()
	m.plugins[name] = replacement
	m.binaries[replacement.Path] = replacement.Hash
	if replacement.Manifest != nil {
		m.failureConfig[replacement.Path] = replacement.Manifest.getFailureConfig()
	}
	m.registerPlugin(replacement)
	if newSocket != nil && newSocket.staging != nil {
		newSocket.staging.Store(false)
	}
}

func (m *Manager) stageReloadPluginLocked(ctx context.Context, previous *PluginInfo) (*stagedPlugin, error) {
	if !previous.deregistered {
		return m.stagePluginLocked(ctx, previous)
	}
	config := m.failureConfig[previous.Path]
	if config.MaxAttempts == 0 {
		config = DefaultFailureConfig
	}
	var lastErr error
	for attempt := range config.MaxAttempts {
		if err := context.Cause(ctx); err != nil {
			return nil, errors.Join(lastErr, err)
		}
		if attempt > 0 {
			backoff := time.Duration(float64(config.BackoffBase) * pow(config.BackoffFactor, float64(attempt-1)))
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, errors.Join(lastErr, context.Cause(ctx))
			case <-timer.C:
			}
		}
		candidate, err := m.stagePluginLocked(ctx, previous)
		if err == nil {
			return candidate, nil
		}
		lastErr = errors.Join(lastErr, err)
	}
	return nil, fmt.Errorf("plugin load failed after %d attempts: %w", config.MaxAttempts, lastErr)
}
