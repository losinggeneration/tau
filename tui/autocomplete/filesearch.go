package autocomplete

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/losinggeneration/tau/internal/ignore"
)

const (
	defaultSearchEntries = 20000
	// searchRecentFolders top-level folders, newest first, are read to
	// searchEarlyDepth before the rest, and every top-level folder is read to
	// that depth before anything deeper.
	searchRecentFolders = 10
	searchEarlyDepth    = 3
	searchReadBatch     = 256
)

// WalkFileSearcher performs cancellation-aware recursive discovery without a
// subprocess. Limit can narrow the requested result count. MaxEntries caps how
// many directory entries one search reads (20000 when zero); the search
// returns the best matches found when it runs out.
type WalkFileSearcher struct {
	Limit      int
	MaxEntries int
}

// searchDir is a folder waiting to be read. ancestors holds the folders above
// it so a symlink back up the tree isn't followed forever.
type searchDir struct {
	path      string
	level     int
	rules     *ignore.Matcher
	ancestors *searchAncestor
	modTime   time.Time
}

type searchAncestor struct {
	info   os.FileInfo
	parent *searchAncestor
}

func (a *searchAncestor) contains(info os.FileInfo) bool {
	for ; a != nil; a = a.parent {
		if os.SameFile(a.info, info) {
			return true
		}
	}
	return false
}

// Search implements FileSearcher. Folders are read breadth first, so shallow
// files across the whole tree use the entry budget before any deep subtree.
func (s WalkFileSearcher) Search(ctx context.Context, basePath, query string, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 20
	}
	if s.Limit > 0 {
		limit = min(limit, s.Limit)
	}
	remaining := cmp.Or(s.MaxEntries, defaultSearchEntries)

	type scored struct {
		item  Item
		score int
	}
	var matches []scored
	// scan reads one folder, records its matches, and returns its child
	// folders. A folder that can't be read is skipped.
	scan := func(dir searchDir) (children []searchDir, err error) {
		f, err := os.Open(dir.path)
		if err != nil {
			return nil, nil
		}
		defer func() {
			if closeErr := f.Close(); closeErr != nil && err == nil {
				children, err = nil, fmt.Errorf("close %s: %w", dir.path, closeErr)
			}
		}()
		for remaining > 0 {
			entries, readErr := f.ReadDir(min(remaining, searchReadBatch))
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if entry.Name() == ".git" {
					continue
				}
				remaining--
				path := filepath.Join(dir.path, entry.Name())
				var info os.FileInfo
				isDir := entry.IsDir()
				if entry.Type()&os.ModeSymlink != 0 {
					if info, err = os.Stat(path); err != nil {
						continue
					}
					isDir = info.IsDir()
					if !isDir && !info.Mode().IsRegular() {
						continue
					}
				} else if !isDir && !entry.Type().IsRegular() {
					continue
				}
				if dir.rules.Ignored(path, isDir) {
					continue
				}
				rel, err := filepath.Rel(basePath, path)
				if err != nil {
					return nil, err
				}
				display := filepath.ToSlash(rel)
				score := scoreEntry(display, query, isDir)
				if query == "" {
					score = 1
				}
				if score > 0 {
					label := entry.Name() + dirSuffix(isDir)
					value := display + dirSuffix(isDir)
					matches = append(matches, scored{item: Item{Value: value, Label: label, Description: display}, score: score})
				}
				if !isDir {
					continue
				}
				if info == nil {
					if info, err = entry.Info(); err != nil {
						continue
					}
				} else if dir.ancestors.contains(info) {
					continue
				}
				// One malformed ignore file shouldn't blank every suggestion,
				// so its folder is listed but not searched.
				rules, err := dir.rules.Child(path)
				if err != nil {
					continue
				}
				children = append(children, searchDir{
					path:      path,
					level:     dir.level + 1,
					rules:     rules,
					ancestors: &searchAncestor{info: info, parent: dir.ancestors},
					modTime:   info.ModTime(),
				})
			}
			if readErr != nil {
				break
			}
		}
		return children, nil
	}
	// walk reads queue breadth first. Folders at maxLevel or deeper are
	// returned unread for a later pass.
	walk := func(queue []searchDir, maxLevel int) ([]searchDir, error) {
		var deferred []searchDir
		for len(queue) > 0 && remaining > 0 {
			dir := queue[0]
			queue = queue[1:]
			children, err := scan(dir)
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				if child.level >= maxLevel {
					deferred = append(deferred, child)
				} else {
					queue = append(queue, child)
				}
			}
		}
		return deferred, nil
	}

	info, err := os.Stat(basePath)
	if err != nil {
		return nil, nil
	}
	rules, err := ignore.New(basePath, basePath)
	if err != nil {
		return nil, err
	}
	top, err := scan(searchDir{path: basePath, rules: rules, ancestors: &searchAncestor{info: info}})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].modTime.After(top[j].modTime) })
	recent := min(len(top), searchRecentFolders)
	deferred, err := walk(top[:recent:recent], searchEarlyDepth)
	if err != nil {
		return nil, err
	}
	rest, err := walk(top[recent:], searchEarlyDepth)
	if err != nil {
		return nil, err
	}
	if _, err := walk(append(deferred, rest...), math.MaxInt); err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		a, b := matches[i].item.Description, matches[j].item.Description
		aDepth, bDepth := strings.Count(strings.Trim(a, "/"), "/"), strings.Count(strings.Trim(b, "/"), "/")
		if aDepth != bDepth {
			return aDepth < bDepth
		}
		aLength, bLength := len(utf16.Encode([]rune(a))), len(utf16.Encode([]rune(b)))
		if aLength != bLength {
			return aLength < bLength
		}
		return a < b
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	items := make([]Item, len(matches))
	for i, match := range matches {
		items[i] = match.item
	}
	return items, nil
}
