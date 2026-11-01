package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
	"github.com/losinggeneration/tau/tui/autocomplete"
	"github.com/losinggeneration/tau/tui/extensions"
	"github.com/losinggeneration/tau/tui/keybindings"
	"github.com/losinggeneration/tau/tui/keymap"
	"github.com/losinggeneration/tau/tui/widgets"
)

// App-layer semantic actions. These start above the tauui.Action range to avoid
// collisions with toolkit actions. The Go names use the AppAction* prefix
// because Go identifiers can't contain dots. Each action's external id is
// documented on its line.
//
// External-id mapping:
//
//	AppActionEditorSubmit         → tau.input.submit    (Enter in editor)
//	AppActionEditorComplete       → tau.input.complete  (Tab in editor)
//	AppActionSelectorUp           → tau.select.up
//	AppActionSelectorDown         → tau.select.down
//	AppActionSelectorActivate     → tau.select.activate (Enter w/ selector)
//	AppActionMessageFollowUp      → app.message.followUp (internal/future explicit queue action)
//	AppActionMessageDequeue       → app.message.dequeue  (Alt+Backspace: pop)
//	AppActionExit                 → app.exit            (Ctrl+C: confirm-then-quit)
//	AppActionQuitNow              → (internal post-confirm quit)
const (
	// AppActionQuitNow is the internal post-confirm quit, triggered by
	// AppActionExit after the user confirms. Not a spec action id — it
	// exists only because the confirm-or-quit flow needs a distinct
	// target from the user-facing app.exit trigger.
	AppActionQuitNow tauui.Action = iota + 100

	// AppActionEditorSubmit is Enter in the editor when the inline
	// selector is NOT visible. prompt editor treats the stock ActionSubmit
	// as "insert newline", so plain Enter needs a distinct action that
	// can bubble to the Root and send the prompt.
	AppActionEditorSubmit

	// AppActionEditorComplete applies a visible choice or requests completion.
	AppActionEditorComplete

	// AppActionSelectorUp, AppActionSelectorDown, AppActionSelectorPageUp,
	// and AppActionSelectorPageDown are navigation keys in the editor while the
	// inline selector is visible. They are distinct from the stock navigation
	// actions so the prompt editor declines them and the actions bubble to Root,
	// which dispatches to the selector instead.
	AppActionSelectorUp
	AppActionSelectorDown
	AppActionSelectorPageUp
	AppActionSelectorPageDown

	// AppActionSelectorActivate is Enter in the editor when the inline
	// selector is visible. Applies the selector's focused item AND
	// commits it in one step:
	//   - commands mode: applies the command and dispatches it
	//     (e.g. /help → help command, /quit → quit)
	//   - model mode: applies the model and commits it via SetModel
	// Without this, Enter while the selector is visible would route to
	// AppActionEditorSubmit which only looks at the editor text — so
	// for model mode it would just re-trigger the picker instead of
	// picking the focused model.
	AppActionSelectorActivate

	// AppActionMessageFollowUp queues text until the active task has fully
	// stopped. Most terminals encode Alt+Enter and Shift+Enter identically
	// as ESC+Enter, so Tau does not bind it directly: preserving
	// Shift+Enter/Alt+Enter as editor newline is more important than a
	// completion-only queue shortcut. External id "app.message.followUp".
	// When the session doesn't implement FollowUpper the action is a no-op
	// and surfaces a chat-log error.
	AppActionMessageFollowUp

	// AppActionMessageDequeue is Alt+Backspace in the editor. Removes
	// the first queued follow-up message. The agent's own queue
	// doesn't expose a pop API yet (it drains strictly in FIFO order
	// at the start of the next turn), so this is a UI-only operation
	// for now: it pops the first entry from a mirror list and reports
	// the change. Future SDK work will turn this into a real pop.
	// External id "app.message.dequeue".
	AppActionMessageDequeue

	// AppActionHeaderExpand toggles the startup header between its compact and
	// expanded forms. External id "app.header.expand"; §9 requires the compact
	// form to advertise how to expand, which needs a binding to advertise.
	AppActionHeaderExpand

	// AppActionModelCycleForward and AppActionModelCycleBackward cycle the
	// active model through the current scoped-model set. External ids
	// "app.model.cycleForward" and "app.model.cycleBackward".
	AppActionModelCycleForward
	AppActionModelCycleBackward

	// AppActionModelSelect opens the model picker — the same inline selector
	// /model opens, so the key and the command cannot drift apart. External id
	// "app.model.select".
	AppActionModelSelect

	// AppActionThinkingCycle steps the session's thinking level through the
	// ordered ladder (off → minimal → … → xhigh → off), and
	// AppActionThinkingToggle flips between off and the last non-off level.
	// External ids "app.thinking.cycle" and "app.thinking.toggle".
	AppActionThinkingCycle
	AppActionThinkingToggle

	// AppActionSuspend stops the process and returns to the shell, restoring
	// the terminal on the way out and repainting on resume. External id
	// "app.suspend".
	AppActionSuspend

	// AppActionExtensionShortcut runs the extension shortcut the keymap
	// matched. Not a spec id: extension shortcuts are keyed by the extension's
	// own id, so one Go action carries all of them and the keymap records which
	// one on the App. See App.takePendingShortcut.
	AppActionExtensionShortcut

	// AppActionScopedModels* are /scoped-models dialog actions mirroring
	// Pi's current keymap: all, clear, provider toggle, reorder, and save.
	AppActionScopedModelsAll
	AppActionScopedModelsClear
	AppActionScopedModelsProvider
	AppActionScopedModelsMoveUp
	AppActionScopedModelsMoveDown
	AppActionScopedModelsSave

	// AppActionHistorySearchBackward and AppActionHistorySearchForward open a
	// dialog-backed prompt-history picker. External ids
	// "tau.input.historySearchBackward" and "tau.input.historySearchForward".
	AppActionHistorySearchBackward
	AppActionHistorySearchForward

	// AppActionEditorBackslashSubmit removes the escape before submitting when Enter inserts a newline.
	AppActionEditorBackslashSubmit

	// AppActionExit is Ctrl+C. The Root handler decides whether to
	// clear the editor (when it has text), start a confirm-to-quit
	// timer (when the editor is empty and no confirm is pending), or
	// actually quit (when a confirm is already pending). External id
	// "app.exit"; the confirm step protects against accidental exits.
	AppActionExit

	// AppActionPaste requests a terminal clipboard read (Ctrl+V) and inserts
	// the async response into the editor through the same paste path as
	// bracketed paste. External id "app.clipboard.pasteImage".
	AppActionPaste

	// AppActionChatScrollBottom jumps the chat viewport to the end of the
	// transcript and resumes follow-the-tail. It backs the scroll-to-bottom
	// status line shown while the user is scrolled away. External id
	// "app.chat.scrollBottom".
	AppActionChatScrollBottom

	// AppActionMessageCopy copies the login dialog's sign-in URL while that
	// dialog is open and the last reply otherwise. External id
	// "app.message.copy".
	AppActionMessageCopy
)

// selectorMode enumerates the modes the inline Selector can be in. The
// selector itself is the same widget regardless of mode; only its
// header, item list, and apply callback differ.
type selectorMode int

const (
	selectorModeNone     selectorMode = iota // selector hidden
	selectorModeCommands                     // filtering slash commands
	selectorModeModels                       // picking a model
	selectorModeSessions                     // picking a session to resume
	selectorModeFiles                        // picking a file reference after @
	selectorModeFork                         // picking a message to fork from
	selectorModeForget                       // picking a message to forget from
)

// EditorHeight is the number of content rows reserved for the prompt editor
// (the border adds one cell of padding on each side).
const EditorHeight = 5

// App is the interactive tau TUI. It owns the top-level toolkit App, the
// conversation model, the chat-log view, the prompt editor, an inline
// Selector that drives both slash-command completion and the model
// picker, and the agent session. Agent events arrive on a background
// goroutine and are forwarded to the toolkit loop via App.Post so all
// view mutations stay single-threaded.
//
// Layout (top → bottom): header, chat log, prompt editor, inline
// selector (only when active), footer. The selector's height is 0
// when hidden so the VStack collapses the row and the layout looks
// like the spec example:
//
//	+------------------------------+
//	| header                       |
//	| chat log (grows)             |
//	| prompt editor                |
//	| selector (commands or models)|  <-- only when active
//	| footer                       |
//	+------------------------------+
type App struct {
	app    *tauui.App
	sess   sdk.Session
	conv   *ConversationModel
	chat   *ChatLog
	scroll *widgets.ScrollView
	// followChat records app-loop-owned follow-the-tail intent, consumed by a
	// current render result.
	followChat bool

	// historyNext is the session entry to page older chat history in before,
	// empty when nothing older is held in the file. historyLoading marks a
	// page read in flight; historyGeneration invalidates reads that a
	// transcript replacement or a drop overtook. All loop-owned.
	historyNext       string
	historyLoading    bool
	historyGeneration uint64
	// autoScrollOff suspends follow-the-tail while the user reads away from the
	// end of the chat buffer. It is runtime UI state, never configuration: it
	// is set by user scroll events through chatScrollViewport.onUserScroll and
	// cleared when the user scrolls back to the buffer's end, when the
	// transcript is replaced, or when the layout re-anchors to the end. While
	// set it outranks any future autoscroll configuration — the user's scroll
	// position is authoritative — and at the end of the buffer the viewport is
	// following again, so a configured value would have nothing left to do.
	//
	// Mutate only via setAutoScrollOff so the pendingNewMessages counter is
	// reset whenever the user comes back to the bottom — without that, the
	// status line's new-message count would survive a return-to-bottom and
	// misreport the state.
	autoScrollOff bool
	// pendingNewMessages is the count of new messages that arrived while the
	// user was scrolled away from the end of the chat buffer. The conversation
	// event adapter increments it per message boundary (assistant message_start,
	// user message_end with a real row) while autoScrollOff is set, and the
	// chat status line paints the count while it is non-zero. setAutoScrollOff
	// clears it when the user returns to the bottom; transcript replacement and
	// layout switches clear it for the same reason. The counter only ever
	// increments in the fixed layout, where user-driven scroll-away is tracked;
	// the document layout does not surface this indicator yet.
	pendingNewMessages int
	// lastChatStatusRect is the rect where the chat status line last painted,
	// kept so a transition from visible to hidden can invalidate the cells the
	// line was occupying. Without it, the line would persist on screen until
	// the next full repaint.
	lastChatStatusRect tauui.Rect
	editor             *tauui.PromptEditor
	footer             *Footer
	root               *Root

	// pending renders the queued follow-up messages above the editor
	// in the layout stack. It collapses to 0x0 when the queue is
	// empty, so the VStack treats it as absent in the common case.
	// The widget is driven by the App's local mirror of the agent's
	// follow-up queue (a.pendingQueue), kept in sync via
	// refreshPending().
	pending *pendingView
	// pendingQueue is a UI-only mirror of the agent's follow-up queue
	// maintained by the App. The agent's queue is the source of truth
	// (drained in FIFO order at the start of the next turn), but the
	// agent doesn't currently expose a pop-by-index API, so this
	// mirror is what the user sees. Mutations happen on the app loop.
	pendingQueue []string
	// steeringQueue mirrors active-run steering messages separately from
	// completion-only follow-ups so the pending region can render the spec
	// Steering:/Follow-up: labels.
	steeringQueue []string
	// userBashCancel cancels the running !/!! shell command. Loop-owned.
	userBashCancel context.CancelFunc
	userBashSeq    int

	// status renders the keyed status lines (extension status,
	// working messages) above the editor and below the pending
	// view. Driven by App.setStatus / clearStatus on the app
	// loop. The view collapses to 0x0 when the entry map is empty
	// so the VStack treats the region as absent.
	status        *tauui.StatusEntriesView
	programStatus programStatusReporter
	// statusEntries is the keyed map of status text. Each entry
	// is "key: text"; the same key can be re-set to update the
	// text. Empty text and explicit clearStatus both remove the
	// entry. The map is mutated on the app loop only.
	statusEntries map[string]string
	// widgetsAbove / widgetsBelow render extension-supplied
	// widgets on each side of the editor. They are driven by
	// App.setWidget / clearWidget. Each container holds a keyed
	// map of string-array content; per-widget lines are capped at
	// MaxWidgetLines. Empty maps collapse the container to 0x0.
	widgetsAbove *widgetContainer
	widgetsBelow *widgetContainer
	// widgetEntries is the per-side map of widget lines. There are
	// two maps (above and below); the placement argument on
	// setWidget picks which one. Both maps are mutated on the
	// app loop only.
	widgetEntriesAbove map[string][]string
	widgetEntriesBelow map[string][]string
	widgetOrderAbove   []string
	widgetOrderBelow   []string

	// selector is the inline non-capturing overlay used for
	// slash-command autocomplete and `/model` selection. It is shown as a
	// separate child of the VStack (below the editor); the editor keeps
	// focus so the user can keep typing their command/model filter. The
	// picker's MinSize collapses to 0×0 when hidden so the VStack treats
	// it as absent in the common case.
	selector *tauui.Selector
	// selectorMode records what the inline selector is currently
	// displaying so the editor's OnChange handler can decide
	// whether the new text should keep showing the same mode
	// (e.g. typing more characters of a command or model filter).
	selectorMode     selectorMode
	completionSeq    int
	forcedCompletion bool
	// completionSelector mirrors the top-level autocomplete provider's suggestions
	// while the inline selector is showing provider-backed completions (currently
	// @file references). The selector only owns rendering/focus; the provider owns
	// matching and tauui.CompletionSelector owns editor rewrites.
	completionSelector tauui.CompletionSelector
	// dialogPicker is the focusable picker for future full dialogs
	// (theme/session pickers, etc.). `/model` intentionally does not use
	// it: model filtering is driven by prompt text and the editor must keep
	// focus. The dialog picker lives as a separate child of the VStack
	// (below the editor, above the footer); its MinSize collapses to 0×0
	// when hidden so the VStack treats it as absent. When shown for a true
	// dialog, the picker takes focus (via Post to ensure the toolkit's node
	// map has been rebuilt) so the user can type to filter and navigate
	// with arrows. The editor remains in the layout but loses focus while
	// the dialog is active.
	dialogPicker      *tauui.DialogPicker
	dialogMu          sync.Mutex
	dialogStopped     chan struct{}
	dialogInputActive atomic.Bool
	extensionDialog   *extensionDialog
	loginDialog       *loginDialog
	loginWorkers      sync.WaitGroup
	// openURL and clipboardWrite default to the system browser and the
	// toolkit clipboard; tests replace them so no helper process starts.
	openURL func(string) error
	// mcpMenu identifies the /mcp menu the dialog picker shows, so a stale
	// menu's change listener unsubscribes.
	mcpMenu        *int
	clipboardWrite func(string, func(error))
	// currentModel is the key (provider/id) of the session's active
	// model. Cached so model items can be sorted with the active model
	// at the top of the list and marked with the active marker (the
	// selector's `→` glyph naturally falls on the focused row, so we
	// position focus on the active model when the picker opens).
	currentModel string
	// pendingShortcut carries the extension shortcut the keymap matched from
	// Resolve to HandleAction. Guarded because Resolve can be reached from the
	// input goroutine while a handler runs on the loop.
	pendingMu       sync.Mutex
	pendingShortcut *extensions.Shortcut
	// lastThinkingLevel is the non-off level app.thinking.toggle restores, so
	// switching thinking off and back on returns to the effort the user was
	// working at rather than to a fixed default.
	lastThinkingLevel string
	// credentialStore is an auth snapshot source for filtering the model
	// picker to providers the user is currently authenticated for. It uses
	// List only so opening/filtering the picker does not refresh OAuth tokens.
	credentialStore CredentialRefLister
	// authStore is the mutable credential store used by /login and /logout.
	authStore CredentialStore
	// settingsPath is where /model persists the selected default model.
	// Empty means /model only changes the active session model.
	settingsPath string

	// emptyResponseCount tracks consecutive empty assistant responses. Empty
	// responses are summarized in one system row instead of appending one blank
	// assistant row per provider response.
	emptyResponseCount int
	conversationEvents *conversationEventAdapter

	// pendingModelKey is the full provider/id key the user just picked
	// from the inline model picker. Set in applySelector when the user
	// confirms a model item (Tab or Enter on the visible picker), and
	// consumed by trySubmit when committing `/model <id>`. Without
	// this, the editor would contain just the bare model id (Pi
	// style), which is ambiguous when multiple providers expose the
	// same id (e.g. `claude-sonnet-4-6` exists under anthropic,
	// amazon-bedrock, google-antigravity, opencode, etc.) — the bare
	// id would match 7+ models and trySubmit would reject it as
	// unknown. The pending key remembers which of those the user
	// actually picked so the commit step can resolve unambiguously.
	// Cleared whenever the editor text diverges from what the picker
	// filled, so editing the id always re-resolves from the current
	// text.
	pendingModelKey string

	// scopedModelKeys is nil when all models are enabled for model cycling.
	// A non-nil slice is the explicit ordered set selected through
	// /scoped-models. An empty non-nil slice means no models are enabled.
	scopedModelKeys []string
	// scopedThinking holds the ":level" suffixes of the startup scope
	// patterns by model key. Cycling to such a model applies its level.
	scopedThinking map[string]string

	// extensions is the plugin extension registry. It is the public
	// surface plugins and tests use to register custom message/tool
	// renderers, widgets, shortcuts, editor/paste manipulation,
	// theme queries, and tool expansion state. The App exposes the
	// registry so the same surface the TUI's own custom-renderer
	// pipeline (pluginRenderers) reads from is also reachable by
	// plugins running in the same process.
	extensions *extensions.Registry

	// header is region 1: the startup banner. Held so the app can hide it once
	// the transcript has content and toggle its expanded form.
	header *Header

	// workingOverride replaces the default run message when an extension sets
	// one. Guarded by workingMu because extensions may set it off-loop.
	workingOverride string
	// usageLimitWaitUntil/Provider drive the run-message countdown while the
	// agent is sleeping out a provider usage-limit window. Guarded by
	// workingMu for the same reason.
	usageLimitWaitUntil time.Time
	usageLimitProvider  string
	// pausedSince/Target drive the run message while a pause flag holds
	// provider requests.
	pausedSince  time.Time
	pausedTarget string
	workingMu    sync.Mutex

	// themeColors resolves discovered theme files to their colour tokens.
	// Nil means only the built-in token sets are available.
	themeColors   ThemeColorLookup
	themeLookup   ThemeLookup
	refreshTheme  func(string) (bool, error)
	terminalTheme terminalThemeState

	// keybindings is the app-layer action registry (app.md §12). Held so
	// /hotkeys and the settings UI can describe the live bindings rather than
	// a hardcoded list, and so tests can assert on resolution.
	keybindings *keybindings.Manager

	// testExternalCommands lets tests inject additional slash
	// commands without standing up a full session. The field is
	// ignored when nil; production code never reads it.
	testExternalCommands []ExternalCommand
	// hideSkillCommands keeps /skill: commands out of completion and help
	// when enableSkillCommands is false; typed skill commands still run.
	hideSkillCommands bool

	// pluginRenderers is the role/tool-keyed registry the chat log
	// consults before falling back to the built-in projection.
	// Plugins populate it through the extensions registry; the chat
	// log takes a per-component snapshot so the registry can be
	// mutated from any goroutine without serializing renders.
	pluginRenderers *pluginRenderers

	cmds *Commands

	ctx       context.Context
	cancelRun context.CancelFunc

	// pendingUserDisplays maps submitted prompt payloads back to the text the user
	// typed before client-side expansion (for example @file/@directory expansion)
	// so user rows in the chat log stay readable and retryable. Mutated on the app
	// loop by routePrompt/submit and consumed by conversationEventAdapter.
	pendingUserDisplays []submittedPromptDisplay
	// pastedImages maps the bare @markers the TUI minted for pasted clipboard
	// images ("pasted-image.png") to the saved file's absolute path. Pasted
	// images live in the session data dir, outside the prompt root, so these
	// markers resolve against the data dir first — otherwise a same-named
	// working-tree file would silently shadow the image the user pasted.
	// Mutated on the app loop by savePastedImage; read at expansion time.
	pastedImages map[string]string
	// nextImageDisplayNumber labels user-attached images as [Image #n]. It is UI
	// state only; the model receives the original image blocks.
	nextImageDisplayNumber int

	// initialPrompt is sent once the event loop starts (Options.InitialPrompt).
	initialPrompt string
	// sessionFactory creates replacement sessions for interactive session
	// switching commands such as /resume. When nil, those commands report
	// that switching is unavailable rather than constructing a partial runtime.
	sessionFactory SessionFactory
	sessionLister  SessionListFunc
	// sessionSelectorCache holds the saved-session list while `/resume ...` is
	// being filtered. Loading sessions can stat/read the session index, so keep
	// that out of the per-keystroke filter path and clear it when the picker is
	// selected, cancelled, or the editor leaves `/resume` mode.
	sessionSelectorCache      []sdk.SessionInfo
	sessionSelectorCacheValid bool
	forkEntries               []sdk.ForkEntry
	forkShowAll               bool
	// renameSession renames a persisted session from the workspace UI.
	renameSession func(context.Context, string, string) error
	// pinSession and unpinSession update workspace pin state.
	pinSession   SessionActionFunc
	unpinSession SessionActionFunc
	// deleteSession moves a persisted session to the trash from the
	// workspace UI.
	deleteSession SessionActionFunc
	importSession func(ctx context.Context, src, cwd string) (string, error)

	// loopCtx is the most recent *tauui.Ctx observed during a paint pass. The
	// toolkit's Invalidate closure is stable for the app's lifetime, so it is
	// safe to reuse for off-loop scroll calls (e.g. from posted callbacks).
	loopMu  sync.RWMutex
	loopCtx *tauui.Ctx

	// streaming tracks whether an agent run is in flight.
	mu        sync.Mutex
	streaming bool

	// throbber drives the animated Working/Thinking indicators while a
	// run is active. Its ticker is owned by setStreaming and posts frame
	// updates onto the app loop.
	throbber      throbber
	throbberTimer *time.Ticker
	throbberDone  chan struct{}

	// compactionRunning tracks whether the out-of-band /compact loader is active.
	// It shares the same throbber/ticker as the agent Working indicator so the
	// frame cadence cannot drift between the two status rows.
	compactionRunning bool

	// confirmQuitActive is true while the "press ctrl+c to exit"
	// notice is showing in the footer. Set by confirmOrQuit when the
	// editor is empty and Ctrl+C is pressed; cleared by the confirm
	// timer when it elapses (1.5s) or when Ctrl+C is pressed a second
	// time (which actually quits). Guarded by confirmQuitMu so the
	// timer goroutine and the app loop don't race.
	confirmQuitMu     sync.Mutex
	confirmQuitActive bool
	confirmQuitTimer  *time.Timer
}

type submittedPromptDisplay struct {
	sent    string
	display string
	images  []ImageComponent
}

// CredentialRefLister is the non-refreshing auth snapshot surface the TUI uses
// to decide which providers should appear in the model picker.
type CredentialRefLister interface {
	List(context.Context) ([]ai.CredentialRef, error)
}

// Options configures the interactive TUI.
// SessionFactory creates a session for in-TUI session switching.
type SessionFactory func(context.Context, sdk.SessionOptions) (sdk.Session, error)

// SessionListFunc returns the saved sessions available to interactive pickers.
type SessionListFunc func(context.Context) ([]sdk.SessionInfo, error)

func boolPtr(v bool) *bool {
	return &v
}

func appTerminalTitle(sessionID string, streaming bool) string {
	title := "tau"
	if sessionID != "" {
		title = "tau — " + sessionID
	}
	if streaming {
		title = "⚙ " + title
	}
	return title
}

type Options struct {
	// Session is the agent session that receives prompts. Required.
	Session sdk.Session
	// Version is shown in the header.
	Version string
	// CWD is shown in the footer.
	CWD string
	// ModelSpec is the resolved model id shown in the footer.
	ModelSpec string
	// InitialPrompt is sent as the first user message once the TUI starts.
	// Empty means the session opens with an empty chat log.
	InitialPrompt string
	// CredentialStore provides credential access for /login, /logout, and the
	// authenticated model picker. Nil means only environment-authenticated
	// providers are shown and auth slash commands report unavailable.
	CredentialStore CredentialStore
	// SettingsPath is where /model persists the selected default model. Empty
	// keeps /model as a session-only switch.
	SettingsPath string
	// ModelScope holds --models or enabledModels patterns that limit model
	// cycling. Empty cycles every available model.
	ModelScope []string
	// SessionFactory creates replacement sessions for commands like /resume.
	// Nil disables in-process session switching.
	SessionFactory SessionFactory
	// ListSessions returns saved sessions for /resume and /session.
	ListSessions SessionListFunc
	// RenameSession renames a persisted session from the workspace UI.
	RenameSession func(context.Context, string, string) error
	// PinSession pins a persisted session in workspace lists.
	PinSession SessionActionFunc
	// UnpinSession removes a workspace pin.
	UnpinSession SessionActionFunc
	// DeleteSession moves a persisted session to the trash from the
	// workspace UI.
	DeleteSession SessionActionFunc
	// ImportSession copies a session file into storage for cwd and returns
	// the session ID, which /import then resumes.
	ImportSession func(ctx context.Context, src, cwd string) (string, error)
	// ThemeColors resolves a theme name to its colour tokens, so themes
	// discovered as resources actually repaint the UI. Nil restricts the app to
	// its built-in token sets.
	ThemeColors ThemeColorLookup
	// ThemeLookup preserves file appearance metadata and takes precedence over ThemeColors.
	ThemeLookup ThemeLookup
	// RefreshTheme rereads the selected file, retaining the last valid registry entry on error.
	RefreshTheme func(string) (bool, error)
	// StartupMessages are shown under the startup header before the first turn.
	StartupMessages []string
}

// New builds the toolkit app and assembles the layout region stack:
// header, chat log, prompt editor, optional pending view, inline
// selector, and footer. It does not start the event loop; call Run.
func New(opts Options) (*App, error) {
	if opts.Session == nil {
		return nil, fmt.Errorf("tui: Options.Session is required")
	}

	conv := NewConversationModel()
	chatLog := NewChatLog(conv)

	// The chat log is a full-width vertical flow: no border, no background, no
	// gutter. ScrollbarAuto reserved a column the moment content overflowed,
	// which is not just a visual gutter — the chat's wrap width changed as the
	// transcript crossed one screen, re-wrapping the whole history and flushing
	// the width-keyed render cache at exactly the point the transcript got
	// expensive to wrap. Scrolling itself (wheel, keys, selection) does not
	// depend on the bar being drawn.
	scroll := widgets.NewScrollView(widgets.ScrollViewOpts{
		Child:     chatLog,
		Focusable: true,
		Scrollbar: widgets.ScrollbarHidden,
	})
	chatViewport := &chatScrollViewport{ScrollView: scroll, chat: chatLog}

	editor := tauui.NewPromptEditor()

	footerData := &footerData{cwd: opts.CWD, model: opts.ModelSpec}
	footer := NewFooter(footerData)
	header := NewHeader(opts.Version)

	cmds := NewCommands()

	// Build the pickers. The inline selector filters from the editor's
	// text and stays out of the way (editor keeps focus); it backs slash
	// commands and `/model`. The dialog picker is reserved for future true
	// modal pickers that intentionally own their input.
	selector := tauui.NewSelector()
	dialogPicker := tauui.NewDialogPicker()
	pending := NewPendingView()
	status := tauui.NewStatusEntriesView(5)
	widgetsAbove := NewWidgetContainer(widgetPlacementAbove)
	widgetsBelow := NewWidgetContainer(widgetPlacementBelow)

	a := &App{
		conv:            conv,
		chat:            chatLog,
		scroll:          scroll,
		editor:          editor,
		footer:          footer,
		cmds:            cmds,
		sess:            opts.Session,
		selector:        selector,
		dialogPicker:    dialogPicker,
		pending:         pending,
		status:          status,
		widgetsAbove:    widgetsAbove,
		widgetsBelow:    widgetsBelow,
		initialPrompt:   opts.InitialPrompt,
		sessionFactory:  opts.SessionFactory,
		sessionLister:   opts.ListSessions,
		renameSession:   opts.RenameSession,
		pinSession:      opts.PinSession,
		unpinSession:    opts.UnpinSession,
		deleteSession:   opts.DeleteSession,
		importSession:   opts.ImportSession,
		authStore:       opts.CredentialStore,
		credentialStore: opts.CredentialStore,
		settingsPath:    opts.SettingsPath,
		themeColors:     opts.ThemeColors,
		themeLookup:     opts.ThemeLookup,
		refreshTheme:    opts.RefreshTheme,
		header:          header,
		extensions:      extensions.New(),
		pluginRenderers: newPluginRenderers(),
	}
	a.conversationEvents = newConversationEventAdapter(a)
	// A manual scroll suspends follow-the-tail until the user returns to the
	// end of the buffer: while messages stream in, renders must not yank a
	// reading user to the bottom.
	chatViewport.onUserScroll = a.chatUserScrolled
	// Hook the plugin renderer registry into the chat log so registered
	// message/tool renderers can replace the default projection. The
	// chat log takes a snapshot of the registry under mu during each
	// component render so plugins can register from any goroutine
	// without holding the chat log's lock.
	if a.pluginRenderers != nil {
		chatLog.setPluginRenderers(a.pluginRenderers)
	}
	// Pull any renderers a plugin may have already registered into the
	// chat log's plugin renderer chain. Without this bridge a plugin
	// would only be able to query its own renderers through
	// Registry.RenderMessage/RenderTool — the chat viewport would never
	// consult them. Re-running ApplyExtensionsRegistry after future
	// plugin loads reuses the same path to pick up late registrations.
	if a.pluginRenderers != nil && a.extensions != nil {
		a.pluginRenderers.ApplyExtensionsRegistry(a.extensions)
	}
	// Subscribe to the session's out-of-band events (compaction). These do not
	// arrive on a prompt's event channel because they happen without a prompt
	// in flight; see sdk.EventObserver.
	a.wireSessionEvents()
	// Wire the registry's on-change callbacks so plugin-initiated
	// theme/editor-text/tool-expansion edits propagate to the live UI
	// rather than living only in the registry's internal state. The
	// helpers below run on the app loop (via Post when called from a
	// background goroutine) so they don't race the toolkit render.
	if a.extensions != nil {
		a.extensions.OnThemeChanged(func(theme extensions.Theme) {
			a.applyThemeFromExtension(theme)
		})
		a.extensions.OnEditorTextChanged(func(text string) {
			a.applyEditorTextFromExtension(text)
		})
		a.extensions.OnToolExpandedChanged(func(id string, expanded bool) {
			a.refreshPluginToolExpansion(id)
			_ = id
			_ = expanded
		})
	}
	if opts.Session != nil {
		a.currentModel = opts.Session.State().Model()
		a.seedChatFromSession(opts.Session, "")
		a.refreshChat()
	}
	// Seed the plugin extension registry with the App's theme. The
	// extensions package's SetTheme defensively copies the values
	// map, so the caller can safely mutate their working copy.
	initialTheme := systemThemeName
	loadedSettings := sessionSettings(opts.Session)
	if loadedSettings == nil && opts.SettingsPath != "" {
		settings, err := ai.LoadSettings(opts.SettingsPath)
		if err != nil {
			_ = chatLog.Close()
			return nil, fmt.Errorf("load tui settings: %w", err)
		}
		loadedSettings = settings
	}
	if settings := loadedSettings; settings != nil {
		if settings.Theme != "" {
			initialTheme = settings.Theme
		}
		a.applyDisplaySettings(settings)
	}
	a.applyDefaultSteeringMode(loadedSettings)
	if len(opts.ModelScope) > 0 {
		a.scopedModelKeys = []string{}
		a.scopedThinking = map[string]string{}
		for _, scoped := range ai.ExpandModelScope(opts.ModelScope, a.allScopedModelKeys()) {
			key := scoped.Model.String()
			a.scopedModelKeys = append(a.scopedModelKeys, key)
			if scoped.ThinkingLevel != "" {
				a.scopedThinking[key] = scoped.ThinkingLevel
			}
		}
	}

	// The header renders the startup banner as region 1 and reports the live
	// keybindings, so a rebind shows up in the hints rather than the defaults
	// we shipped.
	header.SetThemeTokens(func() themeTokens { return chatLog.ThemeTokens() })
	header.SetHintProvider(a.headerHints)
	header.SetMessages(opts.StartupMessages)

	// The footer reads its dim/warning/error colours from the active theme
	// rather than from hardcoded terminal colours.
	footer.SetThemeTokens(func() themeTokens { return chatLog.ThemeTokens() })
	a.applyTheme(initialTheme)

	// Wire the plugin extension APIs that need live App state:
	// editor text, paste, and tool expansion. Each of these
	// delegates through a small adapter so the App's own state
	// stays the source of truth while plugins see the same shape
	// the Registry's contract documents.
	if a.extensions != nil && a.editor != nil {
		a.extensions.SetPasteProvider(&appEditorPasteProvider{app: a})
	}
	if a.extensions != nil && a.chat != nil {
		a.chat.SetToolExpansionHandler(func(id string, expanded bool) {
			if a.extensions == nil {
				return
			}
			a.extensions.SetToolExpanded(id, expanded)
			// Mirror the read so plugins that registered before
			// the chat log finishes its initial render still see
			// the right value.
			_ = a.extensions.ToolExpanded(id) == expanded
		})
	}

	selector.SetOnSelect(a.applySelector)
	selector.SetOnClose(a.dismissSelector)
	dialogPicker.SetOnCancel(a.dismissDialog)
	dialogPicker.SetOnVisibilityChanged(func(shown bool) {
		a.programStatus.blocked = tauui.ProgramStatus{}
		if shown {
			a.programStatus.blocked = tauui.ProgramStatus{State: "blocked", Kind: "question", Message: dialogPicker.Header()}
		}
		a.refreshProgramStatus()
	})

	// Region stack (top → bottom). The middle chat region is the only flexible
	// one; every other region sizes to its content. Pending/status/widget views
	// collapse to 0x0 when empty, matching Pi's fixed eight-region shell. The
	// inline selector is still an app-specific ninth row for slash/model/file
	// completions; true dialogs replace the editor through EditorDialogRegion.
	editorRegion := NewEditorDialogRegion(editor, dialogPicker, selector)
	appLayout := tauui.NewAppLayout(tauui.AppLayoutOpts{
		Header:       header,
		Chat:         chatViewport,
		Pending:      pending,
		Status:       status,
		WidgetsAbove: widgetsAbove,
		Editor:       editorRegion,
		WidgetsBelow: widgetsBelow,
		Footer:       footer,
	})

	// Both layout models are built up front over the same region views, so
	// switching is a pointer swap rather than a rebuild. The document model
	// takes the raw chat log rather than the scroll viewport: a viewport-sized
	// chat reports the terminal height, and the document could then never grow
	// past one screen.
	documentLayout := tauui.NewDocumentLayout(tauui.AppLayoutOpts{
		Header:       header,
		Chat:         chatLog,
		Pending:      pending,
		Status:       status,
		WidgetsAbove: widgetsAbove,
		Editor:       editorRegion,
		WidgetsBelow: widgetsBelow,
		Footer:       footer,
	})

	root := &Root{AppLayout: appLayout, app: a, document: documentLayout}
	root.layout = appLayout
	a.root = root

	// User keybinding overrides live beside settings.json. A broken config must
	// not stop the app from starting — fall back to defaults and say so in the
	// chat log, because silently ignoring the file is how a user concludes
	// their rebind "just doesn't work".
	keyManager, keyErr := newAppKeybindingManager(appKeybindingsPath(opts.SettingsPath))
	if keyErr != nil {
		a.conv.AddError(fmt.Sprintf("keybindings: using defaults: %v", keyErr))
	}
	a.keybindings = keyManager
	keybindings.SetDefaultManager(keybindings.NewDefault(keyManager.UserBindings()))

	initialTokens := chatLog.ThemeTokens()
	app, err := tauui.New(tauui.Opts{
		WheelScrollLines:      new(loadedSettings.ResolveFullscreenWheelScrollLines()),
		OnTerminalColorScheme: a.applyTerminalThemeScheme,
		OnResume:              a.startTerminalTheme,
		Root:                  root,
		Title:                 appTerminalTitle(a.SessionID(), false),
		Theme:                 initialTokens.nativeTheme(),
		Keymap:                appKeymap{app: a, manager: keyManager},
		Mouse:                 true,
		BracketedPaste:        boolPtr(true),
	})
	if err != nil {
		_ = chatLog.Close()
		return nil, err
	}
	a.app = app
	app.AddInputListener(func(e tauui.Event) (tauui.Event, bool) {
		if a.dialogInputActive.Load() {
			return e, false
		}
		pe, ok := e.(tauui.PasteEvent)
		if !ok {
			return e, false
		}
		a.post(func(_ *tauui.UpdateCtx) {
			ctx := a.loopContext()
			if ctx == nil {
				ctx = &tauui.Ctx{}
			}
			ctx.FocusedID = a.editor.ID()
			a.handlePaste(pe, ctx)
		})
		return nil, true
	})

	chatLog.setReachTopHandler(a.loadOlderHistory)

	// Render preparation happens on ChatLog's private worker, but completed
	// state belongs to the toolkit loop. Keep one coalesced post outstanding;
	// the callback installs the newest prepared result before invalidating the
	// ScrollView viewport and any height-dependent layout.
	chatLog.setRenderNotifier(func() error {
		return app.Post(func(ctx *tauui.UpdateCtx) {
			update := chatLog.applyPreparedRender()
			if ctx == nil {
				return
			}
			a.handleChatRenderUpdate(update, ctx.Invalidate, ctx.InvalidateLayout)
		})
	})

	// Wire the editor's text-change callback so the inline selector can
	// show, filter, switch modes, and dismiss as the user types and deletes.
	editor.SetOnChange(func(text string, ctx *tauui.Ctx) {
		a.onEditorTextChanged(text, ctx)
	})

	// Keep regular completion in sync with cursor movement. Model picking also
	// reopens after applying a choice when the cursor returns to its name.
	editor.SetOnCursorMove(func(e tauui.EditorCursorMoveEvent, ctx *tauui.Ctx) {
		if ctx == nil || e.Reason != tauui.EditorCursorMoveNavigate {
			return
		}
		text := a.editor.Text()
		if a.forcedCompletion && a.selectorVisible() {
			a.requestCompletion(ctx, true, false)
		} else if strings.HasPrefix(text, "/model") || (a.selectorVisible() && (a.selectorMode == selectorModeCommands || a.selectorMode == selectorModeFiles)) {
			a.refreshSelector(text, ctx)
		}
	})

	return a, nil
}

// SessionID returns the currently active session identifier.
func (a *App) SessionID() string {
	if a == nil || a.sess == nil {
		return ""
	}
	return a.sess.ID()
}

// Run enables the terminal, starts the event loop, and blocks until the user
// quits. It restores the terminal on return.
func (a *App) Run(ctx context.Context) error {
	defer func() { a.stopExtensionDialogs(); a.stopLoginWorkers() }()
	a.refreshProgramStatus()
	a.ctx, a.cancelRun = context.WithCancel(ctx)
	defer a.cancelRun()
	defer func() { _ = a.chat.Close() }()

	if err := a.app.Post(func(*tauui.UpdateCtx) { a.startTerminalTheme() }); err != nil {
		return fmt.Errorf("initialize terminal theme: %w", err)
	}
	defer func() { a.terminalTheme.started = false }()
	defer a.startThemeFileWatcher(a.ctx)()

	// If an initial prompt was passed on the command line, submit it on the
	// first loop iteration so the editor is clear and the run streams in.
	if opts := a.initialPrompt; opts != "" {
		prompt := opts
		_ = a.app.Post(func(_ *tauui.UpdateCtx) {
			a.submit(prompt)
		})
	}

	// Populate footer fields that don't change during a session
	// (git branch, provider count) and seed the live ones from the
	// session's first Stats() call so the bar is correct on the
	// initial paint.
	a.initFooterStaticFields()
	a.refreshFooterStats(a.loopContext())

	// If a resumed session has a non-trivial transcript, New seeded only the
	// tail so the first frame can paint quickly. Install the full backlog only
	// after that seed render has actually completed; queuing it earlier lets the
	// full wrap cancel/replace the fast path before the terminal ever paints it.
	// The render-ready callback below calls maybeInstallDeferredSessionTranscript
	// once it observes that current seed render.

	// make sure all widgets render initially
	if err := a.app.PostInvalidateAll(); err != nil {
		return fmt.Errorf("invalidate initial tui: %w", err)
	}

	defer quitOnContextCancel(a.ctx, a.app.Quit)()

	return a.app.Run()
}

// quitOnContextCancel turns context cancellation into a quit call and returns a
// function that stops watching.
//
// The toolkit loop blocks until the user quits, so a cancelled context has to
// be translated or the TUI outlives its parent — a SIGTERM or a shutting-down
// host would otherwise leave the terminal in raw mode with the loop still
// reading keys. The returned stop function must be called when the loop exits
// for the ordinary reason (the user quit), so the watcher goroutine does not
// outlive the run.
func quitOnContextCancel(ctx context.Context, quit func()) (stop func()) {
	if ctx == nil || quit == nil {
		return func() {}
	}

	// A plain two-way select is not enough: once the run ends, both channels
	// can be ready at the same time and select picks at random, so an ordinary
	// user quit followed by parent shutdown could still fire quit. The flag
	// makes "stopped before cancelled" authoritative — stop's store
	// happens-before any later cancel, so the watcher observes it.
	var stopped atomic.Bool
	done := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			if !stopped.Load() {
				quit()
			}
		case <-done:
		}
	}()

	return sync.OnceFunc(func() {
		stopped.Store(true)
		close(done)
	})
}

func (a *App) handleChatRenderUpdate(update chatLogRenderUpdate, invalidate func(tauui.Rect), invalidateLayout func()) {
	if !update.installed {
		return
	}
	a.dropOffscreenHistory()
	// The banner is region 1, not part of the transcript, so its visibility has
	// to be reconciled whenever the transcript changes.
	a.refreshHeaderVisibility()
	if update.layoutChanged && invalidateLayout != nil {
		invalidateLayout()
	}

	viewport := update.rect
	if a.scroll != nil {
		viewport = a.scroll.Rect()
		// The fixed shell scrolls from the top, so rows the chat window
		// gained or dropped above the view would otherwise slide the content
		// under the reader. The document layout is bottom-anchored and needs
		// no correction.
		if update.topDelta != 0 && a.LayoutMode() == LayoutFixed && invalidate != nil {
			a.scroll.ScrollBy(&tauui.Ctx{Invalidate: invalidate}, update.topDelta)
		}
		// Consume follow-latest intent only when this result has caught
		// up to the newest scheduled revision. A same-width intermediate
		// result may paint during continuous streaming, but must not clear
		// the intent before the latest completed height is available.
		if a.followChat && update.current && invalidate != nil {
			// The user's scroll position outranks the follow intent: once
			// they have scrolled away from the end of the buffer, streaming
			// renders must not pull the viewport out from under them.
			// followChat stays armed so the first render after they scroll
			// back to the end re-pins the view.
			if !a.autoScrollOff {
				a.scroll.ScrollBottom(&tauui.Ctx{Invalidate: invalidate})
				a.followChat = false
			}
		}
	}
	if invalidate != nil && viewport.W > 0 && viewport.H > 0 {
		invalidate(viewport)
	}
}

// captureCtx records the context observed during a paint pass so off-loop
// code (posted callbacks) can call ctx-dependent methods like ScrollBottom.
// On the first capture it also moves focus to the editor so typing works
// immediately (the layout's first focusable widget is otherwise the chat
// scroll view). It also schedules a follow-up render so the new focusedID
// is reflected on the next paint (the current paint still uses the
// pre-request focusedID, so the cursor and other focus-dependent visuals
// would otherwise wait for the next user event).
func (a *App) captureCtx(ctx *tauui.Ctx) {
	a.loopMu.Lock()
	first := a.loopCtx == nil
	a.loopCtx = ctx
	a.loopMu.Unlock()
	if first && ctx != nil && ctx.RequestFocus != nil {
		ctx.RequestFocus(a.editor.ID())
		_ = a.app.Post(func(_ *tauui.UpdateCtx) {})
	}
}

// loopContext returns the most recently captured paint context, or nil if no
// paint has occurred yet.
func (a *App) loopContext() *tauui.Ctx {
	a.loopMu.RLock()
	defer a.loopMu.RUnlock()
	return a.loopCtx
}

// setAutoScrollOff is the single mutation point for autoScrollOff. Centralising
// it keeps the pendingNewMessages counter honest: any transition to false
// (user returned to the bottom, transcript replaced, layout re-anchored) also
// drops the counter, because there are no longer any "new messages since
// scrolling" to advertise. Transitions to true do not touch the counter — it
// starts fresh from zero on the next scrolled-away message.
func (a *App) setAutoScrollOff(off bool) {
	if a == nil {
		return
	}
	if a.autoScrollOff == off {
		return
	}
	a.autoScrollOff = off
	if !off {
		a.resetNewMessageCount()
	}
	// The status line appears or disappears with this flag, and the scroll
	// that flipped it only invalidates the viewport — invalidate both the
	// line's old and new cells so the next paint matches the state.
	a.invalidateChatStatus()
}

// chatScrolledAway reports whether new conversation rows should be counted
// toward the "New messages" count in the chat status line. It mirrors the
// follow-the-tail state: in the fixed layout that is autoScrollOff (set by
// user scroll events); in the document layout no scroll-away tracking exists
// yet, so this stays false and the line never paints there.
func (a *App) chatScrolledAway() bool {
	if a == nil {
		return false
	}
	return a.autoScrollOff
}

// noteNewMessage bumps the new-message counter when the user is scrolled away
// from the bottom. Called by the conversation event adapter at message
// boundaries (assistant message_start, user message_end with a real row). The
// caller is responsible for filtering to those boundaries — this helper is
// just the count + invalidate half.
//
// Must be called on the app loop.
func (a *App) noteNewMessage() {
	if a == nil || !a.chatScrolledAway() {
		return
	}
	a.pendingNewMessages++
	a.invalidateChatStatus()
}

// resetNewMessageCount clears the counter and invalidates wherever the chat
// status line last painted so the count disappears on the next frame. Called
// when the user returns to the bottom, the transcript is replaced, or the
// layout is switched in a way that re-anchors autoscroll.
func (a *App) resetNewMessageCount() {
	if a == nil {
		return
	}
	if a.pendingNewMessages == 0 {
		return
	}
	a.pendingNewMessages = 0
	a.invalidateChatStatus()
}

// invalidateChatStatus marks the area the chat status line occupies — both
// where it last painted and where it would paint now — as dirty so the next
// paint updates or hides the line. Counts only change at message boundaries,
// so the rate is bounded by message arrival — not by streaming deltas.
func (a *App) invalidateChatStatus() {
	if a == nil {
		return
	}
	ctx := a.loopContext()
	if ctx == nil || ctx.Invalidate == nil {
		return
	}
	if r := a.lastChatStatusRect; r.W > 0 && r.H > 0 {
		ctx.Invalidate(r)
	}
	if r := a.chatStatusRect(); r.W > 0 && r.H > 0 {
		ctx.Invalidate(r)
	}
}

// chatStatusText is the exact text painted in the chat status line. The line
// is the single floating indicator above the prompt editor: the new-message
// count while the user is scrolled away, plus how to jump back to the bottom.
// Format kept in one place so tests can assert on it and a future
// localisation only changes the strings here.
func (a *App) chatStatusText() string {
	if a == nil || !a.autoScrollOff {
		return ""
	}
	parts := make([]string, 0, 2)
	if a.pendingNewMessages > 0 {
		parts = append(parts, fmt.Sprintf("New messages (%d)", a.pendingNewMessages))
	}
	hint := "scroll to bottom"
	if keys := a.chatScrollBottomKeys(); len(keys) > 0 {
		hint += fmt.Sprintf(" (%s)", strings.Join(keys, "/"))
	}
	parts = append(parts, hint)
	return strings.Join(parts, " · ")
}

// chatScrollBottomKeys returns the user's configured keys for jumping to the
// bottom of the chat, for display in the status line.
func (a *App) chatScrollBottomKeys() []string {
	manager := a.keybindings
	if manager == nil {
		manager = defaultAppKeybindings()
	}
	return manager.Keys(actionIDChatScrollBottom)
}

// chatStatusRect computes where the chat status line would paint right now,
// given the current layout. Returns a zero rect when the user is not scrolled
// away, the editor has not been laid out, or the editor's row is outside the
// viewport (the document layout's scrolled-away editor). The rect is one row
// tall and centred horizontally across the root width — capped so a very
// narrow terminal does not produce negative widths.
func (a *App) chatStatusRect() tauui.Rect {
	if a == nil || !a.autoScrollOff || a.root == nil || a.editor == nil {
		return tauui.Rect{}
	}
	rootRect := a.root.Rect()
	if rootRect.W <= 0 || rootRect.H <= 0 {
		return tauui.Rect{}
	}
	text := a.chatStatusText()
	w := tauui.TextWidth(text)
	if w > rootRect.W {
		w = rootRect.W
	}
	x := rootRect.X + (rootRect.W-w)/2
	y := a.editor.Rect().Y - 1
	if y < rootRect.Y {
		y = rootRect.Y
	}
	if y >= rootRect.Y+rootRect.H {
		y = rootRect.Y + rootRect.H - 1
	}
	return tauui.Rect{X: x, Y: y, W: w, H: 1}
}

// paintChatStatus draws the chat status line while the user is scrolled away
// from the end of the transcript. It is called from Root.Paint after the
// layout has painted so the line lands on top of whatever row sits above the
// editor. The cells the line overdraws are the chat log's bottom row in the
// common case (no widgets-above); that is the trade-off of a floating line vs
// inserting a real layout region.
//
// Records lastChatStatusRect so invalidateChatStatus can erase exactly the
// cells the line occupied when it disappears.
func (a *App) paintChatStatus(d tauui.Drawer) {
	if a == nil || a.chat == nil {
		return
	}
	rect := a.chatStatusRect()
	if rect.W <= 0 || rect.H <= 0 {
		return
	}
	tokens := a.chat.ThemeTokens()
	// Inverse of the chat background: the line's background is the foreground
	// colour, its foreground the background colour. On the default dark theme
	// that reads as light grey (Text) on near-black (Surface) — matching the
	// "gray background & black foreground" the spec describes for the toast
	// this line grew out of.
	bg := tokens.Text
	fg := tokens.Surface
	d.FillRect(rect, tauui.Style{FG: bg, BG: bg})
	d.DrawText(tauui.Point{X: rect.X, Y: rect.Y}, a.chatStatusText(), tauui.Style{FG: fg, BG: bg})
	a.lastChatStatusRect = rect
}

// Root embeds the layout VStack and implements HandleAction so app-level
// actions (quit, submit, interrupt) dispatched by the toolkit can land here
// after the focused view declines them.
// regionLayout is the shape both layout models satisfy: a view over the eight
// application regions that also exposes them for focus traversal.
type regionLayout interface {
	tauui.View
	Children() []tauui.View
}

// Root hosts whichever region layout is active. Both models are built up front
// and share the same region views, so switching between them is a pointer swap
// plus a relayout — no component is rebuilt and no state is lost.
type Root struct {
	*tauui.AppLayout
	app *App

	// layout is the active model. It is either the embedded AppLayout (the
	// fixed shell) or the DocumentLayout (bottom-anchored document).
	layout regionLayout
	// document is retained so the app can drive its scroll position and query
	// its metrics while it is the active layout.
	document *tauui.DocumentLayout
}

// MouseOpaque makes Root the hit-test target for every mouse event in its rect,
// so it can offer the wheel to the document before delegating.
//
// rovel dispatches a mouse event to the *deepest* view containing the point and
// does not bubble. In the fixed shell that is fine: the chat sits in a
// ScrollView which owns the wheel. The document has no such wrapper, so a wheel
// event landed on whichever region was under the pointer — usually the chat
// log, which uses mouse events for drag-selection and ignores the wheel — and
// died there. The document could not be scrolled at all.
func (r *Root) MouseOpaque() {}

// Handle offers wheel and page keys to the document layout, then falls back to
// the region under the pointer.
func (r *Root) Handle(e tauui.Event, ctx *tauui.Ctx) bool {
	if pe, ok := e.(tauui.PasteEvent); ok && r.app != nil {
		return r.app.handlePaste(pe, ctx)
	}
	if ce, ok := e.(tauui.ClipboardResponseEvent); ok && r.app != nil {
		return r.app.handlePaste(tauui.PasteEvent(ce), ctx)
	}
	if me, ok := e.(tauui.MouseEvent); ok && r.app != nil && r.app.chatStatusClick(me, ctx) {
		return true
	}
	if r.app != nil && r.app.LayoutMode() == LayoutDocument && r.document != nil {
		if r.document.Handle(e, ctx) {
			return true
		}
	}
	// Not a scroll (or nothing moved): deliver to the same view rovel's own
	// hit test would have chosen, so claiming the event changes nothing else.
	if me, ok := e.(tauui.MouseEvent); ok {
		target := deepestViewAt(r.Children(), me.X, me.Y)
		if target != nil && target.Handle(e, ctx) {
			return true
		}
		wheel := me.Action == tauui.MousePress && (me.Button == tauui.MouseButtonWheelUp || me.Button == tauui.MouseButtonWheelDown)
		if wheel && r.app != nil && r.app.LayoutMode() == LayoutFixed && r.Chat != nil && target != r.Chat {
			return r.Chat.Handle(e, ctx)
		}
		return false
	}
	return r.AppLayout.Handle(e, ctx)
}

func (a *App) requestClipboardPaste(ctx *tauui.Ctx) bool {
	if a != nil && a.dialogPicker != nil && a.dialogPicker.Shown() && ctx != nil && ctx.ClipboardRead != nil {
		ctx.ClipboardRead()
		return true
	}
	if a == nil || a.editor == nil || ctx == nil || ctx.FocusedID != a.editor.ID() {
		return false
	}
	if a.pasteNativeClipboardImage(a.ctxOrBackground()) {
		return true
	}
	if ctx.ClipboardRead == nil {
		a.conv.AddError("paste: terminal clipboard read is unavailable; use your terminal paste shortcut")
		a.refreshChat()
		return true
	}
	ctx.ClipboardRead()
	return true
}

func (a *App) handlePaste(e tauui.PasteEvent, ctx *tauui.Ctx) bool {
	if a != nil && a.dialogPicker != nil && a.dialogPicker.Shown() && a.dialogPicker.Handle(e, ctx) {
		return true
	}
	if a != nil && (a.extensionDialog != nil || a.loginDialog != nil) {
		if input := a.hostedDialogInput(); input != nil {
			input.Paste(e.Text)
			if ctx != nil && ctx.InvalidateAll != nil {
				ctx.InvalidateAll()
			}
		}
		return true
	}
	if a == nil || a.editor == nil || ctx == nil || ctx.FocusedID != a.editor.ID() {
		return false
	}
	text := e.Text
	if a.extensions != nil {
		a.extensions.InsertPaste(text)
		return true
	}
	var ok bool
	text, ok = a.rewritePastedImage(text)
	if !ok {
		return true
	}
	a.editor.Paste(ctx, text)
	return true
}

// deepestViewAt mirrors rovel's hit test: the deepest view containing the
// point, searching children last-to-first so a later sibling drawn on top wins,
// and stopping at any view that claims mouse events for its whole rect.
//
// Root claims mouse events wholesale (see MouseOpaque), so it owes the rest of
// the tree the dispatch it displaced. Delivering only to the top-level region
// would work today — every region happens to forward — but it would silently
// break the first region that stops.
func deepestViewAt(views []tauui.View, x, y int) tauui.View {
	for i := len(views) - 1; i >= 0; i-- {
		v := views[i]
		rect := v.Rect()
		if x < rect.X || x >= rect.X+rect.W || y < rect.Y || y >= rect.Y+rect.H {
			continue
		}
		if _, opaque := v.(interface{ MouseOpaque() }); opaque {
			return v
		}
		if parent, ok := v.(interface{ Children() []tauui.View }); ok {
			if target := deepestViewAt(parent.Children(), x, y); target != nil {
				return target
			}
		}
		return v
	}
	return nil
}

// Children reports the active layout's regions so focus traversal follows the
// model actually on screen.
func (r *Root) Children() []tauui.View {
	if r.layout == nil {
		return r.AppLayout.Children()
	}
	return r.layout.Children()
}

// MinSize and Rect follow the active layout for the same reason.
func (r *Root) MinSize() tauui.Size {
	if r.layout == nil {
		return r.AppLayout.MinSize()
	}
	return r.layout.MinSize()
}

func (r *Root) Rect() tauui.Rect {
	if r.layout == nil {
		return r.AppLayout.Rect()
	}
	return r.layout.Rect()
}

// Compile-time check: Root must satisfy the toolkit's actionHandler
// interface so the dispatch path (focused view → ancestors → root)
// actually calls our HandleAction. The parameter type MUST be
// tauui.Action (which is action.Action, an int alias) — not bare int —
// because Go's interface satisfaction is nominal: a method with
// `act int` does NOT satisfy an interface requiring `act tauui.Action`,
// even though they share the same underlying type. A mismatch here
// silently breaks every action the Root handles (quit, send prompt,
// confirm-to-quit, selector navigation, etc.) because the dispatch
// skips views whose HandleAction doesn't match the expected signature.
var _ interface {
	HandleAction(act tauui.Action, ctx *tauui.Ctx) bool
} = (*Root)(nil)

// Layout publishes the terminal height to the prompt editor before the region
// stack measures anything. The editor's height cap is a share of the terminal
// (see PromptEditorMaxHeightPercent) and the VStack allocates fixed regions
// from MinSize, so the cap has to be current before that measurement runs.
func (r *Root) Layout(rect tauui.Rect) {
	if r.app != nil && r.app.editor != nil {
		r.app.editor.SetMaxHeight(rect.H)
	}
	// Reconcile the banner here rather than only on the render path: the header
	// occupies rows in the fixed shell, so its visibility has to be settled
	// before the region stack measures anything.
	if r.app != nil {
		r.app.refreshHeaderVisibility()
	}
	if r.layout != nil {
		r.layout.Layout(rect)
		return
	}
	r.AppLayout.Layout(rect)
}

// Paint captures the paint context for off-loop scroll calls, then delegates
// to the embedded VStack.
func (r *Root) Paint(d tauui.Drawer, ctx *tauui.Ctx) {
	r.app.captureCtx(ctx)
	if r.layout != nil {
		r.layout.Paint(d, ctx)
	} else {
		r.VStack.Paint(d, ctx)
	}
	// The chat status line (new-message count and scroll-to-bottom hint)
	// floats above the editor. Painting it after the layout pass keeps it on
	// top of whatever the chat log or widgets-above drew on that row.
	r.app.paintChatStatus(d)
}

func (r *Root) HandleAction(act tauui.Action, ctx *tauui.Ctx) bool {
	switch act {
	case AppActionModelCycleForward, AppActionModelCycleBackward:
		r.app.cycleScopedModel()
		return true
	case AppActionModelSelect:
		return r.app.openModelSelector()
	case AppActionThinkingCycle:
		r.app.cycleThinkingLevel()
		return true
	case AppActionThinkingToggle:
		r.app.toggleThinkingLevel()
		return true
	case AppActionSuspend:
		return r.app.suspend(ctx)
	case AppActionExtensionShortcut:
		return r.app.runPendingShortcut(ctx)
	case AppActionScopedModelsAll:
		return r.app.handleScopedModelsAction(scopedModelsActionAll, ctx)
	case AppActionScopedModelsClear:
		return r.app.handleScopedModelsAction(scopedModelsActionClear, ctx)
	case AppActionScopedModelsProvider:
		return r.app.handleScopedModelsAction(scopedModelsActionProvider, ctx)
	case AppActionScopedModelsMoveUp:
		return r.app.handleScopedModelsAction(scopedModelsActionMoveUp, ctx)
	case AppActionScopedModelsMoveDown:
		return r.app.handleScopedModelsAction(scopedModelsActionMoveDown, ctx)
	case AppActionScopedModelsSave:
		return r.app.handleScopedModelsAction(scopedModelsActionSave, ctx)
	case AppActionHistorySearchBackward:
		return r.app.openHistorySearch(historySearchBackward, ctx)
	case AppActionHistorySearchForward:
		return r.app.openHistorySearch(historySearchForward, ctx)
	case AppActionExit:
		r.app.confirmOrQuit(ctx)
		return true
	case AppActionPaste:
		return r.app.requestClipboardPaste(ctx)
	case AppActionMessageCopy:
		if d := r.app.loginDialog; d != nil {
			d.copyURL()
			return true
		}
		cmdCopy(r.app, "")
		return true
	case AppActionQuitNow:
		r.app.quit()
		return true
	case tauui.ActionCancel:
		// Esc: dismiss the inline selector first; otherwise interrupt
		// an in-flight run. Esc never quits the app — use Ctrl+C or
		// /quit to exit. When there is nothing to dismiss or interrupt,
		// we swallow the key so it does not fall through to anything
		// else.
		if r.app.dismissSelectorIfVisibleCtx(ctx) {
			return true
		}
		if r.app.userBashCancel != nil {
			r.app.userBashCancel()
			return true
		}
		if r.app.consumeStreamingForInterrupt() {
			return true
		}
		return true
	case AppActionEditorBackslashSubmit:
		if !r.app.editor.HandleAction(tauui.ActionDeleteBack, ctx) {
			return false
		}
		return r.app.trySubmit(ctx)
	case AppActionEditorSubmit:
		if ctx != nil && ctx.FocusedID == r.app.editor.ID() {
			m := r.app.editor.Model()
			if m.Cursor() > 0 && m.Text()[m.Cursor()-1] == '\\' {
				if !r.app.editor.HandleAction(tauui.ActionDeleteBack, ctx) {
					return false
				}
				return r.app.editor.HandleAction(tauui.ActionSubmit, ctx)
			}
		}
		if r.app.trySubmit(ctx) {
			return true
		}
	case AppActionEditorComplete:
		if r.app.applySelectorFromAction(ctx) {
			return true
		}
	case AppActionSelectorActivate:
		if r.app.activateAndSend(ctx) {
			return true
		}
	case AppActionSelectorUp:
		if r.app.routeSelectorUp(ctx) {
			return true
		}
	case AppActionSelectorDown:
		if r.app.routeSelectorDown(ctx) {
			return true
		}
	case AppActionSelectorPageUp:
		if r.app.routeSelectorPageUp(ctx) {
			return true
		}
	case AppActionSelectorPageDown:
		if r.app.routeSelectorPageDown(ctx) {
			return true
		}
	case AppActionMessageFollowUp:
		r.app.handleFollowUpAction(ctx)
		return true
	case AppActionMessageDequeue:
		// Alt+Backspace binding for app.message.dequeue. Pops
		// the first queued follow-up from the UI mirror.
		r.app.dequeueFollowUp(ctx)
		return true
	case AppActionHeaderExpand:
		r.app.ToggleHeaderExpanded()
		return true
	case AppActionChatScrollBottom:
		r.app.jumpToChatBottom(ctx)
		return true
	}
	// Returning false lets the toolkit apply its own focus-navigation fallback
	// for ActionFocusNext/ActionFocusPrev.
	return false
}

// appTheme returns the application look. Do not use DefaultTheme here: in this
// library that is the monochrome ASCII theme. Modern is colored and uses the
// Unicode light-box chrome.
func appTheme() tauui.Theme {
	return tauui.DefaultThemeModern()
}

// appInputOpts returns the terminal input features tau needs. New wires these
// through top-level tui.Opts; the helper remains the single documented source
// for the app's requested input behavior.
func appInputOpts() tauui.InputOpts {
	return tauui.InputOpts{
		Mouse:          true,
		BracketedPaste: true,
		ModifiedKeys:   true,
	}
}

// appInputOpts returns the terminal input features tau needs. Bracketed paste
// must stay enabled so pasted newlines arrive as one PasteEvent and are inserted
// into the editor instead of being decoded as Enter keypresses that submit.
// appKeymap layers app-level bindings over the toolkit defaults. Ctrl+C
// triggers the confirm-or-quit flow (AppActionExit): when the
// editor is focused and has text it clears the text; when the editor
// is focused and empty it starts a confirm timer; pressing Ctrl+C
// again within the timer actually quits. When the editor is not
// focused (e.g. the user tabbed to the chat log or selector) Ctrl+C
// quits immediately. In the prompt editor, plain Enter sends and
// modified Enter (Shift+Enter, or Alt+Enter on terminals that encode
// it the same way) keeps the prompt editor's default newline behavior. Tab
// applies the focused selector item when the selector is visible;
// otherwise it falls through to the toolkit default
// (ActionFocusNext).
// manager holds the app-layer action registry (app.md §12). Physical keys for
// the flat app actions resolve through it, so user configuration can rebind or
// disable them. The contextual branches below stay in Go: which action Enter or
// Tab means depends on whether a selector is visible, and a static key table
// cannot express that.
type appKeymap struct {
	app     *App
	manager *keybindings.Manager
}

// scopedModelsActionIDs are only routable while the /scoped-models dialog is
// visible. Ctrl+P is deliberately shared with app.model.cycleForward: inside
// the dialog it toggles a provider, outside it cycles the model.
var scopedModelsActionIDs = map[string]struct{}{
	actionIDScopedModelsAll:      {},
	actionIDScopedModelsClear:    {},
	actionIDScopedModelsProvider: {},
	actionIDScopedModelsSave:     {},
	actionIDScopedModelsMoveUp:   {},
	actionIDScopedModelsMoveDown: {},
}

func (k appKeymap) Resolve(ctx tauui.KeyContext, ks tauui.Keystroke) (tauui.Action, bool) {
	if k.app != nil && k.app.dialogPicker != nil && k.app.dialogPicker.Shown() {
		if _, settings := k.app.dialogPicker.Component().(*settingsOverlay); settings {
			if action, ok := k.resolveAppAction(ks, func(id string) bool { return id == actionIDPaste }); ok {
				return action, true
			}
			return tauui.ActionNone, false
		}
	}
	// Hosted login and extension inputs own editing keys that the global
	// keymap assigns to activation or quitting.
	if k.app != nil && (k.app.extensionDialog != nil || k.app.loginDialog != nil) {
		if k.app.hostedDialogInput() != nil || k.app.loginDialog != nil {
			if ks.Key == tauui.KeyCtrlC || ks.Mod == tauui.ModCtrl && ks.Key == tauui.KeyRune && (ks.Rune == 'c' || ks.Rune == 'C') {
				return AppActionExit, true
			}
			if ks.Mod == tauui.ModCtrl && ks.Key == tauui.KeyRune && (ks.Rune == 'v' || ks.Rune == 'V') {
				return AppActionPaste, true
			}
			if k.app.loginDialog != nil {
				if act, ok := k.resolveAppAction(ks, func(id string) bool { return id == actionIDMessageCopy }); ok {
					return act, true
				}
			}
			return tauui.ActionNone, false
		}
	}

	if ks.Mod == tauui.ModCtrl && ks.Key == tauui.KeyRune && (ks.Rune == 'd' || ks.Rune == 'D') {
		if k.app == nil || k.app.editor == nil || k.app.editor.Text() == "" {
			return AppActionQuitNow, true
		}
	}
	// Scoped-model bindings are checked first so the dialog's Ctrl+P wins over
	// the global model cycle while it is open.
	if k.app != nil && k.app.scopedModelsDialogVisible() {
		inDialog := func(id string) bool { _, ok := scopedModelsActionIDs[id]; return ok }
		if act, ok := k.resolveAppAction(ks, inDialog); ok {
			return act, true
		}
	}
	// §12: extension shortcuts are checked *before* app keybindings, except for
	// the reserved subset. Done here rather than in Root.Handle because
	// ResolveAction is the earliest hook rovel offers — Root.Handle only sees
	// keys the focused view already declined.
	if act, ok := k.resolveExtensionShortcut(ks); ok {
		return act, true
	}
	if ctx == tauui.KeyCtxTextInput && (k.app == nil || k.app.dialogPicker == nil || !k.app.dialogPicker.Shown()) {
		kb := keybindings.DefaultManager()
		if kb.Matches(ks, "tui.editor.historyPrevious") || kb.Matches(ks, "tui.editor.historyNext") {
			return (keymap.EditorKeymap{}).Resolve(ctx, ks)
		}
	}
	notScopedModels := func(id string) bool { _, ok := scopedModelsActionIDs[id]; return !ok }
	if ctx != tauui.KeyCtxTextInput {
		notScopedModels = func(id string) bool {
			_, scoped := scopedModelsActionIDs[id]
			return !scoped && id != actionIDHistorySearchBackward && id != actionIDHistorySearchForward
		}
	}
	// Alt+Backspace (app.message.dequeue by default) only applies in the
	// editor; elsewhere it stays a plain backspace-word.
	if ks.Mod == tauui.ModAlt && ctx != tauui.KeyCtxTextInput {
		notScopedModels = func(id string) bool {
			_, scoped := scopedModelsActionIDs[id]
			return !scoped && id != actionIDMessageDequeue
		}
	}
	if act, ok := k.resolveAppAction(ks, notScopedModels); ok {
		return act, true
	}
	// Component-backed dialogs (including /scoped-models) own their
	// keys even if focus has not yet moved off the editor. This keeps a
	// fast Space after opening /scoped-models from being inserted into
	// the prompt, which would dismiss the dialog through editor OnChange.
	if ctx == tauui.KeyCtxTextInput && k.app != nil && k.app.dialogPicker != nil && k.app.dialogPicker.Shown() && k.app.dialogPicker.Component() != nil && ks.Mod == 0 {
		switch ks.Key {
		case tauui.KeyTab:
			return AppActionEditorComplete, true
		case tauui.KeyEnter:
			return AppActionSelectorActivate, true
		case tauui.KeyUp:
			return AppActionSelectorUp, true
		case tauui.KeyDown:
			return AppActionSelectorDown, true
		case tauui.KeyPageUp:
			return AppActionSelectorPageUp, true
		case tauui.KeyPageDown:
			return AppActionSelectorPageDown, true
		case tauui.KeyRune:
			if ks.Rune == ' ' && k.app.extensionDialog == nil && k.app.loginDialog == nil {
				return AppActionSelectorActivate, true
			}
		}
	}
	// Tab, Enter, Up, and Down in the editor route to the inline
	// selector when it is visible. The selector uses distinct action
	// ids so the prompt editor (which always handles ActionMoveUp/Down and
	// ActionSubmit) declines and the actions bubble up to Root, which
	// dispatches to the selector.
	//
	// Enter when the selector is visible selects the focused item AND
	// commits it in one step (AppActionSelectorActivate): commands mode
	// applies the command and dispatches it, model mode applies the
	// model and commits it via SetModel. Without this, Enter would
	// route to AppActionEditorSubmit which only looks at the editor text —
	// so for model mode it would just re-trigger the picker instead
	// of picking the focused model.
	if ctx == tauui.KeyCtxTextInput && k.app != nil && k.app.selectorVisible() {
		kb := keybindings.DefaultManager()
		switch {
		case kb.Matches(ks, "tui.select.cancel"):
			return tauui.ActionCancel, true
		case kb.Matches(ks, "tui.select.up"):
			return AppActionSelectorUp, true
		case kb.Matches(ks, "tui.select.down"):
			return AppActionSelectorDown, true
		case kb.Matches(ks, "tui.input.tab"):
			return AppActionEditorComplete, true
		case kb.Matches(ks, "tui.select.confirm"):
			return AppActionSelectorActivate, true
		case kb.Matches(ks, "tui.select.pageUp"):
			return AppActionSelectorPageUp, true
		case kb.Matches(ks, "tui.select.pageDown"):
			return AppActionSelectorPageDown, true
		}
	}

	if ctx == tauui.KeyCtxTextInput && ks.Key == tauui.KeyRune && (ks.Rune == 'v' || ks.Rune == 'V') && ks.Mod&tauui.ModCtrl != 0 && ks.Mod&^(tauui.ModCtrl|tauui.ModShift) == 0 {
		return AppActionPaste, true
	}

	// When the dialog picker is focused (not in text-input mode —
	// the picker isn't a TextInput), the toolkit's default keymap
	// would resolve Tab to ActionFocusNext, which would move focus
	// away from the picker. We intercept Tab here and route it to
	// AppActionEditorComplete so the picker activates the focused
	// item instead. Up/Down/Enter/Esc are already handled by the
	// picker's HandleAction via the toolkit's default action
	// dispatch; only Tab needs this special-case.
	if k.app != nil && k.app.dialogPicker != nil && k.app.dialogPicker.Shown() && ks.Mod == 0 && ks.Key == tauui.KeyTab {
		return AppActionEditorComplete, true
	}
	if ctx == tauui.KeyCtxTextInput {
		kb := keybindings.DefaultManager()
		if kb.Matches(ks, "tui.input.newLine") {
			if k.app != nil && k.app.editor != nil && ks.Key == tauui.KeyEnter && ks.Mod == 0 &&
				kb.Matches(tauui.Keystroke{Key: tauui.KeyEnter, Mod: tauui.ModShift}, "tui.input.submit") {
				m := k.app.editor.Model()
				if m.Cursor() > 0 && m.Text()[m.Cursor()-1] == '\\' {
					return AppActionEditorBackslashSubmit, true
				}
			}
			return tauui.ActionSubmit, true
		}
		if kb.Matches(ks, "tui.input.submit") {
			return AppActionEditorSubmit, true
		}
	}
	if ctx == tauui.KeyCtxTextInput && keybindings.DefaultManager().Matches(ks, "tui.input.tab") {
		return AppActionEditorComplete, true
	}
	if ctx == tauui.KeyCtxTextInput {
		return (keymap.EditorKeymap{}).Resolve(ctx, ks)
	}
	return tauui.StandardKeymap{}.Resolve(ctx, ks)
}

// quit tears down the session and stops the toolkit loop.
func (a *App) quit() {
	a.stopExtensionDialogs()
	if a.loginDialog != nil {
		a.dismissDialog()
	}
	// Stop any pending confirm timer so its callback doesn't fire
	// after we've already torn down state.
	a.stopConfirmQuitTimer()
	a.cancelRun()
	if a.sess != nil {
		_ = a.sess.Close(a.ctx)
	}
	a.app.Quit()
}

// confirmQuitTimeout is how long the "press ctrl+c to exit" notice
// stays visible before auto-clearing. 1.5s is long enough for a
// deliberate second press but short enough that the notice doesn't
// linger if the user changes their mind.
const confirmQuitTimeout = 1500 * time.Millisecond

// confirmOrQuit implements the Ctrl+C handling policy:
//
//  1. If the editor is focused and has text, clear the text. This
//     protects against accidentally quitting while typing.
//  2. If the editor is focused and empty and a confirm is already
//     pending, actually quit.
//  3. If the editor is focused and empty and no confirm is pending,
//     show the "press ctrl+c to exit" notice in the footer and start
//     a 1.5s timer; a second Ctrl+C within the window quits, the
//     timer auto-clears the notice.
//  4. If the editor is not focused (chat log, selector, etc.), quit
//     immediately — there is no text to protect and the user is
//     unlikely to be mid-edit.
//
// Must be called on the app loop.
func (a *App) confirmOrQuit(ctx *tauui.Ctx) {
	// When a modal picker (history search, settings, scoped models) is open,
	// Ctrl+C closes the picker rather than quitting the app. Reaching for the
	// confirm-to-quit flow while a dialog is up means an accidental keystroke
	// — the user already escaped explicitly with Esc to land here, and the
	// picker is modal enough that Ctrl+C most plausibly means "back out".
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dismissDialogCtx(ctx)
		return
	}
	if ctx == nil || ctx.FocusedID != a.editor.ID() {
		a.quit()
		return
	}
	text := a.editor.Text()
	if text != "" {
		// Clear the editor text and reset the cursor. SetText
		// fires OnChange which re-runs refreshSelector, so the
		// picker (if visible) will also dismiss naturally when
		// the new text doesn't match its filter.
		a.editor.ClearInlineImagePreviews()
		a.editor.SetText(ctx, "")
		// Any pending confirm is now stale — the user clearly
		// intended to edit, not to quit.
		a.clearConfirmQuit()
		return
	}
	// Editor is focused and empty.
	a.confirmQuitMu.Lock()
	active := a.confirmQuitActive
	a.confirmQuitMu.Unlock()
	if active {
		a.quit()
		return
	}
	a.startConfirmQuit(ctx)
}

// startConfirmQuit shows the "press ctrl+c to exit" notice in the
// footer and arms the 1.5s auto-clear timer. Must be called on the
// app loop.
func (a *App) startConfirmQuit(ctx *tauui.Ctx) {
	a.confirmQuitMu.Lock()
	a.confirmQuitActive = true
	// Cancel any previous timer before arming a new one (shouldn't
	// happen in practice since startConfirmQuit is only called when
	// active==false, but defensive).
	if a.confirmQuitTimer != nil {
		a.confirmQuitTimer.Stop()
	}
	a.confirmQuitTimer = time.AfterFunc(confirmQuitTimeout, func() {
		// Timer fires on its own goroutine. Post to the loop so
		// all state mutation and invalidation happens on the
		// single-threaded app loop.
		a.post(func(uctx *tauui.UpdateCtx) {
			a.clearConfirmQuitOnLoop(uctx)
		})
	})
	a.confirmQuitMu.Unlock()

	// Set the footer notice and invalidate so the new state paints.
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.notice = "press ctrl+c to exit"
	}
	if ctx != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// clearConfirmQuit cancels any pending confirm (called when the user
// starts editing again, dismissing the notice). Must be called on the
// app loop.
func (a *App) clearConfirmQuit() {
	a.stopConfirmQuitTimer()
	a.confirmQuitMu.Lock()
	active := a.confirmQuitActive
	a.confirmQuitActive = false
	a.confirmQuitMu.Unlock()
	if !active {
		return
	}
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.notice = ""
	}
	if ctx := a.loopContext(); ctx != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// clearConfirmQuitOnLoop is the timer-callback entry point. It runs
// on the app loop (posted from the timer goroutine) and only clears
// state if a confirm is still pending. This guards against the race
// where the user pressed Ctrl+C a second time (which set active=false
// and called quit) just before the timer fired.
func (a *App) clearConfirmQuitOnLoop(uctx *tauui.UpdateCtx) {
	a.confirmQuitMu.Lock()
	active := a.confirmQuitActive
	a.confirmQuitActive = false
	a.confirmQuitMu.Unlock()
	if !active {
		return
	}
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.notice = ""
	}
	if uctx != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			uctx.Invalidate(r)
		}
	}
}

// stopConfirmQuitTimer stops and clears the pending confirm timer
// without touching the active flag. Used by quit() to prevent the
// timer from firing after teardown.
func (a *App) stopConfirmQuitTimer() {
	a.confirmQuitMu.Lock()
	t := a.confirmQuitTimer
	a.confirmQuitTimer = nil
	a.confirmQuitMu.Unlock()
	if t != nil {
		t.Stop()
	}
}

// setStreaming updates the in-flight flag and status-region loader. Must be called
// on the app loop.
func (a *App) setStreaming(v bool) {
	a.mu.Lock()
	changed := a.streaming != v
	a.streaming = v
	a.mu.Unlock()

	if a.footer != nil && a.footer.data != nil {
		a.footer.data.streaming = v
		if v {
			a.footer.data.workingFrame = a.throbber.Frame()
		} else {
			a.footer.data.workingFrame = ""
		}
	}
	if v {
		a.setStatus(loaderStatusKey, a.throbber.Frame()+" "+a.workingMessage())
	} else {
		a.clearStatus("agent")
	}
	if changed {
		if v {
			a.startThrobber()
		} else if !a.compactionRunning {
			a.stopThrobber()
		}
	}

	// Update the terminal title through the top-level TUI app where possible.
	// Backends that do not support runtime titles simply ignore this; the initial
	// lifecycle title is set from tauui.Opts.Title in New.
	if changed && a.app != nil {
		_ = a.app.SetTitle(appTerminalTitle(a.SessionID(), v))
	}

	if !changed {
		return
	}
	if ctx := a.loopContext(); ctx != nil && ctx.Invalidate != nil && a.footer != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// refreshFooterStats pulls session statistics (token usage, context
// window, provider count, auto-compaction flag, thinking level) from the
// session and mirrors them onto the footer data record. It is invoked on the
// app loop whenever something that might affect those numbers changes:
// session startup, usage events, model change, and message
// boundaries.
// The session's Statser interface is optional; sessions that don't
// implement it just leave the existing footer values untouched.
//
// Safe to call from any goroutine that runs on the app loop. When the
// paint context is known the footer rect is invalidated so the new
// values render immediately.
func (a *App) refreshFooterStats(ctx *tauui.Ctx) {
	if a.sess == nil || a.footer == nil {
		return
	}
	s, ok := a.sess.(sdk.Statser)
	if !ok {
		return
	}
	stats := s.Stats()
	d := a.footer.data
	d.model = stats.Model
	// The footer needs the provider name (for the "(provider) " prefix) and
	// whether the model reasons (to decide whether a thinking suffix belongs
	// at all). Both come from the model id, which carries "provider/id".
	d.provider = providerFromModelSpec(stats.Model)
	d.supportsReasoning = stats.ThinkingLevel != ""
	if d.thinkingLevel != stats.ThinkingLevel {
		d.thinkingLevel = stats.ThinkingLevel
		a.refreshEditorChrome()
	}
	d.contextUsed = stats.TotalTokens
	d.contextWindow = stats.ContextWindow
	d.contextUsedPct = stats.ContextUsedPct
	d.totalTokens = stats.TotalTokens
	d.autoCompaction = stats.AutoCompaction
	d.inputTokens = stats.Usage.Input
	d.outputTokens = stats.Usage.Output
	d.cacheReadTokens = stats.Usage.CacheRead
	d.cacheWriteTokens = stats.Usage.CacheWrite
	d.costUSD = stats.Usage.Cost.Total

	if n, ok := a.sess.(sdk.Namer); ok {
		d.sessionName = n.GetSessionName()
	}
	// Also sync the queue count from the session's Queuer interface
	// so the footer shows pending queued messages alongside the
	// pending view.
	if q, ok := a.sess.(sdk.Queuer); ok {
		_, followUp := q.QueueSizes()
		d.queueCount = followUp
	}

	if ctx != nil && ctx.Invalidate != nil && a.footer != nil {
		r := a.footer.Rect()
		if r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// providerFromModelSpec splits "provider/model-id" into its provider. Model ids
// without a provider segment yield "", which suppresses the footer prefix.
func providerFromModelSpec(spec string) string {
	provider, _, found := strings.Cut(spec, "/")
	if !found {
		return ""
	}
	return provider
}

// initFooterStaticFields populates the parts of the footer that don't
// change during a session: the git branch (one-shot subprocess) and
// the available provider count (snapshot of ai.ListAvailableProviders).
// These run once at startup; per-paint work is handled by
// refreshFooterStats.
func (a *App) initFooterStaticFields() {
	if a.footer == nil {
		return
	}
	d := a.footer.data
	d.branch = detectGitBranch(d.cwd)
	d.providerCount = len(ai.ListAvailableProviders())
}

// detectGitBranch returns the current branch name for cwd, or "" if
// cwd is not a git repository or the git lookup fails. The subprocess
// has a short timeout so a hung git daemon can't stall startup; we
// accept a 5s ceiling because most users run git on a local filesystem
// and resolution completes in <50ms.
func detectGitBranch(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	// "HEAD" indicates a detached HEAD (rare in interactive use).
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

func (a *App) isStreaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming
}

func (a *App) startThrobber() {
	a.stopThrobber()
	a.throbber.Reset()
	if a.app == nil {
		return
	}
	a.throbberDone = make(chan struct{})
	a.throbberTimer = time.NewTicker(throbberInterval)
	done := a.throbberDone
	ticker := a.throbberTimer
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				a.post(func(_ *tauui.UpdateCtx) {
					a.advanceThrobberFrame()
				})
			}
		}
	}()
}

func (a *App) stopThrobber() {
	if a.throbberTimer != nil {
		a.throbberTimer.Stop()
		a.throbberTimer = nil
	}
	if a.throbberDone != nil {
		close(a.throbberDone)
		a.throbberDone = nil
	}
	a.throbber.Reset()
}

func (a *App) advanceThrobberFrame() {
	if !a.isStreaming() && !a.compactionRunning {
		return
	}
	frame := a.throbber.Advance()
	if a.isStreaming() {
		if a.footer != nil && a.footer.data != nil {
			a.footer.data.workingFrame = frame
		}
		a.setStatus(loaderStatusKey, frame+" "+a.workingMessage())
	}
	if a.compactionRunning {
		a.setStatus(compactionStatusKey, frame+" "+compactionRunningMessage)
	}
}

// consumeStreamingForInterrupt aborts an in-flight run and returns true. If no
// run is active it returns false so the caller can fall through to quit.
func (a *App) consumeStreamingForInterrupt() bool {
	if !a.isStreaming() {
		return false
	}
	_ = a.sess.Interrupt()
	a.conv.EndAssistantMessage()
	a.setStreaming(false)
	a.conv.AddSystem("interrupted")
	a.syncPendingFromSession(nil)
	a.refreshChat()
	return true
}

// parseUserBash recognizes interactive shell input: !command records the
// output for the model, !!command keeps it out of model context.
func parseUserBash(text string) (command string, excluded, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "!") {
		return "", false, false
	}
	excluded = strings.HasPrefix(text, "!!")
	command = strings.TrimSpace(strings.TrimPrefix(text[1:], "!"))
	return command, excluded, command != ""
}

// runUserBash executes a shell command through the session so hooks,
// operations, and recording match RPC bash. The row streams local output and
// completes from the returned record.
func (a *App) runUserBash(command string, excluded bool) {
	executor, ok := a.sess.(sdk.BashExecutor)
	if !ok {
		a.conv.AddError("bash: session does not support shell commands")
		a.refreshChat()
		return
	}
	args, err := json.Marshal(map[string]any{"command": command, "exclude_from_context": excluded})
	if err != nil {
		a.conv.AddError("bash: " + err.Error())
		a.refreshChat()
		return
	}
	a.userBashSeq++
	id := fmt.Sprintf("user-bash-%d", a.userBashSeq)
	a.conv.AddToolCallWithID(id, "bash", string(args))
	a.refreshChat()
	ctx, cancel := context.WithCancel(a.ctxOrBackground())
	a.userBashCancel = cancel
	go func() {
		execution, err := executor.ExecuteBash(ctx, command, sdk.BashOptions{
			ExcludeFromContext: excluded,
			OnOutput: func(chunk string) {
				a.post(func(*tauui.UpdateCtx) {
					a.conv.AddToolProgress(id, chunk)
					a.refreshChat()
				})
			},
		})
		a.post(func(*tauui.UpdateCtx) {
			cancel()
			a.userBashCancel = nil
			if err != nil {
				a.conv.addToolResult(id, "bash", err.Error(), true, nil, nil, excluded)
			} else {
				addUserBashResult(a.conv, id, execution)
			}
			a.refreshChat()
		})
	}()
}

func addUserBashResult(conv *ConversationModel, id string, execution sdk.BashExecution) {
	output := execution.Output
	if execution.Cancelled {
		output = strings.TrimRight(output, "\n") + "\n(command cancelled)"
	}
	conv.addToolResult(id, "bash", output, execution.Cancelled || execution.ExitCode != 0, execution, execution.DurationMs, execution.ExcludeFromContext)
}

// trySubmit sends the editor's text as a user prompt when the editor is
// focused and non-empty. If the text is a slash command it is dispatched to
// the matching command handler; otherwise it is sent as a regular prompt.
// Returns whether it consumed the submit action.
func (a *App) trySubmit(ctx *tauui.Ctx) bool {
	if ctx == nil || ctx.FocusedID != a.editor.ID() {
		return false
	}
	text := a.editorTextForSubmit()
	if text == "" {
		a.editor.Reset(ctx)
		a.clearPendingAttachments(ctx)
		a.dismissSelector()
		return true
	}

	// Slash command: dispatch to the registered handler.
	if name, args, ok := parseCommandLine(text); ok {
		// `/model` is special-cased. With no args it transitions into
		// the in-layout model picker; with args the args are the model
		// id the user typed (or pre-filled by Tab), and we resolve the
		// id to a provider/id key and switch the session's model.
		if name == "forget" {
			a.editor.SetText(ctx, "/forget ")
			a.refreshSelector("/forget ", ctx)
			return true
		}
		if name == "model" {
			if args == "" {
				a.editor.SetText(ctx, "/model ")
				a.refreshSelector("/model ", ctx)
				return true
			}
			// If the user just picked this exact id from the picker
			// (applySelector set pendingModelKey, and the editor text
			// still matches what it filled), commit the remembered
			// provider/id key directly. This sidesteps the ambiguity
			// case where the same bare id exists under multiple
			// providers (e.g. `claude-sonnet-4-6` under anthropic,
			// amazon-bedrock, google-antigravity, opencode) — the bare
			// id alone is ambiguous, but the picker has already
			// disambiguated it for us.
			//
			// pendingModelKey is cleared on any mismatch: if the user
			// edits the id after Tab, the text no longer matches what
			// the picker filled, so we fall through to applyModelFromArgs
			// which re-resolves from the current text.
			if a.pendingModelKey != "" {
				// Strip the leading "<provider>/" to compare just the
				// bare id against the args the user is submitting.
				bareID := a.pendingModelKey
				if idx := strings.Index(bareID, "/"); idx >= 0 {
					bareID = bareID[idx+1:]
				}
				if bareID == args {
					// The editor text still matches what the picker
					// filled — commit the remembered full key directly.
					// This sidesteps the ambiguity where the same bare
					// id exists under multiple providers.
					key := a.pendingModelKey
					a.pendingModelKey = ""
					a.editor.Model().AddHistory(text)
					a.applyModelFromKey(key)
					a.editor.Reset(ctx)
					a.clearPendingAttachments(ctx)
					a.dismissSelector()
					return true
				}
				// Text diverged from what the picker filled (the user
				// edited the id). Clear the pending key and fall
				// through to resolve from the current text.
				a.pendingModelKey = ""
			}
			if a.applyModelFromArgs(args) {
				a.editor.Model().AddHistory(text)
				a.editor.Reset(ctx)
				a.clearPendingAttachments(ctx)
				a.dismissSelector()
				return true
			}
			// Couldn't resolve the id. Previously we fell through to
			// refreshSelector(text, ctx) which re-showed the picker —
			// but that re-opens the picker on every Tab+Enter cycle
			// even when the id came straight from the picker (the user
			// just pressed Tab on a filtered list and hit Enter).
			// Showing the picker again with the same filter is
			// confusing and never helps. Clear the editor and surface
			// a chat-log error so the user knows the id didn't resolve.
			a.editor.Model().AddHistory(text)
			a.editor.Reset(ctx)
			a.clearPendingAttachments(ctx)
			a.dismissSelector()
			a.conv.AddError("model: unknown id \"" + args + "\"")
			a.refreshChat()
			return true
		}
		if name == "resume" && args == "" {
			a.editor.SetText(ctx, "/resume ")
			a.refreshSelector("/resume ", ctx)
			return true
		}
		a.editor.Model().AddHistory(text)
		a.editor.Reset(ctx)
		a.clearPendingAttachments(ctx)
		a.dismissSelector()
		if cmd, found := a.cmds.Lookup(name); found {
			if cmd.Handler != nil {
				cmd.Handler(a, args)
				return true
			}
			// A locally registered nil handler is an explicit promise that
			// the session owns this command (for example a plugin command
			// registered for completion only). Forward it to the session
			// command pipeline, not to local handlers.
			a.routePrompt(text, ctx)
			return true
		}
		if a.sessionSlashCommandExists(name) {
			// The session advertises this slash command, so it is safe to
			// forward the raw line to the session's command dispatcher.
			a.routePrompt(text, ctx)
			return true
		}
		// Unknown slash commands must not fall through to the model. A
		// typo like /show-model in a session without command metadata used
		// to become a normal prompt and could leave the TUI in a working
		// state if the user interrupted it.
		a.conv.AddError("unknown command: /" + name)
		a.refreshChat()
		return true
	}

	if command, excluded, ok := parseUserBash(text); ok {
		if a.userBashCancel != nil {
			a.conv.AddError("a bash command is already running; press Esc to cancel it first")
			a.refreshChat()
			return true
		}
		a.editor.Model().AddHistory(text)
		a.editor.Reset(ctx)
		a.dismissSelector()
		a.runUserBash(command, excluded)
		return true
	}

	// Clear the editor first, then send. The send runs on a goroutine so the
	// loop stays responsive; results land via App.Post.
	a.editor.Model().AddHistory(text)
	a.editor.Reset(ctx)
	a.clearPendingAttachments(ctx)
	a.dismissSelector()
	a.routePrompt(text, ctx)
	return true
}

func (a *App) editorTextForSubmit() string {
	if a == nil || a.editor == nil || a.editor.Model() == nil {
		return ""
	}
	return strings.TrimSpace(a.editor.Model().ExpandedText())
}

func (a *App) expandInlineFileReferences(text string) (string, []ai.ContentBlock, error) {
	root := a.fileSelectorRoot()
	if root == "" {
		root = "."
	}
	return expandTextFileReferences(text, root)
}

func (a *App) expandInlineAttachmentReferences(text string) (string, []ai.ContentBlock, []string, error) {
	root := a.fileSelectorRoot()
	if root == "" {
		root = "."
	}
	if _, ok := a.sess.(sdk.AttachmentManager); !ok {
		expanded, images, err := expandTextFileReferences(text, root)
		return expanded, images, nil, err
	}
	return a.expandTextAttachmentReferences(text, root)
}

func expandTextFileReferences(text, root string) (string, []ai.ContentBlock, error) {
	var out strings.Builder
	var images []ai.ContentBlock
	for i := 0; i < len(text); {
		if text[i] != '@' || (i > 0 && (text[i-1] == '\\' || !isFileRefBoundary(rune(text[i-1])))) {
			out.WriteByte(text[i])
			i++
			continue
		}
		j := i + 1
		for j < len(text) && !isFileRefBoundary(rune(text[j])) && text[j] != '\n' {
			j++
		}
		path := text[i+1 : j]
		if path == "" {
			out.WriteByte(text[i])
			i++
			continue
		}
		expanded, image, err := expandTextFileReference(path, root)
		if err != nil {
			return "", nil, err
		}
		if image.Type == ai.ContentBlockImage {
			images = append(images, image)
		}
		out.WriteString(expanded)
		i = j
	}
	return out.String(), images, nil
}

func (a *App) expandTextAttachmentReferences(text, root string) (string, []ai.ContentBlock, []string, error) {
	var out strings.Builder
	var images []ai.ContentBlock
	var attachmentIDs []string
	for i := 0; i < len(text); {
		if text[i] != '@' || (i > 0 && (text[i-1] == '\\' || !isFileRefBoundary(rune(text[i-1])))) {
			out.WriteByte(text[i])
			i++
			continue
		}
		j := i + 1
		for j < len(text) && !isFileRefBoundary(rune(text[j])) && text[j] != '\n' {
			j++
		}
		path := text[i+1 : j]
		if path == "" {
			out.WriteByte(text[i])
			i++
			continue
		}
		expanded, image, id, err := a.expandTextAttachmentReference(path, root)
		if err != nil {
			return "", nil, nil, err
		}
		if image.Type == ai.ContentBlockImage {
			images = append(images, image)
		}
		if id != "" {
			attachmentIDs = append(attachmentIDs, id)
		}
		out.WriteString(expanded)
		i = j
	}
	return out.String(), images, attachmentIDs, nil
}

// resolveAttachmentPath maps an @-reference token to a file on disk.
// Markers minted by image pasting resolve straight to the saved file, so a
// same-named working-tree file cannot shadow what the user pasted. Every
// other token keeps the prompt-root-first order, with the session data dir
// as a last resort so a recalled "@pasted-image.png" from editor history
// still resolves after a restart into the same session.
// Must be called on the app loop.
func (a *App) resolveAttachmentPath(path, root string) string {
	if abs, ok := a.pastedImages[path]; ok {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	full := resolveAtReferencePath(path, root)
	if _, err := os.Stat(full); err == nil {
		return full
	}
	if dir := a.sessionState().DataDir(); dir != "" {
		if alt := resolveAtReferencePath(path, dir); alt != full {
			if _, err := os.Stat(alt); err == nil {
				return alt
			}
		}
	}
	return full
}

func (a *App) expandTextAttachmentReference(path, root string) (string, ai.ContentBlock, string, error) {
	full := a.resolveAttachmentPath(path, root)
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "@" + path, ai.ContentBlock{}, "", nil
		}
		return "", ai.ContentBlock{}, "", fmt.Errorf("@%s: %w", path, err)
	}
	if info.IsDir() {
		expanded, err := expandTextDirectoryListing(path, full)
		if err != nil {
			return "", ai.ContentBlock{}, "", err
		}
		return expanded, ai.ContentBlock{}, "", nil
	}
	mgr, ok := a.sess.(sdk.AttachmentManager)
	if !ok {
		expanded, image, err := expandTextFileReference(path, root)
		return expanded, image, "", err
	}
	record, err := mgr.ImportAttachmentFile(full)
	if err != nil {
		return "", ai.ContentBlock{}, "", fmt.Errorf("@%s: %w", path, err)
	}
	if record.Kind == "image" {
		block, err := ai.LoadImageContentBlock(record.Path)
		if err != nil {
			return "", ai.ContentBlock{}, "", fmt.Errorf("@%s: %w", path, err)
		}
		return fmt.Sprintf("<file name=%q>[image content]</file>", path), block, record.ID, nil
	}
	if record.ExtractedText == "" {
		return "", ai.ContentBlock{}, "", fmt.Errorf("@%s: unsupported attachment type %s", path, record.MimeType)
	}
	return fmt.Sprintf("<file name=%q>[attached]</file>", path), ai.ContentBlock{}, record.ID, nil
}

func expandTextFileReference(path, root string) (string, ai.ContentBlock, error) {
	// Honor absolute paths and ".."; the picker lets the user type
	// anything relative to root, but a real user also wants to reach
	// siblings and parent dirs without round-tripping through the
	// shell. The whole history of rejecting ".." or "/foo" here was
	// one of the two things that made @dir balloon into the
	// contents-of-the-whole-tree bug: callers had no way to point
	// the prompt at a file outside root, so the "only inside cwd"
	// guard was useless and confusing.
	full := resolveAtReferencePath(path, root)
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "@" + path, ai.ContentBlock{}, nil
		}
		return "", ai.ContentBlock{}, fmt.Errorf("@%s: %w", path, err)
	}
	if info.IsDir() {
		expanded, err := expandTextDirectoryListing(path, full)
		if err != nil {
			return "", ai.ContentBlock{}, err
		}
		return expanded, ai.ContentBlock{}, nil
	}
	if isImagePath(full) {
		block, err := ai.LoadImageContentBlock(full)
		if err != nil {
			return "", ai.ContentBlock{}, fmt.Errorf("@%s: %w", path, err)
		}
		return fmt.Sprintf("<file name=%q>[image content]</file>", path), block, nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", ai.ContentBlock{}, fmt.Errorf("@%s: %w", path, err)
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", ai.ContentBlock{}, fmt.Errorf("@%s: resolve: %w", path, err)
	}
	return fmt.Sprintf("<file name=%q>\n%s\n</file>", abs, string(data)), ai.ContentBlock{}, nil
}

// resolveAtReferencePath joins the picker path with root. Absolute
// paths bypass root entirely so `@/etc/hosts` and `@~/notes` resolve
// the way the user typed them; relative paths are anchored at root.
func resolveAtReferencePath(path, root string) string {
	if path == "" {
		return root
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, filepath.FromSlash(path[2:]))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.FromSlash(path)
	}
	if root == "" {
		root = "."
	}
	return filepath.Join(root, filepath.FromSlash(path))
}

func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}

// expandTextDirectoryListing walks a directory and emits a file list,
// never the contents. Reading every file in a moderately-sized project
// would max out the model's context window and dump the user's source
// tree into the prompt without consent; the model only needs to know
// which files exist. Paths are sorted for stable output and emitted
// relative to the directory itself, so the model can see the tree
// shape without absolute-path noise.
//
// The wrapper tags the listing with an absolute directory name so the
// model can still ground relative entries back to a real path on
// disk. Skipped: any dotfile or dotfile directory (`.git`,
// `node_modules`, dependency caches) at any depth — the tree
// frequently carries dotfile noise that wasn't the user's intent,
// and only regular files are included (no symlinks to devices,
// FIFOs, sockets).
func expandTextDirectoryListing(path, full string) (string, error) {
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("@%s: resolve: %w", path, err)
	}

	return fmt.Sprintf("<directory name=%q>\n</directory>", abs), nil
}

// routePrompt sends text as a new prompt while idle. While an agent run is
// active, plain Enter sends a steering message so guidance is admitted at the
// next turn boundary, before the agent continues after its current tool calls.
//
// Safe to call from the app loop (trySubmit is the only caller).
func (a *App) routePrompt(text string, ctx *tauui.Ctx) {
	text = strings.TrimSpace(text)
	if text == "" || a.sess == nil {
		return
	}
	displayText := text
	expanded, images, attachmentIDs, err := a.expandInlineAttachmentReferences(text)
	if err != nil {
		a.conv.AddUserMessage(text)
		a.conv.AddError("file: " + err.Error())
		a.refreshChat()
		return
	}
	sendImages := images
	if len(attachmentIDs) > 0 {
		sendImages = nil
	}
	if a.isStreaming() {
		a.enqueueSteering(expanded, sendImages, attachmentIDs, ctx)
		return
	}
	a.submitWithDisplay(expanded, displayText, sendImages, images, attachmentIDs)
}

// enqueueSteering queues guidance for the active task. Steering cannot preempt
// a tool already executing, but it is consumed before the next model turn.
func (a *App) enqueueSteering(text string, images []ai.ContentBlock, attachmentIDs []string, ctx *tauui.Ctx) {
	steerer, ok := a.sess.(sdk.Steerer)
	if !ok {
		a.conv.AddError("steering queue not supported by this session")
		a.refreshChat()
		return
	}
	if err := steerer.Steer(sdk.Message{Role: "user", Text: text, Images: images, AttachmentIDs: attachmentIDs}); err != nil {
		a.conv.AddError("steer: " + err.Error())
		a.refreshChat()
		return
	}
	a.steeringQueue = append(a.steeringQueue, text)
	a.refreshPending(ctx)
}

// handleFollowUpAction implements the explicit follow-up action. During an
// active run it queues completion-only work and clears the committed editor
// text. While idle it delegates to the normal submit path so the message starts
// immediately rather than sitting in a queue that has no active run to drain it.
func (a *App) handleFollowUpAction(ctx *tauui.Ctx) {
	if a.editor == nil || a.editor.Text() == "" {
		return
	}
	if !a.isStreaming() {
		a.trySubmit(ctx)
		return
	}

	text := a.editorTextForSubmit()
	a.editor.SetText(ctx, "")
	a.dismissSelector()
	a.enqueueFollowUp(text, ctx)
}

// enqueueFollowUp adds text to the session's follow-up queue, mirrors
// it in the App's local list, and refreshes the pending view. It is
// called explicitly via AppActionMessageFollowUp for work that should wait
// until the active task fully stops.
//
// The session's FollowUpper.FollowUp may return an error (e.g. the
// queue is full, which the agent enforces as a back-pressure
// signal). We surface the error as a chat-log message rather than
// silent failure so the user knows the keystroke didn't queue.
func (a *App) enqueueFollowUp(text string, ctx *tauui.Ctx) {
	if a.sess == nil {
		return
	}
	fu, ok := a.sess.(sdk.FollowUpper)
	if !ok {
		a.conv.AddError("follow-up queue not supported by this session")
		a.refreshChat()
		return
	}
	if err := fu.FollowUp(sdk.Message{Role: "user", Text: text}); err != nil {
		a.conv.AddError("follow-up: " + err.Error())
		a.refreshChat()
		return
	}
	a.pendingQueue = append(a.pendingQueue, text)
	a.refreshPending(ctx)
}

// dequeueFollowUp removes the first entry from the App's mirror
// queue. The agent's own queue doesn't expose a pop-by-index API,
// so this is a UI-only operation today (the spec calls for
// app.message.dequeue to remove a queued message). When the agent's
// queue grows a Pop() method this will become a real pop against the
// session.
//
// The action is a no-op (silently) when the queue is empty so the
// keystroke doesn't surface an error every time the user happens to
// press Alt+Backspace on an empty queue.
func (a *App) dequeueFollowUp(ctx *tauui.Ctx) {
	if len(a.pendingQueue) == 0 {
		return
	}
	a.pendingQueue = a.pendingQueue[1:]
	a.refreshPending(ctx)
}

// refreshPending pushes the App's mirror queue into the pending view
// and invalidates the affected region so the layout picks up the
// size change. The view's MinSize collapses to 0x0 when the queue
// is empty, so a transition queued→empty makes the VStack reclaim
// the rows; queued→non-empty (or size change) does the opposite.
//
// Safe to call from the app loop.
func (a *App) refreshPending(ctx *tauui.Ctx) {
	if a.pending == nil {
		return
	}
	prevShown := a.pending.MinSize().H > 0
	a.pending.SetQueues(a.steeringQueue, a.pendingQueue)
	nowShown := a.pending.MinSize().H > 0
	if ctx != nil && ctx.Invalidate != nil {
		// Always invalidate the rect we have; the layout may have
		// moved the view when the size changed, so a full-screen
		// invalidate is the safe choice for a transition. For
		// in-place changes the per-rect invalidation is enough.
		if prevShown != nowShown {
			if ctx.InvalidateLayout != nil {
				ctx.InvalidateLayout()
			}
			if ctx.InvalidateAll != nil {
				ctx.InvalidateAll()
			} else if a.root != nil && a.root.Rect().W > 0 {
				ctx.Invalidate(a.root.Rect())
			}
		} else {
			r := a.pending.Rect()
			if r.W > 0 && r.H > 0 {
				ctx.Invalidate(r)
			}
		}
	}
}

// defaultWorkingMessage is the text beside the run spinner (app.md §9). It was
// previously the hardcoded literal " working" with no way to change it.
const defaultWorkingMessage = "Working..."

// workingMessage returns the active run message: the usage-limit countdown
// while waiting out a provider window, an extension's override when one is
// set, otherwise the default.
func (a *App) workingMessage() string {
	if a == nil {
		return defaultWorkingMessage
	}
	a.workingMu.Lock()
	defer a.workingMu.Unlock()
	if !a.pausedSince.IsZero() {
		return fmt.Sprintf("paused by flag %q since %s", a.pausedTarget, a.pausedSince.Format(time.TimeOnly))
	}
	if !a.usageLimitWaitUntil.IsZero() {
		label := "usage limit"
		if a.usageLimitProvider != "" {
			label = a.usageLimitProvider + " usage limit"
		}
		if d := time.Until(a.usageLimitWaitUntil); d > 0 {
			return fmt.Sprintf("%s, retrying at %s (in %s)", label, a.usageLimitWaitUntil.Format(time.TimeOnly), d.Round(time.Second))
		}
		return label + ", retrying now"
	}
	if a.workingOverride != "" {
		return a.workingOverride
	}
	return defaultWorkingMessage
}

// setUsageLimitWait switches the run message to a usage-limit countdown that
// ticks with the run spinner until cleared. Safe to call from the app loop.
func (a *App) setUsageLimitWait(provider string, until time.Time) {
	if a == nil {
		return
	}
	a.workingMu.Lock()
	a.usageLimitProvider = provider
	a.usageLimitWaitUntil = until
	a.workingMu.Unlock()
}

// setPaused switches the run message to the pause notice until cleared. Safe
// to call from the app loop.
func (a *App) setPaused(target string) {
	if a == nil {
		return
	}
	a.workingMu.Lock()
	a.pausedSince = time.Now()
	a.pausedTarget = target
	a.workingMu.Unlock()
}

// clearUsageLimitWait restores the ordinary run message after a usage-limit
// wait or a pause. Safe to call from the app loop.
func (a *App) clearUsageLimitWait() {
	if a == nil {
		return
	}
	a.workingMu.Lock()
	a.pausedSince = time.Time{}
	a.pausedTarget = ""
	a.usageLimitProvider = ""
	a.usageLimitWaitUntil = time.Time{}
	a.workingMu.Unlock()
}

// SetWorkingMessage overrides the run message; an empty string restores the
// default. Backs the extension UI's setWorkingMessage (app.md §8a). Safe to
// call from any goroutine.
func (a *App) SetWorkingMessage(msg string) {
	if a == nil {
		return
	}
	a.workingMu.Lock()
	a.workingOverride = strings.TrimSpace(msg)
	a.workingMu.Unlock()
}

// loaderStatusKey identifies the app's own run indicator inside the status
// region. It is distinct from extension status keys, which live in the footer.
const loaderStatusKey = "agent"

// setStatus adds or updates a keyed line in the status region. Re-setting the
// same key updates in place; an empty text removes the entry. Text is
// sanitized for single-line display so embedded newlines cannot break layout.
//
// The status region is for **active loaders only** — the run spinner,
// compaction, retry (app.md §9). Extension statuses belong to footer row 3 and
// are routed there by setExtensionStatus.
//
// Safe to call from the app loop.
func (a *App) setStatus(key, text string) {
	if a.status == nil || key == "" {
		return
	}
	if a.statusEntries == nil {
		a.statusEntries = map[string]string{}
	}
	if text == "" {
		delete(a.statusEntries, key)
	} else {
		a.statusEntries[key] = sanitizeStatus(text)
	}
	a.refreshStatus(a.loopContext())
}

// setExtensionStatus sets or clears a keyed extension status in footer row 3.
//
// These used to go to the status region, which left the footer's status row
// dead code — nothing in production ever wrote footerData.extensionStatuses —
// and put persistent extension text in the region the spec reserves for
// transient loaders.
func (a *App) setExtensionStatus(key, text string) {
	if a == nil || key == "" || a.footer == nil || a.footer.data == nil {
		return
	}
	if a.footer.data.extensionStatuses == nil {
		a.footer.data.extensionStatuses = map[string]string{}
	}
	prevRows := a.footer.MinSize().H
	if text == "" {
		delete(a.footer.data.extensionStatuses, key)
	} else {
		a.footer.data.extensionStatuses[key] = sanitizeStatus(text)
	}

	ctx := a.loopContext()
	if ctx == nil {
		return
	}
	// The status row appears and disappears, so the footer's height changes;
	// that needs a relayout, not just a repaint of the old rect.
	if a.footer.MinSize().H != prevRows && ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
	if ctx.Invalidate != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// clearStatus removes the entry under key. Silent no-op when the
// key is not set.
func (a *App) clearStatus(key string) {
	if a.status == nil || a.statusEntries == nil {
		return
	}
	if _, ok := a.statusEntries[key]; !ok {
		return
	}
	delete(a.statusEntries, key)
	a.refreshStatus(a.loopContext())
}

// refreshStatus pushes the entry map into the status view and
// invalidates the affected rect. Mirrors the structure of
// refreshPending.
func (a *App) refreshStatus(ctx *tauui.Ctx) {
	if a.status == nil {
		return
	}
	prevShown := a.status.MinSize().H > 0
	a.status.SetEntries(a.statusEntries)
	nowShown := a.status.MinSize().H > 0
	if ctx != nil && ctx.Invalidate != nil {
		if prevShown != nowShown {
			if a.root != nil && a.root.Rect().W > 0 {
				ctx.Invalidate(a.root.Rect())
			}
		} else {
			r := a.status.Rect()
			if r.W > 0 && r.H > 0 {
				ctx.Invalidate(r)
			}
		}
	}
}

// setWidget adds, updates, or removes a widget on one side of the
// editor. Lines is the string-array content; passing nil removes
// the widget. The placement is "above" or "below" and picks which
// container receives the update. Per-widget lines are capped at
// MaxWidgetLines inside the view; the caller may pass any length
// and the view truncates on render.
//
// Safe to call from the app loop.
func (a *App) setWidget(key string, lines []string, placement string) {
	container, entries := a.widgetForPlacement(placement)
	if container == nil {
		return
	}
	if key == "" {
		return
	}
	// Lazy-allocate the per-side map. This is the same pattern
	// setStatus uses: a nil map can't be assigned to, but the
	// caller shouldn't have to pre-allocate just to set a widget.
	switch placement {
	case "above":
		if a.widgetEntriesAbove == nil {
			a.widgetEntriesAbove = map[string][]string{}
		}
		entries = a.widgetEntriesAbove
	case "below":
		if a.widgetEntriesBelow == nil {
			a.widgetEntriesBelow = map[string][]string{}
		}
		entries = a.widgetEntriesBelow
	}
	if lines == nil {
		delete(entries, key)
		a.removeWidgetOrder(placement, key)
	} else {
		cp := make([]string, len(lines))
		copy(cp, lines)
		if _, exists := entries[key]; !exists {
			a.appendWidgetOrder(placement, key)
		}
		entries[key] = cp
	}
	a.refreshWidgets(placement, a.loopContext())
}

// widgetForPlacement maps the public placement string
// ("above"/"below") to the internal container and entry map.
// Returns (nil, nil) for any other value so callers can no-op on
// a bad input without panicking.
func (a *App) widgetForPlacement(placement string) (*widgetContainer, map[string][]string) {
	switch placement {
	case "above":
		return a.widgetsAbove, a.widgetEntriesAbove
	case "below":
		return a.widgetsBelow, a.widgetEntriesBelow
	}
	return nil, nil
}

func (a *App) appendWidgetOrder(placement, key string) {
	switch placement {
	case "above":
		a.widgetOrderAbove = append(a.widgetOrderAbove, key)
	case "below":
		a.widgetOrderBelow = append(a.widgetOrderBelow, key)
	}
}

func (a *App) removeWidgetOrder(placement, key string) {
	remove := func(in []string) []string {
		out := in[:0]
		for _, existing := range in {
			if existing != key {
				out = append(out, existing)
			}
		}
		return out
	}
	switch placement {
	case "above":
		a.widgetOrderAbove = remove(a.widgetOrderAbove)
	case "below":
		a.widgetOrderBelow = remove(a.widgetOrderBelow)
	}
}

func (a *App) widgetEntriesInOrder(placement string, entries map[string][]string) []widgetEntry {
	order := a.widgetOrderAbove
	if placement == "below" {
		order = a.widgetOrderBelow
	}
	out := make([]widgetEntry, 0, len(entries))
	seen := map[string]bool{}
	for _, key := range order {
		lines, ok := entries[key]
		if !ok {
			continue
		}
		out = append(out, widgetEntry{key: key, lines: lines})
		seen[key] = true
	}
	for key, lines := range entries {
		if !seen[key] {
			out = append(out, widgetEntry{key: key, lines: lines})
		}
	}
	return out
}

// refreshWidgets pushes the per-side entry map into the container
// and invalidates the affected rect. Same shape as refreshStatus.
func (a *App) refreshWidgets(placement string, ctx *tauui.Ctx) {
	container, entries := a.widgetForPlacement(placement)
	if container == nil {
		return
	}
	prevShown := container.MinSize().H > 0
	container.SetWidgetEntries(a.widgetEntriesInOrder(placement, entries))
	nowShown := container.MinSize().H > 0
	if ctx != nil && ctx.Invalidate != nil {
		if prevShown != nowShown {
			if a.root != nil && a.root.Rect().W > 0 {
				ctx.Invalidate(a.root.Rect())
			}
		} else {
			r := container.Rect()
			if r.W > 0 && r.H > 0 {
				ctx.Invalidate(r)
			}
		}
	}
}

// submit sends a user prompt and drains the resulting event stream on a
// background goroutine, forwarding each event to the app loop. The shared event
// stream is the source of truth for user, assistant, and tool conversation rows;
// submit only starts run-level UI state immediately.
func (a *App) submit(text string) {
	a.submitWithDisplay(text, text, nil, nil, nil)
}

func (a *App) submitWithDisplay(text, display string, images, displayOnlyImages []ai.ContentBlock, attachmentIDs []string) {
	displayImages := a.displayImagesFor(append(append([]ai.ContentBlock(nil), images...), displayOnlyImages...))
	if text != display || len(displayImages) > 0 || len(attachmentIDs) > 0 {
		a.pendingUserDisplays = append(a.pendingUserDisplays, submittedPromptDisplay{sent: text, display: display, images: displayImages})
	}
	if a.conversationEvents == nil {
		a.conversationEvents = newConversationEventAdapter(a)
	}
	a.conversationEvents.beginRun()
	a.setStreaming(true)
	a.refreshChat()

	go func(prompt string, images []ai.ContentBlock, attachmentIDs []string) {
		events, err := a.sess.Send(a.ctx, sdk.Message{Role: "user", Text: prompt, Images: images, AttachmentIDs: attachmentIDs})
		if err != nil {
			a.post(func(_ *tauui.UpdateCtx) {
				a.conv.EndAssistantMessage()
				a.conv.AddError(err.Error())
				a.setStreaming(false)
				a.syncPendingFromSession(nil)
				a.refreshChat()
			})
			return
		}
		a.forwardSessionEvents(events)
	}(text, images, attachmentIDs)
}

func (a *App) displayForSubmittedPrompt(content string) (string, []ImageComponent) {
	if a == nil || content == "" || len(a.pendingUserDisplays) == 0 {
		return content, nil
	}
	for i, pending := range a.pendingUserDisplays {
		if pending.sent != content {
			continue
		}
		a.pendingUserDisplays = append(a.pendingUserDisplays[:i], a.pendingUserDisplays[i+1:]...)
		if pending.display != "" {
			return pending.display, pending.images
		}
		return content, pending.images
	}
	return content, nil
}

func (a *App) displayImagesFor(images []ai.ContentBlock) []ImageComponent {
	if len(images) == 0 {
		return nil
	}
	out := make([]ImageComponent, 0, len(images))
	for _, image := range images {
		if image.Type != ai.ContentBlockImage {
			continue
		}
		a.nextImageDisplayNumber++
		label := fmt.Sprintf("[Image #%d]", a.nextImageDisplayNumber)
		dims, _ := tauui.DecodeImageDimensions(image.Data, image.MimeType)
		out = append(out, ImageComponent{
			MIMEType:       image.MimeType,
			AltText:        label,
			Width:          dims.WidthPx,
			Height:         dims.HeightPx,
			ContentBlockID: image.ID,
			Data:           image.Data,
		})
	}
	return out
}

const (
	tuiStreamFrameInterval  = time.Second / 60
	tuiStreamEventsPerFrame = 8
	tuiStreamPendingLimit   = 256
	tuiThinkingMinimumDwell = 4 * throbberInterval
)

// forwardSessionEvents prevents provider deltas from flooding App.Post faster
// than the toolkit can paint. App.Run drains up to 64 posts before rendering;
// posting every delta directly can therefore consume an entire response in one
// loop iteration and make it appear only at message_end. This pump retains
// event order but admits a bounded frame of events at 60 Hz, giving ChatLog's
// worker result post a render opportunity between streaming chunks.
func (a *App) forwardSessionEvents(events <-chan sdk.Event) {
	ticker := time.NewTicker(tuiStreamFrameInterval)
	defer ticker.Stop()

	var pending []sdk.Event
	var lastThinkingFrameAt time.Time
	source := events
	for source != nil || len(pending) > 0 {
		receive := source
		if len(pending) >= tuiStreamPendingLimit {
			receive = nil
		}

		select {
		case <-a.ctx.Done():
			return
		case ev, ok := <-receive:
			if !ok {
				source = nil
				continue
			}
			pending = append(pending, ev)
		case <-ticker.C:
			if len(pending) == 0 {
				continue
			}
			count := tuiStreamFrameCount(pending, tuiStreamEventsPerFrame)
			frame := coalesceTUIStreamEvents(pending[:count])
			pending = pending[count:]
			if delay := tuiStreamFrameDelay(frame, lastThinkingFrameAt, time.Now()); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-a.ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return
				case <-timer.C:
				}
			}
			postedAt := time.Now()
			a.post(func(_ *tauui.UpdateCtx) {
				for _, ev := range frame {
					a.handleEvent(ev)
				}
			})
			if tuiStreamFrameHasThinking(frame) {
				lastThinkingFrameAt = postedAt
			} else if tuiStreamFrameClearsThinking(frame) {
				lastThinkingFrameAt = time.Time{}
			}
		}
	}

	// Channel closed: ensure the streaming message is finalized and the
	// indicator cleared even if no explicit message_end arrived. This post is
	// queued after the final bounded event frame.
	a.post(func(_ *tauui.UpdateCtx) {
		a.conv.EndAssistantMessage()
		a.setStreaming(false)
		// The agent may have drained the follow-up queue during the run;
		// resync the mirror from the session's authoritative QueueSizes().
		a.syncPendingFromSession(nil)
		a.refreshChat()
	})
}

// tuiStreamFrameCount limits each paint frame to one run of events with the
// same high-level UI phase. Without this boundary, a burst containing all
// thinking deltas followed immediately by final text can be processed in a
// single app-loop post; the transient Thinking row is then added and removed
// before the toolkit has any chance to paint it.
func tuiStreamFrameCount(events []sdk.Event, limit int) int {
	if len(events) == 0 || limit <= 0 {
		return 0
	}
	if len(events) < limit {
		limit = len(events)
	}
	firstPhase := tuiStreamEventPhase(events[0].Type)
	count := 1
	for count < limit && tuiStreamEventPhase(events[count].Type) == firstPhase {
		count++
	}
	return count
}

func tuiStreamEventPhase(eventType string) string {
	switch eventType {
	case "thinking_delta":
		return "thinking"
	case "text_delta":
		return "text"
	case "tool_call", "tool_result", "error", "message_end", "agent_end":
		return "visible"
	default:
		return "structural"
	}
}

func tuiStreamFrameDelay(frame []sdk.Event, lastThinkingFrameAt, now time.Time) time.Duration {
	if lastThinkingFrameAt.IsZero() || !tuiStreamFrameClearsThinking(frame) {
		return 0
	}
	elapsed := now.Sub(lastThinkingFrameAt)
	if elapsed >= tuiThinkingMinimumDwell {
		return 0
	}
	return tuiThinkingMinimumDwell - elapsed
}

func tuiStreamFrameHasThinking(frame []sdk.Event) bool {
	for _, ev := range frame {
		if ev.Type == "thinking_delta" {
			return true
		}
	}
	return false
}

func tuiStreamFrameClearsThinking(frame []sdk.Event) bool {
	for _, ev := range frame {
		switch ev.Type {
		case "text_delta", "tool_call", "tool_result", "error", "message_end", "agent_end":
			return true
		}
	}
	return false
}

// coalesceTUIStreamEvents merges adjacent text/thinking deltas within one
// display frame. Structural events keep their exact order and remain separate.
func coalesceTUIStreamEvents(events []sdk.Event) []sdk.Event {
	out := make([]sdk.Event, 0, len(events))
	for _, ev := range events {
		if len(out) > 0 {
			if merged, ok := mergeTUIStreamDelta(out[len(out)-1], ev); ok {
				out[len(out)-1] = merged
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

func mergeTUIStreamDelta(previous, next sdk.Event) (sdk.Event, bool) {
	if previous.Type != next.Type || (next.Type != "text_delta" && next.Type != "thinking_delta") || previous.SessionID != next.SessionID {
		return sdk.Event{}, false
	}
	previousPayload, previousOK := previous.Payload.(map[string]any)
	nextPayload, nextOK := next.Payload.(map[string]any)
	previousDelta, previousDeltaOK := previousPayload["delta"].(string)
	nextDelta, nextDeltaOK := nextPayload["delta"].(string)
	if !previousOK || !nextOK || !previousDeltaOK || !nextDeltaOK {
		return sdk.Event{}, false
	}

	payload := make(map[string]any, len(nextPayload))
	for key, value := range nextPayload {
		payload[key] = value
	}
	payload["delta"] = previousDelta + nextDelta
	merged := next
	merged.Payload = payload
	return merged, true
}

// syncPendingFromSession rebuilds the App's mirror queue from the
// session's QueueSizes() count. We don't have a way to enumerate
// queued messages (the agent only exposes counts today), so the
// mirror resets to N empty placeholder rows. The widget renders
// placeholders as "queued message #N" so the user sees the count is
// right but loses the text preview when the agent drains. This is a
// known limitation; a real list-queued-messages API in the SDK
// would fix it.
//
// Pass nil to skip the invalidate (e.g. when followed by another
// call that will invalidate). Safe on the app loop.
func (a *App) syncPendingFromSession(ctx *tauui.Ctx) {
	q, ok := a.sess.(sdk.Queuer)
	if !ok {
		return
	}
	steering, followUp := q.QueueSizes()
	if followUp == len(a.pendingQueue) && steering == len(a.steeringQueue) {
		return
	}
	if followUp == 0 {
		a.pendingQueue = nil
	} else {
		// Re-render the queue as a list of generic placeholders; the
		// real messages are inside the agent and will surface in
		// the next turn's chat log.
		a.pendingQueue = make([]string, followUp)
		for i := range a.pendingQueue {
			a.pendingQueue[i] = "queued message"
		}
	}
	if steering == 0 {
		a.steeringQueue = nil
	} else {
		a.steeringQueue = make([]string, steering)
		for i := range a.steeringQueue {
			a.steeringQueue[i] = "queued message"
		}
	}
	if ctx == nil {
		ctx = a.loopContext()
	}
	a.refreshPending(ctx)
}

// post is a goroutine-safe wrapper around App.Post that tolerates a nil app
// (e.g. during construction).
func (a *App) post(fn func(*tauui.UpdateCtx)) {
	if a.app == nil {
		return
	}
	_ = a.app.Post(fn)
}

// selectorVisible reports whether either picker is currently taking layout
// space. Used by the keymap to decide whether to route selector actions
// (Up/Down/Tab/Enter) or let them fall through to the toolkit default.
func (a *App) selectorVisible() bool {
	if a.selector != nil && a.selector.Shown() {
		return true
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		return true
	}
	return false
}

// completionItems returns the merged list of local and session-provided
// commands matching the given prefix. Local commands sort first so the
// user always sees TUI builtins (/help, /model, /quit) before agent
// commands; session commands follow in their own declaration order.
// Both sources are filtered by the same case-insensitive prefix match.
func (a *App) completionItems(prefix string) []Command {
	local := a.cmds.Filter(prefix)
	// Wrap external commands as local Command values with a nil handler.
	// The TUI never calls a nil handler; trySubmit sees nil and forwards
	// the line to the session instead.
	for _, ext := range a.externalCommands() {
		if prefix == "" || strings.HasPrefix(strings.ToLower(ext.Name), strings.ToLower(prefix)) {
			local = append(local, Command{
				Name:        ext.Name,
				Description: ext.Description,
				Handler:     nil, // forwarded to the session
			})
		}
	}
	return local
}

func (a *App) modelAuthContext() context.Context {
	if a != nil && a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) authenticatedProviderSet(ctx context.Context) map[string]struct{} {
	if a == nil {
		return ai.AuthenticatedProviderSet(ctx, nil)
	}
	return ai.AuthenticatedProviderSet(ctx, a.credentialStore)
}

func (a *App) isProviderAuthenticated(provider string) bool {
	if provider == "" {
		return false
	}
	_, ok := a.authenticatedProviderSet(a.modelAuthContext())[provider]
	return ok
}

func (a *App) availableModels() []*ai.Model {
	if a == nil {
		return ai.AuthenticatedModels(context.Background(), nil, "")
	}
	return ai.AuthenticatedModels(a.modelAuthContext(), a.credentialStore, "")
}

// sessionSelectorItems returns saved sessions for `/resume `, sorted newest
// first. The filter matches ID, name, CWD, or filename case-insensitively.
func (a *App) sessionSelectorItems(filter string) []tauui.SelectorItem {
	if a == nil || a.sessionLister == nil {
		return nil
	}
	if !a.sessionSelectorCacheValid {
		sessions, err := a.sessionLister(a.ctxOrBackground())
		if err != nil {
			if a.conv != nil {
				a.conv.AddError("resume: list sessions: " + err.Error())
				a.refreshChat()
			}
			return nil
		}
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
		a.sessionSelectorCache = sessions
		a.sessionSelectorCacheValid = true
	}
	lower := strings.ToLower(strings.TrimSpace(filter))
	items := make([]tauui.SelectorItem, 0, len(a.sessionSelectorCache))
	for _, sess := range a.sessionSelectorCache {
		if !sessionInfoMatches(sess, lower) {
			continue
		}
		items = append(items, tauui.SelectorItem{
			Name:        sessionInfoDisplayPrompt(sess),
			Description: sessionInfoDisplayMeta(sess),
			Value:       sess.ID,
		})
	}
	return items
}

func (a *App) clearSessionSelectorCache() {
	if a == nil {
		return
	}
	a.sessionSelectorCache = nil
	a.sessionSelectorCacheValid = false
}

func sessionInfoMatches(info sdk.SessionInfo, lowerFilter string) bool {
	if lowerFilter == "" {
		return true
	}
	for _, value := range []string{info.ID, info.Name, info.CWD, info.Filename, info.Created, info.LastModified, info.FirstPrompt, info.Model, info.Status} {
		if strings.Contains(strings.ToLower(value), lowerFilter) {
			return true
		}
	}
	return false
}

func (a *App) modelSelectorItems(prefix string) []tauui.SelectorItem {
	models := a.availableModels()
	filter := strings.TrimSpace(prefix)
	items := make([]tauui.SelectorItem, 0, len(models))
	for _, m := range models {
		if filter != "" && !fuzzyMatch(m.ID, filter) {
			continue
		}
		items = append(items, tauui.SelectorItem{
			Name:        m.ID,
			Description: m.Provider,
		})
	}
	return items
}

// fileSelectorItems returns @-file completions from the top-level
// autocomplete provider. The provider owns file discovery, fuzzy matching,
// quoting, and scoped path semantics; internal/app/tui only adapts its items to
// selector rows and applies the chosen completion later.
func (a *App) fileSelectorItems(text string, cursorCol int) tauui.CompletionSelector {
	provider, err := a.buildAutocompleteProvider()
	if err != nil || provider == nil {
		return tauui.CompletionSelector{}
	}
	before := text[:min(max(cursorCol, 0), len(text))]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	suggestions, err := provider.GetSuggestions(a.ctxOrBackground(), strings.Split(text, "\n"), strings.Count(before, "\n"), len(before)-lineStart, false)
	if err != nil || suggestions == nil || len(suggestions.Items) == 0 || !strings.HasPrefix(suggestions.Prefix, "@") {
		return tauui.CompletionSelector{}
	}
	return tauui.NewCompletionSelector(suggestions)
}

// commandArgItems delegates argument completion on the active logical line.
func (a *App) commandArgItems(text string, cursorCol int) tauui.CompletionSelector {
	before := text[:min(max(cursorCol, 0), len(text))]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	command := strings.TrimLeftFunc(before[lineStart:], func(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' })
	if lineStart != 0 || !strings.HasPrefix(command, "/") || !strings.Contains(command, " ") {
		return tauui.CompletionSelector{}
	}
	provider, err := a.buildAutocompleteProvider()
	if err != nil || provider == nil {
		return tauui.CompletionSelector{}
	}
	suggestions, err := provider.GetSuggestions(a.ctxOrBackground(), strings.Split(text, "\n"), strings.Count(before, "\n"), len(before)-lineStart, false)
	if err != nil {
		return tauui.CompletionSelector{}
	}
	return tauui.NewCompletionSelector(suggestions)
}

func (a *App) fileSelectorRoot() string {
	if a != nil {
		if state := a.sessionState(); state.CWD() != "" {
			return state.CWD()
		}
		if a.footer != nil && a.footer.data != nil && a.footer.data.cwd != "" {
			return a.footer.data.cwd
		}
	}
	return "."
}

func fuzzyMatch(s, pattern string) bool {
	if pattern == "" {
		return true
	}
	s = strings.ToLower(s)
	pattern = strings.ToLower(pattern)
	if strings.Contains(s, pattern) {
		return true
	}
	idx := 0
	for _, r := range s {
		if idx >= len(pattern) {
			return true
		}
		if r == rune(pattern[idx]) {
			idx++
		}
	}
	return idx == len(pattern)
}

// applyModelFromArgs resolves a model id (the args after `/model `) to
// a full provider/id key, then hops back to the loop to call
// sess.SetModel. Returns true when the id uniquely matches a known
// model — ambiguous or unknown ids return false so the caller can fall
// back to the picker for the user to disambiguate. Side effect: posts
// an update-message on the conversation log so the change is visible.
func (a *App) applyModelFromArgs(modelID string) bool {
	models := a.availableModels()
	// Lookup the entered id in the registry. We match by id (not
	// provider/id) since that's what the user typed in the editor.
	lowerID := strings.ToLower(strings.TrimSpace(modelID))
	var matches []string
	for _, m := range models {
		if strings.EqualFold(m.ID, lowerID) {
			matches = append(matches, m.Provider+"/"+m.ID)
			continue
		}
	}
	if len(matches) != 1 {
		// 0 → unknown model id (let the picker filter on it instead
		// of dispatching an error). >1 → ambiguous id (multiple
		// providers have it; require the picker to disambiguate).
		return false
	}
	key := matches[0]
	a.applyModelFromKey(key)
	return true
}

// applyModelFromKey commits a model switch by its full provider/id
// key without needing to resolve a bare id. This is the path used
// when the user picks a model from the inline picker: the picker
// already disambiguated the bare id (which may exist under multiple
// providers), so we commit the full key directly.
//
// Runs synchronously on the app loop (called from trySubmit via the
// keymap dispatch path). The SetModel call is just a state mutation
// on the session — no toolkit dependency — so we don't need to post
// it back to the loop. Doing it synchronously means the UI state
// (currentModel, footer) is updated immediately and is directly
// observable in unit tests without draining a post queue.
func (a *App) applyModelFromKey(key string) {
	a.selectModel(key, "")
}

// selectModel switches to key and then applies thinkingLevel, or the saved
// per-model level when thinkingLevel is empty.
func (a *App) selectModel(key, thinkingLevel string) {
	if a.sess == nil {
		return
	}
	provider, _, ok := strings.Cut(key, "/")
	if !ok || provider == "" {
		a.conv.AddError("model: invalid id \"" + key + "\"")
		a.refreshChat()
		return
	}
	if !a.isProviderAuthenticated(provider) {
		a.conv.AddError("model: provider is not authenticated: " + provider)
		a.refreshChat()
		return
	}
	if err := a.commitModel(key); err != nil {
		a.conv.AddError("model: " + err.Error())
		a.refreshChat()
		return
	}
	a.conv.AddSystem("model: " + key)
	// A saved per-model thinking level follows the model switch; without this
	// the session keeps the previous model's level. Runs after the model system
	// row so the transcript reads model-first, and after refreshFooterStats so
	// its footer/editor-chrome update is the last word on the displayed level.
	if thinkingLevel != "" {
		a.applyThinkingLevel(thinkingLevel)
	} else {
		a.applySavedThinkingLevel(key)
	}
	a.refreshChat()
}

// commitModel switches the session to key and saves it as the default model.
func (a *App) commitModel(key string) error {
	provider, _, _ := strings.Cut(key, "/")
	if err := a.sess.SetModel(key); err != nil {
		return err
	}
	a.currentModel = key
	// Sync the footer's model display. The footer reads from its
	// own data record on each paint, so we must update it here —
	// currentModel is used for picker focus positioning (marking
	// the active model with →), not for the footer. Re-pull Stats()
	// too because the model's ContextWindow changes with the model.
	if a.footer != nil && a.footer.data != nil {
		a.footer.data.model = key
	}
	a.refreshFooterStats(a.loopContext())
	if a.settingsPath != "" {
		if err := ai.UpdateSettings(a.ctxOrBackground(), a.settingsPath, map[string]any{"defaultProvider": provider, "defaultModel": strings.TrimPrefix(key, provider+"/")}); err != nil {
			return fmt.Errorf("selected %s but failed to save default: %w", key, err)
		}
	}
	return nil
}

// sessionSettings returns the merged settings a session applies, or nil when
// the session does not report them.
func sessionSettings(sess sdk.Session) *ai.Settings {
	if provider, ok := sess.(sdk.SettingsProvider); ok {
		return provider.Settings()
	}
	return nil
}

// reapplySessionSettings uses admitted session settings even when old-plugin cleanup failed.
func (a *App) reapplySessionSettings() {
	if a == nil {
		return
	}
	settings := sessionSettings(a.sess)
	if settings == nil {
		return
	}
	a.applyDisplaySettings(settings)
	a.applyDefaultSteeringMode(settings)
	theme := settings.Theme
	if theme == "" {
		theme = systemThemeName
	}
	a.applyTheme(theme)
}

// applyDisplaySettings pushes the presentation settings into the components
// that own them. These are parsed from settings.json but each one has to be
// handed to a specific widget — without this they load into the Settings struct
// and stop there, so setting them in the file appears to do nothing.
func (a *App) applyDisplaySettings(settings *ai.Settings) {
	if a == nil || settings == nil {
		return
	}
	a.hideSkillCommands = !settings.SkillCommandsEnabled()
	if a.app != nil {
		a.app.SetWheelScrollLines(settings.ResolveFullscreenWheelScrollLines())
	}
	if a.chat != nil {
		a.chat.SetCodeBlockIndent(settings.ResolveCodeBlockIndent())
		a.chat.SetThinkingDisplay(settings.HideThinkingBlock, "")
		a.chat.SetImageDisplay(settingsShowImages(settings))
	}
	if a.header != nil {
		a.header.SetQuietStartup(settings.QuietStartup)
	}
	if a.editor != nil {
		a.editor.SetPadding(settings.ResolveEditorPaddingX())
		a.editor.Model().SetAutocompleteMaxVisible(settings.ResolveAutocompleteMaxVisible())
	}
	a.applyQueueModeSettings(settings)

	if mode, ok := ParseLayoutMode(settings.LayoutMode); ok {
		a.SetLayoutMode(mode)
	} else if a.conv != nil {
		a.conv.AddError(fmt.Sprintf("settings: layoutMode %q is not recognised; using %s",
			settings.LayoutMode, DefaultLayoutMode))
	}
}

// applyQueueModeSettings forwards the steering/follow-up drain modes to the
// session. ai.Settings and agent.Config both carry fields with these names, but
// nothing copied one into the other, so configuring them had no effect. Invalid
// values are reported rather than silently ignored: a typo in settings.json
// should not quietly leave the queue on a mode the user did not ask for.
func (a *App) applyQueueModeSettings(settings *ai.Settings) {
	if a == nil || settings == nil || a.sess == nil {
		return
	}
	setter, ok := a.sess.(sdk.QueueModeSetter)
	if !ok {
		return
	}
	if mode := strings.TrimSpace(settings.SteeringMode); mode != "" {
		if err := setter.SetSteeringMode(mode); err != nil {
			a.conv.AddError(fmt.Sprintf("settings: steeringMode %q: %v", mode, err))
		}
	}
	if mode := strings.TrimSpace(settings.FollowUpMode); mode != "" {
		if err := setter.SetFollowUpMode(mode); err != nil {
			a.conv.AddError(fmt.Sprintf("settings: followUpMode %q: %v", mode, err))
		}
	}
}

func (a *App) applyDefaultSteeringMode(settings *ai.Settings) {
	if a == nil || a.sess == nil || (settings != nil && strings.TrimSpace(settings.SteeringMode) != "") {
		return
	}
	setter, ok := a.sess.(sdk.QueueModeSetter)
	if !ok {
		return
	}
	if err := setter.SetSteeringMode("all"); err != nil {
		a.conv.AddError(fmt.Sprintf("settings: default steeringMode %q: %v", "all", err))
	}
}

// settingsShowImages resolves the showImages setting, whose documented default
// is enabled. Nil means "not configured", which is why the field is a *bool:
// with a plain omitempty bool, absent and false encode identically on disk.
func settingsShowImages(settings *ai.Settings) bool {
	if settings == nil || settings.Terminal.ShowImages == nil {
		return true
	}
	return *settings.Terminal.ShowImages
}

// refreshEditorChrome re-resolves the prompt editor's rule colour from the
// active theme tokens and the session's thinking level. It runs whenever either
// input changes — theme switch, thinking-level change — so the rule is the
// standing indicator that reasoning is on and at what effort.
func (a *App) refreshEditorChrome() {
	if a == nil || a.editor == nil {
		return
	}
	level := ""
	if a.footer != nil && a.footer.data != nil {
		level = a.footer.data.thinkingLevel
	}
	tokens := a.chat.ThemeTokens()
	a.editor.SetBorderStyle(tokens.editorBorderStyle(level))
}

// refreshSelector is the central decision point: given the current editor
// text, choose a selector mode (commands or models), build the items,
// and update the inline selector. Hides the selector when neither mode
// applies (e.g. empty text, slash command followed by space and args).
//
// Runs on the app loop (called from the editor's OnChange handler and
// from trySubmit when `/model` is applied).
//
// On visibility transitions (hidden↔shown) the call also marks the
// toolkit's layout dirty and invalidates the affected rects. This is
// required because the selector's MinSize changes from 0×0 to 80×N
// (or back) on these transitions; without an explicit
// InvalidateLayout the toolkit keeps the cached VStack allocation
// where the selector has height 0, and Paint no-ops because the
// selector's rect is empty — so the selector never appears even
// though Shown() is true.
func (a *App) refreshSelector(text string, ctx *tauui.Ctx) {
	if a.forcedCompletion && a.selectorVisible() {
		a.requestCompletion(ctx, true, false)
		return
	}
	a.forcedCompletion = false
	a.completionSeq++
	seq := a.completionSeq
	if a.selector == nil {
		return
	}

	// If the editor text diverges from the pending model key the
	// picker filled, clear the pending key so the next submit
	// re-resolves from the current text. The picker fills the editor
	// with `/model <bare-id>`; if the user then edits the id (types,
	// deletes, pastes), the text no longer matches what was picked,
	// and trySubmit should fall through to applyModelFromArgs instead
	// of committing the stale remembered key.
	if a.pendingModelKey != "" {
		if idx := strings.Index(a.pendingModelKey, "/"); idx >= 0 {
			bareID := a.pendingModelKey[idx+1:]
			expected := "/model " + bareID
			if text != expected {
				a.pendingModelKey = ""
			}
		} else {
			// Malformed key (no provider prefix) — clear it to be safe.
			a.pendingModelKey = ""
		}
	}

	// File picker mode. Triggered by typing @ followed by the current
	// path/search token. Discovery and completion application are delegated to
	// the top-level autocomplete provider so selector cleanup does not carry a
	// second app-local file completion implementation.
	//
	// Keep this path behind a cheap lexical guard. Building the provider snapshots
	// session slash commands and may walk the filesystem for @ suggestions; doing
	// that on every ordinary character made fast typing burn CPU and feel sticky.
	draftText, draftCursor := text, len(text)
	if a.editor != nil {
		draftText, draftCursor = a.editor.Text(), a.editor.Model().Cursor()
	}
	cursorCol := len(text)
	if a.editor != nil && a.editor.Text() == text {
		cursorCol = a.editor.Model().Cursor()
	}
	var completion tauui.CompletionSelector
	if shouldCheckFileSelector(text, cursorCol) {
		completion = a.fileSelectorItems(text, cursorCol)
	}
	if len(completion.Items) == 0 {
		completion = a.commandArgItems(text, cursorCol)
	}
	if seq != a.completionSeq {
		return
	}
	if a.editor != nil && (a.editor.Text() != draftText || a.editor.Model().Cursor() != draftCursor) {
		wasShown := a.selector.Shown()
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return
	}
	if len(completion.Items) > 0 {
		if a.dialogPicker != nil && a.dialogPicker.Shown() {
			a.dismissDialogCtx(ctx)
		}
		wasShown := a.selector.Shown()
		wasSize := a.selector.MinSize()
		a.selector.SetItems(completion.SelectorItems())
		a.selector.SetFocused((&autocomplete.Suggestions{Prefix: completion.Prefix, Items: completion.Items}).FocusedIndex())
		a.selector.SetShown(true)
		a.selectorMode = selectorModeFiles
		a.completionSelector = completion
		forceLayout := wasShown && a.selector.MinSize() != wasSize
		a.invalidateSelectorLayout(wasShown, forceLayout, ctx)
		return
	}

	// Fork picker mode. Triggered by typing `/fork ` (with trailing
	// whitespace). It uses the same inline selector as /model and /resume;
	// Tab switches between prompts and all messages.
	if strings.HasPrefix(text, "/fork") && len(text) > 5 && (text[5] == ' ' || text[5] == '\t') {
		a.openForkSelector(ctx)
		return
	}

	// Forget picker mode. Same picker as /fork, but a selection is required:
	// `/forget` alone also lands here via trySubmit.
	if strings.HasPrefix(text, "/forget") && len(text) > 7 && (text[7] == ' ' || text[7] == '\t') {
		a.openForgetSelector(ctx)
		return
	}

	// Session picker mode. Triggered by typing `/resume ` (with trailing
	// whitespace). The focused row fills/commits the session ID through the
	// same inline selector used for commands and models.
	if strings.HasPrefix(text, "/resume") && len(text) > 7 && (text[7] == ' ' || text[7] == '\t') {
		filter := strings.TrimSpace(text[7:])
		if a.dialogPicker != nil && a.dialogPicker.Shown() {
			a.dismissDialogCtx(ctx)
		}
		wasShown := a.selector.Shown()
		wasSize := a.selector.MinSize()
		items := a.sessionSelectorItems(filter)
		if len(items) == 0 {
			a.dismissSelector()
			a.invalidateSelectorLayout(wasShown, false, ctx)
			return
		}
		a.selector.SetItems(items)
		a.selector.SetShown(true)
		a.selectorMode = selectorModeSessions
		forceLayout := wasShown && a.selector.MinSize() != wasSize
		a.invalidateSelectorLayout(wasShown, forceLayout, ctx)
		return
	}

	// Model picker mode. Triggered by typing `/model ` (with trailing
	// whitespace separating the command name from any filter the user
	// is typing). The inline selector remains driven by the editor text:
	// printable input keeps going to the prompt, while the app keymap
	// routes Up/Down/Tab/Enter to the selector when it is visible.
	//
	// Bare `/model` (no trailing) is still slash command filter mode
	// showing the one matching command; the user has to type a space
	// (or press Tab via Tab→AppActionEditorComplete) to commit and
	// switch to the picker.
	if strings.HasPrefix(text, "/model") && len(text) > 6 && (text[6] == ' ' || text[6] == '\t') {
		filter := strings.TrimSpace(text[6:])
		// The dialog picker must not own `/model` input. If a stale dialog
		// is still visible from an older code path, close it before showing
		// the inline selector so prompt focus remains the source of truth.
		if a.dialogPicker != nil && a.dialogPicker.Shown() {
			a.dismissDialogCtx(ctx)
		}
		wasShown := a.selector.Shown()
		wasSize := a.selector.MinSize()
		items := a.modelSelectorItems(filter)
		if len(items) == 0 {
			a.dismissSelector()
			a.invalidateSelectorLayout(wasShown, false, ctx)
			return
		}
		// Position focus on the active model so the → marker lines up
		// with the model that's currently in use. If we don't have an
		// active model cached we leave the focused index at 0.
		focus := 0
		for i, it := range items {
			fullKey := it.Description + "/" + it.Name
			if fullKey == a.currentModel {
				focus = i
				break
			}
		}
		a.selector.SetItems(items)
		a.selector.SetFocused(focus)
		a.selector.SetShown(true)
		a.selectorMode = selectorModeModels
		forceLayout := wasShown && a.selector.MinSize() != wasSize
		a.invalidateSelectorLayout(wasShown, forceLayout, ctx)
		return
	}

	// Otherwise, if a stale dialog picker is open, close it before
	// returning to slash-command handling. `/model` no longer opens this
	// picker, but this keeps older/future dialog states from lingering
	// after the prompt text changes.
	a.clearSessionSelectorCache()
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dismissDialog()
	}

	// Capture visibility and size BEFORE we mutate so shown→shown item
	// refreshes that alter MinSize can still invalidate layout.
	wasShown := a.selector.Shown()
	wasSize := a.selector.MinSize()

	// Slash-name completion follows the provider on the active logical line.
	before := text[:min(max(cursorCol, 0), len(text))]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	line := before[lineStart:]
	commandLine := strings.TrimLeftFunc(line, func(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' })
	if lineStart != 0 || !strings.HasPrefix(commandLine, "/") || strings.Contains(commandLine, " ") {
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return
	}
	provider, err := a.buildAutocompleteProvider()
	var suggestions *autocomplete.Suggestions
	if err == nil && provider != nil {
		suggestions, err = provider.GetSuggestions(a.ctxOrBackground(), []string{line}, 0, len(line), false)
	}
	if err != nil || suggestions == nil || !strings.HasPrefix(suggestions.Prefix, "/") {
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return
	}
	completion = tauui.NewCompletionSelector(suggestions)
	a.selector.SetItems(completion.SelectorItems())
	a.selector.SetFocused((&autocomplete.Suggestions{Prefix: completion.Prefix, Items: completion.Items}).FocusedIndex())
	a.selector.SetShown(true)
	a.selectorMode = selectorModeCommands
	a.completionSelector = completion
	forceLayout := wasShown && a.selector.MinSize() != wasSize
	a.invalidateSelectorLayout(wasShown, forceLayout, ctx)
}

func isFileRefBoundary(r rune) bool {
	return r == ' ' || r == '\t' || r == '(' || r == '[' || r == '{' || r == ',' || r == ':'
}

func shouldCheckFileSelector(text string, cursor int) bool {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(text) {
		cursor = len(text)
	}
	before := text[:cursor]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	line := before[lineStart:]
	return autocomplete.HasAttachmentPrefix(line) && autocomplete.HasTriggerPrefix(line, []rune{'@'})
}

// invalidateSelectorLayout emits the toolkit signals needed to keep
// the rendered layout in sync with the selector's MinSize. forceLayout
// covers shown→shown updates that still alter MinSize. Two cases:
//
//	Transition hidden→shown: invalidate the full screen so the
//	  VStack reallocates (and the selector gets the new rows it
//	  now needs). The editor's previously-allocated rect shrinks
//	  by the same amount, so we mark the editor rect dirty too.
//
//	Transition shown→hidden: invalidate the editor + footer + scroll
//	  rects that previously gave up space to the selector.
//
//	No transition (just an item update): invalidate the selector's
//	  current rect so its contents repaint, plus the editor rect. When
//	  MinSize changes while shown, forceLayout upgrades this to a
//	  layout/full-screen invalidation.
//
//	ctx may be nil (called from tests, or from trySubmit which posts
//
// to the loop); fall back to a no-op in that case — the next paint
// will eventually refresh regardless.
func (a *App) invalidateSelectorLayout(wasShown bool, forceLayout bool, ctx *tauui.Ctx) {
	if ctx == nil {
		return
	}
	isShown := a.selector.Shown()
	if wasShown == isShown && !forceLayout {
		// Same visibility, just an item/header update. Invalidate the
		// selector's current rect (it may have content from a prior
		// shorter item list that needs repainting) and the editor
		// rect so the layout pass picks up our change.
		if r := a.selector.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
		if r := a.editor.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
		return
	}
	// Visibility transition. Mark the layout dirty so the VStack
	// reallocates and the selector's new rect lands correctly, and
	// invalidate the whole screen so any cells that were talking
	// about the old layout get repainted.
	if ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
	ctx.InvalidateAll()
}

// onEditorTextChanged is the prompt editor OnChange callback. It routes the
// new text through refreshSelector which decides whether to show,
// filter, switch, or hide. Runs on the app loop (the prompt editor fires it
// synchronously from its mutators, which run inside the toolkit's event
// dispatch path).
func (a *App) onEditorTextChanged(text string, ctx *tauui.Ctx) {
	// If the user starts typing after a confirm-to-quit notice, the
	// notice is no longer relevant — clear it so the footer returns
	// to its usual cwd/model/status display.
	if text != "" {
		a.clearConfirmQuit()
	}
	a.updatePendingAttachments(text, ctx)
	a.refreshSelector(text, ctx)
}

// applySelector handles selection from the inline selector. In command
// mode it rewrites the editor text to `/<name> ` and refreshes (which
// will switch to model mode if the user picked `/model`). In model
// mode it FILLS the editor with `/model <id>` and dismisses the
// picker; the model isn't actually switched yet — the user has to
// press Enter (or pick from the picker again) to commit. This avoids
// the accidental "Tab picked a model, the model switched" UX.
func (a *App) applySelector(_ int, item tauui.SelectorItem) {
	ctx := a.loopContext()
	if ctx == nil {
		return
	}
	switch a.selectorMode {
	case selectorModeCommands:
		// Replace the editor's text with /<name> and let refreshSelector
		// recompute the selector (which will keep showing commands, or
		// switch to models if `name == "model"`).
		//
		// Picking a command invalidates any pending model key — the
		// user is now in command-argument territory, not model-pick
		// territory. The pending key is only meaningful when the
		// editor contains exactly the bare id the picker filled.
		a.pendingModelKey = ""
		if selected := a.completionSelector.Suggestions(-1, item.Name); selected != nil {
			a.editor.ApplyAutocomplete(ctx, selected)
		} else {
			a.editor.SetText(ctx, "/"+item.Name+" ")
		}
		a.refreshSelector(a.editor.Text(), ctx)
	case selectorModeModels:
		// Fill the editor with `/model <id>` (just the model id, in
		// Pi style — the provider isn't part of what the user types).
		// Dismiss the picker. The user can commit by pressing Enter
		// (trySubmit), edit the id, or back-out with Escape; if they
		// move the cursor back into the model name (Left arrow, etc.)
		// OnCursorMove re-arms the picker via refreshSelector.
		//
		// Also remember the full provider/id key so trySubmit can
		// commit unambiguously. Many model ids are shared across
		// providers (e.g. `claude-sonnet-4-6` exists under
		// anthropic, amazon-bedrock, google-antigravity, opencode);
		// the bare id in the editor would match all of them and
		// trySubmit would reject it as ambiguous. The pending key
		// is cleared whenever the editor text diverges from what
		// the picker filled (see trySubmit / refreshSelector).
		newText := "/model " + item.Name
		a.pendingModelKey = item.Description + "/" + item.Name
		a.editor.SetText(ctx, newText)
		a.dismissSelector()
		a.invalidateSelectorLayout(true, false, ctx)
	case selectorModeSessions:
		id := item.Value
		if id == "" {
			id = item.Name
		}
		newText := "/resume " + id
		a.editor.SetText(ctx, newText)
		a.clearSessionSelectorCache()
		a.dismissSelector()
		a.invalidateSelectorLayout(true, false, ctx)
	case selectorModeFiles:
		if selected := a.completionSelector.Suggestions(a.selector.Focused(), item.Name); selected != nil {
			a.editor.ApplyAutocomplete(ctx, selected)
		} else {
			a.editor.SetText(ctx, tauui.ReplaceActiveAtReference(a.editor.Text(), item.Name))
		}
		a.dismissSelector()
		a.invalidateSelectorLayout(true, false, ctx)
	case selectorModeFork:
		a.dismissSelector()
		forkTo(a, item.Value)
	case selectorModeForget:
		a.dismissSelector()
		forgetFrom(a, item.Value)
	}
}

// applySelectorFromAction is the entry point for the
// AppActionEditorComplete handler (Tab key). With no picker it requests
// completion on the active logical line.
//
// The dialog picker claims Tab when it's visible — Tab activates
// the focused item the same way Enter does.
func (a *App) applySelectorFromAction(ctx *tauui.Ctx) bool {
	if (a.selectorMode == selectorModeFork || a.selectorMode == selectorModeForget) && a.selector != nil && a.selector.Shown() {
		a.toggleForkSelector(ctx)
		return true
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dialogPicker.HandleAction(int(tauui.ActionActivate), ctx)
		return true
	}
	if a.selector == nil || !a.selector.Shown() {
		return a.requestTabCompletion(ctx)
	}
	// The tauui.Selector has no concept of focused index selection here;
	// we delegate to its HandleAction entry point by calling
	// handleSelectorAction with ActionSelect so it routes to onSelect.
	return a.handleSelectorAction(int(tauui.ActionSelect))
}

func (a *App) requestTabCompletion(ctx *tauui.Ctx) bool {
	if a.editor == nil || a.selector == nil {
		return false
	}
	text := a.editor.Text()
	cursor := a.editor.Model().Cursor()
	before := text[:cursor]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	command := strings.TrimLeftFunc(before[lineStart:], func(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff' })
	force := lineStart != 0 || !strings.HasPrefix(command, "/") || strings.Contains(command, " ")
	return a.requestCompletion(ctx, force, true)
}

func (a *App) requestCompletion(ctx *tauui.Ctx, force, explicitTab bool) bool {
	a.completionSeq++
	seq := a.completionSeq
	text := a.editor.Text()
	cursor := a.editor.Model().Cursor()
	before := text[:cursor]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	lines := strings.Split(text, "\n")
	line := strings.Count(before, "\n")
	col := len(before) - lineStart
	if force && !autocomplete.ShouldTriggerFileCompletion(lines, line, col) {
		return true
	}
	provider, err := a.buildAutocompleteProvider()
	if err != nil || provider == nil {
		return true
	}
	suggestions, err := provider.GetSuggestions(a.ctxOrBackground(), lines, line, col, force)
	if seq != a.completionSeq {
		return true
	}
	if a.editor.Text() != text || a.editor.Model().Cursor() != cursor {
		wasShown := a.selector.Shown()
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return true
	}
	if err != nil || suggestions == nil || len(suggestions.Items) == 0 {
		wasShown := a.selector.Shown()
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return true
	}
	if force && explicitTab && len(suggestions.Items) == 1 {
		a.editor.ApplyAutocomplete(ctx, suggestions)
		return true
	}
	wasShown := a.selector.Shown()
	a.completionSelector = tauui.NewCompletionSelector(suggestions)
	a.forcedCompletion = force
	a.selector.SetItems(a.completionSelector.SelectorItems())
	a.selector.SetFocused(suggestions.FocusedIndex())
	a.selector.SetShown(true)
	a.selectorMode = selectorModeFiles
	if !force && strings.HasPrefix(suggestions.Prefix, "/") {
		a.selectorMode = selectorModeCommands
	}
	a.invalidateSelectorLayout(wasShown, false, ctx)
	return true
}

// activateAndSend is the AppActionSelectorActivate handler. It applies
// the visible picker's focused item and commits the result in one
// step.
//
//   - inline model mode: fills the editor with `/model <id>`, dismisses
//     the selector, and commits the disambiguated provider/id key.
//
//   - inline command mode: applies the command and dispatches it via
//     trySubmit (e.g. /help → help command, /quit → quit). For /model,
//     applying the command writes `/model ` and refreshSelector switches
//     the same inline selector into model mode.
//
//   - inline file mode: fills the active @file token and stops there; unlike
//     /model selection, Enter must not also send the prompt.
//
// This is what Enter does when a picker is visible. When no picker
// is visible, Enter falls through to AppActionEditorSubmit (handled
// by the keymap resolver), which calls trySubmit directly with the
// current editor text — so the "Tab then Enter" path still works.
//
// Returns false when no picker is visible so the caller can fall
// through to the default send behavior.
func (a *App) activateAndSend(ctx *tauui.Ctx) bool {
	// Dialog picker takes priority when it's visible. Production dialog pickers
	// host components (settings, scoped models) that own activation internally;
	// inline model/session/file picking is handled by the selector below.
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		if a.dialogPicker.Component() != nil {
			a.dialogPicker.HandleAction(int(tauui.ActionActivate), ctx)
		}
		return true
	}
	if a.selector == nil || !a.selector.Shown() {
		return false
	}
	// Capture the mode before applySelector — applySelector can
	// transition command mode → model mode (when /model is picked), and
	// we want to know which mode the user was in when they hit Enter so
	// the commit step does the right thing.
	mode := a.selectorMode
	// Select directly. Tab has mode-specific behavior for the fork picker and
	// must not leak into Enter activation.
	a.handleSelectorAction(int(tauui.ActionSelect))
	// Now commit. trySubmit looks at the editor text and dispatches:
	//   - /model <id>  → applyModelFromArgs (commits the model)
	//   - /model       → switches the inline selector into model mode
	//   - /<name>      → dispatches to the command handler
	//   - plain text   → submits as a prompt to the model
	//
	// trySubmit requires ctx.FocusedID == editor.ID(); pass ctx
	// through so a nil ctx falls through cleanly.
	if mode == selectorModeNone {
		return true
	}
	if mode == selectorModeFiles || mode == selectorModeFork || mode == selectorModeForget {
		return true
	}
	if ctx == nil || ctx.FocusedID != a.editor.ID() {
		return true
	}
	a.trySubmit(ctx)
	return true
}

// routeSelectorUp routes AppActionSelectorUp (from the app keymap) to
// the inline selector OR the dialog picker, whichever is visible. The
// Editor doesn't recognize this action id, so it bubbles up to Root
// which calls here. Returns false so the toolkit falls through to
// its default focus navigation when no picker is visible.
func (a *App) routeSelectorUp(ctx *tauui.Ctx) bool {
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dialogPicker.HandleAction(int(tauui.ActionMoveUp), ctx)
		return true
	}
	return a.handleSelectorAction(int(tauui.ActionMoveUp))
}

// routeSelectorDown mirrors routeSelectorUp for the Down arrow.
func (a *App) routeSelectorDown(ctx *tauui.Ctx) bool {
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dialogPicker.HandleAction(int(tauui.ActionMoveDown), ctx)
		return true
	}
	return a.handleSelectorAction(int(tauui.ActionMoveDown))
}

func (a *App) routeSelectorPageUp(ctx *tauui.Ctx) bool {
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dialogPicker.HandleAction(int(tauui.ActionPageUp), ctx)
		return true
	}
	return a.handleSelectorAction(int(tauui.ActionPageUp))
}

func (a *App) routeSelectorPageDown(ctx *tauui.Ctx) bool {
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		a.dialogPicker.HandleAction(int(tauui.ActionPageDown), ctx)
		return true
	}
	return a.handleSelectorAction(int(tauui.ActionPageDown))
}

// handleSelectorAction dispatches a semantic action to the inline
// selector. Returns false when the selector is hidden so the caller
// can fall through to the toolkit default.
func (a *App) handleSelectorAction(act int) bool {
	if a.selector == nil || !a.selector.Shown() {
		return false
	}
	a.selector.HandleActionWithTab(act, a.loopContext())
	return true
}

// dismissSelector hides the inline selector and resets its mode. Safe to
// call unconditionally; a no-op when the selector is already hidden.
func (a *App) dismissSelector() {
	a.completionSeq++
	if a.selector == nil {
		return
	}
	if a.selectorMode == selectorModeSessions {
		a.clearSessionSelectorCache()
	}
	if a.selectorMode == selectorModeFork || a.selectorMode == selectorModeForget {
		a.forkEntries = nil
		a.forkShowAll = false
	}
	a.selector.SetShown(false)
	a.selectorMode = selectorModeNone
	a.forcedCompletion = false
	a.completionSelector = tauui.CompletionSelector{}
}

// closeDialogComponent releases any component currently hosted by the shared
// dialog picker. Dialog components commonly close over the App or store an App
// pointer, so leaving the last component installed after the dialog is hidden
// keeps more of the object graph reachable than the closed UI needs.
func (a *App) closeDialogComponent() {
	if a == nil || a.dialogPicker == nil {
		return
	}
	a.dialogInputActive.Store(false)
	if d := a.loginDialog; d != nil {
		d.cancel()
		a.loginDialog = nil
		a.dialogPicker.SetShown(false)
	}
	if request := a.extensionDialog; request != nil {
		request.finish(extensionDialogResult{index: -1})
		a.extensionDialog = nil
		a.dialogPicker.SetShown(false)
	}
	a.dialogPicker.SetComponent(nil)
	a.dialogPicker.SetOnSelect(nil)
	a.dialogPicker.SetOnChange(nil)
	a.dialogPicker.SetOnCancel(a.dismissDialog)
}

// dismissDialog hides the dialog picker and returns focus to the
// editor. No-op if the dialog is already hidden.
func (a *App) dismissDialog() {
	a.dismissDialogCtx(a.loopContext())
}

// dismissDialogCtx is dismissDialog with an explicit context.
// ctx may be nil (e.g. tests); the layout / focus changes are
// skipped in that case.
func (a *App) dismissDialogCtx(ctx *tauui.Ctx) {
	if a.dialogPicker == nil {
		return
	}
	if !a.dialogPicker.Shown() {
		return
	}
	a.dialogPicker.SetShown(false)
	a.closeDialogComponent()
	if ctx != nil {
		if ctx.InvalidateLayout != nil {
			ctx.InvalidateLayout()
		}
		ctx.InvalidateAll()
	}
	// Post the focus return to the next loop iteration so the
	// tree is rebuilt first.
	if a.app != nil {
		_ = a.app.Post(func(_ *tauui.UpdateCtx) {
			if !a.dialogPicker.Shown() {
				a.app.SetFocus(a.editor.ID())
			}
		})
	}
}

// dismissSelectorIfVisible is the ActionCancel handler entry point used
// by tests that do not need to inspect invalidation side effects.
func (a *App) dismissSelectorIfVisible() bool {
	return a.dismissSelectorIfVisibleCtx(a.loopContext())
}

// dismissSelectorIfVisibleCtx closes whichever picker is currently shown
// and emits the layout invalidation required when a picker's MinSize
// collapses to zero. Returns true when a picker was visible so callers can
// stop Esc processing before it becomes a streaming interrupt.
func (a *App) dismissSelectorIfVisibleCtx(ctx *tauui.Ctx) bool {
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		// The picker's own cancel handler may step back instead of closing.
		if !a.dialogPicker.Cancel() {
			a.dismissDialogCtx(ctx)
		}
		return true
	}
	if a.selector != nil && a.selector.Shown() {
		wasShown := a.selector.Shown()
		a.dismissSelector()
		a.invalidateSelectorLayout(wasShown, false, ctx)
		return true
	}
	return false
}

// externalCommands queries the session for additional slash commands
// (agent builtins, prompt templates, skills, extensions, plugins) and
// adapts them to the TUI's ExternalCommand shape for the completion
// popup. Returns nil if the session does not implement
// ExternalCommandsProvider or exposes no commands. Safe to call from
// the app loop.
func (a *App) externalCommands() []ExternalCommand {
	// Same-named session commands are shadowed by the local registry:
	// dispatch already prefers the local handler, so listing both would
	// show one entry twice (e.g. /sessions as both the workspace and the
	// shell-oriented builtin).
	local := a.cmds.All()
	owned := make(map[string]struct{}, len(local))
	for _, c := range local {
		owned[c.Name] = struct{}{}
	}
	var out []ExternalCommand
	if a.sess != nil {
		for _, info := range a.sess.SlashCommands() {
			if _, dup := owned[info.Name]; dup || (a.hideSkillCommands && info.Source == "skill") {
				continue
			}
			out = append(out, ExternalCommand{
				Name:         info.Name,
				Description:  info.Description,
				ArgumentHint: info.ArgumentHint,
				Source:       info.Source,
			})
		}
	}
	if len(a.testExternalCommands) > 0 {
		out = append(out, a.testExternalCommands...)
	}
	return out
}

func (a *App) sessionSlashCommandExists(name string) bool {
	if a.sess == nil || name == "" {
		return false
	}
	for _, info := range a.sess.SlashCommands() {
		if info.Name == name {
			return true
		}
	}
	return false
}

// ensureAssistantMessage creates a streaming assistant row when SDK events say
// an assistant message has started or when a provider omits message_start but
// later emits text/final content. It is intentionally idempotent.
func (a *App) ensureAssistantMessage(messageID ...string) {
	if !a.conv.IsStreaming() {
		id := ""
		if len(messageID) > 0 {
			id = messageID[0]
		}
		a.conv.BeginAssistantMessageWithID(id)
	}
}

// handleEvent maps an sdk.Event onto conversation-model mutations. It runs on
// the app loop (inside a posted callback).
func (a *App) handleEvent(ev sdk.Event) {
	a.programStatus.handleEvent(ev)
	a.refreshProgramStatus()
	switch ev.Type {
	case "usage_limit_wait":
		provider, _ := plString(ev.Payload, "provider")
		until := time.Time{}
		if raw, _ := plString(ev.Payload, "resets_at"); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err == nil {
				until = parsed
			}
		}
		a.setUsageLimitWait(provider, until)
	case "paused":
		target, _ := plString(ev.Payload, "target")
		a.setPaused(target)
	case "usage":
		a.refreshFooterStats(a.loopContext())
	case "tool_call":
		a.setToolRunning(true)
		if a.conversationEvents == nil {
			a.conversationEvents = newConversationEventAdapter(a)
		}
		a.conversationEvents.handle(ev)
	case "tool_result":
		a.setToolRunning(false)
		if a.conversationEvents == nil {
			a.conversationEvents = newConversationEventAdapter(a)
		}
		a.conversationEvents.handle(ev)
	case "agent_end":
		if a.conversationEvents == nil {
			a.conversationEvents = newConversationEventAdapter(a)
		}
		a.conversationEvents.handle(ev)
		a.refreshEmptyAssistantResponseSummary()
		a.conv.ClearThinking()
		a.setStreaming(false)
		a.setToolRunning(false)
		a.clearUsageLimitWait()
		a.syncPendingFromSession(a.loopContext())
	default:
		if a.conversationEvents == nil {
			a.conversationEvents = newConversationEventAdapter(a)
		}
		a.conversationEvents.handle(ev)
	}
	if ev.Type == "message_end" {
		a.refreshFooterStats(a.loopContext())
		a.syncPendingFromSession(a.loopContext())
	}
	a.refreshChat()
}

// refreshChat records follow-latest intent without repainting stale cached
// content. ChatLog's render-ready app-loop callback consumes the intent only
// after the newest scheduled revision has been installed and its completed
// height is available to ScrollView.
func (a *App) refreshChat() {
	a.followChat = true
}

// setToolRunning updates the footer's persistent tool-running indicator and
// invalidates the footer rect so the new state is visible. Safe to
// call from the app loop.
func (a *App) setToolRunning(v bool) {
	if a.footer == nil || a.footer.data == nil {
		return
	}
	a.footer.data.toolRunning = v
	if ctx := a.loopContext(); ctx != nil && ctx.Invalidate != nil {
		if r := a.footer.Rect(); r.W > 0 && r.H > 0 {
			ctx.Invalidate(r)
		}
	}
}

// plString extracts a string field from a map[string]any payload.
func plString(payload any, key string) (string, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// plBool extracts a bool field from a map[string]any payload.
func plBool(payload any, key string) (bool, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}
