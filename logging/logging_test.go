package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestDiscardLoggerDropsRecords(t *testing.T) {
	logger := OrDiscard(nil)
	logger.Info("should be discarded")
}

func TestTextLoggerWritesToConfiguredSink(t *testing.T) {
	var buf bytes.Buffer
	logger := NewText(&buf, slog.LevelDebug)
	logger.Debug("configured sink", "component", "test")

	got := buf.String()
	if !strings.Contains(got, "configured sink") || !strings.Contains(got, "component=test") {
		t.Fatalf("log output missing expected record: %q", got)
	}
}

func TestNilWriterDiscards(t *testing.T) {
	logger := NewText(nil, slog.LevelInfo)
	logger.Info("discard")
}

func TestContextLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := NewJSON(&buf, slog.LevelInfo)
	ctx := WithContext(context.Background(), logger)

	FromContext(ctx).Info("from context")
	if !strings.Contains(buf.String(), `"msg":"from context"`) {
		t.Fatalf("expected context logger to write JSON record, got %q", buf.String())
	}
}

func TestStandardLoggerUsesConfiguredSlogHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := NewText(&buf, slog.LevelInfo)
	std := StandardLogger(logger, slog.LevelWarn)
	std.Print("standard warning")

	if !strings.Contains(buf.String(), "standard warning") {
		t.Fatalf("expected standard logger output, got %q", buf.String())
	}
}

func TestNewJSONNilWriter(t *testing.T) {
	logger := NewJSON(io.Discard, slog.LevelDebug)
	logger.Debug("discard json")
}
