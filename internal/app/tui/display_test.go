package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
	tauui "github.com/losinggeneration/tau/tui"
)

// A 1x1 transparent PNG. Small enough to keep the fixture inline and real
// enough that the image path does not bail out before rendering.
const testPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func renderedImageLines(t *testing.T, configure func(*ChatLog)) []string {
	t.Helper()
	tauui.SetTerminalCapabilities(tauui.TerminalCapabilities{Images: tauui.ImageProtocolKitty, TrueColor: true})
	t.Cleanup(tauui.ResetTerminalCapabilities)

	conv := NewConversationModel()
	conv.AddImage(RoleAssistant, ImageComponent{
		Data:     testPNGBase64,
		MIMEType: "image/png",
		Width:    1,
		Height:   1,
		AltText:  "pixel.png",
	})
	log := newTestChatLog(t, conv)
	if configure != nil {
		configure(log)
	}
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 60, H: 100})
	waitForChatLog(t, log)
	log.mu.Lock()
	defer log.mu.Unlock()
	return renderLineTexts(log.lines)
}

// showImages=false must fall back to the text placeholder rather than dropping
// the component: the user still needs to know an image was there.
func TestHideImagesRendersThePlaceholder(t *testing.T) {
	hidden := renderedImageLines(t, func(log *ChatLog) { log.SetImageDisplay(false) })

	joined := strings.Join(hidden, "\n")
	if strings.Contains(joined, testPNGBase64) {
		t.Fatalf("image data still rendered with showImages=false:\n%s", joined)
	}
	for _, line := range hidden {
		if tauui.IsImageLine(line) {
			t.Fatalf("image protocol line emitted with showImages=false:\n%s", joined)
		}
	}
	if !strings.Contains(joined, "pixel.png") {
		t.Fatalf("placeholder should name the image, got:\n%s", joined)
	}
}

// The default must stay "show". A regression here would silently disable
// images for every user who has never written the setting.
func TestImagesShownByDefault(t *testing.T) {
	shown := renderedImageLines(t, nil)
	hidden := renderedImageLines(t, func(log *ChatLog) { log.SetImageDisplay(false) })

	if strings.Join(shown, "\n") == strings.Join(hidden, "\n") {
		t.Fatalf("SetImageDisplay(false) changed nothing; the default is not rendering the image:\n%#v", shown)
	}
}

func TestSetImageDisplayInvalidatesRenderCache(t *testing.T) {
	conv := NewConversationModel()
	conv.AddImage(RoleAssistant, ImageComponent{Data: testPNGBase64, MIMEType: "image/png", AltText: "p.png"})
	log := newTestChatLog(t, conv)
	log.Layout(tauui.Rect{X: 0, Y: 0, W: 40, H: 20})
	waitForChatLog(t, log)

	log.SetImageDisplay(false)

	log.mu.Lock()
	cached := len(log.renderCache)
	log.mu.Unlock()
	if cached != 0 {
		t.Fatalf("render cache holds %d entries after an image-display change, want it dropped", cached)
	}
}

// quietStartup now suppresses the Header region, since that is where the
// banner lives. Covered by TestQuietStartupSuppressesTheHeader.

// queueModeSetter records what the session was told, so the test asserts on the
// forwarded value rather than on agent internals.
type queueModeSetter struct {
	sdk.Session
	steering  string
	followUp  string
	failWith  error
	callCount int
}

func (q *queueModeSetter) SetSteeringMode(mode string) error {
	q.callCount++
	if q.failWith != nil {
		return q.failWith
	}
	q.steering = mode
	return nil
}

func (q *queueModeSetter) SetFollowUpMode(mode string) error {
	q.callCount++
	if q.failWith != nil {
		return q.failWith
	}
	q.followUp = mode
	return nil
}

// ai.Settings and agent.Config both have SteeringMode/FollowUpMode fields, but
// nothing copied one into the other — configuring them did nothing at all.
func TestQueueModeSettingsReachTheSession(t *testing.T) {
	a := newTestApp(t)
	setter := &queueModeSetter{}
	a.sess = setter

	a.applyDisplaySettings(&ai.Settings{SteeringMode: "all", FollowUpMode: "one-at-a-time"})

	if setter.steering != "all" {
		t.Errorf("steering mode = %q, want the configured %q", setter.steering, "all")
	}
	if setter.followUp != "one-at-a-time" {
		t.Errorf("follow-up mode = %q, want the configured %q", setter.followUp, "one-at-a-time")
	}
}

func TestNewAppliesDefaultSteeringModeAllWhenUnset(t *testing.T) {
	setter := &queueModeSetter{Session: &fakeSession{}}

	a, err := New(Options{Session: setter})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.chat.Close() })

	if setter.steering != "all" {
		t.Fatalf("default steering mode = %q, want all", setter.steering)
	}
}

// An unset mode must leave the session's own default alone rather than pushing
// the empty string at it.
func TestUnsetQueueModesAreNotForwarded(t *testing.T) {
	a := newTestApp(t)
	setter := &queueModeSetter{}
	a.sess = setter

	a.applyDisplaySettings(&ai.Settings{})

	if setter.callCount != 0 {
		t.Fatalf("empty settings made %d queue-mode calls, want none", setter.callCount)
	}
}

// A typo in settings.json should be visible, not swallowed.
func TestInvalidQueueModeIsReported(t *testing.T) {
	a := newTestApp(t)
	a.sess = &queueModeSetter{failWith: errBadQueueMode{}}

	a.applyDisplaySettings(&ai.Settings{SteeringMode: "sometimes"})

	found := false
	for _, m := range a.conv.Messages() {
		if strings.Contains(m.Text, "steeringMode") && strings.Contains(m.Text, "sometimes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("invalid steeringMode was not reported: %#v", a.conv.Messages())
	}
}

type errBadQueueMode struct{}

func (errBadQueueMode) Error() string { return "unknown queue mode" }

// End-to-end through a real settings file, covering the *bool default for
// showImages: an absent key means enabled.
func TestShowImagesDefaultsToEnabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body, _ := json.Marshal(map[string]any{"theme": "tau"})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	settings, err := ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settings.Terminal.ShowImages != nil {
		t.Fatalf("absent showImages should stay nil, got %v", *settings.Terminal.ShowImages)
	}
	if !settingsShowImages(settings) {
		t.Fatal("absent showImages must resolve to enabled")
	}

	body, _ = json.Marshal(map[string]any{"terminal": map[string]any{"showImages": false}})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	settings, err = ai.LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if settingsShowImages(settings) {
		t.Fatal("explicit showImages=false must resolve to disabled")
	}
}
