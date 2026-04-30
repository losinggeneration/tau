package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const readTimeout = 800 * time.Millisecond

// ReadImage reads an image directly from the OS clipboard when a small native
// helper is available. It returns ok=false when no supported helper/image exists.
func ReadImage(ctx context.Context) (Image, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	switch runtime.GOOS {
	case "linux":
		return readLinuxImage(ctx)
	case "darwin":
		return readCommandImage(ctx, "image/png", "pngpaste", "-")
	default:
		return Image{}, false, nil
	}
}

func readLinuxImage(ctx context.Context) (Image, bool, error) {
	if img, ok, err := readWlPasteImage(ctx); ok || err != nil {
		return img, ok, err
	}
	return readXclipImage(ctx)
}

func readWlPasteImage(ctx context.Context) (Image, bool, error) {
	if _, ok := os.LookupEnv("WAYLAND_DISPLAY"); !ok {
		return Image{}, false, nil
	}
	out, ok, err := commandOutput(ctx, "wl-paste", "--list-types")
	if !ok || err != nil {
		return Image{}, false, nil
	}
	mime := firstImageMIME(string(out))
	if mime == "" {
		return Image{}, false, nil
	}
	return readCommandImage(ctx, mime, "wl-paste", "--no-newline", "--type", mime)
}

func readXclipImage(ctx context.Context) (Image, bool, error) {
	out, ok, err := commandOutput(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	if !ok || err != nil {
		return Image{}, false, nil
	}
	mime := firstImageMIME(string(out))
	if mime == "" {
		return Image{}, false, nil
	}
	return readCommandImage(ctx, mime, "xclip", "-selection", "clipboard", "-t", mime, "-o")
}

func readCommandImage(ctx context.Context, mime, name string, args ...string) (Image, bool, error) {
	out, ok, err := commandOutput(ctx, name, args...)
	if !ok || err != nil {
		return Image{}, ok, err
	}
	if len(out) == 0 {
		return Image{}, false, nil
	}
	return Image{MIME: mime, Data: out}, true, nil
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err == nil {
		return out, true, nil
	}
	var pathErr *exec.Error
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, exec.ErrNotFound) {
		return nil, false, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, true, fmt.Errorf("%s: %w", name, ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return nil, true, fmt.Errorf("%s: %s: %w", name, strings.TrimSpace(string(exitErr.Stderr)), err)
	}
	return nil, true, fmt.Errorf("%s: %w", name, err)
}

func firstImageMIME(types string) string {
	for _, want := range []string{"image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp"} {
		for _, got := range strings.FieldsFunc(types, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' || r == ' ' }) {
			if bytes.EqualFold([]byte(got), []byte(want)) {
				return want
			}
		}
	}
	return ""
}
