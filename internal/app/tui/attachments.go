package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

const pendingAttachmentsWidgetKey = "attachments"

type pendingAttachment struct {
	kind string
	name string
}

func (a *App) updatePendingAttachments(text string, ctx *tauui.Ctx) {
	if a == nil || a.widgetsAbove == nil {
		return
	}
	records := a.pendingAttachments(text)
	if len(records) == 0 {
		a.setWidget(pendingAttachmentsWidgetKey, nil, "above")
		return
	}
	a.setWidget(pendingAttachmentsWidgetKey, []string{pendingAttachmentsText(records)}, "above")
	_ = ctx
}

func (a *App) clearPendingAttachments(ctx *tauui.Ctx) {
	if a == nil || a.widgetsAbove == nil {
		return
	}
	a.setWidget(pendingAttachmentsWidgetKey, nil, "above")
	_ = ctx
}

func pendingAttachmentsText(records []pendingAttachment) string {
	parts := make([]string, 0, len(records))
	for _, record := range records {
		parts = append(parts, fmt.Sprintf("%s %s", attachmentKindIcon(record.kind), record.name))
	}
	return "Attached: " + strings.Join(parts, ", ")
}

func attachmentKindIcon(kind string) string {
	if kind == "image" {
		return "🖼"
	}
	return "📎"
}

func (a *App) pendingAttachments(text string) []pendingAttachment {
	root := a.fileSelectorRoot()
	paths := pendingAtPaths(text, func(path string) string {
		return a.resolveAttachmentPath(path, root)
	})
	out := make([]pendingAttachment, 0, len(paths))
	for _, path := range paths {
		kind := "document"
		if isImagePath(path) {
			kind = "image"
		}
		out = append(out, pendingAttachment{kind: kind, name: filepath.Base(path)})
	}
	return out
}

func pendingAtPaths(text string, resolve func(string) string) []string {
	var out []string
	seen := map[string]struct{}{}
	for i := 0; i < len(text); {
		if text[i] != '@' || (i > 0 && (text[i-1] == '\\' || !isFileRefBoundary(rune(text[i-1])))) {
			i++
			continue
		}
		j := i + 1
		for j < len(text) && !isFileRefBoundary(rune(text[j])) && text[j] != '\n' {
			j++
		}
		path := text[i+1 : j]
		if path == "" {
			i++
			continue
		}
		full := resolve(path)
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(full); err == nil {
				full = abs
			}
			if _, ok := seen[full]; !ok {
				seen[full] = struct{}{}
				out = append(out, full)
			}
		}
		i = j
	}
	return out
}
