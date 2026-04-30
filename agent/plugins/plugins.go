// Package plugins implements the extension/plugin system for tau using hashicorp/go-plugin.
// Plugins are subprocesses that communicate with tau over gRPC.
package plugins // import "github.com/losinggeneration/tau/agent/plugins"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	Name        string
	Description string
	Parameters  map[string]string // JSON schema as string map
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
}

// PluginInfo holds information about a loaded plugin.
type PluginInfo struct {
	Name          string
	Path          string
	ManifestPath  string
	Manifest      *Manifest // parsed tau-package.toml, if found
	Hash          string
	APIVersion    string
	Registrations Registrations
	Cleanup       func()

	// client is the go-plugin client (used for shutdown coordination).
	client *plugin.Client

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
	manifestPaths map[string]string // path -> manifestPath

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

	// failureConfig holds retry configuration per plugin path.
	failureConfig map[string]FailureConfig

	// sessionIDProvider returns the current session ID (may be nil).
	sessionIDProvider func() string

	// sessionAccessor handles session read/write for Host service RPCs.
	sessionAccessor SessionAccessor

	// commandRegistry is the command registry for plugin commands.
	commandRegistry *commands.InMemoryRegistry

	// settings holds the merged plugin configuration.
	settings *ai.Settings

	// gRPC server for the Host service (served to plugins).
	grpcServer *grpc.Server

	// CLI flags holds resolved CLI flag values per plugin.
	// These are injected into the config namespace so Host.GetConfig returns them.
	cliFlags map[string]map[string]string // pluginName -> key -> value

	progressMu          sync.RWMutex
	progressSubscribers map[string][]chan<- ToolProgressEvent

	// hostMu guards hostSockets. Kept separate from mu so socket setup
	// (which involves net.Listen and file IO) does not need m.mu and can
	// run while m.mu is held by the plugin loader.
	hostMu      sync.Mutex
	hostSockets map[string]*hostSocket

	// sessionCWDProvider returns the current session CWD (may be nil).
	sessionCWDProvider func() string

	// messageSender routes plugin sendUserMessage calls to the active
	// session's Send/Steer/FollowUp methods. May be nil.
	// deliverAs: "" -> Send (normal); "steer" -> Steer; "followUp" -> FollowUp.
	messageSender func(text string, deliverAs string) error

	// loadFn is the function used by AttemptLoadWithRetry to load a plugin.
	// Defaults to m.Load. Tests can override to avoid launching subprocesses.
	loadFn func(ctx context.Context, path string, manifestPath ...string) (*PluginInfo, error)

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

	shimReady       chan error
	shimReadyClosed bool
	shimInstallFn   piext.InstallFn
	shimStatusFn    piext.StatusFn
}

// ToolProgressEvent represents a tool progress update from a plugin.
type ToolProgressEvent struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// ManagerOption configures a plugin Manager.
type ManagerOption func(*Manager)

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
	Handler    func(name string, payload json.RawMessage, sessionID string) EventResponse
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
	Close         func() // releases plugin resources
}

type pluginStartup struct {
	client        *plugin.Client
	grpcClient    proto.PluginClient
	apiVersion    string
	registrations Registrations
	cleanup       func()
}

func (m *Manager) startPlugin(ctx context.Context, path string, manifest *Manifest, manifestPath string, timeouts DefaultRPCConfig) (*pluginStartup, error) {
	pluginName := filepath.Base(path)
	hostSocket, err := m.prepareHostSocketLocked(pluginName)
	if err != nil {
		return nil, fmt.Errorf("prepare host socket: %w", err)
	}

	cmd, err := buildPluginCmd(path, manifest, manifestPath, hostSocket)
	if err != nil {
		m.closeHostSocketLocked(pluginName)
		return nil, fmt.Errorf("build plugin command: %w", err)
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
		m.closeHostSocketLocked(pluginName)
	}

	conn, err := client.Client()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("plugin client: %w", err)
	}

	grpcConn, err := GetPluginConn(conn)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("plugin does not use gRPC protocol: %w", err)
	}
	grpcClient := proto.NewPluginClient(grpcConn)

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
		cleanup()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != "" {
		cleanup()
		return nil, fmt.Errorf("plugin init error: %s", resp.Error)
	}
	if !isAPIVersionSupported(resp.ApiVersion) {
		cleanup()
		return nil, fmt.Errorf("unsupported api version: %s (supported: %v)", resp.ApiVersion, SupportedAPIVersions)
	}

	regCtx, regCancel := context.WithTimeout(ctx, 5*time.Second)
	defer regCancel()

	regs, err := grpcClient.ListRegistrations(regCtx, &proto.ListRegistrationsRequest{})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("list registrations: %w", err)
	}

	return &pluginStartup{
		client:     client,
		grpcClient: grpcClient,
		apiVersion: resp.ApiVersion,
		registrations: Registrations{
			Tools:              convertToolRegistrations(regs.Tools),
			Commands:           convertCommandRegistrations(regs.Commands),
			EventSubscriptions: regs.EventSubscriptions,
			CLIFlags:           convertCLIFlags(regs.CliFlags),
			ToolPrefix:         regs.ToolPrefix,
		},
		cleanup: cleanup,
	}, nil
}

// Handshake runs the plugin handshake protocol: starts the plugin subprocess,
// performs gRPC handshake, initializes, and lists registrations.
// It does not register the plugin into the Manager — use LoadPlugin for that.
func (m *Manager) Handshake(ctx context.Context, path string) (*HandshakeResult, error) {
	hash, manifest, manifestPath, err := m.handshakeMetadata(path)
	if err != nil {
		return nil, fmt.Errorf("load plugin metadata: %w", err)
	}

	timeouts := DefaultRPCTimeouts
	if manifest != nil {
		m.mu.Lock()
		shimErr := m.waitForShimReady(ctx, manifest)
		m.mu.Unlock()
		if shimErr != nil {
			return nil, shimErr
		}
		timeouts = manifestTimeouts(manifest)
	}
	started, err := m.startPlugin(ctx, path, manifest, manifestPath, timeouts)
	if err != nil {
		return nil, err
	}

	return &HandshakeResult{
		Name:          filepath.Base(path),
		APIVersion:    started.apiVersion,
		Registrations: started.registrations,
		ManifestPath:  manifestPath,
		Manifest:      manifest,
		Hash:          hash,
		Close:         started.cleanup,
	}, nil
}

// handshakeMetadata extracts the metadata needed for a plugin handshake
// without actually loading the plugin into the manager.
func (m *Manager) handshakeMetadata(path string) (hash string, manifest *Manifest, manifestPath string, err error) {
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
		manifest, parseErr = parseManifest(manifestPath)
		if parseErr != nil {
			// Log but continue without manifest.
			m.logger.Warn("plugin manifest parse failed", "path", manifestPath, "error", parseErr)
		}
	}

	if manifest != nil && manifest.HasRunCommand() {
		if manifestNeedsManagedShim(manifest) {
			if hash, err := computeManagedShimHash(manifestPath, manifest); err == nil {
				return hash, manifest, manifestPath, nil
			}
		}
		// Run-command plugins: hash the package directory so extension-file
		// changes are detected on reload. Falls back to the resolved command
		// binary when the dir can't be read.
		if dirHash, dirErr := computeDirHash(filepath.Dir(manifestPath)); dirErr == nil {
			return dirHash, manifest, manifestPath, nil
		}
		if resolved := resolveRunCommandPath(filepath.Dir(manifestPath), manifest.Run.Command); resolved != "" {
			hash, err = m.computeHash(resolved)
			if err != nil {
				return "", nil, "", fmt.Errorf("compute hash: %w", err)
			}
			return hash, manifest, manifestPath, nil
		}
	}
	hash, err = m.computeHash(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("compute hash: %w", err)
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
		logger:              logging.Discard(),
		shimInstallFn:       piext.NPMInstall,
	}

	// Default loadFn delegates to Load. Tests override to avoid subprocess overhead.
	m.loadFn = m.Load

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

// SetUI updates the UI implementation used for Host.UI* RPCs.
func (m *Manager) SetUI(impl ui.UI) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if impl == nil {
		m.ui = nil
		return
	}
	m.ui = &uiAdapter{ui: impl}
}

// Discover scans for plugins in the configured directories and loads them.
func (m *Manager) Discover(ctx context.Context) error {
	dirs := m.pluginDirs()

	for _, dir := range dirs {
		if err := m.scanDir(dir); err != nil {
			// Log but continue
			m.logger.Warn("plugin scan failed", "dir", dir, "error", err)
		}
	}

	// Scan for plugins in installed packages.
	pkgDirs := m.packageDirs()
	for _, dir := range pkgDirs {
		if err := m.scanPackagesDir(dir); err != nil {
			m.logger.Warn("plugin package scan failed", "dir", dir, "error", err)
		}
	}

	m.prepareShimIfNeeded(ctx)

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

// loadLocked loads a single plugin. Caller must hold m.mu.
func (m *Manager) loadLocked(ctx context.Context, path string, manifestPath string) (*PluginInfo, error) {
	var manifest *Manifest
	var err error
	if manifestPath != "" {
		manifest, err = parseManifest(manifestPath)
		if err != nil {
			m.logger.Warn("plugin manifest parse failed", "path", manifestPath, "error", err)
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
		if err := m.validatePluginConfig(filepath.Base(path), manifest.ConfigSchema.Schema); err != nil {
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

	info := &PluginInfo{
		Name:          filepath.Base(path),
		Path:          path,
		ManifestPath:  manifestPath,
		Manifest:      manifest,
		Hash:          hash,
		APIVersion:    started.apiVersion,
		Registrations: started.registrations,
		client:        started.client,
		grpcClient:    started.grpcClient,
		Cleanup:       started.cleanup,
	}

	m.plugins[info.Name] = info
	m.binaries[path] = hash
	if manifest != nil {
		m.failureConfig[path] = manifest.getFailureConfig()
	}
	m.registerPlugin(info)

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
	if manifest != nil && manifest.HasRunCommand() {
		if manifestNeedsManagedShim(manifest) {
			if hash, err := computeManagedShimHash(manifestPath, manifest); err == nil {
				return hash, nil
			}
		}
		manifestDir := filepath.Dir(manifestPath)
		if manifestPath == "" {
			manifestDir = filepath.Dir(path)
		}
		if dirHash, err := computeDirHash(manifestDir); err == nil {
			return dirHash, nil
		}
		if resolved := resolveRunCommandPath(manifestDir, manifest.Run.Command); resolved != "" {
			hash, err := m.computeHash(resolved)
			if err != nil {
				return "", fmt.Errorf("compute hash: %w", err)
			}
			return hash, nil
		}
	}

	hash, err := m.computeHash(path)
	if err != nil {
		return "", fmt.Errorf("compute hash: %w", err)
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
// Returns an error only after all attempts are exhausted.
func (m *Manager) AttemptLoadWithRetry(ctx context.Context, path string, manifestPath string) (*PluginInfo, error) {
	config := m.failureConfig[path]
	if config.MaxAttempts == 0 {
		config = DefaultFailureConfig
	}

	var lastErr error
	for attempt := 0; attempt < config.MaxAttempts; attempt++ {
		if attempt > 0 {
			// Calculate backoff: base * factor^attempt
			backoff := float64(config.BackoffBase) * pow(config.BackoffFactor, float64(attempt-1))
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("plugin load retry canceled: %w", ctx.Err())
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

		lastErr = err
		m.logger.Warn("plugin load attempt failed", "attempt", attempt+1, "max_attempts", config.MaxAttempts, "error", err)

		// Clean up failed load attempt resources.
		m.mu.Lock()
		if info := m.plugins[filepath.Base(path)]; info != nil && info.Cleanup != nil {
			pluginCleanup(info.Cleanup)
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

// ReattemptDeregistered re-attempts loading of plugins that were deregistered.
// Called by /reload to bring back plugins that previously exhausted retries.
func (m *Manager) ReattemptDeregistered(ctx context.Context) {
	m.mu.Lock()
	var toReattempt []struct {
		name         string
		path         string
		manifestPath string
	}
	for name, info := range m.plugins {
		if info.deregistered {
			toReattempt = append(toReattempt, struct {
				name         string
				path         string
				manifestPath string
			}{name, info.Path, info.ManifestPath})
			delete(m.plugins, name)
		}
	}
	m.mu.Unlock()

	for _, item := range toReattempt {
		m.logger.Info("plugin re-attempting deregistered plugin", "plugin", item.name)
		info, err := m.AttemptLoadWithRetry(ctx, item.path, item.manifestPath)
		if err != nil {
			m.logger.Warn("plugin re-attempt failed", "plugin", item.name, "error", err)
			m.mu.Lock()
			// Mark as deregistered again.
			if p, ok := m.plugins[item.name]; ok {
				p.deregistered = true
			} else {
				// Re-register with deregistered flag.
				m.plugins[item.name] = &PluginInfo{
					Name:         item.name,
					Path:         item.path,
					ManifestPath: item.manifestPath,
					deregistered: true,
				}
			}
			m.mu.Unlock()
		} else {
			_ = info // already registered via Load
		}
	}
}

// convertToolRegistrations converts proto registrations to internal type.
func convertToolRegistrations(tools []*proto.ToolRegistration) []ToolRegistration {
	result := make([]ToolRegistration, 0, len(tools))
	for _, t := range tools {
		result = append(result, ToolRegistration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
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
			return m.executeTool(ctx, pluginName, toolName, toolCallID, arguments)
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
			Handler: func(name string, payload json.RawMessage, sessionID string) EventResponse {
				return m.handleEvent(pluginName, eventName, payload, sessionID)
			},
		})
	}

	// Initialize CLI flags with their default values.
	// These are injected into the config namespace so Host.GetConfig returns them.
	flags := make(map[string]string)
	for _, flag := range info.Registrations.CLIFlags {
		flags[flag.Name] = flag.DefaultValue
	}
	m.cliFlags[pluginName] = flags
}

// executeTool executes a tool call on a plugin.
func (m *Manager) executeTool(ctx context.Context, pluginName, toolName, toolCallID, arguments string) (string, bool, error) {
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
	execCtx, cancel := context.WithTimeout(ctx, timeouts.ExecuteTool)
	defer cancel()

	result, err := grpcClient.ExecuteTool(execCtx, &proto.ToolCall{
		ToolName:   toolName,
		ToolCallId: toolCallID,
		Arguments:  arguments,
	})
	if err != nil {
		return "", true, fmt.Errorf("execute tool: %w", err)
	}

	if result.IsError {
		return result.Output, true, fmt.Errorf("%s", result.ErrorMessage)
	}
	return result.Output, false, nil
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
func (m *Manager) handleEvent(pluginName, eventName string, payload json.RawMessage, sessionID string) EventResponse {
	ctx := context.Background()

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
	// Re-attempt loading any plugins that were previously deregistered.
	m.ReattemptDeregistered(ctx)
	return m.ReloadNamed(ctx, "")
}

// ReloadNamed rehashes plugins and restarts those matching name (or all if name is empty).
// If name is non-empty, the matching plugin is force-reloaded even if its hash unchanged.
func (m *Manager) ReloadNamed(ctx context.Context, name string) error {
	// Scan directories to discover current binaries and hashes.
	dirs := m.pluginDirs()
	for _, dir := range dirs {
		if err := m.scanDir(dir); err != nil {
			m.logger.Warn("plugin scan failed", "dir", dir, "error", err)
		}
	}
	pkgDirs := m.packageDirs()
	for _, dir := range pkgDirs {
		if err := m.scanPackagesDir(dir); err != nil {
			m.logger.Warn("plugin package scan failed", "dir", dir, "error", err)
		}
	}

	m.prepareShimIfNeeded(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check each loaded plugin for hash changes or force restart.
	for pluginName, info := range m.plugins {
		// If name is specified and doesn't match, skip.
		if name != "" && pluginName != name {
			continue
		}

		newHash, exists := m.binaries[info.Path]
		if !exists {
			// Binary no longer exists, shut down and remove.
			m.logger.Info("plugin binary no longer found; shutting down", "plugin", pluginName)
			if err := m.shutdownPluginLocked(ctx, pluginName, info, DefaultDrainTimeout); err != nil {
				m.logger.Warn("plugin shutdown failed", "plugin", pluginName, "error", err)
			}
			m.unregisterPluginLocked(info)
			delete(m.plugins, pluginName)
			continue
		}

		if newHash != info.Hash || name != "" {
			// Hash changed, or this is a named (forced) reload — restart this plugin.
			if name != "" {
				m.logger.Info("plugin force-reloading", "plugin", pluginName)
			} else {
				m.logger.Info("plugin hash changed; reloading", "plugin", pluginName)
			}
			if err := m.shutdownPluginLocked(ctx, pluginName, info, DefaultDrainTimeout); err != nil {
				m.logger.Warn("plugin shutdown failed", "plugin", pluginName, "error", err)
			}
			m.unregisterPluginLocked(info)
			delete(m.plugins, pluginName)

			// Reload the plugin.
			if _, err := m.loadLocked(ctx, info.Path, m.manifestPaths[info.Path]); err != nil {
				m.logger.Warn("plugin reload failed", "plugin", pluginName, "error", err)
			}
		}
	}

	// Load any new plugins discovered during scanning.
	return m.autoLoadLocked(ctx)
}

// PluginShutdowner allows a plugin to be shut down gracefully.
type PluginShutdowner interface {
	ShutdownPlugin(ctx context.Context, name string, drainTimeout time.Duration) error
}

// Shutdown gracefully shuts down all plugins via their Shutdown RPC.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, info := range m.plugins {
		if err := m.shutdownPluginLocked(ctx, name, info, DefaultDrainTimeout); err != nil {
			m.logger.Warn("plugin shutdown failed", "plugin", name, "error", err)
		}
	}

	return nil
}

// GracePeriod is the time to wait after SIGTERM before sending SIGKILL.
const GracePeriod = 2 * time.Second

// shutdownPluginLocked calls the Shutdown RPC on a plugin.
// Caller must hold m.mu.
func (m *Manager) shutdownPluginLocked(ctx context.Context, name string, info *PluginInfo, drainTimeout time.Duration) error {
	// If we don't have a gRPC connection, cleanup without blocking the caller.
	if info.Cleanup == nil || info.grpcClient == nil {
		if info.Cleanup != nil {
			pluginCleanup(info.Cleanup)
		}
		return nil
	}

	// Attempt cooperative shutdown via Shutdown RPC.
	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	resp, err := info.grpcClient.Shutdown(drainCtx, &proto.ShutdownRequest{
		DrainTimeoutSeconds: int32(drainTimeout.Seconds()),
	})

	// Cancel the drain context to stop waiting.
	cancel()

	// If the plugin responded gracefully, cleanup and return.
	if err == nil && resp.Graceful {
		m.logger.Debug("plugin shut down gracefully", "plugin", name)
		pluginCleanup(info.Cleanup)
		return nil
	}

	// Plugin didn't shut down gracefully within timeout or returned an error.
	// Send SIGTERM.
	m.logger.Warn("plugin did not shut down gracefully; sending SIGTERM", "plugin", name, "error", err)

	// Get the process to signal.
	// The plugin.Client's process handling is internal, so we use Cleanup which kills it.
	// For more graceful handling, we'd need to track the process directly.
	pluginCleanup(info.Cleanup)

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

	return m.shutdownPluginLocked(ctx, name, info, drainTimeout)
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
	"session_before_fork":    true,
	"session_before_compact": true,
}

// IsCancellableEvent reports whether a plugin returning Block=true for
// the named event should abort the originating operation.
func IsCancellableEvent(name string) bool { return CancellableEvents[name] }

// DispatchEvent sends an event to all subscribed plugins.
// Returns true if any plugin returned a block verdict for a cancellable
// event (see CancellableEvents).
func (m *Manager) DispatchEvent(name string, payload json.RawMessage, sessionID string) bool {
	return m.DispatchEventResponse(name, payload, sessionID).Block
}

// DispatchEventResponse sends an event to all subscribed plugins and returns
// the aggregated response. For cancellable events, the first block verdict is
// propagated. For compaction_request, the first non-nil Compaction result is
// propagated.
func (m *Manager) DispatchEventResponse(name string, payload json.RawMessage, sessionID string) EventResponse {
	responses := m.DispatchEventCollect(name, payload, sessionID)

	resp := EventResponse{}
	for _, r := range responses {
		// Propagate block for cancellable events (tool_call,
		// session_before_fork, session_before_compact). The first block wins.
		if r.Block && IsCancellableEvent(name) {
			resp.Block = true
			resp.BlockReason = r.BlockReason
			break
		}

		// Propagate compaction result for compaction_request events.
		if name == "compaction_request" && r.Compaction != nil && resp.Compaction == nil {
			resp.Compaction = r.Compaction
		}
	}
	return resp
}

// DispatchEventCollect sends an event to all subscribed plugins and returns
// every handler's response in dispatch (registration) order. Use this for
// events whose per-plugin responses matter individually, e.g. chaining
// before_agent_start system-prompt rewrites.
func (m *Manager) DispatchEventCollect(name string, payload json.RawMessage, sessionID string) []EventResponse {
	m.mu.RLock()
	handlers := m.eventHandlers[name]
	m.mu.RUnlock()

	responses := make([]EventResponse, 0, len(handlers))
	for _, handler := range handlers {
		responses = append(responses, handler.Handler(name, payload, sessionID))
	}
	return responses
}

// DispatchCompactionHook dispatches a compaction_request event to all subscribed
// plugins. Returns the first successful result, or an empty result if no plugin
// handled the event.
func (m *Manager) DispatchCompactionHook(transcript string, tokensBefore int64, sessionID string) proto.CompactionHookResult {
	// Build the compaction request payload.
	payload := struct {
		Transcript   string `json:"transcript"`
		TokensBefore int64  `json:"tokens_before"`
	}{transcript, tokensBefore}
	payloadBytes, _ := json.Marshal(payload)

	resp := m.DispatchEventResponse("compaction_request", payloadBytes, sessionID)
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
	Name        string
	Description string
	Parameters  map[string]any
	Handler     ToolHandler
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
	subs := append([]chan<- ToolProgressEvent(nil), m.progressSubscribers[event.ToolCallID]...)
	m.progressMu.RUnlock()

	for _, sub := range subs {
		select {
		case sub <- event:
		default:
		}
	}
}
