package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

type SessionActionFunc func(context.Context, string) error

type workspaceOverlay struct {
	app      *App
	sessions []sdk.SessionInfo
	filter   string
	focused  int
	rename   bool
	name     string
}

func newWorkspaceOverlay(app *App, sessions []sdk.SessionInfo) *workspaceOverlay {
	sessions = append([]sdk.SessionInfo(nil), sessions...)
	sortSessionInfos(sessions)
	return &workspaceOverlay{app: app, sessions: sessions}
}

func (o *workspaceOverlay) Render(width int) []string {
	if width <= 0 {
		width = 80
	}
	var rows []string
	if o.rename {
		rows = append(rows, "Workspace — rename session")
		rows = append(rows, "> "+o.name)
		rows = append(rows, "Enter save · Esc cancel")
		return rows
	}
	rows = append(rows, "Workspace — Enter resume · n new · f fork · r rename · p pin · Esc close")
	rows = append(rows, "> "+o.filter)
	items := o.items()
	if len(items) == 0 {
		rows = append(rows, "no sessions")
		return rows
	}
	for i, sess := range items {
		mark := "  "
		if i == o.focused {
			mark = "→ "
		}
		pin := " "
		if sess.Pinned {
			pin = "★"
		}
		active := " "
		status := workspaceStatusText(sess)
		if o.app != nil && sess.ID == o.app.SessionID() {
			active = "*"
			// The persisted status is stale for the in-process session while it
			// is mid-turn; mirror the same live overlay RPC's workspaceStatus
			// uses so the flat view and remote clients agree.
			if live := o.app.liveWorkspaceStatus(); live != "" {
				status = live
			}
		}
		line := fmt.Sprintf("%s%s%s %-10s %-12s %4d %s", mark, pin, active, shortSessionID(sess.ID), status, sess.MessageCount, sessionInfoDisplayPrompt(sess))
		if meta := sessionInfoDisplayMeta(sess); meta != "" {
			line += "  " + meta
		}
		rows = append(rows, truncatePlain(line, width))
	}
	return rows
}

func (o *workspaceOverlay) HandleKey(ke tauui.KeyEvent) {
	if o == nil {
		return
	}
	if o.rename {
		o.handleRenameKey(ke)
		return
	}
	switch ke.Key {
	case tauui.KeyUp:
		o.move(-1)
	case tauui.KeyDown:
		o.move(1)
	case tauui.KeyPageUp:
		o.move(-10)
	case tauui.KeyPageDown:
		o.move(10)
	case tauui.KeyBackspace:
		if o.filter != "" {
			runes := []rune(o.filter)
			o.filter = string(runes[:len(runes)-1])
			o.clamp()
		}
	case tauui.KeyEnter:
		o.resumeFocused()
	case tauui.KeyRune:
		switch ke.Rune {
		case 'n':
			o.newSession()
		case 'f':
			o.forkFocused()
		case 'p':
			o.togglePinFocused()
		case 'r':
			o.startRename()
		default:
			if ke.Mod == 0 && ke.Rune >= 32 {
				o.filter += string(ke.Rune)
				o.clamp()
			}
		}
	}
}

func (o *workspaceOverlay) handleRenameKey(ke tauui.KeyEvent) {
	switch ke.Key {
	case tauui.KeyEsc:
		o.rename = false
	case tauui.KeyBackspace:
		if o.name != "" {
			runes := []rune(o.name)
			o.name = string(runes[:len(runes)-1])
		}
	case tauui.KeyEnter:
		o.commitRename()
	case tauui.KeyRune:
		if ke.Mod == 0 && ke.Rune >= 32 {
			o.name += string(ke.Rune)
		}
	}
}

func (o *workspaceOverlay) items() []sdk.SessionInfo {
	lower := strings.ToLower(strings.TrimSpace(o.filter))
	out := make([]sdk.SessionInfo, 0, len(o.sessions))
	for _, sess := range o.sessions {
		if sessionInfoMatches(sess, lower) {
			out = append(out, sess)
		}
	}
	return out
}

func (o *workspaceOverlay) focusedSession() (sdk.SessionInfo, bool) {
	items := o.items()
	if len(items) == 0 {
		return sdk.SessionInfo{}, false
	}
	if o.focused >= len(items) {
		o.focused = len(items) - 1
	}
	if o.focused < 0 {
		o.focused = 0
	}
	return items[o.focused], true
}

func (o *workspaceOverlay) move(delta int) {
	o.focused += delta
	o.clamp()
}

func (o *workspaceOverlay) clamp() {
	items := o.items()
	if len(items) == 0 {
		o.focused = 0
		return
	}
	if o.focused < 0 {
		o.focused = 0
	}
	if o.focused >= len(items) {
		o.focused = len(items) - 1
	}
}

func (o *workspaceOverlay) resumeFocused() {
	sess, ok := o.focusedSession()
	if !ok || o.app == nil {
		return
	}
	o.app.dismissDialog()
	cmdResume(o.app, sess.ID)
}

func (o *workspaceOverlay) newSession() {
	if o.app == nil {
		return
	}
	o.app.dismissDialog()
	cmdNew(o.app, "")
}

func (o *workspaceOverlay) forkFocused() {
	sess, ok := o.focusedSession()
	if !ok || o.app == nil {
		return
	}
	if o.app.sessionFactory == nil {
		o.app.conv.AddError("workspace: session switching is unavailable")
		o.app.refreshChat()
		return
	}
	state := o.app.sessionState()
	newSess, err := o.app.sessionFactory(o.app.ctxOrBackground(), sdk.SessionOptions{ForkFrom: sess.ID, CWD: state.CWD(), UI: nil})
	if err != nil {
		o.app.conv.AddError("workspace fork: " + err.Error())
		o.app.refreshChat()
		return
	}
	if o.app.sess != nil {
		if err := o.app.sess.Close(o.app.ctxOrBackground()); err != nil {
			if closeErr := newSess.Close(o.app.ctxOrBackground()); closeErr != nil {
				o.app.conv.AddError("workspace fork: cleanup replacement session: " + closeErr.Error())
			}
			o.app.conv.AddError("workspace fork: close current session: " + err.Error())
			o.app.refreshChat()
			return
		}
	}
	o.app.dismissDialog()
	o.app.sess = newSess
	o.app.wireSessionEvents()
	o.app.currentModel = newSess.State().Model()
	o.app.replaceConversation(NewConversationModel())
	o.app.seedChatFromSession(newSess, "Forked session "+sess.ID+" -> "+newSess.ID())
	o.app.refreshFooterStats(o.app.loopContext())
	o.app.refreshChat()
}

func (o *workspaceOverlay) togglePinFocused() {
	sess, ok := o.focusedSession()
	if !ok || o.app == nil {
		return
	}
	fn := o.app.pinSession
	pinned := true
	if sess.Pinned {
		fn = o.app.unpinSession
		pinned = false
	}
	if fn == nil {
		o.app.conv.AddError("workspace: pinning is unavailable")
		o.app.refreshChat()
		return
	}
	if err := fn(o.app.ctxOrBackground(), sess.ID); err != nil {
		o.app.conv.AddError("workspace pin: " + err.Error())
		o.app.refreshChat()
		return
	}
	for i := range o.sessions {
		if o.sessions[i].ID == sess.ID {
			o.sessions[i].Pinned = pinned
			o.sessions[i].PinnedAt = "now"
			break
		}
	}
	sortSessionInfos(o.sessions)
	o.clamp()
}

func (o *workspaceOverlay) startRename() {
	sess, ok := o.focusedSession()
	if !ok {
		return
	}
	o.rename = true
	o.name = sessionInfoDisplayPrompt(sess)
}

func (o *workspaceOverlay) commitRename() {
	sess, ok := o.focusedSession()
	if !ok || o.app == nil {
		return
	}
	name := strings.TrimSpace(o.name)
	if name == "" {
		o.rename = false
		return
	}
	if o.app.renameSession == nil {
		o.app.conv.AddError("workspace: rename is unavailable")
		o.app.refreshChat()
		return
	}
	if err := o.app.renameSession(o.app.ctxOrBackground(), sess.ID, name); err != nil {
		o.app.conv.AddError("workspace rename: " + err.Error())
		o.app.refreshChat()
		return
	}
	for i := range o.sessions {
		if o.sessions[i].ID == sess.ID {
			o.sessions[i].Name = name
			o.sessions[i].FirstPrompt = ""
			break
		}
	}
	o.rename = false
}

func sortSessionInfos(sessions []sdk.SessionInfo) {
	slices.SortStableFunc(sessions, func(a, b sdk.SessionInfo) int {
		if a.Pinned != b.Pinned {
			if a.Pinned {
				return -1
			}
			return 1
		}
		at := sessionInfoTime(a)
		bt := sessionInfoTime(b)
		if at != bt {
			return strings.Compare(bt, at)
		}
		return strings.Compare(b.Created, a.Created)
	})
}

func workspaceStatusText(sess sdk.SessionInfo) string {
	if sess.Status != "" {
		return sess.Status
	}
	return "waiting"
}

// liveWorkspaceStatus overlays the in-process session's live state on its
// workspace row, using the same status vocabulary the RPC workspace reports
// for the active session: streaming is "running", pending queues are
// "queued", and anything else defers to the persisted status.
func (a *App) liveWorkspaceStatus() string {
	if a == nil || a.sess == nil {
		return ""
	}
	if a.isStreaming() {
		return "running"
	}
	if q, ok := a.sess.(sdk.Queuer); ok {
		if steering, followUp := q.QueueSizes(); steering+followUp > 0 {
			return "queued"
		}
	}
	return ""
}

func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncatePlain(s string, width int) string {
	if width <= 0 || len([]rune(s)) <= width {
		return s
	}
	if width <= 3 {
		return string([]rune(s)[:width])
	}
	return string([]rune(s)[:width-3]) + "..."
}
