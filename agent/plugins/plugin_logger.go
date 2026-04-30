package plugins

import (
	"context"
	"io"
	stdlog "log"
	"log/slog"

	"github.com/hashicorp/go-hclog"
	"github.com/losinggeneration/tau/logging"
)

// pluginLogger adapts hashicorp/go-plugin logging to slog without writing to
// stdout/stderr by default.
type pluginLogger struct {
	logger *slog.Logger
	args   []any
	name   string
}

func newPluginLogger(logger *slog.Logger) *pluginLogger {
	return &pluginLogger{logger: logging.OrDiscard(logger)}
}

func (l *pluginLogger) log(level slog.Level, msg string, args ...any) {
	logger := logging.OrDiscard(l.logger)
	if l.name != "" {
		logger = logger.With("logger", l.name)
	}
	if len(l.args) > 0 {
		logger = logger.With(l.args...)
	}
	logger.Log(context.Background(), level, msg, args...)
}

func (l *pluginLogger) Debug(msg string, args ...any) { l.log(slog.LevelDebug, msg, args...) }
func (l *pluginLogger) Info(msg string, args ...any)  { l.log(slog.LevelInfo, msg, args...) }
func (l *pluginLogger) Warn(msg string, args ...any)  { l.log(slog.LevelWarn, msg, args...) }
func (l *pluginLogger) Error(msg string, args ...any) { l.log(slog.LevelError, msg, args...) }

func (l *pluginLogger) Log(level hclog.Level, msg string, args ...any) {
	switch level {
	case hclog.Trace, hclog.Debug:
		l.Debug(msg, args...)
	case hclog.Info:
		l.Info(msg, args...)
	case hclog.Warn:
		l.Warn(msg, args...)
	case hclog.Error:
		l.Error(msg, args...)
	}
}

func (l *pluginLogger) Name() string { return l.name }

func (l *pluginLogger) IsTrace() bool            { return false }
func (l *pluginLogger) IsDebug() bool            { return false }
func (l *pluginLogger) IsInfo() bool             { return true }
func (l *pluginLogger) IsWarn() bool             { return true }
func (l *pluginLogger) IsError() bool            { return true }
func (l *pluginLogger) ImpliedSeparator() string { return "" }
func (l *pluginLogger) ImpliedArgs() []any       { return append([]any(nil), l.args...) }

func (l *pluginLogger) Trace(msg string, args ...any) { l.log(slog.LevelDebug, msg, args...) }

func (l *pluginLogger) With(args ...any) hclog.Logger {
	clone := *l
	clone.args = append(append([]any(nil), l.args...), args...)
	return &clone
}

func (l *pluginLogger) Named(name string) hclog.Logger {
	clone := *l
	if clone.name == "" {
		clone.name = name
	} else {
		clone.name += "." + name
	}
	return &clone
}

func (l *pluginLogger) ResetNamed(name string) hclog.Logger {
	clone := *l
	clone.name = name
	return &clone
}

func (l *pluginLogger) StandardLogger(opts *hclog.StandardLoggerOptions) *stdlog.Logger {
	return logging.StandardLogger(l.logger, slog.LevelInfo)
}

func (l *pluginLogger) StandardWriter(opts *hclog.StandardLoggerOptions) io.Writer { return io.Discard }

func (l *pluginLogger) NamedInterceptLoggerAllowed(name string) bool { return true }

func (l *pluginLogger) DuplicateChildLogger(name string) hclog.Logger { return l.Named(name) }

func (l *pluginLogger) ResetLogger() error         { return nil }
func (l *pluginLogger) SetLevel(level hclog.Level) {}
func (l *pluginLogger) GetLevel() hclog.Level      { return hclog.NoLevel }

func (l *pluginLogger) Caveat(level hclog.Level, format string, args ...any) hclog.Logger { return l }

func (l *pluginLogger) Intercept(underlying hclog.Logger) hclog.Logger { return l }
