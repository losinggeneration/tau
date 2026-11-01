package tui

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/losinggeneration/tau/tui/clipboard"
	"github.com/losinggeneration/tau/tui/extensions"
	baseui "github.com/losinggeneration/tau/ui"
)

// appEditorPasteProvider is the live-editor implementation of
// extensions.PasteProvider. The Registry owns the wire format; the
// adapter owns the editor interaction so the App can keep its own
// state as the source of truth.
//
// The adapter intentionally routes through the editor's Paste helper
// rather than calling SetText directly: editor.Paste owns the
// bracketed-paste marker handling, line-ending normalization, and
// large-paste threshold logic. Going through the helper preserves
// that behavior for plugin-initiated pastes (e.g. an image
// attachment insert from a /paste-image command).
var _ baseui.UI = (*App)(nil)

type appEditorPasteProvider struct {
	app *App
}

func (p *appEditorPasteProvider) InsertPaste(s string) (string, int) {
	if p == nil || p.app == nil || p.app.editor == nil {
		return "", 0
	}
	s, ok := p.app.rewritePastedImage(s)
	if !ok {
		return "", 0
	}
	return p.app.editor.Paste(p.app.loopContext(), s)
}

func (p *appEditorPasteProvider) ReplacePaste(s string) (string, int) {
	if p == nil || p.app == nil || p.app.editor == nil {
		return "", 0
	}
	p.app.editor.SetText(p.app.loopContext(), s)
	return s, len(s)
}

func (a *App) rewritePastedImage(s string) (string, bool) {
	img, ok := clipboard.ParseDataURL(s)
	if !ok {
		return s, true
	}
	return a.rewriteClipboardImage(img)
}

func (a *App) pasteNativeClipboardImage(ctx context.Context) bool {
	img, ok, err := clipboard.ReadImage(ctx)
	if err != nil {
		a.conv.AddError("paste: " + err.Error())
		a.refreshChat()
		return true
	}
	if !ok {
		return false
	}
	text, ok := a.rewriteClipboardImage(img)
	if !ok {
		return true
	}
	a.editor.Paste(a.loopContext(), text)
	return true
}

func (a *App) rewriteClipboardImage(img clipboard.Image) (string, bool) {
	path, err := a.savePastedImage(img)
	if err != nil {
		a.conv.AddError(err.Error())
		a.refreshChat()
		return "", false
	}
	marker := clipboard.AttachmentMarkdown(path)
	if a.editor != nil {
		marker = a.editor.SetInlineImagePreview(marker, img.Data, img.MIME)
	}
	return marker, true
}

func (a *App) savePastedImage(img clipboard.Image) (string, error) {
	dataDir := a.sessionState().DataDir()
	if dataDir == "" {
		dataDir = a.fileSelectorRoot()
	}
	path, err := clipboard.SaveImage(dataDir, img)
	if err != nil {
		return "", err
	}
	// The editor shows the bare filename — image pasting is a TUI-managed
	// attachment and no other TUI makes users read an absolute path for it.
	// savePastedImage remembers where that name really lives so expansion
	// can map it back (see resolveAttachmentPath).
	name := filepath.ToSlash(filepath.Base(path))
	if a.pastedImages == nil {
		a.pastedImages = map[string]string{}
	}
	a.pastedImages[name] = path
	return name, nil
}

// applyEditorTextFromExtension pushes a plugin-supplied editor text
// into the live prompt editor. The Registry already stores the text in
// its own editorText field; this is the bridge that keeps the
// editor in sync when a plugin calls SetEditorText.
//
// The caller must run on the app loop.
func (a *App) applyEditorTextFromExtension(text string) {
	if a == nil || a.editor == nil {
		return
	}
	a.editor.SetText(a.loopContext(), text)
}

// applyThemeFromExtension is called when a plugin calls SetTheme on
// the Registry. It updates the footer's theme label and refreshes the
// chat so the new theme is visible immediately. It deliberately does
// not call back into App.applyTheme: that would re-enter
// extensions.SetTheme, which would fire OnThemeChanged again, which
// would invoke this function, which would loop forever. The Registry
// already stores the plugin's theme; the app only needs to mirror the
// name into the footer chrome and invalidate the chat paint.
func (a *App) applyThemeFromExtension(theme extensions.Theme) {
	if a == nil {
		return
	}
	if a.terminalTheme.publishing {
		return
	}
	a.terminalTheme.inMemory = true
	a.terminalTheme.autoSync = false
	a.setThemeNotifications()
	name := strings.TrimSpace(theme.Name)
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.theme = name
	}
	if a.chat != nil {
		a.chat.setThemeDefinition(name, ThemeDefinition{Values: theme.Values, Appearance: theme.Appearance, Dim: theme.Dim})
	}
	a.refreshEditorChrome()
	a.refreshNativeTheme()
	if ctx := a.loopContext(); ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		if ctx.InvalidateAll != nil {
			ctx.InvalidateAll()
		}
	}
	a.refreshChat()
}

// refreshPluginToolExpansion re-renders the chat log after a plugin
// flips a tool's expansion state. Mirrors the same invalidation
// flow App.setStreaming and App.setStatus use so the chat log
// repaints with the new expanded/collapsed state.
func (a *App) refreshPluginToolExpansion(id string) {
	if a == nil || a.chat == nil {
		return
	}
	a.refreshChat()
}

// contextBackground returns a context that is safe to pass into
// SDK calls from any goroutine, including background plugin
// callbacks that may outlive the active run.
func (a *App) contextBackground() context.Context {
	return a.ctxOrBackground()
}

// ensureExtensionTheme is called when the Registry's SetTheme is
// updated by a plugin. It propagates the change through the App's
// applyTheme so the toolkit repaints with the new palette and the
// footer data reflects the new name.
func (a *App) ensureExtensionTheme(name string) {
	if a == nil {
		return
	}
	a.applyTheme(name)
}

// extensionsConfigFor is a small helper used by tests to construct
// an extensions.Registry with a known Theme/EditorText configuration
// without invoking the App constructor (which would require a
// toolkit loop).
func extensionsConfigFor() *extensions.Registry {
	r := extensions.New()
	r.SetTheme(extensions.Theme{Name: "default", Values: map[string]string{}})
	return r
}
