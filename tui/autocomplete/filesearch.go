package autocomplete

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WalkFileSearcher is a non-subprocess recursive fuzzy file searcher. It is
// bounded and cancellation-aware, suitable as the production fallback when an
// app wants @file recursive fuzzy completion without shelling out to fd.
type WalkFileSearcher struct {
	Limit int
}

// Search implements FileSearcher.
func (s WalkFileSearcher) Search(ctx context.Context, basePath, query string, limit int) ([]Item, error) {
	if s.Limit > 0 {
		limit = s.Limit
	}
	if limit <= 0 {
		limit = 20
	}

	type scored struct {
		item  Item
		score int
	}
	var matches []scored
	walkErr := filepath.WalkDir(basePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if path == basePath {
			return nil
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(basePath, path)
		if err != nil {
			return nil
		}
		display := filepath.ToSlash(rel)
		score := scoreEntry(display, query, d.IsDir())
		if strings.TrimSpace(query) == "" {
			score = 1
		}
		if score <= 0 {
			return nil
		}
		label := d.Name() + dirSuffix(d.IsDir())
		value := display + dirSuffix(d.IsDir())
		matches = append(matches, scored{item: Item{Value: value, Label: label, Description: display}, score: score})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].item.Description < matches[j].item.Description
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
