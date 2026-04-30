package tui

import (
	"strings"
	"testing"

	tauui "github.com/losinggeneration/tau/tui"
)

func headerText(t *testing.T, h *Header) string {
	t.Helper()
	out, _ := h.Render(120)
	return out
}

// §9: the header is region 1, not transcript content. It used to be rendered
// inside the chat log, which is why it had to be deleted on the first message
// rather than simply scrolled past.
func TestHeaderRendersTheStartupBanner(t *testing.T) {
	h := NewHeader("9.9.9")
	got := headerText(t, h)

	for _, want := range []string{"tau 9.9.9", "Enter send", "/ commands", "/help"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q:\n%s", want, got)
		}
	}
}

// The compact form must say how to expand; that line was absent entirely.
func TestCompactHeaderAdvertisesHowToExpand(t *testing.T) {
	h := NewHeader("1.0.0")
	if got := headerText(t, h); !strings.Contains(got, "startup help") {
		t.Errorf("compact header does not say how to expand:\n%s", got)
	}
}

// §9: compact joins hints with separators; expanded puts one per line and drops
// the expand hint.
func TestHeaderExpandsToOneHintPerLine(t *testing.T) {
	h := NewHeader("1.0.0")
	compact := headerText(t, h)

	if !h.SetExpanded(true) {
		t.Fatal("SetExpanded(true) reported no change")
	}
	expanded := headerText(t, h)

	if strings.Contains(compact, "\n   Enter send") {
		t.Errorf("compact form already lists hints one per line:\n%s", compact)
	}
	if !strings.Contains(expanded, "\n   Enter send") {
		t.Errorf("expanded form does not list hints one per line:\n%s", expanded)
	}
	if len(strings.Split(expanded, "\n")) <= len(strings.Split(compact, "\n")) {
		t.Error("expanding did not make the header taller")
	}
	if !strings.Contains(expanded, "tau 1.0.0") {
		t.Error("expanded form dropped the logo line")
	}

	if h.SetExpanded(true) {
		t.Error("SetExpanded reported a change when the value was unchanged")
	}
}

// The logo is bold accent; everything else is dim (§9).
func TestHeaderStylesTheLogoAsAccent(t *testing.T) {
	h := NewHeader("1.0.0")
	h.SetThemeTokens(func() themeTokens { return defaultThemeTokens() })
	h.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: h.MinSize().H})

	d := &recordingDrawer{}
	h.Paint(d, &tauui.Ctx{Theme: appTheme()})

	tokens := defaultThemeTokens()
	var sawLogo bool
	for _, txt := range d.texts {
		switch {
		case strings.Contains(txt.text, "tau 1.0.0"):
			sawLogo = true
			if txt.st.FG != tokens.Accent {
				t.Errorf("logo drawn in %v, want the accent token %v", txt.st.FG, tokens.Accent)
			}
			if txt.st.Attr&tauui.AttrBold == 0 {
				t.Error("logo is not bold")
			}
		case strings.TrimSpace(txt.text) != "":
			if txt.st.FG != tokens.Dim {
				t.Errorf("header line %q drawn in %v, want dim %v", txt.text, txt.st.FG, tokens.Dim)
			}
		}
	}
	if !sawLogo {
		t.Fatal("header never painted the logo line")
	}
}

// Hints come from the live keybinding registry, so a rebind shows up here
// instead of the default we shipped.
func TestHeaderHintsFollowTheKeybindingRegistry(t *testing.T) {
	h := NewHeader("1.0.0")
	h.SetHintProvider(func() []string { return []string{"ctrl+q quit", "F1 help"} })

	got := headerText(t, h)
	for _, want := range []string{"ctrl+q quit", "F1 help"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing provided hint %q:\n%s", want, got)
		}
	}
}

// Agreed deviation: the banner hides once the transcript has content, in both
// layout models.
func TestHeaderHidesOnceTheTranscriptHasContent(t *testing.T) {
	a := newLayoutTestApp(t)
	a.header = NewHeader("9.9.9")

	a.refreshHeaderVisibility()
	if a.header.Hidden() {
		t.Fatal("header hidden with an empty transcript")
	}
	if got := headerText(t, a.header); !strings.Contains(got, "tau 9.9.9") {
		t.Fatalf("empty-transcript header should show the banner:\n%s", got)
	}

	a.conv.AddUserMessage("hello")
	a.refreshHeaderVisibility()

	if !a.header.Hidden() {
		t.Error("header still visible after the first message")
	}
	if got := headerText(t, a.header); strings.TrimSpace(got) != "" {
		t.Errorf("hidden header still rendered rows:\n%q", got)
	}
	if got := a.header.MinSize().H; got != 0 {
		t.Errorf("hidden header reports height %d, want 0", got)
	}
}

// Clearing the transcript (a /new session) brings the banner back.
func TestHeaderReturnsWhenTheTranscriptIsCleared(t *testing.T) {
	a := newLayoutTestApp(t)
	a.header = NewHeader("9.9.9")

	a.conv.AddUserMessage("hello")
	a.refreshHeaderVisibility()
	if !a.header.Hidden() {
		t.Fatal("setup: header should be hidden")
	}

	a.conv.Clear()
	a.refreshHeaderVisibility()
	if a.header.Hidden() {
		t.Error("header did not return after the transcript was cleared")
	}
}

// §9 quiet startup: the built-in header has no visible rows.
func TestQuietStartupSuppressesTheHeader(t *testing.T) {
	h := NewHeader("9.9.9")
	if got := headerText(t, h); !strings.Contains(got, "tau 9.9.9") {
		t.Fatalf("setup: header should show the banner:\n%s", got)
	}

	h.SetQuietStartup(true)
	if got := headerText(t, h); strings.TrimSpace(got) != "" {
		t.Errorf("quiet startup still rendered header rows:\n%q", got)
	}
	if got := h.MinSize().H; got != 0 {
		t.Errorf("quiet header reports height %d, want 0", got)
	}
}

// §8a setHeader: an extension replaces the content, spacers included.
func TestCustomHeaderReplacesTheBuiltInContent(t *testing.T) {
	h := NewHeader("9.9.9")
	h.SetCustom([]string{"my project", "branch: main"})

	got := headerText(t, h)
	if strings.Contains(got, "tau 9.9.9") || strings.Contains(got, "startup help") {
		t.Errorf("custom header did not replace the built-in content:\n%s", got)
	}
	for _, want := range []string{"my project", "branch: main"} {
		if !strings.Contains(got, want) {
			t.Errorf("custom header missing %q:\n%s", want, got)
		}
	}

	h.SetCustom(nil)
	if got := headerText(t, h); !strings.Contains(got, "tau 9.9.9") {
		t.Errorf("clearing the custom header did not restore the built-in one:\n%s", got)
	}
}

// The transcript no longer carries the banner, so an empty chat log is empty.
func TestChatLogNoLongerRendersTheBanner(t *testing.T) {
	conv := NewConversationModel()
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 80, H: 20})
	waitForChatLog(t, log)

	if joined := strings.Join(log.Render(80), "\n"); strings.Contains(joined, "tau ") {
		t.Errorf("chat log still renders a startup banner:\n%s", joined)
	}
}

func TestToggleHeaderExpandedFlipsBothWays(t *testing.T) {
	a := newLayoutTestApp(t)
	a.header = NewHeader("1.0.0")

	a.ToggleHeaderExpanded()
	if !a.header.Expanded() {
		t.Fatal("first toggle did not expand the header")
	}
	a.ToggleHeaderExpanded()
	if a.header.Expanded() {
		t.Fatal("second toggle did not collapse the header")
	}
}
