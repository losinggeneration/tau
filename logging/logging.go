// Package logging centralizes tau diagnostic logging.
//
// Library code should accept a *slog.Logger from callers and default to this
// package's discard logger. Applications may opt into stdout, stderr, files, or
// any other slog handler when they own a safe output sink. Terminal UI code
// should prefer a file or in-memory sink so diagnostics cannot corrupt the
// controlled terminal stream.
package logging // import "github.com/losinggeneration/tau/logging"

import (
	"context"
	"io"
	stdlog "log"
	"log/slog"
	"os"
)

type contextKey struct{}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Discard returns a logger that drops all records. It is the default for
// library code when the embedding application has not configured logging.
func Discard() *slog.Logger { return discard }

// OrDiscard returns logger when non-nil, otherwise the package discard logger.
func OrDiscard(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return discard
	}
	return logger
}

// NewText returns a text slog logger writing to w. A nil writer discards.
func NewText(w io.Writer, level slog.Leveler) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// NewJSON returns a JSON slog logger writing to w. A nil writer discards.
func NewJSON(w io.Writer, level slog.Leveler) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// NewFile opens path for append and returns a text logger plus a close function.
// Callers own the returned close function.
func NewFile(path string, level slog.Leveler) (*slog.Logger, func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return NewText(f, level), f.Close, nil
}

// StandardLogger adapts a slog logger to the standard library log.Logger API.
// A nil logger discards. This is useful while migrating APIs that still accept
// *log.Logger.
func StandardLogger(logger *slog.Logger, level slog.Level) *stdlog.Logger {
	return slog.NewLogLogger(OrDiscard(logger).Handler(), level)
}

// WithContext stores logger on ctx. Nil loggers are stored as discard loggers.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, OrDiscard(logger))
}

// FromContext returns the logger stored on ctx or the discard logger.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return discard
	}
	logger, ok := ctx.Value(contextKey{}).(*slog.Logger)
	if !ok || logger == nil {
		return discard
	}
	return logger
}
