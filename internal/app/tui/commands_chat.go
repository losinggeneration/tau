package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

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
	if app.app == nil {
		ctx.ClipboardWrite(text)
		app.conv.AddSystem("copy: copied " + mode)
		app.refreshChat()
		return
	}
	app.app.ClipboardWrite(text, func(err error) {
		if err != nil {
			app.conv.AddError("copy: " + err.Error())
		} else {
			app.conv.AddSystem("copy: copied " + mode)
		}
		app.refreshChat()
	})
}

// cmdExport follows upstream: a .jsonl path exports the active branch, any
// other path or none exports HTML. A leading markdown/md/json format keyword
// writes the visible transcript instead.
func cmdExport(app *App, args string) {
	if app == nil || app.conv == nil {
		return
	}
	switch strings.Fields(args + " x")[0] {
	case "markdown", "md", "json":
		cmdExportTranscript(app, args)
		return
	}
	exporter, ok := app.sess.(sdk.Exporter)
	if !ok {
		app.conv.AddError("Failed to export session: export is unavailable")
		app.refreshChat()
		return
	}
	export := exporter.ExportHTML
	path := pathCommandArgument(args)
	if strings.HasSuffix(path, ".jsonl") {
		export = exporter.ExportJSONL
	}
	path, err := export(app.ctxOrBackground(), path)
	if err != nil {
		app.conv.AddError("Failed to export session: " + err.Error())
	} else {
		app.conv.AddSystem("Session exported to: " + path)
	}
	app.refreshChat()
}

func cmdExportTranscript(app *App, args string) {
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
	forkTo(app, strings.TrimSpace(args))
}

// cmdForget opens the forget picker; the editor path reaches it through
// trySubmit, this covers other dispatchers.
func cmdForget(app *App, _ string) {
	if app == nil {
		return
	}
	app.openForgetSelector(app.loopContext())
}

// forgetFrom drops entry and everything after it from the session, then
// redraws the chat from the remaining context.
func forgetFrom(app *App, entry string) {
	if app == nil || app.conv == nil || app.sess == nil {
		return
	}
	if ctx := app.loopContext(); ctx != nil && app.editor != nil {
		app.editor.SetText(ctx, "")
	}
	if sess := app.coordinated; sess != nil {
		// The worker's context_edited event redraws the chat from a new snapshot.
		app.setStatus("coordinator-forget", "Forgetting messages…")
		go func() {
			err := sess.Forget(entry)
			app.post(func(*tauui.UpdateCtx) {
				app.clearStatus("coordinator-forget")
				if err != nil {
					app.conv.AddError("forget: " + err.Error())
					app.refreshChat()
				}
			})
		}()
		return
	}
	f, ok := app.sess.(sdk.Forgetter)
	if !ok {
		app.conv.AddError("forget: session does not support forgetting messages")
		app.refreshChat()
		return
	}
	if err := f.Forget(entry); err != nil {
		app.conv.AddError("forget: " + err.Error())
		app.refreshChat()
		return
	}
	app.replaceConversation(NewConversationModel())
	if lister, ok := app.sess.(sdk.MessageLister); ok {
		app.populateChatFromMessages(lister.GetMessages(), "")
	}
	app.refreshFooterStats(app.loopContext())
	app.refreshChat()
}

// forkTo forks the current session at entry (empty entry = fork from the
// latest point) and switches to the new session.
func forkTo(app *App, entry string) {
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
	// The `/fork ` trigger leaves its text in the editor; it must not
	// survive into the forked session.
	if ctx := app.loopContext(); ctx != nil && app.editor != nil {
		app.editor.SetText(ctx, "")
	}
	app.refreshChat()
}

// cmdImport replaces the current session with a JSONL session file, after
// confirmation. The file is copied into session storage, never overwriting a
// stored session, and the copy is resumed.
func cmdImport(app *App, args string) {
	if app == nil || app.conv == nil {
		return
	}
	path := pathCommandArgument(args)
	if path == "" {
		app.conv.AddError("Usage: /import <path.jsonl>")
		app.refreshChat()
		return
	}
	if app.importSession == nil || app.sessionFactory == nil {
		app.conv.AddError("import: session storage is unavailable")
		app.refreshChat()
		return
	}
	ctx := app.ctxOrBackground()
	cwd := app.sessionState().CWD()
	go func() {
		ok, err := app.Confirm(ctx, "Import session", "Replace current session with "+path+"?")
		var id string
		if ok && err == nil {
			id, err = app.importSession(ctx, path, cwd)
		}
		app.post(func(*tauui.UpdateCtx) {
			switch {
			case err != nil:
				app.conv.AddError("Failed to import session: " + err.Error())
			case !ok:
				app.conv.AddSystem("Import cancelled")
			default:
				cmdResume(app, id)
				app.conv.AddSystem("Session imported from: " + path)
			}
			app.refreshChat()
		})
	}()
}

// pathCommandArgument returns the first argument, honoring one level of
// single or double quotes so paths with spaces work. An unclosed quote yields
// no path.
func pathCommandArgument(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	if q := args[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(args[1:], q)
		if end < 0 {
			return ""
		}
		return args[1 : end+1]
	}
	if i := strings.IndexFunc(args, unicode.IsSpace); i >= 0 {
		return args[:i]
	}
	return args
}
