package tui

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/losinggeneration/rovel"
)

const promptEditorTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestPromptEditorRendersMultiplePastedImagePreviews(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	t.Cleanup(ResetTerminalCapabilities)

	ed := NewPromptEditor()
	png, err := base64.StdEncoding.DecodeString(promptEditorTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	ed.SetInlineImagePreview("@pasted-image.png", png, "image/png")
	ed.SetInlineImagePreview("@pasted-image-2.png", png, "image/png")
	ed.SetText(nil, "@pasted-image.png\n@pasted-image-2.png")
	ed.Layout(Rect{X: 0, Y: 0, W: 40, H: 7})
	d := &ansiRecordingDrawer{}
	ed.Paint(d, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})

	count := 0
	for _, op := range d.raw {
		if strings.Contains(op.text, "a=T") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("rendered %d image previews, want 2: raw=%#v", count, d.raw)
	}
}

func TestPromptEditorRemovesPastedImagePreviewWhenMarkerIsRemoved(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	t.Cleanup(ResetTerminalCapabilities)

	ed := NewPromptEditor()
	png, err := base64.StdEncoding.DecodeString(promptEditorTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	ed.SetInlineImagePreview("@pasted-image.png", png, "image/png")
	ed.SetText(nil, "@pasted-image.png")
	ed.Layout(Rect{X: 0, Y: 0, W: 40, H: 7})
	ed.Paint(&ansiRecordingDrawer{}, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})

	ed.SetText(nil, "")
	d := &ansiRecordingDrawer{}
	ed.Paint(d, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})

	for _, op := range d.raw {
		if strings.Contains(op.text, "a=d,d=I") {
			return
		}
	}
	t.Fatalf("clearing editor did not delete Kitty image: raw=%#v", d.raw)
}

func TestPromptEditorAnchorsPreviewAtMarkerColumn(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	t.Cleanup(ResetTerminalCapabilities)

	ed := NewPromptEditor()
	png, err := base64.StdEncoding.DecodeString(promptEditorTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	ed.SetInlineImagePreview("@pasted-image.png", png, "image/png")
	ed.SetText(nil, "look @pasted-image.png")
	ed.Layout(Rect{X: 0, Y: 0, W: 40, H: 7})
	d := &ansiRecordingDrawer{}
	ed.Paint(d, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})

	for _, op := range d.raw {
		if strings.Contains(op.text, "a=T") && op.pos.X == 6 {
			return
		}
	}
	t.Fatalf("preview was not anchored at marker column: raw=%#v", d.raw)
}

func TestPromptEditorPreviewUsesCurrentWidth(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	t.Cleanup(ResetTerminalCapabilities)

	ed := NewPromptEditor()
	ed.SetText(nil, "@pasted-image.png")
	png, err := base64.StdEncoding.DecodeString(promptEditorTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	ed.SetInlineImagePreview("@pasted-image.png", png, "image/png")
	ed.Layout(Rect{X: 0, Y: 0, W: 40, H: 7})
	d := &ansiRecordingDrawer{}
	ed.Paint(d, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})
	if !rawContains(d.raw, "c=4") {
		t.Fatalf("preview did not use wide editor width: raw=%#v", d.raw)
	}

	ed.Layout(Rect{X: 0, Y: 0, W: 12, H: 7})
	d = &ansiRecordingDrawer{}
	ed.Paint(d, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})
	if !rawContains(d.raw, "c=4") {
		t.Fatalf("preview did not resize to narrow editor width: raw=%#v", d.raw)
	}
}

func TestPromptEditorClickOpensFullscreenImageOverlay(t *testing.T) {
	SetTerminalCapabilities(TerminalCapabilities{Images: ImageProtocolKitty, TrueColor: true})
	t.Cleanup(ResetTerminalCapabilities)

	ed := NewPromptEditor()
	png, err := base64.StdEncoding.DecodeString(promptEditorTestPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	ed.SetInlineImagePreview("@pasted-image.png", png, "image/png")
	ed.SetText(nil, "@pasted-image.png")
	ed.Layout(Rect{X: 0, Y: 0, W: 40, H: 7})
	ed.Paint(&ansiRecordingDrawer{}, &Ctx{Theme: DefaultThemeModern(), FocusedID: ed.ID()})

	var overlayRoot View
	var overlayOpts rovel.OverlayOpts
	ctx := &Ctx{
		Theme:     DefaultThemeModern(),
		FocusedID: ed.ID(),
		ShowOverlay: func(opts rovel.OverlayOpts) *rovel.Overlay {
			overlayRoot = opts.Root
			overlayOpts = opts
			return nil
		},
	}
	if !ed.Handle(MouseEvent{X: 1, Y: 1, Button: MouseButtonLeft, Action: MousePress}, ctx) {
		t.Fatal("click on preview was not handled")
	}
	if overlayRoot == nil || !overlayOpts.Modal {
		t.Fatalf("preview click did not show modal overlay: root=%T opts=%#v", overlayRoot, overlayOpts)
	}
	if got := overlayOpts.Place.Resolve(overlayRoot, Size{W: 80, H: 24}); got != (Rect{W: 80, H: 24}) {
		t.Fatalf("overlay rect = %#v, want fullscreen", got)
	}

	overlayRoot.Layout(Rect{W: 80, H: 24})
	d := &ansiRecordingDrawer{}
	overlayRoot.Paint(d, ctx)
	if !rawContains(d.raw, "a=T") {
		t.Fatalf("overlay image did not render: raw=%#v", d.raw)
	}
	if deleteCount(d.raw) < 2 {
		t.Fatalf("overlay paint did not hide inline preview before rendering fullscreen image: raw=%#v", d.raw)
	}

	d = &ansiRecordingDrawer{}
	ed.Paint(d, ctx)
	if rawContains(d.raw, "a=T") {
		t.Fatalf("hidden inline preview rendered behind overlay: raw=%#v", d.raw)
	}

	dismissed := false
	ctx.DismissOverlay = func() *rovel.Overlay {
		dismissed = true
		return nil
	}
	if !overlayRoot.Handle(MouseEvent{X: 79, Y: 23, Button: MouseButtonLeft, Action: MousePress}, ctx) {
		t.Fatal("fullscreen overlay click was not handled")
	}
	if !dismissed {
		t.Fatal("fullscreen overlay click did not dismiss overlay")
	}
	overlayOpts.OnDismiss()
	d = &ansiRecordingDrawer{}
	ed.Paint(d, ctx)
	if !rawContains(d.raw, "a=d,d=I") {
		t.Fatalf("dismissed fullscreen image was not deleted: raw=%#v", d.raw)
	}
	if !rawContains(d.raw, "a=T") {
		t.Fatalf("inline preview was not restored after fullscreen dismiss: raw=%#v", d.raw)
	}
}

func deleteCount(ops []ansiDrawOp) int {
	count := 0
	for _, op := range ops {
		if strings.Contains(op.text, "a=d,d=I") {
			count++
		}
	}
	return count
}

func rawContains(ops []ansiDrawOp, s string) bool {
	for _, op := range ops {
		if strings.Contains(op.text, s) {
			return true
		}
	}
	return false
}
