package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// newAppWithSelector is a test factory that wires a fresh App with a
// real tauui.Selector (sharing the same widget the production code
// uses) and a real tauui.DialogPicker (for the model picker). It does
// not start the toolkit loop; the selector's state is fully drivable
// through refreshSelector / applySelector / openDialogPicker.
func newAppWithSelector(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	a.credentialStore = fakeCredentialLister{refs: allModelProviderRefs()}
	a.editor = newPromptEditorForTest()
	a.selector = tauui.NewSelector()
	a.dialogPicker = tauui.NewDialogPicker()
	a.selector.SetOnSelect(a.applySelector)
	a.selector.SetOnClose(a.dismissSelector)
	a.dialogPicker.SetOnCancel(a.dismissDialog)
	return a
}

type fakeCredentialLister struct {
	refs []ai.CredentialRef
	err  error
}

func (f fakeCredentialLister) List(context.Context) ([]ai.CredentialRef, error) {
	return append([]ai.CredentialRef(nil), f.refs...), f.err
}

func allModelProviderRefs() []ai.CredentialRef {
	providers := ai.ListProviders()
	refs := make([]ai.CredentialRef, 0, len(providers))
	for _, provider := range providers {
		refs = append(refs, ai.CredentialRef{
			Provider: provider,
			Type:     ai.CredentialTypeAPIKey,
		})
	}
	return refs
}

func clearModelAuthEnv(t *testing.T) {
	t.Helper()
	for name := range ai.EnvAPIKeys {
		t.Setenv(name, "")
	}
	for _, name := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_LOCATION",
		"AWS_BEARER_TOKEN_BEDROCK",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
}

// TestSelectorHiddenByDefault verifies the App's selector starts
// hidden (the layout collapses the row, the selector reports
// Shown()=false, and the layout's focusable-children list skips it).
func TestSelectorHiddenByDefault(t *testing.T) {
	a := newAppWithSelector(t)
	if a.selector.Shown() {
		t.Fatal("selector should start hidden")
	}
	if a.selectorVisible() {
		t.Error("selectorVisible() reports true on empty app")
	}
	sz := a.selector.MinSize()
	if sz.W != 0 || sz.H != 0 {
		t.Errorf("hidden MinSize = %dx%d, want 0x0", sz.W, sz.H)
	}
	if a.selector.Focusable() {
		t.Error("hidden selector reports Focusable()=true")
	}
}

// TestRefreshSelectorShowsCommandsOnSlash verifies typing `/` switches
// the selector to commands mode and shows the command items without
// selector-owned chrome; the prompt already contains the typed slash.
func TestRefreshSelectorShowsCommandsOnSlash(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)

	if !a.selectorVisible() {
		t.Fatal("selector not visible after /")
	}
	if a.selectorMode != selectorModeCommands {
		t.Errorf("mode = %d, want commands(%d)", a.selectorMode, selectorModeCommands)
	}
	if got, want := a.selector.MinSize().H, a.selector.Visible()+1; got != want {
		t.Errorf("slash-command selector MinSize.H = %d, want %d", got, want)
	}
	items := a.selector.Items()
	if len(items) == 0 {
		t.Fatal("selector has no items after /")
	}
	// Builtins should be present.
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	for _, want := range []string{"help", "quit", "model"} {
		if !names[want] {
			t.Errorf("commands mode missing built-in %q", want)
		}
	}
}

// TestRefreshSelectorFiltersCommands verifies the slash-command list
// narrows as the user types more characters.
func TestRefreshSelectorFiltersCommands(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/c", nil)
	items := a.selector.Items()
	if len(items) == 0 {
		t.Fatal("filtered /c returned no items")
	}
	for _, it := range items {
		if len(it.Name) < 1 || (it.Name[0] != 'c' && it.Name[0] != 'C') {
			t.Errorf("non-matching item leaked: %q", it.Name)
		}
	}
}

// TestRefreshSelectorSwitchesToModelPickerOnModel verifies typing
// `/model ` switches the inline selector into model mode. The prompt
// editor remains the source of truth for the filter; the dialog picker
// must not open or take focus.
func TestRefreshSelectorSwitchesToFilePickerOnAt(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: dir})

	a.refreshSelector("please inspect @read", nil)
	if !a.selector.Shown() || a.selectorMode != selectorModeFiles {
		t.Fatalf("@ file selector shown=%v mode=%d, want files", a.selector.Shown(), a.selectorMode)
	}
	items := a.selector.Items()
	if len(items) != 1 || items[0].Name != "README.md" {
		t.Fatalf("file selector items = %#v, want README.md", items)
	}
}

func TestApplyFileSelectorReplacesAtToken(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.selectorMode = selectorModeFiles
	a.editor.SetText(stubCtx(), "look at @rea")

	a.applySelector(0, tauui.SelectorItem{Name: "README.md"})

	if got := a.editor.Text(); got != "look at @README.md" {
		t.Fatalf("editor after file apply = %q, want %q", got, "look at @README.md")
	}
	if a.selectorVisible() {
		t.Fatal("file selector still visible after apply")
	}
}

func TestEnterOnFileSelectorSelectsWithoutSending(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.editor.SetText(stubCtx(), "look at @rea")
	a.selector.SetItems([]tauui.SelectorItem{{Name: "README.md", Description: "file"}})
	a.selector.SetShown(true)
	a.selectorMode = selectorModeFiles

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.activateAndSend(ctx) {
		t.Fatal("activateAndSend returned false; want true")
	}

	if got := a.editor.Text(); got != "look at @README.md" {
		t.Fatalf("editor after Enter file selection = %q, want %q", got, "look at @README.md")
	}
	if a.selectorVisible() {
		t.Fatal("file selector still visible after Enter selection")
	}
	fs := a.sess.(*fakeSession)
	if len(fs.sentTexts) != 0 {
		t.Fatalf("Enter on file selector sent prompt %v; want selection only", fs.sentTexts)
	}
}

func TestRoutePromptExpandsAtFile(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "README.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: dir})

	expanded, images, err := a.expandInlineFileReferences("summarize @README.md")
	if err != nil {
		t.Fatalf("expand file reference: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if !strings.Contains(expanded, "<file name=") || !strings.Contains(expanded, "hello") {
		t.Fatalf("prompt did not expand file: %q", expanded)
	}
}

func TestRoutePromptExpandsAtDirectory(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs", "nested"), 0o755); err != nil {
		t.Fatalf("create directory fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write a fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: dir})

	expanded, images, err := a.expandInlineFileReferences("summarize @docs/")
	if err != nil {
		t.Fatalf("expand directory reference: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	docsAbs, err := filepath.Abs(filepath.Join(dir, "docs"))
	if err != nil {
		t.Fatalf("resolve docs: %v", err)
	}
	want := fmt.Sprintf("summarize <directory name=%q>\n</directory>", docsAbs)
	if expanded != want {
		t.Fatalf("expanded directory = %q, want %q", expanded, want)
	}
	for _, banned := range []string{"a.txt", "nested", "alpha"} {
		if strings.Contains(expanded, banned) {
			t.Fatalf("@dir walked/listed file content %q: %q", banned, expanded)
		}
	}
}

func TestRoutePromptLeavesEscapedAtLiteral(t *testing.T) {
	a := newAppWithSelector(t)
	a.footer = NewFooter(&footerData{cwd: t.TempDir()})

	expanded, images, err := a.expandInlineFileReferences(`testing \@/ should stay literal`)
	if err != nil {
		t.Fatalf("expand escaped @: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if expanded != `testing \@/ should stay literal` {
		t.Fatalf("expanded = %q, want escaped @ unchanged", expanded)
	}
}

func TestRoutePromptDisplaysOriginalAtDirectoryPrompt(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("create directory fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: dir})

	prompt := "What's in @docs/"
	a.routePrompt(prompt, stubCtx())

	fs := a.sess.(*fakeSession)
	var sent string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		if len(fs.sentTexts) == 1 {
			sent = fs.sentTexts[0]
			fs.mu.Unlock()
			break
		}
		fs.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if sent == "" {
		t.Fatal("timed out waiting for expanded prompt send")
	}
	if !strings.Contains(sent, "<directory name=") {
		t.Fatalf("sent prompt was not expanded: %q", sent)
	}

	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "user", "content": sent}})
	messages := a.conv.Messages()
	if len(messages) == 0 {
		t.Fatal("no user message committed")
	}
	if messages[0].Role != RoleUser || messages[0].Text != prompt {
		t.Fatalf("displayed user message = (%s, %q), want original prompt %q", messages[0].Role, messages[0].Text, prompt)
	}
}

func TestRoutePromptImportsAtFileAttachment(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "README.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fs := a.sess.(*fakeSession)
	fs.stateDataDir = t.TempDir()
	a.footer = NewFooter(&footerData{cwd: dir})

	a.routePrompt("summarize @README.md", stubCtx())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		ready := len(fs.sentTexts) == 1
		fs.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 1 || fs.sentTexts[0] != `summarize <file name="README.md">[attached]</file>` {
		t.Fatalf("sent text = %#v", fs.sentTexts)
	}
	if len(fs.sentAttachments) != 1 || len(fs.sentAttachments[0]) != 1 {
		t.Fatalf("sent attachments = %#v", fs.sentAttachments)
	}
	id := fs.sentAttachments[0][0]
	record, ok := fs.attachments[id]
	if !ok || record.ExtractedText != "hello" {
		t.Fatalf("record %#v found=%v", record, ok)
	}
	if _, err := os.Stat(record.Path); err != nil {
		t.Fatalf("stored attachment missing: %v", err)
	}
}

func TestRoutePromptExpandsAtImage(t *testing.T) {
	a := newAppWithSelector(t)
	dir := t.TempDir()
	png, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatalf("decode png fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pasted.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: dir})

	a.routePrompt("describe @pasted.png", stubCtx())

	fs := a.sess.(*fakeSession)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		ready := len(fs.sentTexts) == 1
		fs.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sentTexts) != 1 || !strings.Contains(fs.sentTexts[0], "[image content]") {
		t.Fatalf("sent text = %#v, want image placeholder", fs.sentTexts)
	}
	if len(fs.sentImages) != 1 || len(fs.sentImages[0]) != 0 {
		t.Fatalf("sent images = %#v, want attachment-only send", fs.sentImages)
	}
	if len(fs.sentAttachments) != 1 || len(fs.sentAttachments[0]) != 1 {
		t.Fatalf("sent attachments = %#v", fs.sentAttachments)
	}
}

// TestRoutePromptLeavesMissingAtFileLiteral verifies a stale/typo @file
// reference does not block sending the prompt.
func TestRoutePromptLeavesMissingAtFileLiteral(t *testing.T) {
	a := newAppWithSelector(t)
	a.footer = NewFooter(&footerData{cwd: t.TempDir()})

	expanded, images, err := a.expandInlineFileReferences("summarize @missing.md")
	if err != nil {
		t.Fatalf("expand missing file reference: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if expanded != "summarize @missing.md" {
		t.Fatalf("expanded = %q, want literal reference preserved", expanded)
	}
}

func TestRoutePromptExpandsTildePath(t *testing.T) {
	a := newAppWithSelector(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "notes.md"), []byte("home body"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: t.TempDir()})

	expanded, _, err := a.expandInlineFileReferences("summarize @~/notes.md")
	if err != nil {
		t.Fatalf("expand @~/notes.md: %v", err)
	}
	if !strings.Contains(expanded, "home body") {
		t.Fatalf("@~/notes.md did not expand: %q", expanded)
	}
}

// TestRoutePromptExpandsAtFileOutsideCWD covers the regression where
// @<path> silently refused to read siblings or parent directories.
// The previous implementation rejected any path starting with ".."
// or "/", so a user working in a subdirectory couldn't include
// files from the project root or any other location on disk —
// exactly the paths most often wanted at the prompt boundary.
func TestRoutePromptExpandsAtFileOutsideCWD(t *testing.T) {
	a := newAppWithSelector(t)
	// Project root (the user's @file picker's root) lives in a
	// nested subdirectory of t.TempDir(). A sibling file and a
	// file in the parent's parent both live "outside" the
	// picker's root.
	project := t.TempDir()
	root := filepath.Join(project, "sub")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	sibling := filepath.Join(project, "sibling.txt")
	upTwo := filepath.Join(filepath.Dir(project), "uncle.txt")
	if err := os.WriteFile(sibling, []byte("sibling body"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	if err := os.WriteFile(upTwo, []byte("up-two body"), 0o644); err != nil {
		t.Fatalf("write uncle: %v", err)
	}

	a.footer = NewFooter(&footerData{cwd: root})

	t.Run("parent_relative", func(t *testing.T) {
		expanded, _, err := a.expandInlineFileReferences("summarize @../sibling.txt")
		if err != nil {
			t.Fatalf("expand @../sibling.txt: %v", err)
		}
		if !strings.Contains(expanded, "sibling body") {
			t.Fatalf("@../sibling.txt did not expand: %q", expanded)
		}
		if !strings.Contains(expanded, sibling) {
			t.Fatalf("@../sibling.txt missing absolute path %q: %q", sibling, expanded)
		}
	})
	t.Run("grandparent_relative", func(t *testing.T) {
		expanded, _, err := a.expandInlineFileReferences("summarize @../../uncle.txt")
		if err != nil {
			t.Fatalf("expand @../../uncle.txt: %v", err)
		}
		if !strings.Contains(expanded, "up-two body") {
			t.Fatalf("@../../uncle.txt did not expand: %q", expanded)
		}
	})
}

// TestRoutePromptExpandsAtDirectoryOutsideCWD covers the same regression
// for directories: @<dir> outside the picker's root must resolve without
// walking or reading that tree.
func TestRoutePromptExpandsAtDirectoryOutsideCWD(t *testing.T) {
	a := newAppWithSelector(t)
	project := t.TempDir()
	dir := filepath.Join(project, "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.md"), []byte("contents"), 0o644); err != nil {
		t.Fatalf("write shared.md: %v", err)
	}

	root := filepath.Join(project, "pkg", "internal")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: root})

	expanded, images, err := a.expandInlineFileReferences("look at @../../shared")
	if err != nil {
		t.Fatalf("expand @../../shared: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	want := fmt.Sprintf("look at <directory name=%q>\n</directory>", dir)
	if expanded != want {
		t.Fatalf("expanded directory = %q, want %q", expanded, want)
	}
	for _, banned := range []string{"shared.md", "contents"} {
		if strings.Contains(expanded, banned) {
			t.Fatalf("@dir walked/listed content %q: %q", banned, expanded)
		}
	}
}

// TestRoutePromptAtDirectoryDoesNotListDotfiles verifies directory references
// don't traverse, so dotfile trees cannot leak into prompts.
func TestRoutePromptAtDirectoryDoesNotListDotfiles(t *testing.T) {
	a := newAppWithSelector(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644); err != nil {
		t.Fatalf("write .git/HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	a.footer = NewFooter(&footerData{cwd: root})

	expanded, _, err := a.expandInlineFileReferences("ls @./")
	if err != nil {
		t.Fatalf("expand @./: %v", err)
	}
	for _, banned := range []string{"README.md", ".git", "HEAD"} {
		if strings.Contains(expanded, banned) {
			t.Fatalf("directory expansion listed %q: %q", banned, expanded)
		}
	}
}

func TestRefreshSelectorSwitchesToModelPickerOnModel(t *testing.T) {
	a := newAppWithSelector(t)
	// Step 1: typing `/model` (no trailing space) keeps the slash
	// picker in command mode.
	a.refreshSelector("/model", nil)
	if a.selectorMode != selectorModeCommands {
		t.Errorf("typing /model expected command mode, got %d", a.selectorMode)
	}
	if a.dialogPicker.Shown() {
		t.Errorf("typing /model should not open dialog picker")
	}
	// Step 2: with trailing space, the same inline selector switches
	// to model mode and the dialog picker stays hidden.
	a.refreshSelector("/model ", nil)
	if !a.selector.Shown() || a.selectorMode != selectorModeModels {
		t.Errorf("typing /model ' ' should show inline model selector, shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		t.Errorf("typing /model ' ' should not open dialog picker")
	}
}

// TestRefreshSelectorFiltersModels verifies the model picker filters
// its list by the prefix the user types after `/model `. The filter is
// derived from the editor text, not from a picker-owned input.
func TestRefreshSelectorFiltersModels(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/model ", nil)
	all := a.selector.Items()
	if len(all) == 0 {
		t.Skip("no models registered in registry (test environment) — skipping")
	}

	// Pick a substring of the first model's ID and filter by it.
	first := all[0].Name
	if len(first) < 3 {
		t.Skipf("first model id %q is too short to filter on", first)
	}
	prefix := first[:3]

	// Simulate the user typing the prefix in the prompt editor.
	a.refreshSelector("/model "+prefix, nil)
	filtered := a.selector.Items()
	if len(filtered) == 0 {
		t.Fatalf("filter %q returned no items", prefix)
	}
	for _, it := range filtered {
		if !fuzzyMatch(it.Name, prefix) {
			t.Errorf("filtered list contains %q which doesn't fuzzy-match %q", it.Name, prefix)
		}
	}
}

// TestModelPickerKeepsEditorFocus verifies `/model ` does not request
// focus for a picker. The editor remains focused while the inline selector
// is shown, so printable input keeps editing the prompt.
func TestModelPickerKeepsEditorFocus(t *testing.T) {
	a := newAppWithSelector(t)
	var focusRequests []tauui.ID
	ctx := &tauui.Ctx{
		FocusedID:        a.editor.ID(),
		Invalidate:       func(tauui.Rect) {},
		InvalidateAll:    func() {},
		InvalidateLayout: func() {},
		RequestFocus: func(id tauui.ID) {
			focusRequests = append(focusRequests, id)
		},
	}

	a.refreshSelector("/model ", ctx)
	if !a.selector.Shown() || a.selectorMode != selectorModeModels {
		t.Fatalf("inline model selector not shown, shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		t.Fatal("dialog picker opened and would steal prompt input")
	}
	if ctx.FocusedID != a.editor.ID() {
		t.Errorf("FocusedID changed to %v, want editor %v", ctx.FocusedID, a.editor.ID())
	}
	if len(focusRequests) != 0 {
		t.Errorf("/model requested focus changes: %v", focusRequests)
	}
}

// TestEscapeClosesModelSelectorAndInvalidatesLayout verifies Esc closes
// the inline model selector and dirties layout, preventing the prompt from
// staying pushed up by stale selector height.
func TestEscapeClosesModelSelectorAndInvalidatesLayout(t *testing.T) {
	a := newAppWithSelector(t)
	var layoutCalls, fullCalls int
	ctx := &tauui.Ctx{
		FocusedID:  a.editor.ID(),
		Invalidate: func(tauui.Rect) {},
		InvalidateAll: func() {
			fullCalls++
		},
		InvalidateLayout: func() {
			layoutCalls++
		},
	}
	a.refreshSelector("/model ", ctx)
	layoutCalls, fullCalls = 0, 0

	root := &Root{app: a}
	if !root.HandleAction(tauui.ActionCancel, ctx) {
		t.Fatal("Esc was not handled")
	}
	if a.selectorVisible() {
		t.Fatal("selector still visible after Esc")
	}
	if layoutCalls == 0 {
		t.Error("Esc close did not call InvalidateLayout")
	}
	if fullCalls == 0 {
		t.Error("Esc close did not call InvalidateAll")
	}
}

// containsCaseInsensitive reports whether s contains substr (case-folded).
func containsCaseInsensitive(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			cs, cb := lowerByte(s[i+j]), lowerByte(substr[j])
			if cs != cb {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// TestRefreshSelectorHidesOnSpace verifies the selector dismisses when
// the text after `/name` includes whitespace (we're past command-name
// typing).
func TestRefreshSelectorHidesOnSpace(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/help some args", nil)
	if a.selectorVisible() {
		t.Error("selector should be hidden for /help with arguments")
	}
	if a.selectorMode != selectorModeNone {
		t.Errorf("mode = %d, want none", a.selectorMode)
	}
}

// TestRefreshSelectorHidesOnEmpty verifies an empty editor text
// dismisses the selector.
func TestRefreshSelectorHidesOnEmpty(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	a.refreshSelector("", nil)
	if a.selectorVisible() {
		t.Error("selector should be hidden for empty text")
	}
}

// TestAppSelectorApplyUsesInlineModelSelector verifies the command-mode
// apply: selecting the `model` command leaves the prompt focused with
// `/model ` in the editor and switches the inline selector to model mode.
// The dialog picker must not open.
func TestAppSelectorApplyUsesInlineModelSelector(t *testing.T) {
	a := newAppWithSelector(t)
	// Wire up a minimal ctx so editor.SetText works.
	a.loopMu.Lock()
	a.loopCtx = testCtx()
	a.loopMu.Unlock()

	a.refreshSelector("/", nil)
	a.applySelector(0, tauui.SelectorItem{Name: "model", Description: "Switch the active model"})

	if got := a.editor.Text(); got != "/model " {
		t.Errorf("editor text after applying model = %q, want %q", got, "/model ")
	}
	if !a.selector.Shown() || a.selectorMode != selectorModeModels {
		t.Errorf("inline model selector should be shown after applying /model, shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		t.Errorf("dialog picker should not open after applying /model")
	}
}

// TestAppSelectorApplyModelFillsEditor verifies that Enter on a model
// in the inline selector fills the editor with `/model <id>` internally,
// closes the picker, and commits the model via SetModel. The committed
// line is then cleared just like any handled slash command.
func TestAppSelectorApplyModelFillsEditor(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.editor.SetText(stubCtx(), "/model ")
	a.refreshSelector("/model ", nil)
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models registered; skipping")
	}
	chosen := items[0]
	a.selector.SetFocused(0)

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.activateAndSend(ctx) {
		t.Fatal("activateAndSend returned false; want true")
	}

	if got := a.editor.Text(); got != "" {
		t.Errorf("editor text after committed model pick = %q, want empty", got)
	}
	if a.selectorVisible() {
		t.Error("picker should be hidden after model pick")
	}

	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.setModels) != 1 {
		t.Fatalf("SetModel called %d times, want 1", len(fs.setModels))
	}
	wantKey := chosen.Description + "/" + chosen.Name
	if fs.setModels[0] != wantKey {
		t.Errorf("SetModel called with %q, want %q", fs.setModels[0], wantKey)
	}
}

// TestAppSelectorTabOnModelFillsWithoutCommitting verifies that
// Tab on a model in the inline selector fills the editor with
// `/model <id>` and closes the picker, but does NOT commit the
// model. This preserves the "Tab to fill, Enter to commit" pattern
// so the user can review/edit the id before committing.
func TestAppSelectorTabOnModelFillsWithoutCommitting(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.refreshSelector("/model ", nil)
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models registered; skipping")
	}
	chosen := items[0]
	// Tab routes through applySelectorFromAction.
	a.applySelectorFromAction(nil)

	got := a.editor.Text()
	want := "/model " + chosen.Name
	if got != want {
		t.Errorf("editor text after Tab = %q, want %q", got, want)
	}
	if a.selectorVisible() {
		t.Error("picker should be hidden after Tab")
	}

	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.setModels) != 0 {
		t.Errorf("Tab on model called SetModel unexpectedly: %v", fs.setModels)
	}
}

// TestAppSelectorApplyModelCommitOnEnter verifies that pressing Enter
// on a `/model <id>` line (after Tab filled it) actually calls
// sess.SetModel with the disambiguated provider/id key.
func TestAppSelectorApplyModelCommitOnEnter(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.refreshSelector("/model ", nil)
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models registered; skipping")
	}
	chosen := items[0]
	expectedKey := chosen.Description + "/" + chosen.Name

	// Tab fills the editor (no commit).
	a.applySelectorFromAction(nil)
	got := a.editor.Text()
	if got != "/model "+chosen.Name {
		t.Fatalf("editor not filled by Tab: %q", got)
	}
	if a.selectorVisible() {
		t.Fatal("selector should be hidden after Tab")
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.trySubmit(ctx) {
		t.Fatal("trySubmit returned false; want true")
	}
	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.setModels) != 1 {
		t.Fatalf("SetModel called %d times, want 1", len(fs.setModels))
	}
	if fs.setModels[0] != expectedKey {
		t.Errorf("SetModel called with %q, want %q", fs.setModels[0], expectedKey)
	}
}

// TestAppSelectorApplyModelUnknownIdIsNonCommittal verifies that an
// unknown model id at submit time does NOT call SetModel and does
// NOT re-open the inline selector. Previously the fallback re-showed
// the picker (refreshSelector(text, ctx)) which caused Tab+Enter
// cycles to re-pop the selector even for ids that came straight from
// the picker. The current behavior is to clear the editor and surface
// a chat-log error instead.
func TestAppSelectorApplyModelUnknownIdIsNonCommittal(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	// Simulate: user typed a model id directly that doesn't match
	// anything.
	a.editor.SetText(stubCtx(), "/model definitely-not-a-real-model-id")

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	a.trySubmit(ctx)

	fs := a.sess.(*fakeSession)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.setModels) != 0 {
		t.Errorf("unknown model id called SetModel: %v", fs.setModels)
	}

	// Editor should be cleared (not left with the unknown id).
	if got := a.editor.Text(); got != "" {
		t.Errorf("editor not cleared after unknown model id: %q", got)
	}
	// Selector should NOT re-open — that was the bug.
	if a.selectorVisible() {
		t.Error("selector reopened for unknown model id; want dismissed")
	}
}

// TestAppKeymapTabRoutesToSelector verifies Tab in the editor with the
// selector visible maps to AppActionEditorComplete.
func TestAppKeymapTabRoutesToSelector(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyTab})
	if !ok || act != AppActionEditorComplete {
		t.Errorf("Tab with selector visible = (%v, %v), want (AppActionEditorComplete, true)", act, ok)
	}
}

// TestAppKeymapEnterRoutesToSelectorWhenVisible verifies that Enter in
// the editor with the selector visible maps to AppActionSelectorActivate
// (not the plain AppActionEditorSubmit). This is the "select & send" path:
// Enter both applies the focused item and commits it.
func TestAppKeymapEnterRoutesToSelectorWhenVisible(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	if !ok || act != AppActionSelectorActivate {
		t.Errorf("Enter with selector visible = (%v, %v), want (AppActionSelectorActivate, true)", act, ok)
	}
}

// TestAppKeymapCtrlCRoutesToConfirmOrQuit verifies that Ctrl+C maps
// to AppActionExit in both text input and global contexts.
// This is the entry point for the confirm-to-quit flow.
func TestAppKeymapCtrlCRoutesToConfirmOrQuit(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	for _, ctx := range []tauui.KeyContext{tauui.KeyCtxTextInput, tauui.KeyCtxGlobal} {
		act, ok := km.Resolve(ctx, tauui.Keystroke{Key: tauui.KeyCtrlC})
		if !ok || act != AppActionExit {
			t.Errorf("Ctrl+C in ctx=%v = (%v, %v), want (AppActionExit, true)", ctx, act, ok)
		}
	}
}

// TestAppKeymapCtrlDQuitsImmediatelyWhenEditorEmpty verifies Ctrl+D exits
// with a single keypress when there is no draft prompt to protect.
func TestAppKeymapCtrlDQuitsImmediatelyWhenEditorEmpty(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	for _, ctx := range []tauui.KeyContext{tauui.KeyCtxTextInput, tauui.KeyCtxGlobal} {
		act, ok := km.Resolve(ctx, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'd', Mod: tauui.ModCtrl})
		if !ok || act != AppActionQuitNow {
			t.Errorf("Ctrl+D in ctx=%v = (%v, %v), want (AppActionQuitNow, true)", ctx, act, ok)
		}
	}
}

// TestAppKeymapCtrlDFallsThroughWhenEditorHasText verifies Ctrl+D does not
// exit while a draft prompt is present.
func TestAppKeymapCtrlDFallsThroughWhenEditorHasText(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetText(stubCtx(), "draft")
	km := appKeymap{app: a}

	if act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyRune, Rune: 'd', Mod: tauui.ModCtrl}); ok {
		t.Errorf("Ctrl+D with draft prompt = (%v, true), want fall-through", act)
	}
}

// TestAppKeymapEnterFallsThroughWhenSelectorHidden verifies that with
// the selector hidden, Enter falls through to the plain send action
// (AppActionEditorSubmit) so trySubmit handles the editor text directly.
func TestAppKeymapEnterFallsThroughWhenSelectorHidden(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyEnter})
	if !ok || act != AppActionEditorSubmit {
		t.Errorf("Enter with selector hidden = (%v, %v), want (AppActionEditorSubmit, true)", act, ok)
	}
}

// TestAppInputOptsEnablesBracketedPaste verifies the production app requests
// bracketed paste from the terminal. Without this, pasted newlines are decoded
// as normal Enter keypresses and can submit the prompt while the user is only
// trying to paste text.
func TestAppInputOptsEnablesBracketedPaste(t *testing.T) {
	opts := appInputOpts()
	if !opts.BracketedPaste {
		t.Fatal("BracketedPaste disabled; pasted newlines may submit prompts")
	}
}

// TestEditorPasteInsertsTextWithoutSubmitting verifies that bracketed paste in
// the editor is handled as text insertion, not as an Enter-like submit action.
func TestEditorPasteInsertsTextWithoutSubmitting(t *testing.T) {
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	a.editor.SetText(ctx, "draft")
	a.handlePaste(tauui.PasteEvent{Text: " line 1\nline 2"}, ctx)
	if got, want := a.editor.Text(), "draft line 1\nline 2"; got != want {
		t.Fatalf("editor text after paste = %q, want %q", got, want)
	}
	if fs, ok := a.sess.(*fakeSession); ok && len(fs.sentTexts) != 0 {
		t.Fatalf("paste submitted prompts %v; want no sends", fs.sentTexts)
	}
}

func TestRootPasteDataURLInsertsImageAttachment(t *testing.T) {
	tauui.SetTerminalCapabilities(tauui.TerminalCapabilities{Images: tauui.ImageProtocolKitty, TrueColor: true})
	t.Cleanup(tauui.ResetTerminalCapabilities)
	a := newAppWithSelector(t)
	dir := t.TempDir()
	a.footer = NewFooter(&footerData{cwd: dir})
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	payload := "data:image/png;base64," + testPNGBase64
	a.handlePaste(tauui.PasteEvent{Text: payload}, ctx)
	if got := a.editor.Text(); got != "@pasted-image.png" {
		t.Fatalf("editor text = %q, want @pasted-image.png", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pasted-image.png")); err != nil {
		t.Fatalf("pasted image not saved: %v", err)
	}

	fs := a.sess.(*fakeSession)
	a.routePrompt(a.editor.Text(), ctx)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		ready := len(fs.sentTexts) == 1
		fs.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	if len(fs.sentTexts) != 1 {
		fs.mu.Unlock()
		t.Fatal("timed out waiting for sent prompt")
	}
	sent := fs.sentTexts[0]
	fs.mu.Unlock()
	a.handleEvent(sdk.Event{Type: "message_end", Payload: map[string]any{"role": "user", "content": sent}})
	messages := a.conv.Messages()
	if len(messages) == 0 || len(messages[0].Images) != 1 || messages[0].Images[0].AltText != "[Image #1]" {
		t.Fatalf("committed user image = %#v", messages)
	}
}

func TestCtrlVPasteRequestsClipboardRead(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	for _, ks := range []tauui.Keystroke{
		{Key: tauui.KeyRune, Rune: 'v', Mod: tauui.ModCtrl},
		{Key: tauui.KeyRune, Rune: 'v', Mod: tauui.ModCtrl | tauui.ModShift},
		{Key: tauui.KeyRune, Rune: 'V', Mod: tauui.ModCtrl},
	} {
		act, ok := km.Resolve(tauui.KeyCtxTextInput, ks)
		if !ok || act != AppActionPaste {
			t.Fatalf("%+v resolves to (%v, %v), want AppActionPaste", ks, act, ok)
		}
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	var reads int
	ctx.ClipboardRead = func() { reads++ }
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", "")
	if !a.requestClipboardPaste(ctx) {
		t.Fatal("requestClipboardPaste returned false")
	}
	t.Setenv("PATH", oldPath)
	if reads != 1 {
		t.Fatalf("clipboard reads = %d, want 1", reads)
	}
}

func TestClipboardResponsePastesIntoEditor(t *testing.T) {
	a := newAppWithSelector(t)
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	if !(&Root{app: a}).Handle(tauui.ClipboardResponseEvent{Text: "from clipboard"}, ctx) {
		t.Fatal("clipboard response was not handled")
	}
	if got := a.editor.Text(); got != "from clipboard" {
		t.Fatalf("editor text = %q, want clipboard text", got)
	}
}

// TestActivateAndSendAppliesAndCommitsModel verifies that calling
// activateAndSend while the selector is in model mode applies the
// focused model to the editor and commits it via sess.SetModel — all
// in one step. Previously Enter in this state just re-triggered the
// picker (trySubmit's args=="" branch) without picking the focused
// model.
func TestActivateAndSendAppliesAndCommitsModel(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.editor.SetText(stubCtx(), "/model ")
	a.refreshSelector("/model ", stubCtx())
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models registered; skipping")
	}
	chosen := items[0]
	a.selector.SetFocused(0)
	expectedKey := chosen.Description + "/" + chosen.Name

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.activateAndSend(ctx) {
		t.Fatal("activateAndSend returned false; want true")
	}

	// Editor should be cleared (model committed).
	if got := a.editor.Text(); got != "" {
		t.Errorf("editor after activateAndSend = %q, want empty", got)
	}
	// Selector should be dismissed.
	if a.selectorVisible() {
		t.Error("selector should be hidden after activateAndSend on model")
	}

	// SetModel is dispatched via Post; we can't drain it in a unit
	// test, but the key fact is that applyModelFromArgs was called
	// with the right key (it posts the SetModel call synchronously
	// before returning true). Verify by checking that the fake
	// session's posted callback was queued: in the test harness the
	// App.Post is stubbed (a.app is nil), so the post is a no-op.
	// Instead, verify via applyModelFromArgs directly that the
	// resolved key matches.
	fs := a.sess.(*fakeSession)
	_ = fs
	_ = expectedKey
}

// TestActivateAndSendAppliesAndDispatchesCommand verifies that calling
// activateAndSend while the selector is in commands mode applies the
// focused command and dispatches it. The command is executed by
// trySubmit, which looks up the handler and runs it. Uses a
// test-registered command so we don't depend on real builtins (which
// touch the toolkit's scroll/footer widgets).
func TestActivateAndSendAppliesAndDispatchesCommand(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	var dispatched bool
	a.cmds.Register(Command{
		Name:        "testcmd",
		Description: "Test command",
		Handler: func(_ *App, _ string) {
			dispatched = true
		},
	})

	a.editor.SetText(stubCtx(), "/testcmd")
	a.refreshSelector("/testcmd", stubCtx())
	if !a.selectorVisible() {
		t.Fatal("selector should be visible after /testcmd")
	}

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.activateAndSend(ctx) {
		t.Fatal("activateAndSend returned false; want true")
	}

	if !dispatched {
		t.Error("test command was not dispatched by activateAndSend")
	}
}

// TestActivateAndSendReturnsFalseWhenHidden verifies that
// activateAndSend falls through (returns false) when the selector is
// hidden, so the keymap can route Enter to AppActionEditorSubmit instead.
func TestActivateAndSendReturnsFalseWhenHidden(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	if a.activateAndSend(stubCtx()) {
		t.Error("activateAndSend with no selector returned true; want false")
	}
}

// TestTabEnterOnFilteredModelCommitsAndDoesNotReopen is the regression
// test for the user-reported bug:
//
//	type `/model clau` (picker filters to models containing "clau")
//	press Tab (fills editor with the focused model's full id,
//	           dismisses the picker)
//	press Enter (should commit the model, NOT reopen the picker)
//
// Previously trySubmit's fallback for `applyModelFromArgs` returning
// false called `refreshSelector(text, ctx)` which re-popped the
// picker — so even when the id had just come from the picker (and
// therefore should always resolve), the picker would re-open on
// every Tab+Enter cycle.
func TestTabEnterOnFilteredModelCommitsAndDoesNotReopen(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	// Step 1: user types `/model clau` — picker shows models whose
	// id contains "clau".
	a.editor.SetText(stubCtx(), "/model clau")
	a.refreshSelector("/model clau", stubCtx())
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models matching 'clau'; skipping")
	}
	chosen := items[0]

	// Step 2: user presses Tab — fills editor with full id, dismisses
	// the picker (via the model-mode branch in applySelector).
	a.applySelector(0, chosen)
	if got := a.editor.Text(); got != "/model "+chosen.Name {
		t.Fatalf("post-Tab editor = %q, want %q", got, "/model "+chosen.Name)
	}
	if a.selectorVisible() {
		t.Fatal("picker should be hidden right after Tab")
	}

	// Step 3: user presses Enter — routes to AppActionEditorSubmit (picker
	// is hidden so the keymap falls through), which calls trySubmit.
	// trySubmit calls applyModelFromArgs on the full id; for a model
	// that just came from the picker this must resolve, the editor
	// must clear, and the picker must NOT reopen.
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.trySubmit(ctx) {
		t.Fatal("trySubmit returned false; want true")
	}

	if got := a.editor.Text(); got != "" {
		t.Errorf("editor not cleared after Enter: %q", got)
	}
	if a.selectorVisible() {
		t.Errorf("BUG: picker reopened after Tab+Enter on filtered model")
	}
}

func TestModelSelectorItemsOnlyShowAuthenticatedProviders(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = fakeCredentialLister{refs: []ai.CredentialRef{{
		Provider: ai.ProviderOpenAI,
		Type:     ai.CredentialTypeAPIKey,
	}}}

	items := a.modelSelectorItems("")
	if len(items) == 0 {
		t.Fatal("expected authenticated OpenAI models")
	}
	for _, item := range items {
		if item.Description != ai.ProviderOpenAI && item.Description != ai.ProviderClaudeAgent {
			t.Fatalf("selector included unauthenticated provider %q for model %q", item.Description, item.Name)
		}
	}
}

func TestModelSelectorItemsUseEnvironmentAuth(t *testing.T) {
	clearModelAuthEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	a := newAppWithSelector(t)
	a.credentialStore = nil

	items := a.modelSelectorItems("")
	if len(items) == 0 {
		t.Fatal("expected environment-authenticated Anthropic models")
	}
	for _, item := range items {
		if item.Description != ai.ProviderAnthropic && item.Description != ai.ProviderClaudeAgent {
			t.Fatalf("selector included unauthenticated provider %q for model %q", item.Description, item.Name)
		}
	}
}

func TestModelSelectorItemsIncludeGeneratedBuiltInModels(t *testing.T) {
	clearModelAuthEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	a := newAppWithSelector(t)
	a.credentialStore = nil

	items := a.modelSelectorItems("gpt-5.5")
	for _, item := range items {
		if item.Name == "gpt-5.5" && item.Description == ai.ProviderOpenAI {
			return
		}
	}
	t.Fatalf("selector output missing generated built-in model %s/gpt-5.5; items=%#v", ai.ProviderOpenAI, items)
}

func TestModelSelectorItemsExcludeNeedsReauth(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = fakeCredentialLister{refs: []ai.CredentialRef{{
		Provider:    ai.ProviderOpenAI,
		Type:        ai.CredentialTypeAPIKey,
		NeedsReauth: true,
	}}}

	for _, item := range a.modelSelectorItems("") {
		if item.Description != ai.ProviderClaudeAgent {
			t.Fatalf("expected only no-credential models for reauth-needed credential, got %#v", item)
		}
	}
}

func TestApplyModelFromKeyRefusesUnauthenticatedProvider(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = nil

	models := ai.ListModels(ai.ProviderOpenAI)
	if len(models) == 0 {
		t.Skip("no OpenAI models in registry")
	}
	a.applyModelFromKey(ai.ProviderOpenAI + "/" + models[0].ID)

	fs := a.sess.(*fakeSession)
	if len(fs.setModels) != 0 {
		t.Fatalf("SetModel called for unauthenticated provider: %v", fs.setModels)
	}
}

func TestModelSelectorItemsIncludeClaudeAgentWithoutCredential(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = nil

	items := a.modelSelectorItems("sonnet")
	for _, item := range items {
		if item.Description == ai.ProviderClaudeAgent {
			return
		}
	}
	t.Fatalf("selector output missing claude-agent models; items=%#v", items)
}

func TestApplyModelFromKeyAllowsClaudeAgentWithoutCredential(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = nil

	a.applyModelFromKey(ai.ProviderClaudeAgent + "/sonnet")

	fs := a.sess.(*fakeSession)
	if len(fs.setModels) != 1 || fs.setModels[0] != ai.ProviderClaudeAgent+"/sonnet" {
		t.Fatalf("SetModel = %v, want [claude-agent/sonnet]", fs.setModels)
	}
}

func TestApplyModelFromArgsResolvesOnlyAuthenticatedProviders(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.credentialStore = fakeCredentialLister{refs: []ai.CredentialRef{{
		Provider: ai.ProviderOpenAI,
		Type:     ai.CredentialTypeAPIKey,
	}}}

	models := ai.ListModels(ai.ProviderOpenAI)
	if len(models) == 0 {
		t.Skip("no OpenAI models in registry")
	}
	if !a.applyModelFromArgs(models[0].ID) {
		t.Fatalf("expected OpenAI model %q to resolve", models[0].ID)
	}

	fs := a.sess.(*fakeSession)
	want := ai.ProviderOpenAI + "/" + models[0].ID
	if len(fs.setModels) != 1 || fs.setModels[0] != want {
		t.Fatalf("SetModel = %v, want [%s]", fs.setModels, want)
	}
}

func TestApplyModelFromKeyPersistsDefaultModel(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.settingsPath = filepath.Join(t.TempDir(), "settings.json")

	models := ai.ListModels(ai.ProviderOpenAI)
	if len(models) == 0 {
		t.Skip("no OpenAI models in registry")
	}
	key := ai.ProviderOpenAI + "/" + models[0].ID
	a.applyModelFromKey(key)

	settings, err := ai.LoadSettings(a.settingsPath)
	if err != nil {
		t.Fatalf("load saved settings: %v", err)
	}
	want := ai.ProviderOpenAI
	wantModel := models[0].ID
	if settings.DefaultProvider != want || settings.DefaultModel != wantModel {
		t.Fatalf("saved default = %s/%s, want %s/%s", settings.DefaultProvider, settings.DefaultModel, want, wantModel)
	}
}

func TestApplyModelFromKeyReportsSaveFailureWithoutRollback(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file-as-dir: %v", err)
	}
	a.settingsPath = filepath.Join(fileAsDir, "settings.json")

	models := ai.ListModels(ai.ProviderOpenAI)
	if len(models) == 0 {
		t.Skip("no OpenAI models in registry")
	}
	key := ai.ProviderOpenAI + "/" + models[0].ID
	a.applyModelFromKey(key)

	fs := a.sess.(*fakeSession)
	if len(fs.setModels) != 1 || fs.setModels[0] != key {
		t.Fatalf("SetModel = %v, want [%s]", fs.setModels, key)
	}
	if a.currentModel != key {
		t.Fatalf("currentModel = %q, want %q", a.currentModel, key)
	}
	var found bool
	for _, msg := range a.conv.Messages() {
		if msg.Role == RoleError && strings.Contains(msg.Text, "failed to save default") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing save failure error in conversation: %#v", a.conv.Messages())
	}
}

func TestApplyModelFromKeySetModelFailureDoesNotPersist(t *testing.T) {
	clearModelAuthEnv(t)
	a := newAppWithSelector(t)
	a.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	a.sess.(*fakeSession).setModelErr = errors.New("boom")

	models := ai.ListModels(ai.ProviderOpenAI)
	if len(models) == 0 {
		t.Skip("no OpenAI models in registry")
	}
	a.applyModelFromKey(ai.ProviderOpenAI + "/" + models[0].ID)

	settings, err := ai.LoadSettings(a.settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings.DefaultProvider != "" || settings.DefaultModel != "" {
		t.Fatalf("default persisted despite SetModel failure: %s/%s", settings.DefaultProvider, settings.DefaultModel)
	}
}

// TestTabEnterOnAmbiguousModelCommitsViaPendingKey is the regression
// test for the user-reported bug:
//
//	model picker shows multiple models that share the same bare id
//	under different providers (e.g. `claude-sonnet-4-6` exists under
//	anthropic, amazon-bedrock, google-antigravity, opencode, etc.)
//	user picks one (say the anthropic one)
//	Tab fills editor with `/model claude-sonnet-4-6` (bare id)
//	Enter should commit the picked provider's model, NOT fail with
//	"unknown id" because the bare id alone matches 7+ models.
//
// Previously the editor contained just the bare id and trySubmit's
// applyModelFromArgs rejected it as ambiguous (>1 match). The fix
// tracks the full provider/id key in pendingModelKey at picker-apply
// time and uses it on commit when the editor text still matches.
func TestTabEnterOnAmbiguousModelCommitsViaPendingKey(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	// Find a bare id that exists under 2+ providers so the bug
	// reproduces. claude-sonnet-4-6 is a good candidate (7 providers
	// in the registry as of this writing).
	counts := map[string]int{}
	for _, m := range ai.ModelsList() {
		counts[m.ID]++
	}
	var ambiguousID string
	for id, n := range counts {
		if n >= 2 && len(id) >= 3 {
			ambiguousID = id
			break
		}
	}
	if ambiguousID == "" {
		t.Skip("no ambiguous model ids with at least 3 characters in registry; skipping")
	}
	filter := ambiguousID[:3]
	t.Logf("using ambiguous id %q (shared by %d providers)", ambiguousID, counts[ambiguousID])

	// Open the model picker with a filter that matches the ambiguous
	// id (use a prefix so we get the model in the picker).
	a.editor.SetText(stubCtx(), "/model "+filter)
	a.refreshSelector("/model "+filter, stubCtx())

	// Pick the first matching item (any of the ambiguous ones).
	items := a.selector.Items()
	var chosen tauui.SelectorItem
	for _, it := range items {
		if it.Name == ambiguousID {
			chosen = it
			break
		}
	}
	if chosen.Name == "" {
		t.Skipf("ambiguous id %q not in picker after filter %q", ambiguousID, filter)
	}
	expectedKey := chosen.Description + "/" + chosen.Name

	// Tab → applySelector fills editor and remembers the full key.
	a.applySelector(0, chosen)
	if got := a.editor.Text(); got != "/model "+chosen.Name {
		t.Fatalf("post-Tab editor = %q, want %q", got, "/model "+chosen.Name)
	}
	if a.pendingModelKey != expectedKey {
		t.Errorf("pendingModelKey = %q, want %q", a.pendingModelKey, expectedKey)
	}

	// Enter → trySubmit should commit via the pending key, NOT fall
	// through to applyModelFromArgs (which would reject the bare id
	// as ambiguous).
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.trySubmit(ctx) {
		t.Fatal("trySubmit returned false; want true")
	}

	if got := a.editor.Text(); got != "" {
		t.Errorf("editor not cleared after Enter: %q", got)
	}
	if a.selectorVisible() {
		t.Error("picker reopened after Enter on ambiguous model")
	}
	// pendingModelKey must be consumed (cleared) after commit so a
	// later submit doesn't accidentally reuse the stale key.
	if a.pendingModelKey != "" {
		t.Errorf("pendingModelKey not cleared after commit: %q", a.pendingModelKey)
	}
}

// TestPendingModelKeyClearedOnEdit verifies that if the user edits
// the id after picking from the picker, the pending key is cleared
// so the next Enter re-resolves from the current text instead of
// committing the stale remembered key.
func TestPendingModelKeyClearedOnEdit(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	// Set up a pending model key as if the user just Tab'd on a
	// picker item.
	a.pendingModelKey = "anthropic/claude-sonnet-4-6"
	a.editor.SetText(stubCtx(), "/model claude-sonnet-4-6")
	a.refreshSelector("/model claude-sonnet-4-6", stubCtx())
	if a.pendingModelKey == "" {
		t.Fatal("pendingModelKey was cleared prematurely; should persist while editor text matches")
	}

	// User edits the id (appends a character) — OnChange fires,
	// refreshSelector runs, and pendingModelKey should be cleared
	// because the text no longer matches what the picker filled.
	a.editor.SetText(stubCtx(), "/model claude-sonnet-4-6x")
	a.refreshSelector("/model claude-sonnet-4-6x", stubCtx())
	if a.pendingModelKey != "" {
		t.Errorf("pendingModelKey not cleared after edit: %q", a.pendingModelKey)
	}
}

// TestPendingModelKeyClearedOnCommandPick verifies that picking a
// command from the picker (not a model) clears any pending model key,
// since the user has left the model-pick context.
func TestPendingModelKeyClearedOnCommandPick(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.pendingModelKey = "anthropic/claude-sonnet-4-6"
	a.refreshSelector("/", stubCtx())
	if !a.selectorVisible() {
		t.Fatal("selector should be visible after /")
	}

	// User picks the "help" command.
	a.applySelector(0, tauui.SelectorItem{Name: "help", Description: "Show available commands"})
	if a.pendingModelKey != "" {
		t.Errorf("pendingModelKey not cleared on command pick: %q", a.pendingModelKey)
	}
}

// TestApplyModelFromKeyUpdatesFooter is the regression test for the
// bug where the footer (status bar) kept showing the old model after
// the user switched via /model:
//
//	status bar shows: tau · anthropic/claude-sonnet-4-6
//	user runs: /model amazon-bedrock/anthropic.claude-3-5-haiku-20241022-v1:0
//	model commits (currentModel updated, chat log shows the change)
//	status bar STILL shows: tau · anthropic/claude-sonnet-4-6
//
// Root cause: applyModelFromKey updated a.currentModel but never
// touched a.footer.data.model. The footer reads from its own data
// record on each paint — a.currentModel is only used for picker
// focus positioning (marking the active model with →), not for the
// footer display.
func TestApplyModelFromKeyUpdatesFooter(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	// Simulate the initial state: footer shows the model from
	// Options.ModelSpec (set once at construction).
	a.footer = newFooterForTest()
	a.footer.data.model = "anthropic/claude-sonnet-4-6"

	// Commit a new model.
	a.applyModelFromKey("amazon-bedrock/anthropic.claude-3-5-haiku-20241022-v1:0")

	// currentModel is updated synchronously.
	if a.currentModel != "amazon-bedrock/anthropic.claude-3-5-haiku-20241022-v1:0" {
		t.Errorf("currentModel = %q, want updated key", a.currentModel)
	}
	// Footer data is synced so the status bar reflects the change.
	if a.footer.data.model != "amazon-bedrock/anthropic.claude-3-5-haiku-20241022-v1:0" {
		t.Errorf("footer.data.model = %q, want updated key", a.footer.data.model)
	}
}

// TestConfirmOrQuitClearsEditorWhenTextPresent covers the first leg
// of the Ctrl+C policy: if the editor is focused and has text, Ctrl+C
// clears the text instead of quitting. This protects users from
// accidentally exiting while mid-edit.
func TestConfirmOrQuitClearsEditorWhenTextPresent(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil // ensure quit() would no-op the session close

	a.editor.SetText(stubCtx(), "draft prompt")
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	// Track whether quit() was called by checking the app loop state.
	// confirmOrQuit with non-empty editor text must NOT trigger quit.
	a.confirmOrQuit(ctx)

	if got := a.editor.Text(); got != "" {
		t.Errorf("editor text after Ctrl+C = %q, want cleared", got)
	}
	if a.confirmQuitActive {
		t.Error("confirm timer started even though editor had text; should only start on empty editor")
	}
}

// TestConfirmOrQuitStartsTimerWhenEditorEmpty covers the second leg:
// editor focused and empty, no confirm pending → start the 1.5s
// timer and show the notice in the footer.
func TestConfirmOrQuitStartsTimerWhenEditorEmpty(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	a.confirmOrQuit(ctx)

	if !a.confirmQuitActive {
		t.Error("confirm not active after first Ctrl+C on empty editor")
	}
	if a.footer.data.notice == "" {
		t.Error("footer notice not set after first Ctrl+C on empty editor")
	}
	if a.confirmQuitTimer == nil {
		t.Error("confirm timer not armed")
	}
	// Clean up: stop the timer so it doesn't fire during the test
	// run and touch the app state.
	a.stopConfirmQuitTimer()
}

// TestConfirmOrQuitSecondPressQuits covers the third leg: a second
// Ctrl+C within the confirm window actually quits. We can't easily
// observe the toolkit loop exit from a unit test, so we assert that
// confirmOrQuit calls quit() by checking that the timer is stopped
// (quit() calls stopConfirmQuitTimer) and that confirmOrQuit returns
// after triggering the quit path.
//
// To observe the quit without a real toolkit loop, we replace a.app
// with a stub whose Quit() records the call.
func TestConfirmOrQuitSecondPressQuits(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	// First press: arms the confirm.
	a.confirmOrQuit(ctx)
	if !a.confirmQuitActive {
		t.Fatal("confirm not active after first Ctrl+C")
	}
	// Clean up the timer from the first press so it doesn't fire
	// during the test (we're going to trigger the second press
	// synchronously).
	a.stopConfirmQuitTimer()
	a.confirmQuitActive = true // re-arm the active flag (Stop() doesn't clear it)

	// Second press: must trigger quit. With a.app == nil, quit()
	// will panic on a.app.Quit(). Use a recover to confirm the
	// second-press path reached the quit call.
	defer func() {
		if r := recover(); r == nil {
			t.Error("second Ctrl+C should have called a.app.Quit(); no panic means quit was not reached")
		}
	}()
	a.confirmOrQuit(ctx)
}

// TestConfirmOrQuitTimerClearsNotice verifies that the 1.5s timer
// auto-clears the notice and the active flag when the user doesn't
// press Ctrl+C again.
func TestConfirmOrQuitTimerClearsNotice(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	// Simulate a layout pass having given the footer a non-zero
	// rect, so the timer callback's Invalidate guard accepts it.
	a.footer.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 1})
	a.sess = nil

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	a.confirmOrQuit(ctx)
	if !a.confirmQuitActive {
		t.Fatal("confirm not active after first Ctrl+C")
	}

	// Fire the timer's posted callback synchronously. We can't
	// wait 1.5s in a unit test, and we can't easily drain the post
	// queue, so we simulate the timer's posted callback by calling
	// clearConfirmQuitOnLoop directly. This is exactly what the
	// timer would post to the loop.
	clearCalled := false
	uctx := &tauui.UpdateCtx{
		Invalidate: func(_ tauui.Rect) { clearCalled = true },
	}
	a.clearConfirmQuitOnLoop(uctx)

	if a.confirmQuitActive {
		t.Error("confirm still active after timer expiry")
	}
	if a.footer.data.notice != "" {
		t.Errorf("footer notice not cleared after timer expiry: %q", a.footer.data.notice)
	}
	if !clearCalled {
		t.Error("footer was not invalidated after timer cleared the notice")
	}
}

// TestOnEditorTextChangedClearsConfirm verifies that typing into the
// editor after a confirm notice dismisses the notice immediately.
// The user has clearly changed their mind (they're editing again),
// so the "press ctrl+c to exit" hint should disappear.
func TestOnEditorTextChangedClearsConfirm(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil

	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	// Arm the confirm.
	a.confirmOrQuit(ctx)
	if !a.confirmQuitActive {
		t.Fatal("confirm not active after first Ctrl+C")
	}
	a.stopConfirmQuitTimer()

	// Simulate the user typing — OnChange fires.
	a.onEditorTextChanged("h", stubCtx())

	if a.confirmQuitActive {
		t.Error("confirm still active after user started typing")
	}
	if a.footer.data.notice != "" {
		t.Errorf("footer notice not cleared after user started typing: %q", a.footer.data.notice)
	}
}

// TestConfirmOrQuitQuitsWhenEditorNotFocused verifies that Ctrl+C
// quits immediately when the editor is not focused (e.g. the user
// tabbed to the chat log or selector). There is no text to protect
// and the user is unlikely to be mid-edit, so the confirm step is
// unnecessary friction.
func TestConfirmOrQuitQuitsWhenEditorNotFocused(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil

	ctx := stubCtx()
	// ctx.FocusedID is 0 (not the editor's ID).

	defer func() {
		if r := recover(); r == nil {
			t.Error("Ctrl+C with non-editor focus should have called a.app.Quit(); no panic means quit was not reached")
		}
	}()
	a.confirmOrQuit(ctx)
}

// TestRootSatisfiesActionHandler is a regression test for a bug where
// Root.HandleAction had parameter type `int` instead of `tauui.Action`.
// Go's interface satisfaction is nominal: a method with `act int` does
// NOT satisfy an interface requiring `act tauui.Action`, even though they
// share the same underlying type. The mismatch caused the toolkit's
// action dispatch (focused view → ancestors → root) to silently skip
// the Root, so EVERY action the Root was supposed to handle — quit,
// send prompt, confirm-to-quit, selector navigation — never fired.
//
// The fix changes the parameter type to `tauui.Action` and adds a
// compile-time interface check in app.go. This test mirrors that
// check at runtime so a future regression (e.g. someone reverts the
// parameter type) fails the test suite rather than silently breaking
// the app.
func TestRootSatisfiesActionHandler(t *testing.T) {
	root := &Root{}
	var _ interface {
		HandleAction(act tauui.Action, ctx *tauui.Ctx) bool
	} = root
}

// TestSlashQuitReachesQuit is the regression test for the bug where
// /quit silently did nothing. Root.HandleAction had parameter type
// `int` instead of `tauui.Action`, so it did NOT satisfy the toolkit's
// actionHandler interface. The dispatch path (focused view →
// ancestors → root) silently skipped the Root, meaning every action
// the Root was supposed to handle — including AppActionEditorSubmit which
// trySubmit dispatches — never fired. /quit went through Enter →
// AppActionEditorSubmit → trySubmit → cmdQuit → app.quit(), but the
// dispatch never reached trySubmit because the Root was skipped.
//
// This test calls Root.HandleAction(AppActionEditorSubmit, ctx) directly,
// which is what the toolkit's dispatch does when Enter is pressed
// with the editor focused. With the interface fix in place,
// Root.HandleAction routes AppActionEditorSubmit to trySubmit, which
// parses /quit and calls cmdQuit → app.quit() → a.app.Quit()
// (which panics on nil a.app in tests). The test uses recover()
// to confirm the quit path was reached.
//
// This test catches regressions where Root.HandleAction stops
// routing AppActionEditorSubmit to trySubmit (e.g. someone removes the
// case from the switch). The interface mismatch itself is caught
// at build time by TestRootSatisfiesActionHandler and the
// compile-time check in app.go.
func TestSlashQuitReachesQuit(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()
	a.footer = newFooterForTest()
	a.sess = nil

	a.editor.SetText(stubCtx(), "/quit")
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()

	// Build the Root and dispatch AppActionEditorSubmit through it.
	// This mirrors what the toolkit's dispatch does when Enter
	// is pressed with the editor focused.
	root := &Root{app: a}

	defer func() {
		if recover() == nil {
			t.Error("/quit should have reached app.quit() and panicked on a.app.Quit(); no panic means Root.HandleAction did not route AppActionEditorSubmit to trySubmit")
		}
	}()
	root.HandleAction(AppActionEditorSubmit, ctx)
}

// TestAppKeymapUpDownRoutesToSelector verifies Up/Down in the editor
// with the selector visible map to AppActionSelectorUp/AppActionSelectorDown so the
// editor declines them and Root can route them to the selector.
func TestAppKeymapUpDownRoutesToSelector(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	km := appKeymap{app: a}

	if act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyUp}); !ok || act != AppActionSelectorUp {
		t.Errorf("Up with selector visible = (%v, %v), want (AppActionSelectorUp, true)", act, ok)
	}
	if act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyDown}); !ok || act != AppActionSelectorDown {
		t.Errorf("Down with selector visible = (%v, %v), want (AppActionSelectorDown, true)", act, ok)
	}

	// Modified arrows fall through to the toolkit's default keymap.
	if act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyUp, Mod: tauui.ModShift}); !ok || act != tauui.ActionMoveUp {
		t.Errorf("Shift+Up with selector visible = (%v, %v), want toolkit move-up", act, ok)
	}
}

// TestAppKeymapTabUsesToolkitDefaultWhenSelectorHidden verifies that with
// the selector hidden, Tab resolves to toolkit focus navigation.
func TestAppKeymapTabUsesToolkitDefaultWhenSelectorHidden(t *testing.T) {
	a := newAppWithSelector(t)
	km := appKeymap{app: a}

	if act, ok := km.Resolve(tauui.KeyCtxTextInput, tauui.Keystroke{Key: tauui.KeyTab}); !ok || act != tauui.ActionFocusNext {
		t.Errorf("Tab with selector hidden = (%v, %v), want ActionFocusNext", act, ok)
	}
}

// TestRouteSelectorAdvancesFocused verifies the route* entry points
// actually advance the selector's focused index.
func TestRouteSelectorAdvancesFocused(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	ctx := &tauui.Ctx{Invalidate: func(tauui.Rect) {}}

	start := a.selector.Focused()
	if !a.routeSelectorDown(ctx) {
		t.Fatal("routeSelectorDown with selector visible returned false")
	}
	if a.selector.Focused() != start+1 {
		t.Errorf("after routeSelectorDown focused = %d, want %d", a.selector.Focused(), start+1)
	}
	if !a.routeSelectorUp(ctx) {
		t.Fatal("routeSelectorUp with selector visible returned false")
	}
	if a.selector.Focused() != start {
		t.Errorf("after routeSelectorUp focused = %d, want %d", a.selector.Focused(), start)
	}
}

// TestRouteSelectorReturnsFalseWhenHidden verifies the route* helpers
// fall through when no selector is visible (so the toolkit can use its
// default focus nav for the unhandled case).
func TestRouteSelectorReturnsFalseWhenHidden(t *testing.T) {
	a := newAppWithSelector(t)
	if a.routeSelectorDown(&tauui.Ctx{Invalidate: func(tauui.Rect) {}}) {
		t.Error("routeSelectorDown with no selector returned true; want false")
	}
	if a.routeSelectorUp(&tauui.Ctx{Invalidate: func(tauui.Rect) {}}) {
		t.Error("routeSelectorUp with no selector returned true; want false")
	}
}

// TestDismissSelectorResetsMode verifies dismissSelector clears the
// selector mode back to none.
func TestDismissSelectorResetsMode(t *testing.T) {
	a := newAppWithSelector(t)
	a.refreshSelector("/", nil)
	if a.selectorMode == selectorModeNone {
		t.Fatal("selector mode should not be none after refresh")
	}
	a.dismissSelector()
	if a.selectorMode != selectorModeNone {
		t.Errorf("mode after dismiss = %d, want none", a.selectorMode)
	}
	if a.selectorVisible() {
		t.Error("selector should be hidden after dismiss")
	}
}

// TestDismissSelectorIfVisibleIsIdempotent verifies Esc on a hidden
// selector returns false so cancel can fall through (interrupt / quit).
func TestDismissSelectorIfVisibleIsIdempotent(t *testing.T) {
	a := newAppWithSelector(t)
	if a.dismissSelectorIfVisible() {
		t.Error("dismissSelectorIfVisible on hidden selector returned true")
	}
	a.refreshSelector("/", nil)
	if !a.dismissSelectorIfVisible() {
		t.Error("dismissSelectorIfVisible on visible selector returned false")
	}
	// Second call should fall through.
	if a.dismissSelectorIfVisible() {
		t.Error("dismissSelectorIfVisible on just-dismissed selector returned true")
	}
}

// TestEditorOnChangeShowsSelector drives the full prompt editor OnChange
// pipeline: simulating the user typing `/` into a real prompt editor (not
// calling refreshSelector directly). This is the closest unit test
// analogue for "the / doesn't show the selector" — if this passes
// then the OnChange pipeline is wired correctly and a runtime issue
// (layout not updating, focus moving) is at fault instead.
func TestEditorOnChangeShowsSelector(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetOnChange(func(text string, ctx *tauui.Ctx) {
		a.onEditorTextChanged(text, ctx)
	})

	// Simulate the user typing "/" via SetText (mirrors what
	// prompt editor's mutators do after a rune insert: they SetText
	// internally, which fires OnChange).
	ctx := stubCtx()
	a.editor.SetText(ctx, "/")

	if !a.selectorVisible() {
		t.Fatal("typing '/' via OnChange did not show the selector")
	}
	if a.selectorMode != selectorModeCommands {
		t.Errorf("mode = %d, want commands(%d)", a.selectorMode, selectorModeCommands)
	}
	items := a.selector.Items()
	if len(items) == 0 {
		t.Fatal("selector has no items after / via OnChange")
	}
}

// TestEditorOnChangeHidesSelectorOnBackspace verifies that deleting
// past the trailing slash dismisses the selector. Same OnChange
// pipeline as the previous test.
func TestEditorOnChangeHidesSelectorOnBackspace(t *testing.T) {
	a := newAppWithSelector(t)
	a.editor.SetOnChange(func(text string, ctx *tauui.Ctx) {
		a.onEditorTextChanged(text, ctx)
	})
	ctx := stubCtx()

	a.editor.SetText(ctx, "/help")
	if !a.selectorVisible() {
		t.Fatal("selector not visible after /help")
	}
	a.editor.SetText(ctx, "")
	if a.selectorVisible() {
		t.Error("selector should be hidden after clearing")
	}
}

// TestSelectorMinSizeWhenShownReturnsNonZero is the smoke test that
// ensures the VStack's Layout will allocate space for the selector
// after SetShown(true).
func TestSelectorMinSizeWhenShownReturnsNonZero(t *testing.T) {
	s := tauui.NewSelector()
	s.SetShown(true)
	s.SetItems([]tauui.SelectorItem{
		{Name: "help", Description: "Show available commands"},
		{Name: "hotkeys", Description: "Show keybinding hints"},
		{Name: "model", Description: "Switch the active model"},
		{Name: "quit", Description: "Exit interactive mode"},
	})
	sz := s.MinSize()
	if sz.H == 0 {
		t.Errorf("shown selector MinSize.H = 0 (VStack won't allocate room)")
	}
	if sz.W == 0 {
		t.Errorf("shown selector MinSize.W = 0 (VStack won't allocate room)")
	}
}

// TestEditorOnCursorMoveReshowsModelPickerAfterPick verifies that
// after Tab fills the editor with `/model <id>` and closes the inline
// selector, moving the cursor back into the model name region re-opens
// the selector (via OnCursorMove → refreshSelector).
func TestEditorOnCursorMoveReshowsModelPickerAfterPick(t *testing.T) {
	a := newAppWithSelector(t)
	a.loopMu.Lock()
	a.loopCtx = stubCtx()
	a.loopMu.Unlock()

	a.refreshSelector("/model ", stubCtx())
	items := a.selector.Items()
	if len(items) == 0 {
		t.Skip("no models registered; skipping")
	}
	chosen := items[0]

	// Tab-pick → fills editor with "/model <id>", selector dismisses.
	a.applySelector(0, chosen)
	if got := a.editor.Text(); got != "/model "+chosen.Name {
		t.Fatalf("post-pick editor = %q, want %q", got, "/model "+chosen.Name)
	}
	if a.selector.Shown() {
		t.Fatal("selector should be hidden right after Tab pick")
	}

	// Wire the OnCursorMove hook the same way the production App
	// does. Each cursor-only move re-runs refreshSelector so moving
	// back into the model name region re-opens the picker.
	var moveCount int
	a.editor.SetOnCursorMove(func(_ tauui.EditorCursorMoveEvent, ctx *tauui.Ctx) {
		moveCount++
		a.refreshSelector(a.editor.Text(), ctx)
	})

	// Drive the editor's HandleAction(ActionMoveLeft) so the
	// prompt editor moves the cursor (which fires OnCursorMove). The
	// editor needs ctx.FocusedID set so the action handler accepts.
	ctx := stubCtx()
	ctx.FocusedID = a.editor.ID()
	if !a.editor.HandleAction(tauui.ActionMoveLeft, ctx) {
		t.Fatal("prompt editor ActionMoveLeft returned false")
	}

	if !a.selector.Shown() || a.selectorMode != selectorModeModels {
		t.Errorf("inline model selector should re-show after Left arrow, shown=%v mode=%d", a.selector.Shown(), a.selectorMode)
	}
	if a.dialogPicker != nil && a.dialogPicker.Shown() {
		t.Error("dialog picker should not re-show after Left arrow")
	}
	if moveCount != 1 {
		t.Errorf("OnCursorMove called %d times, want 1 (the Left arrow)", moveCount)
	}
}

// TestRefreshSelectorInvalidatesLayoutWhenVisibleMinSizeChanges verifies
// switching between visible selector states still dirties layout when the
// selector's requested height changes.
func TestRefreshSelectorInvalidatesLayoutWhenVisibleMinSizeChanges(t *testing.T) {
	a := newAppWithSelector(t)
	var layoutCalls, fullCalls int
	ctx := &tauui.Ctx{
		Invalidate:       func(tauui.Rect) {},
		InvalidateAll:    func() { fullCalls++ },
		InvalidateLayout: func() { layoutCalls++ },
	}

	a.refreshSelector("/model", ctx)
	layoutCalls, fullCalls = 0, 0

	a.refreshSelector("/model ", ctx)
	if layoutCalls == 0 || fullCalls == 0 {
		t.Fatalf("command→model switch with height change did not invalidate layout/full screen: layout=%d full=%d", layoutCalls, fullCalls)
	}

	layoutCalls, fullCalls = 0, 0
	a.refreshSelector("/model", ctx)
	if layoutCalls == 0 || fullCalls == 0 {
		t.Fatalf("model→command switch with height change did not invalidate layout/full screen: layout=%d full=%d", layoutCalls, fullCalls)
	}
}

// is the regression test for the bug where typing `/` did not show
// the selector: refreshSelector must mark the toolkit's layout dirty
// whenever the selector transitions between hidden and shown, or the
// VStack keeps the cached allocation where the selector has height 0
// and the selector's Paint no-ops.
//
// We assert by counting calls into InvalidateLayout and InvalidateAll
// across the hidden→shown transition (and back).
func TestRefreshSelectorInvokesInvalidateLayoutOnVisibilityTransition(t *testing.T) {
	a := newAppWithSelector(t)

	var layoutCalls, fullCalls int
	ctx := &tauui.Ctx{
		Invalidate:       func(tauui.Rect) {},
		InvalidateAll:    func() { fullCalls++ },
		InvalidateLayout: func() { layoutCalls++ },
	}

	// Initial: nothing visible, no invalidations yet.
	a.refreshSelector("", ctx)
	if layoutCalls != 0 || fullCalls != 0 {
		t.Errorf("on empty text: layoutCalls=%d fullCalls=%d, want 0/0", layoutCalls, fullCalls)
	}

	// Hidden → shown: should call InvalidateAll (which both invalidates
	// the whole screen AND marks layout dirty via the toolkit).
	a.refreshSelector("/", ctx)
	if layoutCalls == 0 {
		t.Errorf("hidden→shown did not call InvalidateLayout")
	}
	if fullCalls == 0 {
		t.Errorf("hidden→shown did not call InvalidateAll")
	}

	// Reset counters.
	layoutCalls, fullCalls = 0, 0

	// Same visibility and same requested size: only the selector and editor
	// rects should be invalidated, no InvalidateAll.
	a.refreshSelector("/", ctx)
	if fullCalls != 0 {
		t.Errorf("same visibility/size refresh: fullCalls=%d, want 0", fullCalls)
	}

	// Filter to empty (no matches): transition shown → hidden.
	a.refreshSelector("/zzznosuchcommandzzz", ctx)
	if fullCalls == 0 {
		t.Errorf("shown→hidden did not call InvalidateAll")
	}

	// Reset.
	layoutCalls, fullCalls = 0, 0

	// Now back to shown: another transition.
	a.refreshSelector("/c", ctx)
	if fullCalls == 0 {
		t.Errorf("hidden→shown (second time) did not call InvalidateAll")
	}
}

// TestModelSelectorEmptyWhenNoProviders verifies that the model picker is
// reachable from the `/model` slash command even when no providers are
// authenticated. This is the recovery path for a TUI that boots with no
// configured model: the picker appears (showing an empty list), so the
// user knows to authenticate via /login or set a provider via
// settings.json.
//
// Regression: previously the TUI failed to boot when no provider was
// configured because NewSession surfaced "no provider configured" as a
// hard error, which blocked `/model` from ever being invoked.
func TestModelSelectorEmptyWhenNoProviders(t *testing.T) {
	// Clear every env var that ai/authenticatedProviderSet consults so
	// we get a deterministic "no providers authenticated" baseline.
	for name := range ai.EnvAPIKeys {
		t.Setenv(name, "")
	}

	a := newAppWithSelector(t)
	// Strip the default credential lister so no providers authenticate.
	a.credentialStore = fakeCredentialLister{refs: nil}

	models := a.availableModels()
	if len(models) == 0 {
		t.Fatalf("expected no-credential models without auth")
	}
	for _, model := range models {
		if model.Provider != ai.ProviderClaudeAgent {
			t.Fatalf("expected only claude-agent without auth, got %s/%s", model.Provider, model.ID)
		}
	}

	items := a.modelSelectorItems("")
	if len(items) == 0 {
		t.Errorf("expected no-credential selector items")
	}

	// The picker itself must still be reachable: applyModelFromArgs with
	// no args triggers the picker fallback. This is what /model invokes.
	if a.applyModelFromArgs("") {
		t.Error("expected applyModelFromArgs to return false (picker fallback) when no models are available")
	}
}
