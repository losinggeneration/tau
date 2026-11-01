package autocomplete

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/losinggeneration/tau/internal/ignore"
)

// WalkFileSearcher performs cancellation-aware recursive discovery without a subprocess.
// Limit can narrow the requested result count; traversal is not bounded.
type WalkFileSearcher struct {
	Limit int
}

// Search implements FileSearcher.
func (s WalkFileSearcher) Search(ctx context.Context, basePath, query string, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 20
	}
	if s.Limit > 0 {
		limit = min(limit, s.Limit)
	}

	type scored struct {
		item  Item
		score int
	}
	var matches []scored
	var walk func(string, []os.FileInfo) error
	walk = func(dir string, ancestors []os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil
		}
		for _, ancestor := range ancestors {
			if os.SameFile(ancestor, info) {
				return nil
			}
		}
		ancestors = append(ancestors, info)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		rules, err := ignore.New(dir, basePath)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Name() == ".git" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path)
			if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
				continue
			}
			if rules.Ignored(path, info.IsDir()) {
				continue
			}
			rel, err := filepath.Rel(basePath, path)
			if err != nil {
				return err
			}
			display := filepath.ToSlash(rel)
			score := scoreEntry(display, query, info.IsDir())
			if query == "" {
				score = 1
			}
			if score > 0 {
				label := entry.Name() + dirSuffix(info.IsDir())
				value := display + dirSuffix(info.IsDir())
				matches = append(matches, scored{item: Item{Value: value, Label: label, Description: display}, score: score})
			}
			if info.IsDir() {
				if err := walk(path, ancestors); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(basePath, nil); err != nil {
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
