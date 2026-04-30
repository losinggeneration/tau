package session

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	sessionsDirName = "sessions"
	trashDirName    = ".trash"
	indexFileName   = "index.json"
	pinsFileName    = "pins.json"
)

// Storage manages session files on disk.
type Storage struct {
	baseDir string
}

// NewStorage creates a Storage rooted at ~/.tau/agent/sessions/.
// The directory is created if it doesn't exist.
func NewStorage() (*Storage, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	baseDir := filepath.Join(home, ".tau", "agent", sessionsDirName)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	storage := &Storage{baseDir: baseDir}
	if err := storage.MigrateToCWDDirs(); err != nil {
		return nil, fmt.Errorf("migrate sessions: %w", err)
	}
	return storage, nil
}

// NewStorageAt creates a Storage at a custom path (for testing).
func NewStorageAt(dir string) (*Storage, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	storage := &Storage{baseDir: dir}
	if err := storage.MigrateToCWDDirs(); err != nil {
		return nil, fmt.Errorf("migrate sessions: %w", err)
	}
	return storage, nil
}

// Dir returns the base sessions directory.
func (s *Storage) Dir() string {
	return s.baseDir
}

// TrashDir returns the .trash directory.
func (s *Storage) TrashDir() string {
	return filepath.Join(s.baseDir, trashDirName)
}

// Create creates a new session file and returns a Writer.
func (s *Storage) Create(header Header) (*Writer, error) {
	return CreateSessionFile(s.DirForCWD(header.CWD), header)
}

// List returns a summary of all sessions, reading from index.json if fresh
// or scanning files if stale.
func (s *Storage) List() ([]SessionSummary, error) {
	return s.withPins(s.listFromDir(s.baseDir, true))
}

// ListCWD returns summaries for sessions created in cwd. With the default
// scoped directory layout this scans only that cwd's session directory.
func (s *Storage) ListCWD(cwd string) ([]SessionSummary, error) {
	return s.withPins(s.listFromDir(s.DirForCWD(cwd), false))
}

// SetPinned records whether a session is pinned in the workspace list.
func (s *Storage) SetPinned(id string, pinned bool) error {
	if _, err := s.resolveID(id); err != nil {
		return err
	}
	pins, err := s.readPins()
	if err != nil {
		return err
	}
	if pinned {
		pins[id] = NewTimestamp()
	} else {
		delete(pins, id)
	}
	return s.writePins(pins)
}

// DirForCWD returns the session directory for a working directory.
func (s *Storage) DirForCWD(cwd string) string {
	return filepath.Join(s.baseDir, cwdDirName(cwd))
}

// LatestResumable returns the most recently created session that is not owned
// by another process. Returns empty string if no resumable session exists.
func (s *Storage) LatestResumable() (string, error) {
	return s.latestResumableFrom(s.List)
}

// LatestResumableCWD returns the most recent resumable session for cwd.
func (s *Storage) LatestResumableCWD(cwd string) (string, error) {
	return s.latestResumableFrom(func() ([]SessionSummary, error) { return s.ListCWD(cwd) })
}

func (s *Storage) latestResumableFrom(list func() ([]SessionSummary, error)) (string, error) {
	summaries, err := list()
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}

	for _, summary := range summaries {
		// Check if this session file is currently owned by another process
		path, err := s.resolveID(summary.ID)
		if err != nil {
			continue
		}
		ownerPath := path + ".owner"
		if _, err := os.Stat(ownerPath); err == nil {
			// Owner file exists - session is locked, skip it
			continue
		}
		// Session is not locked - it can be resumed
		return summary.ID, nil
	}

	return "", nil
}

// Open opens an existing session for reading.
func (s *Storage) Open(id string) (*Reader, error) {
	path, err := s.resolveID(id)
	if err != nil {
		return nil, err
	}
	return ReadFile(path)
}

// Resume opens an existing session for appending, claiming ownership.
func (s *Storage) Resume(id string) (*Writer, error) {
	path, err := s.resolveID(id)
	if err != nil {
		return nil, err
	}

	if err := ClaimOwner(path); err != nil {
		return nil, err
	}

	return NewWriter(path)
}

// Delete moves a session to the trash directory.
func (s *Storage) Delete(id string) error {
	path, err := s.resolveID(id)
	if err != nil {
		return err
	}

	if err := s.moveToTrashPath(path); err != nil {
		return err
	}

	// Invalidate index cache
	s.invalidateIndex()
	return nil
}

// Restore moves a session from the trash back to the sessions directory.
func (s *Storage) Restore(id string) error {
	trashDir := s.TrashDir()

	// Find the file in trash matching the ID prefix
	matches, err := filepath.Glob(filepath.Join(trashDir, "*"+id+"*"))
	if err != nil {
		return fmt.Errorf("search trash: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("session %q not found in trash", id)
	}

	header, err := ReadHeader(matches[0])
	if err != nil {
		return fmt.Errorf("read trashed session header: %w", err)
	}
	destDir := s.DirForCWD(header.CWD)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create restore dir: %w", err)
	}
	if err := os.Rename(matches[0], filepath.Join(destDir, filepath.Base(matches[0]))); err != nil {
		return err
	}

	// Invalidate index cache
	s.invalidateIndex()
	return nil
}

// Rename changes the display name of a session.
func (s *Storage) Rename(id, newName string) error {
	path, err := s.resolveID(id)
	if err != nil {
		return err
	}

	newPath, err := RenameSession(path, newName)
	if err != nil {
		return err
	}

	// Move owner file if it exists
	_ = os.Rename(path+".owner", newPath+".owner")

	// Invalidate index cache
	s.invalidateIndex()
	return nil
}

// MoveCWD rewrites a session's working directory and moves it into that CWD's scoped directory.
func (s *Storage) MoveCWD(id, newCWD string) (string, error) {
	path, err := s.resolveID(id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path + ".owner"); err == nil {
		return "", fmt.Errorf("session %q is currently owned", id)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	absCWD, err := filepath.Abs(newCWD)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	info, err := os.Stat(absCWD)
	if err != nil {
		return "", fmt.Errorf("stat cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", absCWD)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	firstLineEnd := bytes.IndexByte(data, '\n')
	if firstLineEnd < 0 {
		return "", fmt.Errorf("session %q has no header line", id)
	}
	header, err := ParseHeader(data[:firstLineEnd])
	if err != nil {
		return "", err
	}
	oldCWD := header.CWD
	header.CWD = absCWD
	headerLine, err := MarshalEntry(header)
	if err != nil {
		return "", err
	}

	destDir := s.DirForCWD(absCWD)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	filename, err := uniqueFilename(destDir, filepath.Base(path))
	if err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, filename)
	updated := append(append([]byte{}, headerLine...), data[firstLineEnd:]...)
	if err := os.WriteFile(dest, updated, 0o644); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	s.invalidateIndex()
	return oldCWD, nil
}

// BranchTo moves the current position to a different entry by appending a
// position_change entry. New entries will be children of that entry.
func (s *Storage) BranchTo(sessionID string, branchFromID string) error {
	w, err := s.Resume(sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	entry := NewPositionChangeEntry(branchFromID)
	if err := w.Append(entry); err != nil {
		return err
	}
	return ReleaseOwner(w.Path())
}

// GetBranchEntries returns all entries from the given entry back to root,
// in chronological order. This is used for path navigation.
func (s *Storage) GetBranchEntries(sessionID string, fromID string) ([]Entry, error) {
	r, err := s.Open(sessionID)
	if err != nil {
		return nil, err
	}

	var path []Entry
	currentID := fromID

	// Build ID -> Entry map
	entries := r.Entries()
	byID := make(map[string]*Entry, len(entries))
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}

	for currentID != "" {
		// Check header first
		if currentID == r.Header().ID {
			break
		}

		e, ok := byID[currentID]
		if !ok {
			break
		}
		path = append(path, *e)

		if e.ParentID == nil {
			break
		}
		currentID = *e.ParentID
	}

	// Reverse to chronological order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	return path, nil
}

// Fork creates a new session that references a parent session.
func (s *Storage) Fork(parentID string, atEntry *string) (*Writer, error) {
	parentPath, err := s.resolveID(parentID)
	if err != nil {
		return nil, err
	}

	parentHeader, err := ReadHeader(parentPath)
	if err != nil {
		return nil, fmt.Errorf("read parent header: %w", err)
	}

	pid := parentHeader.ID
	header := NewHeader(parentHeader.CWD, &pid, atEntry)

	w, err := s.Create(header)
	if err != nil {
		return nil, err
	}

	// Invalidate index cache so the new session shows up in List
	s.invalidateIndex()
	return w, nil
}

// Compact appends a compaction entry to the session.
func (s *Storage) Compact(id string, summary string, firstKeptID string, tokensBefore, tokensAfter int) error {
	return s.CompactWithHook(id, summary, firstKeptID, tokensBefore, tokensAfter, false)
}

// CompactWithHook appends a compaction entry to the session, with control over
// whether the compaction came from a plugin hook.
func (s *Storage) CompactWithHook(id string, summary string, firstKeptID string, tokensBefore, tokensAfter int, fromHook bool) error {
	w, err := s.Resume(id)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	entry := NewCompactionEntry(summary, firstKeptID, tokensBefore, tokensAfter, fromHook)
	if err := w.Append(entry); err != nil {
		return err
	}
	return ReleaseOwner(w.Path())
}

// SummarizeBranch appends a branch_summary entry.
func (s *Storage) SummarizeBranch(id, fromID, summary string) error {
	w, err := s.Resume(id)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	entry := NewBranchSummaryEntry(fromID, summary, false)
	if err := w.Append(entry); err != nil {
		return err
	}
	return ReleaseOwner(w.Path())
}

// resolveID finds the session file matching an ID (supports unique prefixes).
func (s *Storage) resolveID(id string) (string, error) {
	// Try exact path first for callers that pass a file path.
	if st, err := os.Stat(id); err == nil && !st.IsDir() {
		return id, nil
	}

	matches, err := s.findSessionFiles("*" + id + "*.jsonl")
	if err != nil {
		return "", fmt.Errorf("search session: %w", err)
	}
	if len(matches) == 0 {
		matches, err = s.findSessionFiles("*-" + id + ".jsonl")
		if err != nil {
			return "", fmt.Errorf("search session: %w", err)
		}
	}
	if len(matches) == 0 {
		// Final fallback: read headers from all files to find matching full ID.
		allMatches, err := s.findSessionFiles("*.jsonl")
		if err != nil {
			return "", fmt.Errorf("search session: %w", err)
		}
		for _, path := range allMatches {
			h, err := ReadHeader(path)
			if err == nil && h.ID == id {
				matches = append(matches, path)
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("session %q not found", id)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous session ID %q matches %d sessions", id, len(matches))
	}
	return matches[0], nil
}

func cwdDirName(cwd string) string {
	resolved, err := filepath.Abs(cwd)
	if err != nil || resolved == "" {
		resolved = cwd
	}
	resolved = filepath.Clean(resolved)
	trimmed := strings.TrimLeft(resolved, string(filepath.Separator)+`\`)
	if trimmed == "" || trimmed == "." {
		return "--root--"
	}
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return "--" + replacer.Replace(trimmed) + "--"
}

// MigrateToCWDDirs moves legacy flat session files from the storage root into
// their cwd-scoped directories. After the first run the root should contain no
// JSONL files, making this effectively a cheap directory listing.
func (s *Storage) MigrateToCWDDirs() error {
	matches, err := filepath.Glob(filepath.Join(s.baseDir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("glob legacy sessions: %w", err)
	}
	for _, path := range matches {
		header, err := ReadHeader(path)
		if err != nil {
			continue
		}
		destDir := s.DirForCWD(header.CWD)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("create scoped session dir: %w", err)
		}
		filename, err := uniqueFilename(destDir, filepath.Base(path))
		if err != nil {
			return fmt.Errorf("session migration filename: %w", err)
		}
		dest := filepath.Join(destDir, filename)
		if err := os.Rename(path, dest); err != nil {
			return fmt.Errorf("move session %s: %w", path, err)
		}
		if err := os.Rename(path+".owner", dest+".owner"); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("move session owner %s: %w", path, err)
		}
	}
	s.invalidateIndex()
	return nil
}

func (s *Storage) findSessionFiles(pattern string) ([]string, error) {
	return s.globFiles(pattern, true)
}

func (s *Storage) ownerFiles() ([]string, error) {
	return s.globFiles("*.owner", true)
}

func (s *Storage) sessionFilesInDir(dir string, recursive bool) ([]string, error) {
	pattern := filepath.Join(dir, "*.jsonl")
	if !recursive {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob sessions: %w", err)
		}
		return matches, nil
	}
	return s.globFiles("*.jsonl", true)
}

func (s *Storage) globFiles(pattern string, recursive bool) ([]string, error) {
	if !recursive {
		matches, err := filepath.Glob(filepath.Join(s.baseDir, pattern))
		if err != nil {
			return nil, err
		}
		return matches, nil
	}

	var matches []string
	err := filepath.WalkDir(s.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if filepath.Base(path) == trashDirName {
				return filepath.SkipDir
			}
			return nil
		}
		ok, err := filepath.Match(pattern, filepath.Base(path))
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// SessionSummary is a cached summary for the index file.
type SessionSummary struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Filename     string      `json:"filename"`
	CWD          string      `json:"cwd"`
	Created      string      `json:"created"`
	LastModified string      `json:"last_modified"`
	Model        string      `json:"model,omitempty"`
	Status       string      `json:"status"`
	MessageCount int         `json:"message_count"`
	Tokens       TokenCounts `json:"tokens"`
	ParentID     *string     `json:"parent_id,omitempty"`
	FirstPrompt  string      `json:"first_prompt,omitempty"`
	Pinned       bool        `json:"pinned,omitempty"`
	PinnedAt     string      `json:"pinned_at,omitempty"`
}

func (s *Storage) scanAndIndexDir(dir string, recursive bool) ([]SessionSummary, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session index dir: %w", err)
	}
	entries, err := s.sessionFilesInDir(dir, recursive)
	if err != nil {
		return nil, err
	}

	summaries := make([]SessionSummary, 0, len(entries))
	for _, path := range entries {
		summary, err := ReadSummary(path)
		if err != nil {
			continue // skip unreadable files
		}

		summaries = append(summaries, summary)
	}

	SortSummaries(summaries)

	if err := s.writeIndex(filepath.Join(dir, indexFileName), summaries); err != nil {
		return nil, fmt.Errorf("write session index: %w", err)
	}

	return summaries, nil
}

func (s *Storage) listFromDir(dir string, recursive bool) ([]SessionSummary, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}
	indexFile := filepath.Join(dir, indexFileName)

	if summaries, err := s.readIndex(indexFile); err == nil {
		if !s.isIndexStaleDir(indexFile, dir, recursive) {
			return summaries, nil
		}
	}

	return s.scanAndIndexDir(dir, recursive)
}

func (s *Storage) withPins(summaries []SessionSummary, err error) ([]SessionSummary, error) {
	if err != nil {
		return nil, err
	}
	pins, err := s.readPins()
	if err != nil {
		return nil, err
	}
	for i := range summaries {
		if ts, ok := pins[summaries[i].ID]; ok {
			summaries[i].Pinned = true
			summaries[i].PinnedAt = ts
		}
	}
	SortSummaries(summaries)
	return summaries, nil
}

// SortSummaries orders pinned sessions first, then newest sessions.
func SortSummaries(summaries []SessionSummary) {
	slices.SortStableFunc(summaries, func(a, b SessionSummary) int {
		if a.Pinned != b.Pinned {
			if a.Pinned {
				return -1
			}
			return 1
		}
		if a.Pinned && a.PinnedAt != b.PinnedAt {
			return cmp.Compare(b.PinnedAt, a.PinnedAt)
		}
		return cmp.Compare(b.Created, a.Created)
	})
}

func (s *Storage) readPins() (map[string]string, error) {
	path := filepath.Join(s.baseDir, pinsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read pins: %w", err)
	}
	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, fmt.Errorf("decode pins: %w", err)
	}
	if pins == nil {
		pins = map[string]string{}
	}
	return pins, nil
}

func (s *Storage) writePins(pins map[string]string) error {
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pins: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, pinsFileName), data, 0o644); err != nil {
		return fmt.Errorf("write pins: %w", err)
	}
	return nil
}

// readIndex reads the cached index file.
func (s *Storage) readIndex(path string) ([]SessionSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var summaries []SessionSummary
	if err := json.Unmarshal(data, &summaries); err != nil {
		return nil, err
	}
	return summaries, nil
}

// writeIndex writes the index cache file.
func (s *Storage) writeIndex(path string, summaries []SessionSummary) error {
	data, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// invalidateIndex removes index cache files so the next List() does a full scan.
func (s *Storage) invalidateIndex() {
	_ = filepath.WalkDir(s.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if filepath.Base(path) == trashDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == indexFileName {
			_ = os.Remove(path)
		}
		return nil
	})
}

func (s *Storage) isIndexStaleDir(indexFile, dir string, recursive bool) bool {
	idxInfo, err := os.Stat(indexFile)
	if err != nil {
		return true
	}

	entries, err := s.sessionFilesInDir(dir, recursive)
	if err != nil {
		return true
	}

	for _, e := range entries {
		info, err := os.Stat(e)
		if err != nil {
			continue
		}
		if info.ModTime().After(idxInfo.ModTime()) {
			return true
		}
	}
	return false
}

// CleanupStaleOwners removes .owner files whose PIDs are no longer running.
func (s *Storage) CleanupStaleOwners() error {
	matches, err := s.ownerFiles()
	if err != nil {
		return err
	}

	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}

		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
			continue
		}

		if !isProcessRunning(pid) {
			_ = os.Remove(m)
		}
	}
	return nil
}

// ExportJSON exports a session as structured JSON.
func (s *Storage) ExportJSON(id string) ([]byte, error) {
	r, err := s.Open(id)
	if err != nil {
		return nil, err
	}
	return r.MarshalJSON()
}

// ExportMarkdown exports the session as Markdown. See agent/session/mdexport.go
// for the renderer that walks the full entry stream (branch summaries, model
// changes, etc., not just the active branch).
func (s *Storage) ExportMarkdown(id string) ([]byte, error) {
	r, err := s.Open(id)
	if err != nil {
		return nil, err
	}
	return RenderMarkdown(r, Options{})
}

// ExportHTML exports the session as a self-contained HTML file. See
// agent/session/htmlexport.go for the renderer.
func (s *Storage) ExportHTML(id string) ([]byte, error) {
	return s.ExportHTMLOptions(id, Options{})
}

// ExportHTMLOptions exports the session as HTML with the given options.
// Used by the CLI export command to honor --no-collapse.
func (s *Storage) ExportHTMLOptions(id string, opts Options) ([]byte, error) {
	r, err := s.Open(id)
	if err != nil {
		return nil, err
	}
	return RenderHTML(r, opts)
}

// FormatTime parses an RFC3339 timestamp and returns a human-friendly format.
func FormatTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.Format("2006-01-02 15:04:05")
}
