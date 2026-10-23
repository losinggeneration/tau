package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/losinggeneration/tau/tui/clipboard"
)

// ClipboardWrite copies text using the shared local-first clipboard policy.
// Clipboard helpers run off the app loop so a slow helper cannot freeze input;
// done runs on the app loop with the result. Call after Start. Unlike
// Ctx.ClipboardWrite, done receives errors, so commands can report failure
// instead of claiming a successful copy.
func (a *App) ClipboardWrite(text string, done func(error)) {
	if a == nil || a.inner == nil {
		if done != nil {
			done(ErrUnsupported)
		}
		return
	}
	go func() {
		err := clipboard.WriteText(context.Background(), text, func(sequence string) error {
			result := make(chan error, 1)
			if err := a.Post(func(*UpdateCtx) { result <- a.inner.WriteTerminal(sequence) }); err != nil {
				return fmt.Errorf("write clipboard terminal sequence: %w", err)
			}
			if err := <-result; err != nil {
				return fmt.Errorf("write clipboard terminal sequence: %w", err)
			}
			return nil
		})
		if done == nil {
			done = reportClipboardError
		}
		if postErr := a.Post(func(*UpdateCtx) { done(err) }); postErr != nil && !errors.Is(postErr, ErrClosed) {
			fmt.Fprintf(os.Stderr, "tui: clipboard: %v\n", errors.Join(err, postErr))
		}
	}()
}

func reportClipboardError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: clipboard: %v\n", err)
	}
}
