package session

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/paths"
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

// NewStorage creates a Storage rooted at <configDir>/sessions.
// The directory is created if it doesn't exist.
func NewStorage() (*Storage, error) {
	return NewStorageAt(filepath.Join(paths.ConfigDir(), sessionsDirName))
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
	path, err := s.resolveID(id)
	if err != nil {
		return err
	}
	header, err := ReadHeader(path)
	if err != nil {
		return fmt.Errorf("read pinned session header: %w", err)
	}
	id = header.ID
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
	if id == "" {
		return errors.New("session ID must not be empty")
	}
	trashDir := s.TrashDir()

	files, err := filepath.Glob(filepath.Join(trashDir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("search trash: %w", err)
	}
	var matches, exactMatches []string
	for _, path := range files {
		header, err := ReadHeader(path)
		if err != nil {
			continue
		}
		if header.ID == id {
			exactMatches = append(exactMatches, path)
		}
		if strings.HasPrefix(header.ID, id) {
			matches = append(matches, path)
		}
	}
	if len(exactMatches) > 0 {
		matches = exactMatches
	}
	if len(matches) == 0 {
		return fmt.Errorf("session %q not found in trash", id)
	}
	if len(matches) > 1 {
		return fmt.Errorf("ambiguous session ID %q matches %d trashed sessions", id, len(matches))
	}

	header, err := ReadHeader(matches[0])
	if err != nil {
		return fmt.Errorf("read trashed session header: %w", err)
	}
	destDir := s.DirForCWD(header.CWD)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create restore dir: %w", err)
	}
	filename, err := uniqueFilename(destDir, filepath.Base(matches[0]))
	if err != nil {
		return fmt.Errorf("restore filename: %w", err)
	}
	if err := os.Rename(matches[0], filepath.Join(destDir, filename)); err != nil {
		return fmt.Errorf("restore session: %w", err)
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
func (s *Storage) BranchTo(sessionID string, branchFromID string) (err error) {
	r, err := s.Open(sessionID)
	if err != nil {
		return fmt.Errorf("open branch session: %w", err)
	}
	if r.FindByID(branchFromID) == nil && branchFromID != r.Header().ID {
		return fmt.Errorf("entry %q not found", branchFromID)
	}
	w, err := s.Resume(sessionID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()

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

	visited := make(map[string]bool)
	for currentID != "" {
		if visited[currentID] {
			return nil, fmt.Errorf("cycle in session branch at entry %q", currentID)
		}
		visited[currentID] = true
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

// Fork creates a new session that references a parent session. The parent's
// inherited entries are copied into the new file (the branch up to atEntry,
// or the whole session when atEntry is nil) so the child is self-contained:
// resume, export, and pickers read one file and cleanup can delete parents
// without orphaning children.
func (s *Storage) Fork(parentID string, atEntry *string) (*Writer, error) {
	parentPath, err := s.resolveID(parentID)
	if err != nil {
		return nil, err
	}

	parentHeader, err := ReadHeader(parentPath)
	if err != nil {
		return nil, fmt.Errorf("read parent header: %w", err)
	}

	r, err := ReadFile(parentPath)
	if err != nil {
		return nil, fmt.Errorf("read parent for fork: %w", err)
	}
	var selected map[string]bool
	if atEntry != nil {
		if *atEntry == "" || (r.FindByID(*atEntry) == nil && *atEntry != parentHeader.ID) {
			return nil, fmt.Errorf("fork entry %q not found", *atEntry)
		}
		branch, err := s.GetBranchEntries(parentHeader.ID, *atEntry)
		if err != nil {
			return nil, fmt.Errorf("read fork branch: %w", err)
		}
		selected = make(map[string]bool, len(branch))
		for _, entry := range branch {
			selected[entry.ID] = true
		}
	}

	pid := parentHeader.ID
	header := NewHeader(parentHeader.CWD, &pid, atEntry)

	w, err := s.Create(header)
	if err != nil {
		return nil, err
	}

	copyErr := func() error {
		for _, entry := range r.Entries() {
			if selected != nil && !selected[entry.ID] {
				// Legacy summaries are detached, but still belong to their kept branch.
				legacySummary := entry.Type == EntryTypeCompaction && entry.ParentID == nil && entry.FirstKeptID != nil && selected[*entry.FirstKeptID]
				summaryUsage := entry.Type == EntryTypeCustom && entry.CustomType != nil && *entry.CustomType == SummaryUsageType && entry.ParentID != nil && selected[*entry.ParentID]
				if !legacySummary && !summaryUsage {
					continue
				}
			}
			if err := w.Append(entry); err != nil {
				return fmt.Errorf("copy fork entry: %w", err)
			}
		}
		return nil
	}()
	if copyErr != nil {
		if closeErr := w.Close(); closeErr != nil {
			return nil, errors.Join(copyErr, fmt.Errorf("close fork writer: %w", closeErr))
		}
		return nil, copyErr
	}

	// Invalidate index cache so the new session shows up in List
	s.invalidateIndex()
	return w, nil
}

// AppendSummaryUsage records billed summary usage at the session's current position.
func (s *Storage) AppendSummaryUsage(id string, usage ai.Usage) (err error) {
	w, err := s.Resume(id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	reader, err := ReadFile(w.Path())
	if err != nil {
		return fmt.Errorf("read summary usage position: %w", err)
	}
	var parentID *string
	if position := reader.CurrentPosition(); position != "" {
		parentID = &position
	}
	entry, err := NewSummaryUsageEntry(parentID, usage)
	if err != nil {
		return err
	}
	if err := w.Append(entry); err != nil {
		return fmt.Errorf("append summary usage: %w", err)
	}
	return ReleaseOwner(w.Path())
}

// Compact appends a compaction entry to the session.
func (s *Storage) Compact(id string, summary string, firstKeptID string, tokensBefore, tokensAfter int) error {
	return s.CompactWithHook(id, summary, firstKeptID, tokensBefore, tokensAfter, false)
}

// CompactWithHook appends a compaction entry to the session, with control over
// whether the compaction came from a plugin hook.
func (s *Storage) CompactWithHook(id string, summary string, firstKeptID string, tokensBefore, tokensAfter int, fromHook bool) (err error) {
	w, err := s.Resume(id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	reader, err := ReadFile(w.Path())
	if err != nil {
		return fmt.Errorf("read compaction position: %w", err)
	}
	entry := NewCompactionEntry(summary, firstKeptID, tokensBefore, tokensAfter, fromHook)
	if position := reader.CurrentPosition(); position != "" {
		entry.ParentID = &position
	}
	if err := w.Append(entry); err != nil {
		return err
	}
	return ReleaseOwner(w.Path())
}

// SummarizeBranch appends a branch_summary entry.
func (s *Storage) SummarizeBranch(id, fromID, summary string) (err error) {
	w, err := s.Resume(id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	reader, err := ReadFile(w.Path())
	if err != nil {
		return fmt.Errorf("read branch summary position: %w", err)
	}

	entry := NewBranchSummaryEntry(fromID, summary, false)
	if position := reader.CurrentPosition(); position != "" {
		entry.ParentID = &position
	}
	if err := w.Append(entry); err != nil {
		return err
	}
	return ReleaseOwner(w.Path())
}

// resolveID finds the session file matching an ID (supports unique prefixes).
func (s *Storage) resolveID(id string) (string, error) {
	if id == "" {
		return "", errors.New("session ID must not be empty")
	}
	// File paths remain supported aliases for storage callers.
	if st, err := os.Stat(id); err == nil && !st.IsDir() {
		return id, nil
	}
	files, err := s.findSessionFiles("*.jsonl")
	if err != nil {
		return "", fmt.Errorf("search session: %w", err)
	}
	var matches, exactMatches []string
	for _, path := range files {
		header, err := ReadHeader(path)
		if err != nil {
			continue
		}
		if header.ID == id {
			exactMatches = append(exactMatches, path)
		}
		if strings.HasPrefix(header.ID, id) {
			matches = append(matches, path)
		}
	}
	if len(exactMatches) > 0 {
		matches = exactMatches
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
			return fmt.Errorf("walk session path %s: %w", path, err)
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

	var cleanupErr error
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("read session owner %s: %w", m, err))
			continue
		}

		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
			continue
		}

		if !isProcessRunning(pid) {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove stale session owner %s: %w", m, err))
			}
		}
	}
	return cleanupErr
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
