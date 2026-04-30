package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// CleanupEmptyOptions controls cleanup of sessions that contain no messages.
type CleanupEmptyOptions struct {
	DryRun bool
}

// CleanupEmptyResult describes the files found and removed by CleanupEmpty.
type CleanupEmptyResult struct {
	Matched         []SessionSummary
	Removed         []SessionSummary
	OrphanDataDirs  []string
	RemovedDataDirs []string
}

// CleanupEmpty moves sessions with no message/custom_message entries to trash.
// Header-only sessions and sessions that only contain metadata entries do not
// carry reusable conversation context, so they are considered empty.
func (s *Storage) CleanupEmpty(ctx context.Context, opts CleanupEmptyOptions) (CleanupEmptyResult, error) {
	entries, err := s.sessionFilesInDir(s.baseDir, true)
	if err != nil {
		return CleanupEmptyResult{}, fmt.Errorf("glob sessions: %w", err)
	}

	var result CleanupEmptyResult
	for _, path := range entries {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("cleanup empty sessions: %w", ctx.Err())
		default:
		}

		// Do not touch a session that is currently owned by a process. The caller
		// can run CleanupStaleOwners before this to clear dead owners.
		if _, err := os.Stat(path + ".owner"); err == nil {
			continue
		} else if err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("stat owner for %s: %w", filepath.Base(path), err)
		}

		r, err := ReadFile(path)
		if err != nil {
			continue
		}
		if r.MessageCount() != 0 {
			continue
		}

		summary := SessionSummary{
			ID:           r.header.ID,
			Name:         r.DisplayName(),
			Filename:     filepath.Base(path),
			CWD:          r.header.CWD,
			Created:      r.header.Timestamp,
			MessageCount: r.MessageCount(),
			Tokens:       r.TotalTokens(),
			ParentID:     r.header.ParentSession,
		}
		result.Matched = append(result.Matched, summary)
		if opts.DryRun {
			continue
		}

		if err := s.moveToTrashPath(path); err != nil {
			return result, fmt.Errorf("move %s to trash: %w", filepath.Base(path), err)
		}
		result.Removed = append(result.Removed, summary)
		if removed, err := s.removeDataDir(r.header.ID); err != nil {
			return result, err
		} else if removed {
			result.RemovedDataDirs = append(result.RemovedDataDirs, r.header.ID)
		}
	}

	if err := s.cleanupOrphanDataDirs(ctx, opts, &result); err != nil {
		return result, err
	}

	if len(result.Removed) > 0 {
		s.invalidateIndex()
	}
	return result, nil
}

func (s *Storage) cleanupOrphanDataDirs(ctx context.Context, opts CleanupEmptyOptions, result *CleanupEmptyResult) error {
	dataRoot := filepath.Join(s.baseDir, ".data")
	entries, err := os.ReadDir(dataRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session data dir: %w", err)
	}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cleanup session data dirs: %w", ctx.Err())
		default:
		}
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if _, err := s.resolveID(id); err == nil {
			continue
		}
		result.OrphanDataDirs = append(result.OrphanDataDirs, id)
		if opts.DryRun {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataRoot, id)); err != nil {
			return fmt.Errorf("remove data dir %s: %w", id, err)
		}
		result.RemovedDataDirs = append(result.RemovedDataDirs, id)
	}
	return nil
}

func (s *Storage) removeDataDir(id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	path := filepath.Join(s.baseDir, ".data", id)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("stat data dir %s: %w", id, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("remove data dir %s: %w", id, err)
	}
	return true, nil
}

func (s *Storage) moveToTrashPath(path string) error {
	trashDir := s.TrashDir()
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return fmt.Errorf("create trash dir: %w", err)
	}

	dest, err := uniqueTrashPath(trashDir, filepath.Base(path))
	if err != nil {
		return err
	}
	if err := os.Rename(path, dest); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func uniqueTrashPath(dir, filename string) (string, error) {
	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path, nil
	} else if err != nil {
		return "", fmt.Errorf("stat trash destination: %w", err)
	}

	base := filename
	ext := filepath.Ext(filename)
	if ext != "" {
		base = filename[:len(filename)-len(ext)]
	}
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("stat trash destination: %w", err)
		}
	}
}
