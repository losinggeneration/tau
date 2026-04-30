package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/sdk"
)

func cmdCopyOrStub(app *App, args string) {
	if strings.TrimSpace(args) == "" {
		cmdStub("copy", "Copy the last assistant reply to the clipboard (coming soon)")(app, args)
		return
	}
	cmdCopy(app, args)
}

func cmdCopy(app *App, args string) {
	if app == nil || app.conv == nil {
		return
	}
	mode := strings.TrimSpace(args)
	if mode == "" {
		mode = "last"
	}
	snapshot := app.conv.Snapshot()
	var text string
	switch mode {
	case "last":
		value, ok := LastMeaningfulTranscriptComponent(snapshot, TranscriptFormatOptions{})
		if !ok {
			app.conv.AddError("copy: transcript is empty")
			app.refreshChat()
			return
		}
		text = value
	case "all":
		text = FormatTranscriptMarkdown(snapshot, TranscriptFormatOptions{})
		if strings.TrimSpace(text) == "" {
			app.conv.AddError("copy: transcript is empty")
			app.refreshChat()
			return
		}
	default:
		app.conv.AddError("copy: usage: /copy [last|all]")
		app.refreshChat()
		return
	}
	ctx := app.loopContext()
	if ctx == nil || ctx.ClipboardWrite == nil {
		app.conv.AddError("copy: clipboard is unavailable")
		app.refreshChat()
		return
	}
	ctx.ClipboardWrite(text)
	app.conv.AddSystem("copy: copied " + mode)
	app.refreshChat()
}

func cmdExportOrStub(app *App, args string) {
	if strings.TrimSpace(args) == "" {
		cmdStub("export", "Export the session to a file (coming soon)")(app, args)
		return
	}
	cmdExport(app, args)
}

func cmdExport(app *App, args string) {
	if app == nil || app.conv == nil {
		return
	}
	format, path, includeThinking, err := parseExportArgs(args)
	if err != nil {
		app.conv.AddError("export: " + err.Error())
		app.refreshChat()
		return
	}
	if path == "" {
		app.conv.AddError("export: usage: /export markdown [--include-thinking] <path>")
		app.refreshChat()
		return
	}
	if err := WriteTranscript(path, format, app.conv.Snapshot(), TranscriptFormatOptions{IncludeThinking: includeThinking}); err != nil {
		app.conv.AddError("export: " + err.Error())
		app.refreshChat()
		return
	}
	app.conv.AddSystem("export: wrote " + filepath.Clean(path))
	app.refreshChat()
}

func parseExportArgs(args string) (format string, path string, includeThinking bool, err error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", false, nil
	}
	format = fields[0]
	for _, field := range fields[1:] {
		switch field {
		case "--include-thinking":
			includeThinking = true
		default:
			if path != "" {
				return "", "", false, fmt.Errorf("unexpected argument %q", field)
			}
			path = field
		}
	}
	if path == "" && len(fields) == 1 && (format == "markdown" || format == "md" || format == "json") {
		return format, "", includeThinking, nil
	}
	if path == "" {
		path = format
		format = "markdown"
	}
	return format, path, includeThinking, nil
}

func cmdTreeOrStub(app *App, args string) {
	cmdTree(app, args)
}

func cmdTree(app *App, _ string) {
	if app == nil || app.conv == nil {
		return
	}
	if app.sessionLister == nil {
		app.conv.AddError("tree: session listing is unavailable")
		app.refreshChat()
		return
	}
	sessions, err := app.sessionLister(app.ctxOrBackground())
	if err != nil {
		app.conv.AddError("tree: " + err.Error())
		app.refreshChat()
		return
	}
	app.conv.AddSystem(formatSessionTree(sessions, app.SessionID()))
	app.refreshChat()
}

func formatSessionTree(sessions []sdk.SessionInfo, activeID string) string {
	if len(sessions) == 0 {
		return "Session tree: no sessions found"
	}
	byParent := make(map[string][]sdk.SessionInfo)
	byID := make(map[string]sdk.SessionInfo, len(sessions))
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}
	for _, sess := range sessions {
		parent := ""
		if sess.ParentID != nil {
			parent = *sess.ParentID
		}
		if parent != "" {
			if _, ok := byID[parent]; !ok {
				parent = ""
			}
		}
		byParent[parent] = append(byParent[parent], sess)
	}
	for parent := range byParent {
		slices.SortStableFunc(byParent[parent], func(a, b sdk.SessionInfo) int {
			return strings.Compare(a.Created, b.Created)
		})
	}
	var b strings.Builder
	b.WriteString("Session tree:")
	var walk func(parent, prefix string)
	walk = func(parent, prefix string) {
		children := byParent[parent]
		for i, sess := range children {
			last := i == len(children)-1
			connector := "├─ "
			nextPrefix := prefix + "│  "
			if last {
				connector = "└─ "
				nextPrefix = prefix + "   "
			}
			marker := " "
			if sess.ID == activeID {
				marker = "*"
			}
			fmt.Fprintf(&b, "\n%s%s%s %s", prefix, connector, marker, sessionTreeLabel(sess))
			walk(sess.ID, nextPrefix)
		}
	}
	walk("", "")
	return b.String()
}

func sessionTreeLabel(sess sdk.SessionInfo) string {
	name := strings.TrimSpace(sess.Name)
	if name == "" {
		name = strings.TrimSpace(sess.FirstPrompt)
	}
	if name == "" {
		name = "(unnamed)"
	}
	id := sess.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return fmt.Sprintf("%s  %s", id, name)
}

func cmdForkOrStub(app *App, args string) {
	cmdFork(app, args)
}

func cmdFork(app *App, args string) {
	if app == nil || app.conv == nil || app.sess == nil {
		return
	}
	if app.sessionFactory == nil {
		app.conv.AddError("fork: session switching is unavailable")
		app.refreshChat()
		return
	}
	state := app.sessionState()
	id := state.SessionID()
	if id == "" {
		id = app.sess.ID()
	}
	if id == "" {
		app.conv.AddError("fork: no active session")
		app.refreshChat()
		return
	}
	entry := strings.TrimSpace(args)
	newSess, err := app.sessionFactory(app.ctxOrBackground(), sdk.SessionOptions{ForkFrom: id, ForkFromEntry: entry, CWD: state.CWD(), UI: nil})
	if err != nil {
		app.conv.AddError("fork: " + err.Error())
		app.refreshChat()
		return
	}
	if err := app.sess.Close(app.ctxOrBackground()); err != nil {
		if closeErr := newSess.Close(app.ctxOrBackground()); closeErr != nil {
			app.conv.AddError("fork: cleanup replacement session: " + closeErr.Error())
		}
		app.conv.AddError("fork: close current session: " + err.Error())
		app.refreshChat()
		return
	}
	app.sess = newSess
	app.wireSessionEvents()
	app.currentModel = newSess.State().Model()
	app.pendingModelKey = ""
	app.pendingQueue = nil
	if app.pending != nil {
		app.refreshPending(app.loopContext())
	}
	if app.footer != nil && app.footer.data != nil {
		newState := newSess.State()
		app.footer.data.model = app.currentModel
		if cwd := newState.CWD(); cwd != "" {
			app.footer.data.cwd = cwd
		}
	}
	app.refreshFooterStats(app.loopContext())
	app.replaceConversation(NewConversationModel())
	app.seedChatFromSession(newSess, "Forked session "+id+" -> "+newSess.ID())
	app.refreshChat()
}
