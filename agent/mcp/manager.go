package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/losinggeneration/tau/agent"
	"github.com/losinggeneration/tau/ai"
)

// Status is a server's connection state.
type Status string

const (
	StatusDisabled   Status = "disabled"
	StatusConnecting Status = "connecting"
	StatusConnected  Status = "connected"
	StatusFailed     Status = "failed"
	StatusNeedsAuth  Status = "needs-auth"
	StatusClosed     Status = "closed"
)

const (
	defaultTimeout   = 60 * time.Second
	maxToolNameLen   = 64
	stderrTailLength = 2000
)

// ServerState is a snapshot of one server for status displays.
type ServerState struct {
	Name  string
	Scope Scope
	// Source is the mcp.json that defines the server.
	Source string
	// Transport is the server URL, or its command and arguments.
	Transport string
	// Override is the project mcp.json overriding this global server.
	Override string
	// ProjectConfig is the trusted project's mcp.json when it could override
	// this global server, which has no override yet.
	ProjectConfig string
	// OAuth reports a server that signs in with /mcp login.
	OAuth    bool
	Status   Status
	Error    string
	Exposure Exposure
	Tools    []ToolState
	// Resources reports the resources capability; resources aren't listed on
	// connect.
	Resources bool
}

// ToolState is one tool a connected server offers.
type ToolState struct {
	Name        string
	Description string
	Exposure    Exposure
}

// TransportFactory opens the transport for a server. cwd is the session's
// working directory.
type TransportFactory func(entry ServerEntry, cwd string) (sdkmcp.Transport, error)

// Option configures a Manager.
type Option func(*Manager)

// WithTransport replaces the stdio and streamable HTTP transports, for tests.
func WithTransport(f TransportFactory) Option { return func(m *Manager) { m.transport = f } }

// WithCredentials signs OAuth servers in with credentials from store.
func WithCredentials(store *CredentialStore) Option {
	return func(m *Manager) { m.credentials = store }
}

// WithOnChange calls fn after every server state change.
func WithOnChange(fn func()) Option { return func(m *Manager) { m.onChange = fn } }

// Manager connects configured servers in the background and exposes their
// tools.
type Manager struct {
	cwd         string
	version     string
	transport   TransportFactory
	onChange    func()
	credentials *CredentialStore

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu            sync.Mutex
	servers       []*server
	names         map[string]string
	closed        bool
	projectConfig string
}

type server struct {
	entry     ServerEntry
	status    Status
	err       string
	session   *sdkmcp.ClientSession
	tools     []agent.Tool
	offered   []*sdkmcp.Tool
	resources bool
	ready     chan struct{}
	// attempt identifies the current connection attempt; a superseded
	// attempt discards its session.
	attempt  int
	auth     *tokenAuth
	progress sync.Map // progress token -> func(*sdkmcp.ProgressNotificationParams)
}

// NewManager prepares the configured servers without connecting them.
func NewManager(cfg Config, cwd, version string, opts ...Option) *Manager {
	m := &Manager{cwd: cwd, version: version, transport: defaultTransport, names: map[string]string{}, projectConfig: cfg.ProjectConfig}
	for _, opt := range opts {
		opt(m)
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for _, entry := range cfg.Servers {
		s := &server{entry: entry, status: StatusDisabled, ready: make(chan struct{})}
		if m.credentials != nil && usesOAuth(entry.Config) {
			s.auth = newTokenAuth(m.ctx, m.credentials, entry.Name, entry.Config.URL)
		}
		if !entry.Config.IsEnabled() {
			close(s.ready)
		}
		m.servers = append(m.servers, s)
	}
	return m
}

// Start connects every enabled server in the background.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for _, s := range m.servers {
		if !s.entry.Config.IsEnabled() || s.status != StatusDisabled {
			continue
		}
		m.connectLocked(s)
	}
	m.changedLocked()
}

// connectLocked starts a connection attempt that supersedes any other.
func (m *Manager) connectLocked(s *server) {
	s.attempt++
	s.status = StatusConnecting
	m.wg.Add(1)
	go m.connect(s, s.entry, s.ready, s.attempt)
}

func (m *Manager) changedLocked() {
	if m.onChange != nil {
		go m.onChange()
	}
}

func (m *Manager) connect(s *server, entry ServerEntry, ready chan struct{}, attempt int) {
	defer m.wg.Done()
	defer close(ready)
	session, tools, resources, err := m.open(s, entry)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || s.attempt != attempt {
		if session != nil {
			if err := session.Close(); err != nil {
				s.err = fmt.Sprintf("close MCP server %q: %v", s.entry.Name, err)
			}
		}
		return
	}
	if errors.Is(err, ErrNeedsAuth) {
		s.status, s.err = StatusNeedsAuth, ""
		m.changedLocked()
		return
	}
	if err != nil {
		s.status, s.err = StatusFailed, fmt.Sprintf("MCP server %q failed to connect: %v", s.entry.Name, err)
		m.changedLocked()
		return
	}
	s.status, s.session, s.resources, s.offered = StatusConnected, session, resources, tools
	s.tools = m.adaptToolsLocked(s, tools)
	m.changedLocked()
}

func (m *Manager) open(s *server, entry ServerEntry) (*sdkmcp.ClientSession, []*sdkmcp.Tool, bool, error) {
	transport, err := m.transport(entry, m.cwd)
	if err != nil {
		return nil, nil, false, err
	}
	if t, ok := transport.(*sdkmcp.StreamableClientTransport); ok && s.auth != nil {
		t.OAuthHandler = s.auth
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "tau", Version: m.version}, &sdkmcp.ClientOptions{
		Capabilities: &sdkmcp.ClientCapabilities{},
		ProgressNotificationHandler: func(_ context.Context, req *sdkmcp.ProgressNotificationClientRequest) {
			if fn, ok := s.progress.Load(req.Params.ProgressToken); ok {
				fn.(func(*sdkmcp.ProgressNotificationParams))(req.Params)
			}
		},
	})
	ctx, cancel := context.WithTimeout(m.ctx, s.timeout())
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, false, withStderr(err, transport)
	}
	var tools []*sdkmcp.Tool
	if caps := session.InitializeResult().Capabilities; caps != nil && caps.Tools != nil {
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return nil, nil, false, errors.Join(fmt.Errorf("list tools: %w", err), session.Close())
			}
			tools = append(tools, tool)
		}
	}
	caps := session.InitializeResult().Capabilities
	return session, tools, caps != nil && caps.Resources != nil, nil
}

func (s *server) timeout() time.Duration {
	if s.entry.Config.Timeout > 0 {
		return time.Duration(s.entry.Config.Timeout * float64(time.Second))
	}
	return defaultTimeout
}

var invalidToolChars = regexp.MustCompile(`[^A-Za-z0-9_]`)

// toolName is mcp__<server>__<tool> with everything but [A-Za-z0-9_] made
// `_`, shortened with a hash suffix when too long or when another tool
// already sanitized to the same name.
func toolName(server, tool string, taken func(string) bool) string {
	name := invalidToolChars.ReplaceAllString("mcp__"+server+"__"+tool, "_")
	if len(name) <= maxToolNameLen && !taken(name) {
		return name
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	hash := hex.EncodeToString(sum[:])[:8]
	return name[:min(len(name), maxToolNameLen-len(hash)-1)] + "_" + hash
}

func (m *Manager) adaptToolsLocked(s *server, tools []*sdkmcp.Tool) []agent.Tool {
	namespace := &agent.ToolNamespace{
		Name:         Namespace(s.entry.Name),
		Description:  s.entry.Config.Description,
		Instructions: s.session.InitializeResult().Instructions,
	}
	out := make([]agent.Tool, 0, len(tools))
	for _, tool := range tools {
		owner := s.entry.Name + "\x00" + tool.Name
		name := toolName(s.entry.Name, tool.Name, func(n string) bool {
			o, ok := m.names[n]
			return ok && o != owner
		})
		m.names[name] = owner
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			description = tool.Title
		}
		if description == "" && tool.Annotations != nil {
			description = tool.Annotations.Title
		}
		if description == "" {
			description = fmt.Sprintf("MCP tool %s from server %s", tool.Name, s.entry.Name)
		}
		out = append(out, agent.Tool{
			Name:         name,
			Label:        s.entry.Name + "/" + tool.Name,
			Description:  description,
			Parameters:   toParameters(tool.InputSchema),
			OutputSchema: resultSchema,
			Exposure:     toolExposure(s.entry.Config.ToolExposureFor(tool.Name)),
			Namespace:    namespace,
			Execute:      m.executor(s, tool.Name),
		})
	}
	return out
}

// toolExposure maps an MCP exposure onto agent exposure. Codemode tools are
// deferred, like upstream: neither is declared until something loads them.
func toolExposure(e Exposure) agent.ToolExposure {
	if e == ExposureCodemode {
		return agent.ToolDeferred
	}
	return agent.ToolExposure(e)
}

// toParameters makes an input schema an object with properties, which some
// providers require.
func toParameters(schema any) map[string]any {
	params := map[string]any{}
	if m, ok := schema.(map[string]any); ok {
		for k, v := range m {
			params[k] = v
		}
	}
	if _, ok := params["type"]; !ok {
		params["type"] = "object"
	}
	if _, ok := params["properties"]; !ok {
		params["properties"] = map[string]any{}
	}
	return params
}

// resultSchema describes the CallToolResult scripts receive as structured
// content.
// ponytail: structuredContent accepts any value instead of embedding the
// tool's output schema, so scripts don't get the server's own schema checked.
var resultSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"content": map[string]any{"type": "array", "items": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"type": map[string]any{"enum": []any{"text", "image", "audio", "resource_link", "resource"}}},
			"required":             []any{"type"},
			"additionalProperties": true,
		}},
		"structuredContent": map[string]any{},
		"isError":           map[string]any{"type": "boolean"},
	},
	"required":             []any{"content"},
	"additionalProperties": true,
}

func (m *Manager) executor(s *server, tool string) agent.ToolFunc {
	return func(tc agent.ToolContext) (*agent.ToolResult, error) {
		parent := tc.Context
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithCancelCause(parent)
		defer cancel(nil)
		timeout := s.timeout()
		timer := time.AfterFunc(timeout, func() {
			cancel(fmt.Errorf("MCP tool %s/%s timed out after %s", s.entry.Name, tool, timeout))
		})
		defer timer.Stop()
		token := uuid.NewString()
		s.progress.Store(token, func(p *sdkmcp.ProgressNotificationParams) {
			timer.Reset(timeout)
			if tc.OnUpdate == nil {
				return
			}
			text := p.Message
			if text == "" {
				text = fmt.Sprintf("Progress %v", p.Progress)
				if p.Total > 0 {
					text += fmt.Sprintf("/%v", p.Total)
				}
			}
			tc.OnUpdate(&agent.ToolResult{Content: text})
		})
		defer s.progress.Delete(token)

		m.mu.Lock()
		session := s.session
		m.mu.Unlock()
		if session == nil {
			return nil, fmt.Errorf("MCP server %q is not connected", s.entry.Name)
		}
		// A call cancelled or timed out before it is sent is never sent.
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		params := &sdkmcp.CallToolParams{Name: tool, Arguments: tc.Args}
		if params.Arguments == nil {
			params.Arguments = map[string]any{}
		}
		params.SetProgressToken(token)
		result, err := session.CallTool(ctx, params)
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		if err != nil {
			return nil, err
		}
		return convertResult(s.entry.Name, tool, result, s.resources)
	}
}

// Tools returns the tools of connected servers.
func (m *Manager) Tools() []agent.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var tools []agent.Tool
	for _, s := range m.servers {
		tools = append(tools, s.tools...)
	}
	return tools
}

// States returns a snapshot of every configured server.
func (m *Manager) States() []ServerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := make([]ServerState, 0, len(m.servers))
	for _, s := range m.servers {
		exposure := s.entry.Config.Exposure
		if exposure == "" {
			exposure = ExposureCodemode
		}
		state := ServerState{
			Name: s.entry.Name, Scope: s.entry.Scope, Source: s.entry.Source, Transport: describeTransport(s.entry.Config),
			Override: s.entry.Override, OAuth: s.auth != nil, Status: s.status, Error: s.err,
			Exposure: exposure, Resources: s.resources,
		}
		if s.entry.Scope == ScopeGlobal && s.entry.Override == "" {
			state.ProjectConfig = m.projectConfig
		}
		for _, tool := range s.offered {
			state.Tools = append(state.Tools, ToolState{Name: tool.Name, Description: strings.TrimSpace(tool.Description), Exposure: s.entry.Config.ToolExposureFor(tool.Name)})
		}
		states = append(states, state)
	}
	return states
}

// WaitDirect waits until every server that can expose direct tools has
// connected or failed, or ctx ends.
func (m *Manager) WaitDirect(ctx context.Context) error {
	m.mu.Lock()
	var waits []chan struct{}
	for _, s := range m.servers {
		if s.mayExposeDirect() {
			waits = append(waits, s.ready)
		}
	}
	m.mu.Unlock()
	for _, ready := range waits {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *server) mayExposeDirect() bool {
	if s.entry.Config.Exposure == ExposureDirect {
		return true
	}
	return slices.ContainsFunc(s.entry.Config.ToolExposure, func(o ToolExposure) bool { return o.Exposure == ExposureDirect })
}

// Close stops pending connections and closes every session. It waits for
// connection attempts to settle.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, s := range m.servers {
		if s.session != nil {
			if err := s.session.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close MCP server %q: %w", s.entry.Name, err))
			}
			s.session = nil
		}
		s.tools, s.offered = nil, nil
		if s.status != StatusDisabled {
			s.status = StatusClosed
		}
	}
	m.changedLocked()
	return errors.Join(errs...)
}

func defaultTransport(entry ServerEntry, cwd string) (sdkmcp.Transport, error) {
	ctx := context.Background()
	cfg := entry.Config
	if cfg.URL != "" {
		headers := http.Header{}
		for k, v := range cfg.Headers {
			resolved, err := ai.ResolveConfigValue(ctx, v, nil)
			if err != nil {
				return nil, fmt.Errorf("header %q: %w", k, err)
			}
			headers.Set(k, resolved)
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{headers: headers, next: http.DefaultTransport}},
		}, nil
	}
	cmd := exec.Command(expandHome(cfg.Command), mapSlice(cfg.Args, expandHome)...)
	cmd.Dir = cwd
	if cfg.CWD != "" {
		dir := expandHome(cfg.CWD)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		resolved, err := ai.ResolveConfigValue(ctx, v, nil)
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", k, err)
		}
		cmd.Env = append(cmd.Env, k+"="+resolved)
	}
	tail := &stderrTail{}
	cmd.Stderr = tail
	return &stdioTransport{CommandTransport: sdkmcp.CommandTransport{Command: cmd}, stderr: tail}, nil
}

func mapSlice(in []string, fn func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fn(s)
	}
	return out
}

// expandHome expands ~ and ~/ like a shell.
func expandHome(value string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	if value == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(value, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return value
}

type headerTransport struct {
	headers http.Header
	next    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header[k] = v
	}
	return t.next.RoundTrip(req)
}

type stdioTransport struct {
	sdkmcp.CommandTransport
	stderr *stderrTail
}

// stderrTail keeps the end of a server's stderr for connection errors.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTailLength {
		t.buf = t.buf[len(t.buf)-stderrTailLength:]
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func withStderr(err error, transport sdkmcp.Transport) error {
	if st, ok := transport.(*stdioTransport); ok {
		if tail := st.stderr.String(); tail != "" {
			return fmt.Errorf("%w\n%s", err, tail)
		}
	}
	return err
}

func (m *Manager) find(name string) *server {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if s.entry.Name == name {
			return s
		}
	}
	return nil
}

// Reconnect drops the server's session and connects it again, for example
// after signing in. It returns once the attempt settles.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	s := m.find(name)
	if s == nil {
		return fmt.Errorf("MCP server %q is not configured", name)
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return fmt.Errorf("MCP server %q is shut down", name)
		}
		if !s.entry.Config.IsEnabled() {
			m.mu.Unlock()
			return fmt.Errorf("MCP server %q is disabled", name)
		}
		if s.status != StatusConnecting {
			break
		}
		ready := s.ready
		m.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	old := s.session
	s.session, s.tools, s.offered, s.err = nil, nil, nil, ""
	s.ready = make(chan struct{})
	ready := s.ready
	m.connectLocked(s)
	m.changedLocked()
	m.mu.Unlock()
	var err error
	if old != nil {
		if closeErr := old.Close(); closeErr != nil {
			err = fmt.Errorf("close MCP server %q: %w", name, closeErr)
		}
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.status == StatusFailed {
		return errors.Join(err, errors.New(s.err))
	}
	return err
}

// SignOut removes the server's stored credentials and disconnects it. It
// reports whether credentials were stored.
func (m *Manager) SignOut(ctx context.Context, name string) (bool, error) {
	s := m.find(name)
	if s == nil || s.auth == nil {
		return false, nil
	}
	removed, err := m.credentials.Remove(ctx, name, s.entry.Config.URL)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	old := s.session
	s.session, s.tools, s.offered, s.err = nil, nil, nil, ""
	if !m.closed && s.entry.Config.IsEnabled() && s.status != StatusConnecting {
		s.status = StatusNeedsAuth
	}
	m.changedLocked()
	m.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			return removed, fmt.Errorf("close MCP server %q: %w", name, err)
		}
	}
	return removed, nil
}

func describeTransport(c ServerConfig) string {
	if c.URL != "" {
		return c.URL
	}
	return strings.Join(append([]string{c.Command}, c.Args...), " ")
}

// Configure saves patch to the mcp.json that defines or overrides the server,
// or with patch.Project as an override in the trusted project's mcp.json, then
// applies it: disabling disconnects, enabling connects and a new exposure
// re-exposes connected tools.
func (m *Manager) Configure(name string, patch ConfigPatch) error {
	s := m.find(name)
	if s == nil {
		return fmt.Errorf("MCP server %q is not configured", name)
	}
	m.mu.Lock()
	path := cmp.Or(s.entry.Override, s.entry.Source)
	if patch.Project {
		path = m.projectConfig
	}
	m.mu.Unlock()
	if path == "" {
		return errors.New("no trusted project mcp.json to save to")
	}
	if err := updateServerConfig(path, name, patch, patch.Project); err != nil {
		return fmt.Errorf("could not update %s: %w", path, err)
	}

	m.mu.Lock()
	var old *sdkmcp.ClientSession
	if patch.Project {
		s.entry.Override = path
	}
	if patch.Exposure != "" {
		s.entry.Config.Exposure = patch.Exposure
		if s.status == StatusConnected {
			s.tools = m.adaptToolsLocked(s, s.offered)
		}
	}
	if patch.Enabled != nil {
		enabled := *patch.Enabled
		s.entry.Config.Enabled = &enabled
		switch {
		case !enabled && s.status != StatusDisabled:
			old = s.session
			s.attempt++
			s.session, s.tools, s.offered, s.err, s.status = nil, nil, nil, "", StatusDisabled
		case enabled && s.status == StatusDisabled && !m.closed:
			s.ready = make(chan struct{})
			m.connectLocked(s)
		}
	}
	m.changedLocked()
	m.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			return fmt.Errorf("close MCP server %q: %w", name, err)
		}
	}
	return nil
}
