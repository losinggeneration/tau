package autocomplete

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWalkFileSearcherLimitOnlyNarrowsRequestedResults(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		mustWrite(t, filepath.Join(dir, name), "body")
	}
	for _, tc := range []struct{ configured, requested, want int }{{3, 2, 2}, {1, 2, 1}, {0, 2, 2}, {2, 0, 2}} {
		got, err := (WalkFileSearcher{Limit: tc.configured}).Search(t.Context(), dir, "", tc.requested)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Fatalf("configured=%d requested=%d results=%d want=%d", tc.configured, tc.requested, len(got), tc.want)
		}
	}
}

func TestWalkFileSearcherExcludesGitFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".git"), "gitdir: elsewhere")
	mustMkdir(t, filepath.Join(dir, "nested", ".git"))
	mustWrite(t, filepath.Join(dir, "nested", ".git", "config"), "body")
	mustWrite(t, filepath.Join(dir, "nested", "visible"), "body")
	got, err := (WalkFileSearcher{}).Search(t.Context(), dir, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != "nested/" || got[1].Value != "nested/visible" {
		t.Fatalf("results=%+v", got)
	}
}

func TestWalkFileSearcherFollowsSymlinksWithoutLosingAliases(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "real"))
	mustWrite(t, filepath.Join(root, "real", "note.md"), "body")
	mustWrite(t, filepath.Join(outside, "external.md"), "outside")
	for _, link := range []struct{ target, name string }{{"real", "alias"}, {outside, "external"}, {filepath.Join(outside, "external.md"), "file.md"}, {"missing", "broken"}, {root, filepath.Join("real", "cycle")}} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, base := range []string{root, filepath.Join(t.TempDir(), "base-link")} {
		if base != root {
			if err := os.Symlink(root, base); err != nil {
				t.Fatal(err)
			}
		}
		got, err := (WalkFileSearcher{}).Search(t.Context(), base, ".md", 20)
		if err != nil {
			t.Fatal(err)
		}
		var values []string
		for _, item := range got {
			values = append(values, item.Value)
		}
		slices.Sort(values)
		want := []string{"alias/note.md", "external/external.md", "file.md", "real/note.md"}
		if !slices.Equal(values, want) {
			t.Fatalf("base=%q results=%v want=%v", base, values, want)
		}
	}
}

func TestWalkFileSearcherHonorsLocalIgnoreRules(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{".git", "nested", "pruned"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{".gitignore": "*.md\n!visible.md\npruned/\n/root-only.txt\n", ".ignore": "!priority.md\n", ".fdignore": "priority.md\n!fd-keep.md\n", "visible.md": "", "root-only.txt": "", "blocked.md": "", "priority.md": "", "fd-keep.md": "", "nested/.ignore": "!nested.md\n/local.txt\n", "nested/nested.md": "", "nested/blocked.md": "", "nested/local.txt": "", "nested/root-only.txt": "", "pruned/leak.md": "", "pruned/.fdignore": "!leak.md\n", ".hidden.txt": ""} {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(name)), body)
	}
	got, err := (WalkFileSearcher{}).Search(t.Context(), dir, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, item := range got {
		values = append(values, item.Value)
	}
	slices.Sort(values)
	want := []string{".fdignore", ".gitignore", ".hidden.txt", ".ignore", "fd-keep.md", "nested/", "nested/.ignore", "nested/nested.md", "nested/root-only.txt", "visible.md"}
	slices.Sort(want)
	if !slices.Equal(values, want) {
		t.Fatalf("values=%q want=%q", values, want)
	}
}

func TestWalkFileSearcherReportsIgnoreReadErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".ignore"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := (WalkFileSearcher{}).Search(t.Context(), dir, "", 20)
	if err == nil {
		t.Fatal("want ignore read error")
	}
	if got != nil {
		t.Fatalf("partial suggestions=%v", got)
	}
}

func TestWalkFileSearcherTreatsWhitespaceQueryAsLiteral(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.md", "with space.md", "with\u00a0space.md", "with\ufeffspace.md"} {
		mustWrite(t, filepath.Join(dir, name), "")
	}
	for _, query := range []string{" ", "\u00a0", "\ufeff"} {
		t.Run(query, func(t *testing.T) {
			got, err := (WalkFileSearcher{}).Search(t.Context(), dir, query, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Value != "with"+query+"space.md" {
				t.Fatalf("query=%q got=%+v", query, got)
			}
		})
	}
}
