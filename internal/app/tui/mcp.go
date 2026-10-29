package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/mcp"
	"github.com/losinggeneration/tau/paths"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/autocomplete"
	baseui "github.com/losinggeneration/tau/ui"
)

const mcpManagerTitle = "MCP servers"

const mcpUsage = "Usage: /mcp, /mcp login [server], /mcp logout [server], /mcp reconnect [server]"

// cmdMCP opens the MCP server manager, which follows connection changes while
// it is open, or runs one of its subcommands.
func cmdMCP(app *App, args string) {
	status, ok := app.sess.(sdk.MCPStatus)
	fields := strings.Fields(args)
	switch {
	case !ok:
		app.conv.AddError("mcp: this session has no MCP servers")
	case len(fields) == 0 && (app.app == nil || app.dialogPicker == nil):
		app.conv.AddSystem(formatMCPStatus(status))
	case len(fields) == 0:
		app.openMCPManager(status)
		return
	case len(fields) > 2 || !slices.Contains([]string{"login", "logout", "reconnect"}, fields[0]):
		app.conv.AddError(mcpUsage)
	default:
		name := ""
		if len(fields) == 2 {
			name = fields[1]
		}
		ctx := app.ctxOrBackground()
		go app.mcpSubcommand(ctx, status, fields[0], name)
		return
	}
	app.refreshChat()
}

// completeMCPArgs completes the subcommand, then the servers it applies to.
func completeMCPArgs(app *App, prefix string) []autocomplete.Item {
	fields := strings.Fields(prefix)
	if strings.HasSuffix(prefix, " ") {
		fields = append(fields, "")
	}
	switch len(fields) {
	case 0, 1:
		var items []autocomplete.Item
		for _, action := range []string{"login", "logout", "reconnect"} {
			if len(fields) == 0 || strings.HasPrefix(action, fields[0]) {
				items = append(items, autocomplete.Item{Value: action + " ", Label: action})
			}
		}
		return items
	case 2:
	default:
		return nil
	}
	action := fields[0]
	status, ok := app.sess.(sdk.MCPStatus)
	if !ok || !slices.Contains([]string{"login", "logout", "reconnect"}, action) {
		return nil
	}
	servers, _ := status.MCPServers()
	var items []autocomplete.Item
	for _, s := range servers {
		eligible := s.OAuth && s.Status != mcp.StatusDisabled
		if action == "reconnect" {
			eligible = s.Status != mcp.StatusDisabled
		}
		if eligible && strings.HasPrefix(s.Name, fields[1]) {
			items = append(items, autocomplete.Item{Value: action + " " + s.Name, Label: s.Name, Description: mcpDescribeState(s)})
		}
	}
	return items
}

// mcpSubcommand runs /mcp login, logout or reconnect off the UI loop.
func (a *App) mcpSubcommand(ctx context.Context, status sdk.MCPStatus, action, name string) {
	report := func(text string, isErr bool) {
		a.post(func(*tauui.UpdateCtx) {
			if isErr {
				a.conv.AddError(text)
			} else {
				a.conv.AddSystem(text)
			}
			a.refreshChat()
		})
	}
	oauth := func(s mcp.ServerState) bool { return s.OAuth && s.Status != mcp.StatusDisabled }
	switch action {
	case "login":
		server, msg := a.pickMCPServer(ctx, status, name, oauth, func(s mcp.ServerState) bool { return s.Status == mcp.StatusNeedsAuth }, mcpNoOAuth)
		if server == "" {
			if msg != "" {
				report(msg, true)
			}
			return
		}
		if failure := a.mcpSignIn(ctx, status, server); failure != "" {
			report(failure, failure != mcpSignInCancelled)
			return
		}
		report(fmt.Sprintf("Signed in to MCP server %q (%d tools).", server, mcpServerTools(status, server)), false)
	case "logout":
		server, msg := a.pickMCPServer(ctx, status, name, oauth, func(s mcp.ServerState) bool { return s.Status == mcp.StatusNeedsAuth }, mcpNoOAuth)
		if server == "" {
			if msg != "" {
				report(msg, true)
			}
			return
		}
		removed, err := status.MCPSignOut(ctx, server)
		switch {
		case err != nil:
			report(err.Error(), true)
		case removed:
			report(fmt.Sprintf("Signed out of MCP server %q.", server), false)
		default:
			report(fmt.Sprintf("No stored credentials for MCP server %q.", server), false)
		}
	case "reconnect":
		enabled := func(s mcp.ServerState) bool { return s.Status != mcp.StatusDisabled }
		broken := func(s mcp.ServerState) bool { return s.Status == mcp.StatusFailed }
		server, msg := a.pickMCPServer(ctx, status, name, enabled, broken, "No enabled MCP server to reconnect.")
		if server == "" {
			if msg != "" {
				report(msg, true)
			}
			return
		}
		if err := status.MCPReconnect(ctx, server); err != nil {
			report(err.Error(), true)
			return
		}
		state, _ := mcpServerState(status, server)
		report(fmt.Sprintf("Reconnected to MCP server %q (%s).", server, mcpDescribeState(state)), false)
	}
}

const (
	mcpNoOAuth         = "No enabled MCP server uses OAuth. Only HTTP servers without an Authorization header do."
	mcpSignInCancelled = "Sign-in cancelled."
)

// pickMCPServer resolves a subcommand's server, asking when the name is
// omitted and more than one is eligible without a single preferred one.
func (a *App) pickMCPServer(ctx context.Context, status sdk.MCPStatus, name string, eligible, preferred func(mcp.ServerState) bool, none string) (string, string) {
	servers, _ := status.MCPServers()
	if name != "" {
		i := slices.IndexFunc(servers, func(s mcp.ServerState) bool { return s.Name == name })
		if i < 0 {
			return "", fmt.Sprintf("No MCP server named %q.", name)
		}
		if !eligible(servers[i]) {
			return "", none
		}
		return name, ""
	}
	servers = slices.DeleteFunc(servers, func(s mcp.ServerState) bool { return !eligible(s) })
	if len(servers) == 0 {
		return "", none
	}
	if len(servers) == 1 {
		return servers[0].Name, ""
	}
	if p := slices.DeleteFunc(slices.Clone(servers), func(s mcp.ServerState) bool { return !preferred(s) }); len(p) == 1 {
		return p[0].Name, ""
	}
	names := make([]string, len(servers))
	for i, s := range servers {
		names[i] = s.Name
	}
	choice, err := a.Select(ctx, "MCP server", names)
	if err != nil || choice < 0 {
		return "", ""
	}
	return names[choice], ""
}

// mcpSignIn signs in to a server and reconnects it. It returns upstream's
// failure message, or "" on success.
func (a *App) mcpSignIn(ctx context.Context, status sdk.MCPStatus, name string) string {
	prompt := mcp.SignInPrompt{
		ShowURL: func(url string) {
			a.post(func(*tauui.UpdateCtx) {
				a.conv.AddSystem(fmt.Sprintf("Sign in to MCP server %q in your browser:\n%s", name, url))
				a.refreshChat()
			})
			if err := a.openBrowser(url); err != nil {
				a.post(func(*tauui.UpdateCtx) {
					a.conv.AddError("open browser: " + err.Error())
					a.refreshChat()
				})
			}
		},
		RedirectURL: func(ctx context.Context) (string, error) {
			value, err := a.Input(ctx, fmt.Sprintf("Waiting for sign-in to %q. If the browser cannot reach this machine, paste the URL it was redirected to.", name), "http://127.0.0.1:.../callback?code=...")
			if errors.Is(err, baseui.ErrNoInput) {
				return "", nil
			}
			return value, err
		},
	}
	if err := status.MCPSignIn(ctx, name, prompt); err != nil {
		if errors.Is(err, mcp.ErrSignInCancelled) {
			return mcpSignInCancelled
		}
		return "Sign-in failed: " + err.Error()
	}
	if err := status.MCPReconnect(ctx, name); err != nil {
		return "Signed in, but " + err.Error()
	}
	return ""
}

func mcpServerState(status sdk.MCPStatus, name string) (mcp.ServerState, bool) {
	servers, _ := status.MCPServers()
	i := slices.IndexFunc(servers, func(s mcp.ServerState) bool { return s.Name == name })
	if i < 0 {
		return mcp.ServerState{}, false
	}
	return servers[i], true
}

func mcpServerTools(status sdk.MCPStatus, name string) int {
	state, _ := mcpServerState(status, name)
	return len(state.Tools)
}

// showMCPMenu shows one manager menu in the dialog picker, rebuilt on every
// server change while it is the menu shown.
func (a *App) showMCPMenu(status sdk.MCPStatus, build func() (string, []tauui.DialogPickerItem), onSelect func(value string), onCancel func()) {
	a.closeDialogComponent()
	a.dismissSelector()
	picker := a.dialogPicker
	picker.Reset()
	menu := new(int)
	a.mcpMenu = menu
	header, items := build()
	picker.SetHeader(header)
	picker.SetItems(items)
	var unsubscribe func()
	current := func() bool { return picker.Shown() && a.mcpMenu == menu }
	unsubscribe = status.OnMCPChange(func() {
		a.post(func(u *tauui.UpdateCtx) {
			if !current() {
				unsubscribe()
				return
			}
			focused, _ := picker.FocusedItem()
			header, items := build()
			picker.SetHeader(header)
			picker.SetItems(items)
			if i := slices.IndexFunc(items, func(item tauui.DialogPickerItem) bool { return item.Value == focused.Value }); i >= 0 {
				picker.SetFocused(i)
			}
			u.InvalidateLayout()
			u.InvalidateAll()
		})
	})
	picker.SetOnSelect(func(_ int, item tauui.DialogPickerItem) {
		unsubscribe()
		a.mcpMenu = nil
		onSelect(item.Value)
	})
	picker.SetOnCancel(func() {
		unsubscribe()
		a.mcpMenu = nil
		a.dismissDialog()
		if onCancel != nil {
			onCancel()
		}
	})
	picker.SetShown(true)
	a.dialogInputActive.Store(false)
	a.refreshChat()
	a.post(func(u *tauui.UpdateCtx) {
		if current() {
			u.InvalidateLayout()
			u.InvalidateAll()
			a.app.SetFocus(picker.ID())
		}
	})
}

func (a *App) openMCPManager(status sdk.MCPStatus) {
	a.showMCPMenu(status, func() (string, []tauui.DialogPickerItem) { return mcpManagerView(status) },
		func(name string) { a.openMCPServerMenu(status, name, "") }, nil)
}

// openMCPServerMenu lists what can be done with one server. message is the
// outcome of the last action, shown under the title.
func (a *App) openMCPServerMenu(status sdk.MCPStatus, name, message string) {
	back := func() { a.openMCPManager(status) }
	a.showMCPMenu(status, func() (string, []tauui.DialogPickerItem) { return mcpServerMenu(status, name, message) },
		func(action string) {
			ctx := a.ctxOrBackground()
			switch action {
			case "signin":
				a.dismissDialog()
				go func() {
					failure := a.mcpSignIn(ctx, status, name)
					a.post(func(*tauui.UpdateCtx) { a.openMCPServerMenu(status, name, failure) })
				}()
			case "reconnect":
				// Connection state already reports failures.
				go func() { _ = status.MCPReconnect(ctx, name) }() // the server's state shows the outcome
				a.openMCPServerMenu(status, name, "")
			case "signout":
				go func() {
					_, err := status.MCPSignOut(ctx, name)
					a.post(func(*tauui.UpdateCtx) { a.openMCPServerMenu(status, name, errorText(err)) })
				}()
			case "tools":
				a.showMCPMenu(status, func() (string, []tauui.DialogPickerItem) { return mcpToolsMenu(status, name) },
					func(string) { a.openMCPServerMenu(status, name, "") }, func() { a.openMCPServerMenu(status, name, "") })
			case "exposure":
				a.openMCPExposureMenu(status, name)
			case "enable", "disable", "enable-project", "disable-project":
				enabled := strings.HasPrefix(action, "enable")
				patch := mcp.ConfigPatch{Enabled: &enabled, Project: strings.HasSuffix(action, "-project")}
				go func() {
					err := status.MCPConfigure(name, patch)
					a.post(func(*tauui.UpdateCtx) { a.openMCPServerMenu(status, name, errorText(err)) })
				}()
			default:
				a.openMCPServerMenu(status, name, message)
			}
		}, back)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type mcpExposureChoice struct {
	exposure    mcp.Exposure
	description string
}

var mcpExposureDescriptions = []mcpExposureChoice{
	{mcp.ExposureCodemode, "called from codemode scripts, which find them with searchTools()"},
	{mcp.ExposureDeferred, "not declared until tool_search loads them, then called directly; no codemode needed"},
	{mcp.ExposureDirect, "declared to the model like built-in tools"},
}

func mcpExposureDescription(e mcp.Exposure) string {
	for _, d := range mcpExposureDescriptions {
		if d.exposure == e {
			return d.description
		}
	}
	return "unreachable"
}

func mcpServerMenu(status sdk.MCPStatus, name, message string) (string, []tauui.DialogPickerItem) {
	server, ok := mcpServerState(status, name)
	if !ok {
		return name + "\nThis server is no longer configured.", nil
	}
	lines := []string{"MCP server " + name}
	if server.Transport != "" {
		lines = append(lines, server.Transport)
	}
	if server.Source != "" {
		lines = append(lines, cmp.Or(string(server.Scope), "config")+": "+server.Source)
	}
	if server.Override != "" {
		lines = append(lines, "project override: "+server.Override)
	}
	lines = append(lines, "State: "+mcpDescribeState(server))
	if message != "" {
		lines = append(lines, message)
	}
	if server.Error != "" && server.Status != mcp.StatusConnected {
		lines = append(lines, server.Error)
	}
	saved := "saved to mcp.json"
	switch {
	case server.Override != "":
		saved = "saved to the project mcp.json"
	case server.Scope != "":
		saved = "saved to the " + string(server.Scope) + " mcp.json"
	}
	item := func(name, description, value string) tauui.DialogPickerItem {
		return tauui.DialogPickerItem{Name: name, Description: description, Value: value}
	}
	var items []tauui.DialogPickerItem
	switch server.Status {
	case mcp.StatusClosed:
	case mcp.StatusDisabled:
		items = append(items, item("Enable", saved, "enable"))
		if server.ProjectConfig != "" {
			items = append(items, item("Enable in this project", "saved to the project mcp.json", "enable-project"))
		}
	default:
		if server.Status == mcp.StatusNeedsAuth {
			items = append(items, item("Sign in", "opens the browser", "signin"))
		}
		if server.Status == mcp.StatusConnected {
			items = append(items, item("Tools", fmt.Sprintf("%d offered", len(server.Tools)), "tools"))
		}
		if server.Status != mcp.StatusConnecting {
			items = append(items, item("Reconnect", "", "reconnect"))
		}
		if server.Status == mcp.StatusConnected && server.OAuth {
			items = append(items, item("Sign out", "deletes the stored credentials", "signout"))
		}
		items = append(items, item("Exposure", string(server.Exposure), "exposure"), item("Disable", saved, "disable"))
		if server.ProjectConfig != "" {
			items = append(items, item("Disable in this project", "saved to the project mcp.json", "disable-project"))
		}
	}
	for i := range lines {
		lines[i] = sanitizeStatus(lines[i])
	}
	return strings.Join(lines, "\n"), items
}

func mcpToolsMenu(status sdk.MCPStatus, name string) (string, []tauui.DialogPickerItem) {
	server, _ := mcpServerState(status, name)
	header := fmt.Sprintf("Tools of %s\nExposure %s: %s", sanitizeStatus(name), server.Exposure, mcpExposureDescription(server.Exposure))
	var items []tauui.DialogPickerItem
	overridden := false
	for _, tool := range server.Tools {
		description, _, _ := strings.Cut(tool.Description, "\n")
		if tool.Exposure != server.Exposure {
			overridden = true
			description = "[" + string(tool.Exposure) + "] " + description
		}
		items = append(items, tauui.DialogPickerItem{Name: sanitizeStatus(tool.Name), Description: sanitizeStatus(description), Value: tool.Name})
	}
	if overridden {
		header += "\nSome tools override it with toolExposure."
	}
	if len(items) == 0 {
		header += "\nThe server offers no tools."
	}
	return header, items
}

// openMCPExposureMenu picks the server's exposure and saves it.
func (a *App) openMCPExposureMenu(status sdk.MCPStatus, name string) {
	back := func() { a.openMCPServerMenu(status, name, "") }
	server, _ := mcpServerState(status, name)
	a.showMCPMenu(status, func() (string, []tauui.DialogPickerItem) {
		var items []tauui.DialogPickerItem
		for _, d := range mcpExposureDescriptions {
			mark := "  "
			if d.exposure == server.Exposure {
				mark = "✓ "
			}
			items = append(items, tauui.DialogPickerItem{Name: mark + string(d.exposure), Description: d.description, Value: string(d.exposure)})
		}
		return "Exposure of " + sanitizeStatus(name) + "\nSaved to " + sanitizeStatus(cmp.Or(server.Override, server.Source)) + ".", items
	}, func(choice string) {
		if mcp.Exposure(choice) == server.Exposure {
			back()
			return
		}
		a.openMCPServerMenu(status, name, errorText(status.MCPConfigure(name, mcp.ConfigPatch{Exposure: mcp.Exposure(choice)})))
	}, back)
	if i := slices.IndexFunc(mcpExposureDescriptions, func(d mcpExposureChoice) bool { return d.exposure == server.Exposure }); i >= 0 {
		a.dialogPicker.SetFocused(i)
	}
}

func mcpManagerView(status sdk.MCPStatus) (string, []tauui.DialogPickerItem) {
	servers, configErrors := status.MCPServers()
	header := mcpManagerTitle
	for _, problem := range configErrors {
		header += "\nconfig: " + sanitizeStatus(problem)
	}
	if len(servers) == 0 {
		return header + "\n" + mcpNoServers(), nil
	}
	slices.SortFunc(servers, func(a, b mcp.ServerState) int {
		return cmp.Or(cmp.Compare(mcpAttentionRank(a), mcpAttentionRank(b)), strings.Compare(a.Name, b.Name))
	})
	items := make([]tauui.DialogPickerItem, 0, len(servers))
	for _, server := range servers {
		scope := string(server.Scope)
		if server.Override != "" {
			scope = "global, project override"
		}
		items = append(items, tauui.DialogPickerItem{
			Name:        sanitizeStatus(server.Name),
			Description: sanitizeStatus(fmt.Sprintf("%s · %s · %s", mcpDescribeState(server), server.Exposure, scope)),
			Value:       server.Name,
		})
	}
	return header, items
}

func mcpNoServers() string {
	return "No MCP servers configured. Add them to " + filepath.Join(paths.ConfigDir(), "mcp.json") + " or .tau/mcp.json."
}

// mcpAttentionRank lists servers that need the user first.
func mcpAttentionRank(s mcp.ServerState) int {
	switch s.Status {
	case mcp.StatusNeedsAuth:
		return 0
	case mcp.StatusFailed:
		return 1
	case mcp.StatusConnected:
		return 4
	case mcp.StatusDisabled:
		return 5
	default:
		return 3
	}
}

func mcpDescribeState(s mcp.ServerState) string {
	switch s.Status {
	case mcp.StatusNeedsAuth:
		return "needs sign-in"
	case mcp.StatusFailed:
		first, _, _ := strings.Cut(cmp.Or(s.Error, "unknown error"), "\n")
		return "failed: " + first
	case mcp.StatusConnected:
		text := fmt.Sprintf("connected · %d tool", len(s.Tools))
		if len(s.Tools) != 1 {
			text += "s"
		}
		if s.Resources {
			text += " · resources"
		}
		return text
	case mcp.StatusConnecting:
		return "connecting…"
	default:
		return string(s.Status)
	}
}

// formatMCPStatus is the plain-text status for frontends without dialogs.
func formatMCPStatus(status sdk.MCPStatus) string {
	servers, configErrors := status.MCPServers()
	if len(servers) == 0 && len(configErrors) == 0 {
		return mcpNoServers()
	}
	var lines []string
	for _, server := range servers {
		if server.Status == mcp.StatusNeedsAuth {
			lines = append(lines, fmt.Sprintf("%s: needs sign-in, run /mcp login %s (%s)", server.Name, server.Name, server.Exposure))
			continue
		}
		line := server.Name + ": " + string(server.Status)
		if server.Status == mcp.StatusConnected {
			line += fmt.Sprintf(", %d tools", len(server.Tools))
		}
		line += " (" + string(server.Exposure) + ")"
		if server.Error != "" && server.Status != mcp.StatusConnected {
			line += "\n    " + strings.ReplaceAll(server.Error, "\n", "\n    ")
		}
		lines = append(lines, line)
	}
	for _, problem := range configErrors {
		lines = append(lines, "config error: "+problem)
	}
	return strings.Join(lines, "\n")
}
