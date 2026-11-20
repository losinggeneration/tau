//go:build unix

package coord

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// A receipt is committed before spawn. Its lock follows the worker process,
// not the connection. Its request ID cannot be reused after pruning.
type launchReceipt struct {
	InstanceID string
	CWD        string
	Generation string
	TokenHash  [sha256.Size]byte
}

const (
	launchReceiptTTL  = 24 * time.Hour
	launchClockSkew   = 5 * time.Minute
	maxLaunchReceipts = 4096
)

var (
	errExpiredLaunchIntent = errors.New("launch intent predates the current receipt window")
	errLegacyLaunchIntent  = errors.New("new recoverable launches require a UUIDv7 request_id")
	errFutureLaunchIntent  = errors.New("launch intent is too far in the future and was not recorded; check the system clocks")
	errStaleLaunchIntent   = errors.New("launch intent is older than the receipt window and was never recorded; check the system clocks")
	errReceiptLimit        = errors.New("launch receipt limit reached; retry after receipts expire")
)

type receiptStore struct {
	dir    string
	mu     sync.RWMutex // serializes reservations, recovery registrations, and pruning
	cutoff time.Time
	count  int
}

func openReceiptStore(dir string) (*receiptStore, error) {
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	s := &receiptStore{dir: dir}
	f, err := os.Open(filepath.Join(dir, "cutoff"))
	switch {
	case err == nil:
		data, err := io.ReadAll(io.LimitReader(f, 32))
		if err := errors.Join(err, f.Close()); err != nil {
			return nil, fmt.Errorf("read launch receipt cutoff: %w", err)
		}
		ms, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse launch receipt cutoff: %w", err)
		}
		s.cutoff = time.UnixMilli(ms)
	case errors.Is(err, os.ErrNotExist):
		// A new or replaced store cannot vouch for previously issued intents.
		s.cutoff = time.Now().Truncate(time.Millisecond)
		if err := s.saveCutoff(s.cutoff); err != nil {
			return nil, err
		}
		if err := syncReceiptDir(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("open launch receipt cutoff: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list launch receipts: %w", err)
	}
	for _, entry := range entries {
		if isReceipt(entry) {
			s.count++
		}
	}
	return s, nil
}

func isReceipt(entry os.DirEntry) bool {
	return entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json")
}

func (s *receiptStore) path(key string) string { return filepath.Join(s.dir, key+".json") }

func receiptTime(key string) time.Time {
	return launchTime(strings.TrimPrefix(strings.TrimPrefix(key, "r-"), "i-"))
}

func (s *receiptStore) check(key string, now time.Time) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkLocked(key, now)
}

func (s *receiptStore) checkLocked(key string, now time.Time) error {
	issued := receiptTime(key)
	if _, err := os.Stat(s.path(key)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return fmt.Errorf("check launch receipt: %w", err)
		}
		return os.ErrExist
	}
	if legacy, ok := strings.CutPrefix(key, "r-"); ok {
		if issued.IsZero() {
			// Unprefixed UUIDv4 receipts predate the separate key namespaces.
			if _, err := os.Stat(s.path(legacy)); !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return fmt.Errorf("check legacy launch receipt: %w", err)
				}
				return os.ErrExist
			}
			return errLegacyLaunchIntent
		}
		// Pruning raises the durable cutoff to every removed intent before
		// deleting it, so only intents at or before the cutoff can have been
		// recorded and forgotten. A later one with no receipt never was.
		if !issued.After(s.cutoff) {
			return errExpiredLaunchIntent
		}
		if !issued.After(now.Add(-launchReceiptTTL)) {
			return errStaleLaunchIntent
		}
	}
	if issued.After(now.Add(launchClockSkew)) {
		return errFutureLaunchIntent
	}
	if s.count >= maxLaunchReceipts {
		return errReceiptLimit
	}
	return nil
}

func (s *receiptStore) create(id string, r launchReceipt) (_ *os.File, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(id, time.Now()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path(id), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	s.count++
	defer func() {
		if err != nil {
			// A partial record still reserves the request ID. No child was started.
			err = errors.Join(err, f.Close())
		}
	}()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock new launch receipt: %w", err)
	}
	if err := json.NewEncoder(f).Encode(r); err != nil {
		return nil, fmt.Errorf("write launch receipt: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync launch receipt: %w", err)
	}
	if err := syncReceiptDir(s.dir); err != nil {
		return nil, err
	}
	return f, nil
}

func syncReceiptDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open launch receipt directory: %w", err)
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return fmt.Errorf("sync launch receipt directory: %w", err)
	}
	return nil
}

// Persist the watermark before deleting anything. Time moving backwards (even
// across a restart) must not make any deleted request ID eligible again.
func (s *receiptStore) saveCutoff(cutoff time.Time) error {
	path := filepath.Join(s.dir, "cutoff")
	temp := path + ".tmp"
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open launch cutoff snapshot: %w", err)
	}
	_, err = f.WriteString(strconv.FormatInt(cutoff.UnixMilli(), 10))
	if err != nil {
		return fmt.Errorf("write launch cutoff snapshot: %w", errors.Join(err, f.Close()))
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("sync launch cutoff snapshot: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("publish launch cutoff snapshot: %w", err)
	}
	return syncReceiptDir(s.dir)
}

// prune returns the keys for which it both proved exit and removed a receipt.
// Old UUIDv4 receipts can be kept for a surviving worker, but can never create
// a new intent, so they need no time-based tombstone.
func (s *receiptStore) prune(now time.Time) (map[string]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list launch receipts: %w", err)
	}
	cutoff := now.Add(-launchReceiptTTL).Truncate(time.Millisecond)
	var removed map[string]struct{}
	for _, entry := range entries {
		if !isReceipt(entry) {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		issued := receiptTime(key)
		if issued.After(cutoff) && issued.After(s.cutoff) {
			continue
		}
		exited, err := s.exited(key)
		if err != nil {
			return removed, fmt.Errorf("probe launch receipt for pruning: %w", err)
		}
		if !exited {
			continue
		}
		if !strings.HasPrefix(key, "i-") && issued.After(s.cutoff) {
			if err := s.saveCutoff(issued); err != nil {
				return removed, err
			}
			s.cutoff = issued
		}
		if err := os.Remove(s.path(key)); err != nil {
			return removed, fmt.Errorf("prune launch receipt: %w", err)
		}
		s.count--
		if removed == nil {
			removed = make(map[string]struct{})
		}
		removed[key] = struct{}{}
	}
	return removed, nil
}

func (s *receiptStore) read(id string) (r launchReceipt, err error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return r, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return r, json.NewDecoder(io.LimitReader(f, 32<<10)).Decode(&r)
}

func (s *receiptStore) exited(id string) (_ bool, err error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
