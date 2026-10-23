package clipboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteTextLocalFirstAndRemoteOSC52(t *testing.T) {
	for _, marker := range []string{"", "SSH_CONNECTION", "SSH_CLIENT", "MOSH_CONNECTION", "PI_CLIPBOARD_OSC52", "TAU_CLIPBOARD_OSC52"} {
		t.Run(marker, func(t *testing.T) {
			var calls []string
			w := textWriter{platform: "linux", getenv: func(key string) string {
				if key == "DISPLAY" {
					return ":0"
				}
				if key == marker {
					return "1"
				}
				return ""
			}, run: func(ctx context.Context, name, text string, args ...string) error {
				require.NoError(t, ctx.Err())
				assert.Equal(t, "héllo", text)
				calls = append(calls, name)
				return nil
			}, emit: func(sequence string) error {
				assert.Equal(t, "\x1b]52;c;aMOpbGxv\x07", sequence)
				calls = append(calls, "osc52")
				return nil
			}}
			require.NoError(t, w.write(context.Background(), "héllo"))
			want := []string{"xclip"}
			if marker != "" {
				want = append(want, "osc52")
			}
			assert.Equal(t, want, calls)
		})
	}
}

func TestWriteTextFailuresFallbackAndLimits(t *testing.T) {
	boom := errors.New("local failed")
	for _, tc := range []struct {
		name, platform string
		env            map[string]string
		wantCalls      []string
		hint           string
	}{
		{"desktop", "linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip", "xsel"}, "X11"},
		{"wayland", "linux", map[string]string{"WAYLAND_DISPLAY": "1", "DISPLAY": ":0"}, []string{"wl-copy", "xclip", "xsel"}, "wl-clipboard"},
		{"termux", "android", map[string]string{"TERMUX_VERSION": "1"}, []string{"termux-clipboard-set"}, "Termux:API"},
		{"headless", "linux", nil, []string{"osc52"}, ""},
		{"remote", "darwin", map[string]string{"SSH_CLIENT": "1"}, []string{"pbcopy", "osc52"}, ""},
		{"override zero is not forced", "darwin", map[string]string{"PI_CLIPBOARD_OSC52": "0"}, []string{"pbcopy"}, "clipboard unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			w := textWriter{platform: tc.platform, getenv: func(k string) string { return tc.env[k] }, run: func(_ context.Context, name, _ string, _ ...string) error { calls = append(calls, name); return boom }, emit: func(string) error { calls = append(calls, "osc52"); return nil }}
			err := w.write(context.Background(), "text")
			if tc.hint != "" {
				require.ErrorContains(t, err, tc.hint)
				require.ErrorIs(t, err, boom)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantCalls, calls)
		})
	}
	w := textWriter{platform: "linux", getenv: func(string) string { return "" }, run: func(context.Context, string, string, ...string) error { t.Fatal("unexpected command"); return nil }, emit: func(string) error { t.Fatal("oversized payload emitted"); return nil }}
	require.ErrorContains(t, w.write(context.Background(), strings.Repeat("x", 75001)), "OSC 52 size limit")
	transportErr := errors.New("transport failed")
	w.emit = func(string) error { return transportErr }
	require.ErrorIs(t, w.write(context.Background(), "x"), transportErr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.write(ctx, "x"), context.Canceled)
}

func TestWriteTextCommandFallbackAndSuccessfulOversize(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows", "android"} {
		t.Run(platform, func(t *testing.T) {
			var calls []string
			w := textWriter{platform: platform, getenv: func(key string) string {
				switch key {
				case "PI_CLIPBOARD_OSC52", "DISPLAY", "WAYLAND_DISPLAY", "TERMUX_VERSION":
					return "1"
				}
				return ""
			}, run: func(_ context.Context, name, _ string, _ ...string) error {
				calls = append(calls, name)
				if name == "xsel" || platform != "linux" {
					return nil
				}
				return errors.New("unavailable")
			}, emit: func(string) error { calls = append(calls, "osc52"); return nil }}
			require.NoError(t, w.write(context.Background(), strings.Repeat("x", 75001)))
			assert.NotContains(t, calls, "osc52")
			if platform == "linux" {
				assert.Equal(t, []string{"termux-clipboard-set", "wl-copy", "xclip", "xsel"}, calls)
			}
			calls = nil
			require.NoError(t, w.write(context.Background(), strings.Repeat("x", 75000)))
			assert.Equal(t, "osc52", calls[len(calls)-1])
		})
	}
}

func TestWriteTextCancellationStopsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := textWriter{platform: "linux", getenv: func(string) string { return "1" }, run: func(context.Context, string, string, ...string) error { cancel(); return context.Canceled }, emit: func(string) error { t.Fatal("emitted after cancellation"); return nil }}
	require.ErrorIs(t, w.write(ctx, "text"), context.Canceled)
}
