package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ownerSuffix = ".owner"
	// ownerLockMarker follows the PID in owner files held with flock. Older
	// readers parse only the leading PID, so they still see a live owner.
	ownerLockMarker   = "flock"
	ownerLockAttempts = 3
	// ownerBusyRetries bounds how often a busy owner lock is retried before
	// the session is reported as open elsewhere.
	ownerBusyRetries = 10
)

var errOwnerReplaced = errors.New("session owner file was replaced")

// ownerBusyWait pauses between attempts to lock a busy owner file.
var ownerBusyWait = func() { time.Sleep(5 * time.Millisecond) }

// Writer appends entries to a session JSONL file with flock-based concurrency.
type Writer struct {
	path   string
	file   *os.File
	owner  *ownerClaim
	mu     sync.Mutex
	closed bool
}

// ownerClaim is this process's exclusive flock on a session's .owner file.
// Writers and explicit ClaimOwner calls in the same process share one claim;
// the lock is dropped when the last reference is released.
type ownerClaim struct {
	key      string
	file     *os.File
	refs     int
	explicit int
}

var owners = struct {
	sync.Mutex
	byPath map[string]*ownerClaim
}{byPath: make(map[string]*ownerClaim)}

// NewWriter opens or creates a session file for appending and claims the
// session for this process until Close. It fails when another process owns
// the session. It acquires an exclusive flock on each Append call and
// releases it after writing.
func NewWriter(path string) (*Writer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	owners.Lock()
	claim, err := acquireOwnerLocked(path)
	owners.Unlock()
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open session file: %w", err), releaseOwnerClaim(claim))
	}

	return &Writer{path: path, file: f, owner: claim}, nil
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

// Close releases the file handle and this writer's ownership claim. Claims
// held by other writers or ClaimOwner calls are unaffected.
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
	if releaseErr := releaseOwnerClaim(w.owner); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release session owner: %w", releaseErr))
	}
	w.owner = nil
	return err
}

// Path returns the file path.
func (w *Writer) Path() string {
	return w.path
}

// CreateSessionFile creates a new session JSONL file with the given header
// and returns the Writer, which owns the session until Close.
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

	return w, nil
}

// ClaimOwner claims a session for this process without opening a writer.
// Each successful call must be paired with ReleaseOwner. It returns an error
// if another live process owns the session.
func ClaimOwner(sessionPath string) error {
	owners.Lock()
	defer owners.Unlock()
	claim, err := acquireOwnerLocked(sessionPath)
	if err != nil {
		return err
	}
	claim.explicit++
	return nil
}

// ReleaseOwner releases one ClaimOwner claim. Without an outstanding
// ClaimOwner claim it does nothing, so it never drops a writer's claim.
func ReleaseOwner(sessionPath string) error {
	key, err := ownerKey(sessionPath)
	if err != nil {
		return err
	}
	owners.Lock()
	defer owners.Unlock()
	claim := owners.byPath[key]
	if claim == nil || claim.explicit == 0 {
		return nil
	}
	claim.explicit--
	return claim.releaseLocked()
}

func ownerKey(sessionPath string) (string, error) {
	key, err := filepath.Abs(sessionPath)
	if err != nil {
		return "", fmt.Errorf("resolve session path: %w", err)
	}
	return key, nil
}

// acquireOwnerLocked returns this process's claim on a session, taking the
// cross-process lock if no claim exists yet. owners must be locked.
func acquireOwnerLocked(sessionPath string) (*ownerClaim, error) {
	key, err := ownerKey(sessionPath)
	if err != nil {
		return nil, err
	}
	if claim := owners.byPath[key]; claim != nil {
		claim.refs++
		return claim, nil
	}
	f, err := lockOwnerFile(key + ownerSuffix)
	if err != nil {
		return nil, err
	}
	claim := &ownerClaim{key: key, file: f, refs: 1}
	owners.byPath[key] = claim
	return claim, nil
}

func releaseOwnerClaim(claim *ownerClaim) error {
	if claim == nil {
		return nil
	}
	owners.Lock()
	defer owners.Unlock()
	return claim.releaseLocked()
}

// releaseLocked drops one reference. The last reference unlinks the owner
// file while still holding its lock, so a waiting claimer either sees the
// file gone or locks a fresh one. owners must be locked.
func (c *ownerClaim) releaseLocked() error {
	c.refs--
	if c.refs > 0 {
		return nil
	}
	if owners.byPath[c.key] == c {
		delete(owners.byPath, c.key)
	}
	ownerPath := c.key + ownerSuffix
	var err error
	same, statErr := sameOwnerFile(c.file, ownerPath)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
	case statErr != nil:
		err = statErr
	case !same:
		// Never unlink a lock file this claim does not hold.
		err = &os.PathError{Op: "remove", Path: ownerPath, Err: errOwnerReplaced}
	default:
		if rmErr := os.Remove(ownerPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = rmErr
		}
	}
	return errors.Join(err, closeOwnerFile(c.file))
}

// lockOwnerFile takes an exclusive, non-blocking flock on the owner file.
// A file unlinked or replaced between open and lock is retried so two
// processes never hold locks on different inodes for the same path.
func lockOwnerFile(ownerPath string) (*os.File, error) {
	for range ownerLockAttempts {
		f, err := os.OpenFile(ownerPath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open session owner: %w", err)
		}
		locked, err := claimOwnerFile(f, ownerPath)
		if err != nil {
			return nil, errors.Join(err, closeOwnerFile(f))
		}
		if locked {
			return f, nil
		}
		if err := closeOwnerFile(f); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("lock session owner %s: file was repeatedly replaced", ownerPath)
}

// claimOwnerFile locks f and records this process as owner. It reports false
// without error when ownerPath no longer names f and the caller should retry.
func claimOwnerFile(f *os.File, ownerPath string) (bool, error) {
	if err := flockOwnerFile(f); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, ownerBusyError(f)
		}
		return false, fmt.Errorf("lock session owner: %w", err)
	}
	same, err := sameOwnerFile(f, ownerPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !same {
		return false, nil
	}
	pid, lockHeld, err := readOwner(f)
	if err != nil {
		return false, err
	}
	// Files written by older versions carry only a PID and no lock.
	if !lockHeld && pid > 0 && pid != os.Getpid() && isProcessRunning(pid) {
		return false, fmt.Errorf("session is open in another process (PID %d)", pid)
	}
	if err := f.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate session owner: %w", err)
	}
	if _, err := f.WriteAt(fmt.Appendf(nil, "%d %s\n", os.Getpid(), ownerLockMarker), 0); err != nil {
		return false, fmt.Errorf("write session owner: %w", err)
	}
	return true, nil
}

// flockOwnerFile takes an exclusive lock on f, retrying briefly while it is
// busy. CleanupStaleOwners and a releasing claimer hold an owner lock only
// momentarily and must not look like a live owner; a real owner still holds
// the lock once the retries run out.
func flockOwnerFile(f *os.File) error {
	for attempt := 0; ; attempt++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || attempt == ownerBusyRetries {
			return err
		}
		ownerBusyWait()
	}
}

func ownerBusyError(f *os.File) error {
	pid, _, err := readOwner(f)
	switch {
	case err != nil:
		return fmt.Errorf("session is open in another process: %w", err)
	case pid == os.Getpid():
		return errors.New("session is already open in this process under another path")
	case pid > 0:
		return fmt.Errorf("session is open in another process (PID %d)", pid)
	}
	return errors.New("session is open in another process")
}

// readOwner parses "<pid>[ flock]". Empty or unparsable content reports PID 0,
// matching older versions that overwrote such files.
func readOwner(f *os.File) (pid int, lockHeld bool, err error) {
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, fmt.Errorf("read session owner: %w", err)
	}
	fields := strings.Fields(string(buf[:n]))
	if len(fields) == 0 {
		return 0, false, nil
	}
	pid, convErr := strconv.Atoi(fields[0])
	if convErr != nil {
		return 0, false, nil
	}
	return pid, len(fields) > 1 && fields[1] == ownerLockMarker, nil
}

// sameOwnerFile reports whether ownerPath still names the open file f. A
// missing path is returned as an os.ErrNotExist error.
func sameOwnerFile(f *os.File, ownerPath string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat held session owner: %w", err)
	}
	current, err := os.Stat(ownerPath)
	if err != nil {
		return false, err
	}
	return os.SameFile(held, current), nil
}

func closeOwnerFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("close session owner: %w", err)
	}
	return nil
}

// errOwnerLeftover marks a rename that moved the session but could not remove
// the previous owner link.
var errOwnerLeftover = errors.New("session renamed but previous owner file remains")

// renameOwnedSession moves a session file and its held owner file together.
// The owner is hard-linked to the new name first, so both paths stay claimed
// while the session moves and an existing owner at the destination is never
// clobbered. Filesystems without hard links cannot rename owned sessions; the
// rename fails before the session moves. An error wrapping errOwnerLeftover
// means the session did move.
func renameOwnedSession(claim *ownerClaim, oldPath, newPath string) error {
	newKey, err := ownerKey(newPath)
	if err != nil {
		return err
	}
	owners.Lock()
	defer owners.Unlock()
	if owners.byPath[newKey] != nil {
		return fmt.Errorf("session destination %s is already open", newPath)
	}
	oldOwner := claim.key + ownerSuffix
	newOwner := newKey + ownerSuffix
	if err := os.Link(oldOwner, newOwner); err != nil {
		return fmt.Errorf("claim renamed session: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		renameErr := fmt.Errorf("rename session file: %w", err)
		if rmErr := os.Remove(newOwner); rmErr != nil {
			return errors.Join(renameErr, fmt.Errorf("remove renamed session owner: %w", rmErr))
		}
		return renameErr
	}
	delete(owners.byPath, claim.key)
	claim.key = newKey
	owners.byPath[newKey] = claim
	if err := os.Remove(oldOwner); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", errOwnerLeftover, err)
	}
	return nil
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

// uniqueFilename generates a unique filename by appending -2, -3, etc. if
// needed. A name whose owner file exists is taken too, since moving a session
// there would leave it claimed by another session's owner.
func uniqueFilename(dir, filename string) (string, error) {
	base := strings.TrimSuffix(filename, ".jsonl")
	candidate := filename
	for i := 2; ; i++ {
		taken, err := sessionNameTaken(filepath.Join(dir, candidate))
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d.jsonl", base, i)
	}
}

func sessionNameTaken(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat session destination: %w", err)
	}
	if _, err := os.Lstat(path + ownerSuffix); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat session destination owner: %w", err)
	}
	return false, nil
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
// moving the file. It fails if another process owns the session. A claim this
// process holds moves with the file, so writers opened on the new path share
// it and writers still open on the old path keep it until they close. If the
// file moved but its previous owner file could not be removed, newPath is
// returned along with the error.
func RenameSession(oldPath, newName string) (newPath string, err error) {
	w, err := NewWriter(oldPath)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := w.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close session: %w", closeErr))
		}
	}()

	info := NewSessionInfoEntry(newName)
	if err := w.Append(info); err != nil {
		return "", fmt.Errorf("write session_info: %w", err)
	}

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
	if newPath == oldPath {
		return newPath, nil
	}
	newFilename, err = uniqueFilename(filepath.Dir(oldPath), newFilename)
	if err != nil {
		return "", fmt.Errorf("rename filename: %w", err)
	}
	newPath = filepath.Join(filepath.Dir(oldPath), newFilename)

	if err := renameOwnedSession(w.owner, oldPath, newPath); err != nil {
		if errors.Is(err, errOwnerLeftover) {
			return newPath, err
		}
		return "", err
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
