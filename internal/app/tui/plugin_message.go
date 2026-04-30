package tui

import (
	"fmt"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"
)

// SendPluginUserMessage routes pi.sendUserMessage through the live TUI instead
// of the SDK's background drain path, so extension-triggered prompts render in
// the chat like user-submitted prompts.
func (a *App) SendPluginUserMessage(text, deliverAs string) error {
	if a == nil {
		return fmt.Errorf("tui not available")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	run := func() error {
		switch deliverAs {
		case "":
			a.submitWithDisplay(text, text, nil, nil, nil)
			return nil
		case "steer":
			a.enqueueSteering(text, nil, nil, a.loopContext())
			return nil
		case "followUp":
			a.enqueueFollowUp(text, a.loopContext())
			return nil
		default:
			return fmt.Errorf("invalid deliver_as %q", deliverAs)
		}
	}
	if a.app == nil {
		return run()
	}

	posted := make(chan error, 1)
	a.post(func(_ *tauui.UpdateCtx) {
		posted <- run()
	})

	return <-posted
}
