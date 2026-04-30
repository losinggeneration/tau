package clipboard

import (
	"encoding/base64"
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
	path := uniquePath(filepath.Join(dir, name))
	if err := os.WriteFile(path, img.Data, 0o644); err != nil {
		return "", fmt.Errorf("clipboard: save image: %w", err)
	}
	return path, nil
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", base, n, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
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
