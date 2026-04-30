package sdk

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/losinggeneration/tau/ai"
)

const (
	maxInlineAttachmentBytes = 16 << 20
	attachmentRecordsFile    = "attachments.json"
	attachmentPreviewMax     = 256
)

// Attachment is a session-scoped file or image imported into tau-owned storage.
type Attachment struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Filename      string `json:"filename"`
	MimeType      string `json:"mime_type"`
	Size          int64  `json:"size"`
	Path          string `json:"path,omitempty"`
	Data          string `json:"data,omitempty"`
	ExtractedText string `json:"extracted_text,omitempty"`
	Preview       string `json:"preview,omitempty"`
}

// AttachmentStore imports and tracks attachments for one session.
type AttachmentStore struct {
	dir     string
	records map[string]Attachment
}

// NewAttachmentStore creates an attachment store rooted in the session data directory.
func NewAttachmentStore(dir string) *AttachmentStore {
	store := &AttachmentStore{dir: dir, records: make(map[string]Attachment)}
	if err := store.load(); err != nil {
		return store
	}
	return store
}

// ImportFile copies a local file into session storage and returns its attachment record.
func (s *AttachmentStore) ImportFile(path string) (Attachment, error) {
	if strings.TrimSpace(path) == "" {
		return Attachment{}, fmt.Errorf("attachment path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, fmt.Errorf("read attachment %s: %w", path, err)
	}
	return s.importBytes(filepath.Base(path), "", data)
}

// ImportData decodes base64 bytes into session storage and returns its attachment record.
func (s *AttachmentStore) ImportData(filename, mimeType, data string) (Attachment, error) {
	if strings.TrimSpace(filename) == "" {
		return Attachment{}, fmt.Errorf("attachment filename is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return Attachment{}, fmt.Errorf("decode attachment data: %w", err)
	}
	if len(decoded) > maxInlineAttachmentBytes {
		return Attachment{}, fmt.Errorf("attachment data exceeds %d bytes", maxInlineAttachmentBytes)
	}
	return s.importBytes(filename, mimeType, decoded)
}

// List returns attachment records in insertion-neutral order.
func (s *AttachmentStore) List() []Attachment {
	out := make([]Attachment, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	return out
}

// Delete removes an attachment record and its stored bytes when present.
func (s *AttachmentStore) Delete(id string) error {
	record, ok := s.records[id]
	if !ok {
		return fmt.Errorf("attachment %q not found", id)
	}
	delete(s.records, id)
	if record.Path != "" {
		if err := os.Remove(record.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove attachment %s: %w", record.Path, err)
		}
	}
	if record.Preview != "" {
		if err := os.Remove(record.Preview); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove attachment preview %s: %w", record.Preview, err)
		}
	}
	if err := s.save(); err != nil {
		return fmt.Errorf("save attachment records: %w", err)
	}
	return nil
}

// ContentBlocks converts attachments into model content blocks.
func (s *AttachmentStore) ContentBlocks(ids []string) ([]ai.ContentBlock, error) {
	blocks := make([]ai.ContentBlock, 0, len(ids))
	for _, id := range ids {
		record, ok := s.records[id]
		if !ok {
			return nil, fmt.Errorf("attachment %q not found", id)
		}
		switch record.Kind {
		case "image":
			block, err := ai.LoadImageContentBlock(record.Path)
			if err != nil {
				return nil, fmt.Errorf("load attachment image %s: %w", record.ID, err)
			}
			blocks = append(blocks,
				ai.ContentBlock{Type: ai.ContentBlockText, Text: fmt.Sprintf("<file name=%q>[image content]</file>", record.Filename)},
				block,
			)
		case "document":
			if record.ExtractedText == "" {
				return nil, fmt.Errorf("attachment %q has no extracted text", record.ID)
			}
			blocks = append(blocks, ai.ContentBlock{Type: ai.ContentBlockText, Text: fmt.Sprintf("<file name=%q>\n%s\n</file>", record.Filename, record.ExtractedText)})
		default:
			return nil, fmt.Errorf("unsupported attachment kind %q", record.Kind)
		}
	}
	return blocks, nil
}

func (s *AttachmentStore) load() error {
	data, err := os.ReadFile(filepath.Join(s.dir, attachmentRecordsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read attachment records: %w", err)
	}
	var records []Attachment
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("decode attachment records: %w", err)
	}
	for _, record := range records {
		if record.ID != "" {
			s.records[record.ID] = record
		}
	}
	return nil
}

func (s *AttachmentStore) save() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create attachment dir: %w", err)
	}
	records := s.List()
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode attachment records: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, attachmentRecordsFile), data, 0o600); err != nil {
		return fmt.Errorf("write attachment records: %w", err)
	}
	return nil
}

func (s *AttachmentStore) importBytes(filename, mimeType string, data []byte) (Attachment, error) {
	if len(data) == 0 {
		return Attachment{}, fmt.Errorf("attachment %s is empty", filename)
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	kind := "document"
	if strings.HasPrefix(mimeType, "image/") {
		kind = "image"
	}
	id := uuid.Must(uuid.NewV7()).String()
	path := filepath.Join(s.dir, id+filepath.Ext(filename))
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Attachment{}, fmt.Errorf("create attachment dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Attachment{}, fmt.Errorf("write attachment: %w", err)
	}
	record := Attachment{ID: id, Kind: kind, Filename: filename, MimeType: mimeType, Size: int64(len(data)), Path: path}
	if kind == "image" {
		preview, err := writeAttachmentPreview(path, mimeType)
		if err != nil {
			return Attachment{}, fmt.Errorf("write attachment preview: %w", err)
		}
		record.Preview = preview
	}
	if kind == "document" && strings.HasPrefix(mimeType, "text/") {
		record.ExtractedText = string(data)
	}
	s.records[id] = record
	if err := s.save(); err != nil {
		return Attachment{}, fmt.Errorf("save attachment records: %w", err)
	}
	return record, nil
}

func writeAttachmentPreview(path, mimeType string) (previewFile string, err error) {
	if !previewImageMime(mimeType) {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open image: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close image: %w", closeErr)
		}
	}()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	bounds := img.Bounds()
	w, h := fitAttachmentPreview(bounds.Dx(), bounds.Dy())
	preview := resizeAttachmentPreview(img, w, h)
	previewPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".preview" + filepath.Ext(path)
	out, err := os.Create(previewPath)
	if err != nil {
		return "", fmt.Errorf("create image preview: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close image preview: %w", closeErr)
		}
	}()
	switch strings.ToLower(mimeType) {
	case "image/png":
		err = png.Encode(out, preview)
	case "image/jpeg", "image/jpg":
		err = jpeg.Encode(out, preview, &jpeg.Options{Quality: 80})
	case "image/gif":
		err = gif.Encode(out, preview, nil)
	}
	if err != nil {
		return "", fmt.Errorf("encode image preview: %w", err)
	}
	return previewPath, nil
}

func previewImageMime(mimeType string) bool {
	switch strings.ToLower(mimeType) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif":
		return true
	default:
		return false
	}
}

func fitAttachmentPreview(w, h int) (int, int) {
	if w <= 0 || h <= 0 {
		return 1, 1
	}
	if w <= attachmentPreviewMax && h <= attachmentPreviewMax {
		return w, h
	}
	if w >= h {
		return attachmentPreviewMax, max(1, h*attachmentPreviewMax/w)
	}
	return max(1, w*attachmentPreviewMax/h), attachmentPreviewMax
}

func resizeAttachmentPreview(src image.Image, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	for y := range h {
		sy := b.Min.Y + y*b.Dy()/h
		for x := range w {
			sx := b.Min.X + x*b.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}
