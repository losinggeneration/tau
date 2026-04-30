// Package plugins implements the extension/plugin system for tau.
package plugins

// ProviderConfig describes a registered provider.
type ProviderConfig struct {
	Name   string
	Config map[string]any // provider-specific config
}

// ProviderRegistration is a queued provider registration for bind-time flush.
type ProviderRegistration struct {
	Name   string
	Config map[string]any
}

// ToolRefreshCallback is called after tool registry refresh.
type ToolRefreshCallback func()

// RegisterProvider queues a provider registration.
// The registration is flushed when the session-host binds (ExtensionRuntime.bind equivalent).
func (m *Manager) RegisterProvider(name string, config map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingProviderRegistrations = append(m.pendingProviderRegistrations, ProviderRegistration{Name: name, Config: config})
}

// FlushProviderRegistrations commits all pending provider registrations.
// Called by session-host after runtime is ready.
func (m *Manager) FlushProviderRegistrations() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, reg := range m.pendingProviderRegistrations {
		if m.activeProviders == nil {
			m.activeProviders = make(map[string]ProviderConfig)
		}
		m.activeProviders[reg.Name] = ProviderConfig(reg)
	}
	m.pendingProviderRegistrations = nil
}

// RegisterToolRefreshCallback registers a callback called after tool registry refresh.
func (m *Manager) RegisterToolRefreshCallback(fn ToolRefreshCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolRefreshCallbacks = append(m.toolRefreshCallbacks, fn)
}

// RefreshToolRegistry rebuilds the tool registry and calls all refresh callbacks.
func (m *Manager) RefreshToolRegistry() {
	m.mu.Lock()
	// Rebuild tool registry from plugin registrations
	// Tool rebuild logic would go here — for now, just call the callbacks.
	callbacks := m.toolRefreshCallbacks
	m.mu.Unlock()

	for _, fn := range callbacks {
		fn()
	}
}

// ActiveProviders returns committed provider registrations.
func (m *Manager) ActiveProviders() map[string]ProviderConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeProviders == nil {
		return nil
	}
	result := make(map[string]ProviderConfig, len(m.activeProviders))
	for k, v := range m.activeProviders {
		result[k] = v
	}
	return result
}
