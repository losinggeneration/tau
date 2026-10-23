package clipboard

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Image struct {
	MIME     string
	Data     []byte
	Filename string
}

func ParseDataURL(s string) (Image, bool) {
	if !strings.HasPrefix(s, "data:") {
		return Image{}, false
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return Image{}, false
	}
	meta := s[5:comma]
	data := s[comma+1:]
	parts := strings.Split(meta, ";")
	mime := parts[0]
	b64 := false
	for _, p := range parts[1:] {
		if p == "base64" {
			b64 = true
		}
	}
	if !b64 {
		return Image{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return Image{}, false
	}
	return Image{MIME: mime, Data: raw}, true
}

func SaveImage(dir string, img Image) (string, error) {
	if img.Filename != "" && (img.Filename == "." || img.Filename == ".." || strings.ContainsAny(img.Filename, "/\\\x00")) {
		return "", fmt.Errorf("clipboard: invalid image filename %q", img.Filename)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("clipboard: create image dir: %w", err)
	}
	if img.MIME == "" {
		img.MIME = "application/octet-stream"
	}
	ext := extFor(img.MIME)
	name := img.Filename
	if name == "" {
		name = "pasted-image" + ext
	}
	path := filepath.Join(dir, name)
	ext = filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for n := 1; ; n++ {
		candidate := path
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d%s", base, n, ext)
		}
		file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("clipboard: create image: %w", err)
		}
		_, writeErr := file.Write(img.Data)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return "", fmt.Errorf("clipboard: save image: %w", errors.Join(err, os.Remove(candidate)))
		}
		return candidate, nil
	}
}

func AttachmentMarkdown(path string) string { return "@" + path }
func extFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}
