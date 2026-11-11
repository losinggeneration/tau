package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/losinggeneration/tau/agent/plugins/proto"
	"github.com/losinggeneration/tau/agent/plugins/state"
	"github.com/losinggeneration/tau/ai"
	"github.com/xeipuuv/gojsonschema"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// HostService implements the Host service that plugins call.
type HostService struct {
	proto.UnimplementedHostServer
	manager     *Manager
	stateMu     sync.Mutex
	stateStores map[string]*state.Store
}

// NewHostService creates a new Host service.
func NewHostService(m *Manager) *HostService {
	return &HostService{manager: m}
}

// getStateStore returns the state store for a plugin, creating it if needed.
func (h *HostService) getStateStore(pluginName string) (*state.Store, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if store, ok := h.stateStores[pluginName]; ok {
		return store, nil
	}

	store, err := state.NewStore(h.manager.stateDir, pluginName)
	if err != nil {
		return nil, err
	}
	if h.stateStores == nil {
		h.stateStores = make(map[string]*state.Store)
	}
	h.stateStores[pluginName] = store
	return store, nil
}

// getPluginName extracts the plugin name from gRPC metadata.
// The plugin should set metadata with key "tau-plugin-name" in each request.
func getPluginName(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if names := md.Get("tau-plugin-name"); len(names) > 0 {
			return names[0]
		}
	}
	return ""
}

// UIConfirm implements Host.UIConfirm.
func (h *HostService) UIConfirm(ctx context.Context, req *proto.UIConfirmRequest) (*proto.UIConfirmResponse, error) {
	if h.manager.ui == nil {
		return &proto.UIConfirmResponse{Confirmed: false, Error: "no UI available"}, nil
	}
	confirmed, err := h.manager.ui.Confirm(ctx, req.Title, req.Message)
	return &proto.UIConfirmResponse{
		Confirmed: confirmed,
		Error:     errToString(err),
	}, nil
}

// UISelect implements Host.UISelect.
func (h *HostService) UISelect(ctx context.Context, req *proto.UISelectRequest) (*proto.UISelectResponse, error) {
	if h.manager.ui == nil {
		return &proto.UISelectResponse{SelectedIndex: -1, Error: "no UI available"}, nil
	}
	idx, err := h.manager.ui.Select(ctx, req.Title, req.Options)
	return &proto.UISelectResponse{
		SelectedIndex: int32(idx),
		Error:         errToString(err),
	}, nil
}

// UIInput implements Host.UIInput.
func (h *HostService) UIInput(ctx context.Context, req *proto.UIInputRequest) (*proto.UIInputResponse, error) {
	if h.manager.ui == nil {
		return &proto.UIInputResponse{Value: "", Error: "no UI available"}, nil
	}
	value, err := h.manager.ui.Input(ctx, req.Title, req.Placeholder)
	return &proto.UIInputResponse{
		Value: value,
		Error: errToString(err),
	}, nil
}

// UINotify implements Host.UINotify.
func (h *HostService) UINotify(ctx context.Context, req *proto.UINotifyRequest) (*proto.UINotifyResponse, error) {
	if h.manager.ui != nil {
		h.manager.ui.Notify(int(req.Level), req.Message)
	}
	return &proto.UINotifyResponse{}, nil
}

// UISetStatus implements Host.UISetStatus.
func (h *HostService) UISetStatus(ctx context.Context, req *proto.UISetStatusRequest) (*proto.UISetStatusResponse, error) {
	// Routed through the manager so the value is retained for a UI that has
	// not attached yet (see Manager.SetUI).
	h.manager.uiSetStatus(req.Key, req.Message)
	return &proto.UISetStatusResponse{}, nil
}

// UISetWidget implements Host.UISetWidget.
func (h *HostService) UISetWidget(ctx context.Context, req *proto.UISetWidgetRequest) (*proto.UISetWidgetResponse, error) {
	h.manager.uiSetWidget(req.Key, req.Lines)
	return &proto.UISetWidgetResponse{}, nil
}

func (h *HostService) UISetTitle(ctx context.Context, req *proto.UISetTitleRequest) (*proto.UISetTitleResponse, error) {
	if h.manager.ui != nil {
		h.manager.ui.SetTitle(req.Title)
	}
	return &proto.UISetTitleResponse{}, nil
}

func (h *HostService) UISetEditorText(ctx context.Context, req *proto.UISetEditorTextRequest) (*proto.UISetEditorTextResponse, error) {
	if h.manager.ui != nil {
		h.manager.ui.SetEditorText(req.Text)
	}
	return &proto.UISetEditorTextResponse{}, nil
}

func (h *HostService) UISetToolsExpanded(ctx context.Context, req *proto.UISetToolsExpandedRequest) (*proto.UISetToolsExpandedResponse, error) {
	if h.manager.ui != nil {
		h.manager.ui.SetToolsExpanded(req.Expanded)
	}
	return &proto.UISetToolsExpandedResponse{}, nil
}

// Log implements Host.Log.
func (h *HostService) Log(ctx context.Context, req *proto.LogRequest) (*proto.LogResponse, error) {
	level := slog.LevelInfo
	switch req.Level {
	case 0:
		level = slog.LevelDebug
	case 2:
		level = slog.LevelWarn
	case 3:
		level = slog.LevelError
	}
	h.manager.logger.Log(ctx, level, "plugin log", "plugin", getPluginName(ctx), "message", req.Message)
	return &proto.LogResponse{}, nil
}

// GetConfig implements Host.GetConfig.
// It returns CLI flag values (if resolved) before settings.json values.
func (h *HostService) GetConfig(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
	pluginName := getPluginName(ctx)
	if pluginName == "" {
		return &proto.GetConfigResponse{Exists: false}, fmt.Errorf("plugin name not found in metadata")
	}

	h.manager.mu.RLock()
	defaults := maps.Clone(h.manager.cliFlags[pluginName])
	overrides := maps.Clone(h.manager.cliFlagOverrides[pluginName])
	settings := h.manager.settings
	var configured json.RawMessage
	if settings != nil {
		configured = slices.Clone(settings.Plugins[pluginName])
	}
	h.manager.mu.RUnlock()
	values := make(map[string]json.RawMessage)
	for key, value := range defaults {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode plugin default: %w", err)
		}
		values[key] = encoded
	}
	if len(configured) > 0 {
		var config map[string]json.RawMessage
		if err := json.Unmarshal(configured, &config); err != nil {
			return nil, fmt.Errorf("parse plugin config: %w", err)
		}
		maps.Copy(values, config)
	}
	for key, value := range overrides {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode plugin CLI value: %w", err)
		}
		values[key] = encoded
	}
	if req.Key != "" {
		value, exists := values[req.Key]
		return &proto.GetConfigResponse{Value: string(value), Exists: exists}, nil
	}
	if len(values) == 0 && len(configured) == 0 {
		return &proto.GetConfigResponse{}, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode plugin config: %w", err)
	}
	return &proto.GetConfigResponse{Value: string(encoded), Exists: true}, nil
}

// ToolProgress implements Host.ToolProgress.
// It forwards the event to the manager's progress channel if configured.
func (h *HostService) ToolProgress(ctx context.Context, req *proto.ToolProgressEvent) (*proto.ToolProgressResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	var records []ai.NestedToolCall
	if req.NestedCalls != "" {
		if err := json.Unmarshal([]byte(req.NestedCalls), &records); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "decode nested progress: %v", err)
		}
		records = slices.Clone(records[max(0, len(records)-128):])
	}
	h.manager.broadcastToolProgress(ToolProgressEvent{
		NestedCalls: records,
		ToolCallID:  req.ToolCallId,
		Content:     req.Content,
		IsError:     req.IsError,
	})
	return &proto.ToolProgressResponse{}, nil
}

// StateGet implements Host.StateGet.
func (h *HostService) StateGet(ctx context.Context, req *proto.StateGetRequest) (*proto.StateGetResponse, error) {
	pluginName := getPluginName(ctx)
	if pluginName == "" {
		return &proto.StateGetResponse{Exists: false}, fmt.Errorf("plugin name not found in metadata")
	}
	store, err := h.getStateStore(pluginName)
	if err != nil {
		return &proto.StateGetResponse{Exists: false}, fmt.Errorf("get state store: %w", err)
	}

	value, ok := store.Get(req.Key)
	return &proto.StateGetResponse{
		Value:  value,
		Exists: ok,
	}, nil
}

// StateSet implements Host.StateSet.
func (h *HostService) StateSet(ctx context.Context, req *proto.StateSetRequest) (*proto.StateSetResponse, error) {
	pluginName := getPluginName(ctx)
	if pluginName == "" {
		return &proto.StateSetResponse{}, fmt.Errorf("plugin name not found in metadata")
	}
	store, err := h.getStateStore(pluginName)
	if err != nil {
		return &proto.StateSetResponse{}, fmt.Errorf("get state store: %w", err)
	}

	if err := store.Set(req.Key, req.Value); err != nil {
		return &proto.StateSetResponse{}, err
	}
	return &proto.StateSetResponse{}, nil
}

// StateAppend implements Host.StateAppend.
func (h *HostService) StateAppend(ctx context.Context, req *proto.StateAppendRequest) (*proto.StateAppendResponse, error) {
	pluginName := getPluginName(ctx)
	if pluginName == "" {
		return &proto.StateAppendResponse{}, fmt.Errorf("plugin name not found in metadata")
	}
	store, err := h.getStateStore(pluginName)
	if err != nil {
		return &proto.StateAppendResponse{}, fmt.Errorf("get state store: %w", err)
	}

	if err := store.Append(req.Key, req.Value); err != nil {
		return &proto.StateAppendResponse{}, err
	}
	return &proto.StateAppendResponse{}, nil
}

// StateDelete implements Host.StateDelete.
func (h *HostService) StateDelete(ctx context.Context, req *proto.StateDeleteRequest) (*proto.StateDeleteResponse, error) {
	pluginName := getPluginName(ctx)
	if pluginName == "" {
		return &proto.StateDeleteResponse{}, fmt.Errorf("plugin name not found in metadata")
	}
	store, err := h.getStateStore(pluginName)
	if err != nil {
		return &proto.StateDeleteResponse{}, fmt.Errorf("get state store: %w", err)
	}

	if err := store.Delete(req.Key); err != nil {
		return &proto.StateDeleteResponse{}, err
	}
	return &proto.StateDeleteResponse{}, nil
}

func (h *HostService) activeSession() (SessionAccessor, string, error) {
	if h.manager.sessionAccessor == nil {
		return nil, "", status.Error(codes.FailedPrecondition, "session accessor not configured")
	}

	sessionID := ""
	if h.manager.sessionIDProvider != nil {
		sessionID = h.manager.sessionIDProvider()
	}
	if sessionID == "" {
		return nil, "", status.Error(codes.FailedPrecondition, "no active session")
	}
	return h.manager.sessionAccessor, sessionID, nil
}

func statusMessage(err error) string {
	if err == nil {
		return ""
	}
	return status.Convert(err).Message()
}

// SessionAppend implements Host.SessionAppend.
func (h *HostService) SessionAppend(ctx context.Context, req *proto.SessionAppendRequest) (*proto.SessionAppendResponse, error) {
	accessor, sessionID, err := h.activeSession()
	if err != nil {
		return &proto.SessionAppendResponse{Error: statusMessage(err)}, err
	}

	var parentID *string
	if req.ParentId != "" {
		parentID = &req.ParentId
	}

	entryID, err := accessor.AppendEntry(ctx, sessionID, req.EntryType, []byte(req.Payload), parentID)
	if err != nil {
		return &proto.SessionAppendResponse{Error: err.Error()}, status.Errorf(codes.Internal, "append entry: %v", err)
	}
	return &proto.SessionAppendResponse{EntryId: entryID}, nil
}

// SessionScan implements Host.SessionScan.
func (h *HostService) SessionScan(ctx context.Context, req *proto.SessionScanRequest) (*proto.SessionScanResponse, error) {
	accessor, sessionID, err := h.activeSession()
	if err != nil {
		return &proto.SessionScanResponse{Error: statusMessage(err)}, err
	}

	entries, err := accessor.ScanEntries(ctx, sessionID, req.EntryTypeFilter, req.CustomTypeFilter)
	if err != nil {
		return &proto.SessionScanResponse{Error: err.Error()}, status.Errorf(codes.Internal, "scan entries: %v", err)
	}

	protoEntries := make([]*proto.SessionEntry, len(entries))
	for i, e := range entries {
		parentID := ""
		if e.ParentID != nil {
			parentID = *e.ParentID
		}
		protoEntries[i] = &proto.SessionEntry{
			Id:         e.ID,
			EntryType:  e.EntryType,
			CustomType: e.CustomType,
			Payload:    string(e.Payload),
			ParentId:   parentID,
		}
	}
	return &proto.SessionScanResponse{Entries: protoEntries}, nil
}

// SessionSetLabel implements Host.SessionSetLabel.
func (h *HostService) SessionSetLabel(ctx context.Context, req *proto.SessionSetLabelRequest) (*proto.SessionSetLabelResponse, error) {
	accessor, sessionID, err := h.activeSession()
	if err != nil {
		return &proto.SessionSetLabelResponse{Error: statusMessage(err)}, err
	}

	entryID, err := accessor.SetLabel(ctx, sessionID, req.EntryId, req.Label)
	if err != nil {
		return &proto.SessionSetLabelResponse{Error: err.Error()}, status.Errorf(codes.Internal, "set label: %v", err)
	}
	return &proto.SessionSetLabelResponse{EntryId: entryID}, nil
}

// SessionGetLabel implements Host.SessionGetLabel.
func (h *HostService) SessionGetLabel(ctx context.Context, req *proto.SessionGetLabelRequest) (*proto.SessionGetLabelResponse, error) {
	accessor, sessionID, err := h.activeSession()
	if err != nil {
		return &proto.SessionGetLabelResponse{Error: statusMessage(err)}, err
	}

	label, err := accessor.GetLabel(ctx, sessionID, req.EntryId)
	if err != nil {
		return &proto.SessionGetLabelResponse{Error: err.Error()}, status.Errorf(codes.Internal, "get label: %v", err)
	}
	return &proto.SessionGetLabelResponse{Label: label}, nil
}

// SessionSendUserMessage implements Host.SessionSendUserMessage. Routes a
// user message to the active session via the manager's configured sender
// (Send/Steer/FollowUp), matching pi's pi.sendUserMessage semantics.
func (h *HostService) SessionSendUserMessage(ctx context.Context, req *proto.SessionSendUserMessageRequest) (*proto.SessionSendUserMessageResponse, error) {
	h.manager.mu.RLock()
	sender := h.manager.messageSender
	h.manager.mu.RUnlock()

	if sender == nil {
		return &proto.SessionSendUserMessageResponse{
			Error: "message sender not configured",
		}, status.Errorf(codes.Unavailable, "message sender not configured")
	}

	deliverAs := req.DeliverAs
	switch deliverAs {
	case "", "steer", "followUp":
		// valid
	default:
		return &proto.SessionSendUserMessageResponse{
			Error: "invalid deliver_as: must be \"\", \"steer\", or \"followUp\"",
		}, status.Errorf(codes.InvalidArgument, "invalid deliver_as")
	}

	if err := sender(req.Text, deliverAs); err != nil {
		return &proto.SessionSendUserMessageResponse{
			Ok:    false,
			Error: err.Error(),
		}, status.Errorf(codes.Internal, "send user message: %v", err)
	}
	return &proto.SessionSendUserMessageResponse{Ok: true}, nil
}

// errToString converts an error to a string, or empty string if nil.
func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// getSettings returns the current plugin settings.
func (m *Manager) getSettings() *ai.Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// SetSettings updates the plugin settings (e.g., from merged settings.json).
func (m *Manager) SetSettings(s *ai.Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s == nil {
		m.settings = &ai.Settings{}
	} else {
		m.settings = s
	}
}

// ResolveCLIFlags merges parsed CLI flag values into the per-plugin config namespace.
// After this is called, Host.GetConfig returns CLI-provided values over defaults.
func (m *Manager) ResolveCLIFlags(pluginName string, flags map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if flags == nil {
		return
	}
	// Initialize if not exists.
	if _, ok := m.cliFlags[pluginName]; !ok {
		m.cliFlags[pluginName] = make(map[string]string)
	}
	if m.cliFlagOverrides == nil {
		m.cliFlagOverrides = make(map[string]map[string]string)
	}
	if m.cliFlagOverrides[pluginName] == nil {
		m.cliFlagOverrides[pluginName] = make(map[string]string)
	}
	// Merge CLI values (CLI overrides defaults).
	for k, v := range flags {
		m.cliFlags[pluginName][k] = v
		m.cliFlagOverrides[pluginName][k] = v
	}
}

// RegisteredCLIFlags returns the currently registered CLI flags by plugin.
func (m *Manager) RegisteredCLIFlags() map[string][]CLIFlag {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]CLIFlag, len(m.plugins))
	for name, info := range m.plugins {
		result[name] = append([]CLIFlag(nil), info.Registrations.CLIFlags...)
	}
	return result
}

// SetSessionAccessor configures the session accessor for Host service RPCs.
// This should be called before plugins are loaded.
func (m *Manager) SetSessionAccessor(accessor SessionAccessor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionAccessor = accessor
}

// SetSessionIDProvider configures the session ID provider for Host service RPCs.
// This should be called during runtime setup with a function that returns the
// current active session ID.
func (m *Manager) SetSessionIDProvider(provider func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionIDProvider = provider
}

// SetSessionCWDProvider configures the session CWD provider used during plugin
// initialization and Host RPC handling.
func (m *Manager) SetSessionCWDProvider(provider func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionCWDProvider = provider
}

// SetMessageSender wires the active session's message-sending methods for the
// SessionSendUserMessage Host RPC. deliverAs must be one of "", "steer",
// "followUp".
func (m *Manager) SetMessageSender(sender func(text, deliverAs string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messageSender = sender
}

// SetCompactor wires the active session's Compact for the SessionCompact
// Host RPC.
func (m *Manager) SetCompactor(compact func(ctx context.Context, instructions string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compactor = compact
}

// SessionCompact implements Host.SessionCompact. Runs the active session's
// compaction — the same path auto-compaction and /compact use.
func (h *HostService) SessionCompact(ctx context.Context, req *proto.SessionCompactRequest) (*proto.SessionCompactResponse, error) {
	h.manager.mu.RLock()
	compact := h.manager.compactor
	h.manager.mu.RUnlock()

	if compact == nil {
		return &proto.SessionCompactResponse{Error: "compactor not configured"}, status.Errorf(codes.Unavailable, "compactor not configured")
	}
	if err := compact(ctx, req.Instructions); err != nil {
		return &proto.SessionCompactResponse{Ok: false, Error: err.Error()}, status.Errorf(codes.Internal, "compact: %v", err)
	}
	return &proto.SessionCompactResponse{Ok: true}, nil
}

func (m *Manager) currentCWD() string {
	m.mu.RLock()
	provider := m.sessionCWDProvider
	m.mu.RUnlock()

	if provider != nil {
		if cwd := provider(); cwd != "" {
			return cwd
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	return cwd
}

// getCLIFlag returns a CLI flag value for the given plugin, or "" if not set.
func (m *Manager) getCLIFlag(pluginName, key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if flags, ok := m.cliFlags[pluginName]; ok {
		return flags[key]
	}
	return ""
}

// validatePluginConfig validates the plugin's settings.json config against its JSON Schema.
// If validation fails, the plugin load is rejected with a clear error.
func (m *Manager) validatePluginConfig(pluginName, schemaStr string) error {
	return validatePluginSettings(m.getSettings(), pluginName, schemaStr)
}

func validatePluginSettings(settings *ai.Settings, pluginName, schemaStr string) error {
	if settings == nil || settings.Plugins == nil {
		// No settings means nothing to validate.
		return nil
	}

	pluginConfig, ok := settings.Plugins[pluginName]
	if !ok || pluginConfig == nil {
		// No plugin config means nothing to validate.
		return nil
	}

	// Load the schema.
	schemaLoader := gojsonschema.NewStringLoader(schemaStr)

	// Load the plugin config as a JSON document.
	// pluginConfig is already json.RawMessage, so we can use it directly.
	configDoc := gojsonschema.NewBytesLoader(pluginConfig)

	// Validate.
	result, err := gojsonschema.Validate(schemaLoader, configDoc)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}

	if !result.Valid() {
		var errMsg string
		for _, desc := range result.Errors() {
			errMsg += fmt.Sprintf("\n  - %s", desc)
		}
		return fmt.Errorf("config validation failed for plugin %q:%s", pluginName, errMsg)
	}

	return nil
}

// SetCustomMessageSender routes extension context to the active session.
func (m *Manager) SetCustomMessageSender(sender func(context.Context, json.RawMessage, *bool, string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customMessageSender = sender
}

// SetSessionCustomMessageSender installs explicitly addressed custom delivery.
// The callback must capture the target session or reject it without falling back
// to the active conversation.
func (m *Manager) SetSessionCustomMessageSender(sender func(context.Context, string, json.RawMessage, *bool, string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionCustomMessageSender = sender
}

// SessionSendCustomMessage implements extension custom-message delivery.
func (h *HostService) SessionSendCustomMessage(ctx context.Context, req *proto.SessionSendCustomMessageRequest) (*proto.SessionSendUserMessageResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if !json.Valid([]byte(req.MessageJson)) {
		return nil, status.Error(codes.InvalidArgument, "invalid custom message JSON")
	}
	switch req.DeliverAs {
	case "", "steer", "followUp", "nextTurn":
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid custom delivery mode")
	}
	h.manager.mu.RLock()
	sender := h.manager.customMessageSender
	scoped := h.manager.sessionCustomMessageSender
	h.manager.mu.RUnlock()
	if req.SessionId != "" {
		if scoped == nil {
			return nil, status.Error(codes.Unavailable, "session-addressed custom message sender not configured")
		}
		sender = func(ctx context.Context, raw json.RawMessage, trigger *bool, delivery string) error {
			return scoped(ctx, req.SessionId, raw, trigger, delivery)
		}
	}
	if sender == nil {
		return nil, status.Error(codes.Unavailable, "custom message sender not configured")
	}
	if err := sender(ctx, json.RawMessage(req.MessageJson), req.TriggerTurn, req.DeliverAs); err != nil {
		return &proto.SessionSendUserMessageResponse{Error: err.Error()}, status.Errorf(codes.Internal, "send custom message: %v", err)
	}
	return &proto.SessionSendUserMessageResponse{Ok: true}, nil
}

// SetToolDiscovery connects plugin discovery to the active session.
func (m *Manager) SetToolDiscovery(discover func(context.Context, *proto.ToolDiscoveryRequest) (json.RawMessage, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolDiscovery = discover
}

// ToolDiscovery performs cancellable discovery without holding the manager lock.
func (h *HostService) ToolDiscovery(ctx context.Context, req *proto.ToolDiscoveryRequest) (*proto.ToolDiscoveryResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	switch req.Operation {
	case "registered", "active", "activate", "search", "tool", "namespace":
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown tool discovery operation")
	}
	h.manager.mu.RLock()
	discover := h.manager.toolDiscovery
	h.manager.mu.RUnlock()
	if discover == nil {
		return nil, status.Error(codes.Unavailable, "tool discovery not configured")
	}
	result, err := discover(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Errorf(codes.FailedPrecondition, "discover tools: %v", err)
	}
	return &proto.ToolDiscoveryResponse{ResultJson: string(result)}, nil
}

// SessionSendCustomMessageToSession requires an explicit originating session.
// This separate method fails closed on hosts predating addressed delivery.
func (h *HostService) SessionSendCustomMessageToSession(ctx context.Context, req *proto.SessionSendCustomMessageRequest) (*proto.SessionSendUserMessageResponse, error) {
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "originating session is required")
	}
	return h.SessionSendCustomMessage(ctx, req)
}
