package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Writer appends entries to a session JSONL file with flock-based concurrency.
type Writer struct {
	path   string
	file   *os.File
	mu     sync.Mutex
	closed bool
}

var ownerRefs = struct {
	sync.Mutex
	byPath map[string]int
}{byPath: make(map[string]int)}

// NewWriter opens or creates a session file for appending. It acquires an
// exclusive flock on each Append call and releases it after writing.
func NewWriter(path string) (*Writer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}

	return &Writer{path: path, file: f}, nil
}

// Append marshals the entry and appends it as a JSON line with flock.
func (w *Writer) Append(entry any) (err error) {
	data, err := MarshalEntry(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session writer closed")
	}

	if err := syscall.Flock(int(w.file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock session: %w", err)
	}
	defer func() {
		if unlockErr := syscall.Flock(int(w.file.Fd()), syscall.LOCK_UN); unlockErr != nil && err == nil {
			err = fmt.Errorf("unlock session: %w", unlockErr)
		}
	}()

	if _, err := w.file.Write(data); err != nil {
		return fmt.Errorf("write entry: %w", err)
	}
	return w.file.Sync()
}

// MarkDeleted sets "status": "deleted" on the entries with the given IDs.
// The file is rewritten atomically and the writer reopens the replacement, so
// later appends land in the rewritten file.
func (w *Writer) MarkDeleted(ids []string) (err error) {
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("session writer closed")
	}

	old := w.file
	if err := syscall.Flock(int(old.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock session: %w", err)
	}
	defer func() {
		if old == w.file {
			if unlockErr := syscall.Flock(int(old.Fd()), syscall.LOCK_UN); unlockErr != nil && err == nil {
				err = fmt.Errorf("unlock session: %w", unlockErr)
			}
		}
	}()

	data, err := os.ReadFile(w.path)
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	var lines []json.RawMessage
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if i > 0 {
			if line, err = markDeleted(line, drop); err != nil {
				return err
			}
		}
		lines = append(lines, line)
	}
	if err := writeMigratedFile(w.path, lines); err != nil {
		return fmt.Errorf("rewrite session: %w", err)
	}

	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("reopen session file: %w", err)
	}
	w.file = f
	if err := old.Close(); err != nil {
		return fmt.Errorf("close replaced session file: %w", err)
	}
	return nil
}

func markDeleted(line []byte, drop map[string]bool) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		// Malformed lines are skipped by readers; keep them untouched.
		return line, nil
	}
	var id string
	if err := json.Unmarshal(fields["id"], &id); err != nil || !drop[id] {
		return line, nil
	}
	fields["status"] = json.RawMessage(`"` + EntryStatusDeleted + `"`)
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshal deleted entry: %w", err)
	}
	return out, nil
}

// AppendHeader writes the session header as the first line.
// This should only be called on newly created session files.
func (w *Writer) AppendHeader(h Header) error {
	return w.Append(h)
}

// Close releases the file handle and owner file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if closeErr := w.file.Close(); closeErr != nil {
		err = fmt.Errorf("close session file: %w", closeErr)
	}
	if releaseErr := ReleaseOwner(w.path); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release session owner: %w", releaseErr))
	}
	return err
}

// Path returns the file path.
func (w *Writer) Path() string {
	return w.path
}

// CreateSessionFile creates a new session JSONL file with the given header
// and returns the Writer. It also creates the .owner file.
func CreateSessionFile(dir string, header Header) (*Writer, error) {
	slug := slugFromHeader(header)
	var baseFilename string
	if slug != "" {
		baseFilename = fmt.Sprintf("%s-%s-%s.jsonl",
			header.Timestamp[:10],
			header.ID[:8],
			slug,
		)
	} else {
		// No display name yet; use ID-only format (slug will be derived
		// from first user message later via RenameSession)
		baseFilename = fmt.Sprintf("%s-%s.jsonl",
			header.Timestamp[:10],
			header.ID[:8],
		)
	}

	// Ensure filename is unique to avoid overwriting existing sessions
	filename, err := uniqueFilename(dir, baseFilename)
	if err != nil {
		return nil, fmt.Errorf("generate unique filename: %w", err)
	}

	path := filepath.Join(dir, filename)

	w, err := NewWriter(path)
	if err != nil {
		return nil, err
	}

	if err := w.AppendHeader(header); err != nil {
		if closeErr := w.Close(); closeErr != nil {
			return nil, fmt.Errorf("write header: %w; close session: %v", err, closeErr)
		}
		return nil, fmt.Errorf("write header: %w", err)
	}

	// Create .owner file
	if err := writeOwnerFile(path, os.Getpid()); err != nil {
		// Non-fatal: owner file is advisory
		_ = err
	}

	return w, nil
}

// ClaimOwner creates the .owner file for an existing session. Returns an
// error if another live process owns it (not this process).
func ClaimOwner(sessionPath string) error {
	ownerPath := sessionPath + ".owner"
	key := filepath.Clean(sessionPath)

	ownerRefs.Lock()
	defer ownerRefs.Unlock()

	// Check existing owner
	data, err := os.ReadFile(ownerPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read owner file: %w", err)
	}

	if err == nil && len(data) > 0 {
		var pid int
		if _, perr := fmt.Sscanf(string(data), "%d", &pid); perr == nil {
			if pid == os.Getpid() {
				ownerRefs.byPath[key]++
				return nil
			}
			if isProcessRunning(pid) {
				return fmt.Errorf("session is open in another process (PID %d)", pid)
			}
			// Stale owner file; clean it up
			_ = os.Remove(ownerPath)
		}
	}

	if err := writeOwnerFile(sessionPath, os.Getpid()); err != nil {
		return err
	}
	ownerRefs.byPath[key] = 1
	return nil
}

// ReleaseOwner removes the .owner file.
func ReleaseOwner(sessionPath string) error {
	key := filepath.Clean(sessionPath)
	ownerRefs.Lock()
	if refs := ownerRefs.byPath[key]; refs > 1 {
		ownerRefs.byPath[key] = refs - 1
		ownerRefs.Unlock()
		return nil
	}
	delete(ownerRefs.byPath, key)
	ownerRefs.Unlock()

	err := os.Remove(sessionPath + ".owner")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeOwnerFile writes the PID to the .owner file.
func writeOwnerFile(sessionPath string, pid int) error {
	ownerPath := sessionPath + ".owner"
	return os.WriteFile(ownerPath, []byte(fmt.Sprintf("%d", pid)), 0o644)
}

// isProcessRunning checks if a process with the given PID exists.
func isProcessRunning(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check existence.
	return p.Signal(syscall.Signal(0)) == nil
}

// slugFromHeader derives a name slug from the header.
// Per the spec: use header.Name if set, otherwise return empty string
// (slug will be derived from the first user message later via RenameSession).
func slugFromHeader(h Header) string {
	if h.Name != "" {
		return SlugFromName(h.Name)
	}
	return ""
}

// uniqueFilename generates a unique filename by appending -1, -2, etc. if needed.
func uniqueFilename(dir, filename string) (string, error) {
	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return filename, nil
	} else if err != nil {
		return "", fmt.Errorf("stat session destination: %w", err)
	}

	// File exists, find a unique name
	base := strings.TrimSuffix(filename, ".jsonl")
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d.jsonl", base, i)
		path := filepath.Join(dir, candidate)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("stat session destination: %w", err)
		}
	}
}

// SlugFromName converts a display name to a filesystem-safe slug.
func SlugFromName(name string) string {
	if len(name) > 40 {
		name = name[:40]
	}
	var buf []byte
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			buf = append(buf, byte(r))
		} else if r >= 'A' && r <= 'Z' {
			buf = append(buf, byte(r+'a'-'A'))
		} else if r == ' ' || r == '-' || r == '_' {
			buf = append(buf, '-')
		}
	}
	result := string(buf)
	// Trim leading/trailing dashes
	for len(result) > 0 && result[0] == '-' {
		result = result[1:]
	}
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	if result == "" {
		result = "untitled"
	}
	return result
}

// RenameSession renames a session file by writing a session_info entry and
// moving the file.
func RenameSession(oldPath, newName string) (newPath string, err error) {
	w, err := NewWriter(oldPath)
	if err != nil {
		return "", err
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := w.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close session: %w", closeErr)
		}
	}()

	info := NewSessionInfoEntry(newName)
	if err := w.Append(info); err != nil {
		return "", fmt.Errorf("write session_info: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close session before rename: %w", err)
	}
	closed = true

	// Parse the header to get the ID for the new filename
	h, err := ReadHeader(oldPath)
	if err != nil {
		return "", err
	}

	slug := SlugFromName(newName)
	newFilename := fmt.Sprintf("%s-%s-%s.jsonl",
		h.Timestamp[:10],
		h.ID[:8],
		slug,
	)
	newPath = filepath.Join(filepath.Dir(oldPath), newFilename)
	if newPath != oldPath {
		newFilename, err = uniqueFilename(filepath.Dir(oldPath), newFilename)
		if err != nil {
			return "", fmt.Errorf("rename filename: %w", err)
		}
		newPath = filepath.Join(filepath.Dir(oldPath), newFilename)
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return "", fmt.Errorf("rename session file: %w", err)
	}

	return newPath, nil
}

// ReadHeader reads and parses the first line of a session file.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return Header{}, fmt.Errorf("decode header: %w", err)
	}
	return h, nil
}
