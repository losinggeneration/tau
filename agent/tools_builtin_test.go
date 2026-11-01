package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/losinggeneration/tau/agent/fmqueue"
	"github.com/losinggeneration/tau/ai"
)

func TestApplyEditsExactMatch(t *testing.T) {
	content := "hello world\nhello world\n"
	edits := []EditEdit{{OldText: "hello", NewText: "HI", ReplaceAll: true}}
	_, newContent, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newContent != "HI world\nHI world\n" {
		t.Errorf("newContent = %q, want %q", newContent, "HI world\nHI world\n")
	}
}

func TestApplyEditsReplaceAllSingleEdit(t *testing.T) {
	content := "foo bar foo baz foo\n"
	edits := []EditEdit{{OldText: "foo", NewText: "FOO", ReplaceAll: true}}
	_, newContent, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newContent != "FOO bar FOO baz FOO\n" {
		t.Errorf("newContent = %q, want %q", newContent, "FOO bar FOO baz FOO\n")
	}
}

func TestApplyEditsReplaceAllNoMatch(t *testing.T) {
	// ReplaceAll with no occurrences should error, not silently succeed.
	content := "no occurrences here\n"
	edits := []EditEdit{{OldText: "missing", NewText: "X", ReplaceAll: true}}
	_, _, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err == nil {
		t.Fatal("expected error for replaceAll with no matches, got nil")
	}
	if !strings.Contains(err.Error(), "could not find") {
		t.Errorf("error = %q, want substring 'could not find'", err.Error())
	}
}

func TestApplyEditsAmbiguousReturnsStructuredError(t *testing.T) {
	content := "foo bar foo baz\n"
	edits := []EditEdit{{OldText: "foo", NewText: "X"}}
	_, _, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err == nil {
		t.Fatal("expected error for ambiguous match, got nil")
	}

	var ambig *ErrEditAmbiguousPayload
	if !errors.As(err, &ambig) {
		t.Fatalf("expected *ErrEditAmbiguousPayload, got %T: %v", err, err)
	}
	if ambig.MatchCount != 2 {
		t.Errorf("MatchCount = %d, want 2", ambig.MatchCount)
	}
	if ambig.Path != "test.txt" {
		t.Errorf("Path = %q, want test.txt", ambig.Path)
	}
	if len(ambig.SampleLines) == 0 {
		t.Error("SampleLines should be non-empty")
	}
}

func TestApplyEditsAmbiguousMessageMentionsReplaceAll(t *testing.T) {
	content := "foo bar foo\n"
	edits := []EditEdit{{OldText: "foo", NewText: "X"}}
	_, _, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The structured error's Error() should mention replace_all so users
	// can find the fix.
	if !strings.Contains(err.Error(), "2 locations") {
		t.Errorf("error = %q, want substring '2 locations'", err.Error())
	}
}

func TestApplyEditsReplaceAllMixedWithSingleMatch(t *testing.T) {
	// One edit with multiple matches + replaceAll, plus one edit with a
	// unique match. Both should be applied.
	content := "foo bar foo baz qux\n"
	edits := []EditEdit{
		{OldText: "foo", NewText: "FOO", ReplaceAll: true},
		{OldText: "qux", NewText: "QUX"},
	}
	_, newContent, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newContent != "FOO bar FOO baz QUX\n" {
		t.Errorf("newContent = %q, want %q", newContent, "FOO bar FOO baz QUX\n")
	}
}

func TestApplyEditsFuzzyBasisIsConsistent(t *testing.T) {
	// When one edit needs fuzzy normalization, the basis must be uniform
	// across all edits so that subsequent exact matches still find their
	// targets. The previous bug mutated `content` mid-loop, so the second
	// edit saw a different basis than the first.
	//
	// Content has trailing-whitespace differences that require fuzzy
	// normalization for the first edit; the second edit is a unique
	// substring that should also be found in the (now-fuzzy) basis.
	//
	// Fuzzy match preserves the original content's bytes; it only changes
	// how matches are located. The replacement text replaces the matched
	// region in the original content (which still includes the trailing
	// spaces for the first match).
	content := "alpha   \nbeta\n" // "alpha" has trailing spaces
	edits := []EditEdit{
		{OldText: "alpha", NewText: "ALPHA"},
		{OldText: "beta", NewText: "BETA"},
	}
	_, newContent, err := applyEditsToNormalizedContent(content, edits, "test.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newContent != "ALPHA   \nBETA\n" {
		t.Errorf("newContent = %q, want %q (fuzzy match preserves original bytes)", newContent, "ALPHA   \nBETA\n")
	}
}

func TestGenerateDiffStringSingleLineChangeKeepsFollowingContextUnchanged(t *testing.T) {
	oldContent := strings.Join([]string{
		"func Test() {",
		"\teventDone := make(chan struct{})",
		"\t// this is a test",
		"\tgo func() {",
		"\t\tdefer close(eventDone)",
		"\t\tfor evt := range c.Events() {",
		"\t\t\teventTypes[evt.Type]++",
		"\t\t}",
		"\t}()",
		"}",
	}, "\n")
	newContent := strings.Replace(oldContent, "\t// this is a test", "\t// this is a test.", 1)

	diff, _ := generateDiffString(oldContent, newContent)

	if !strings.Contains(diff, "- 3 \t// this is a test") {
		t.Fatalf("diff missing removed comment line:\n%s", diff)
	}
	if !strings.Contains(diff, "+ 3 \t// this is a test.") {
		t.Fatalf("diff missing added comment line:\n%s", diff)
	}
	for _, unexpected := range []string{
		"+ 4 \tgo func() {",
		"+ 5 \t\tdefer close(eventDone)",
		"+ 6 \t\tfor evt := range c.Events() {",
		"+ 7 \t\t\teventTypes[evt.Type]++",
	} {
		if strings.Contains(diff, unexpected) {
			t.Fatalf("diff marked unchanged following line as added %q:\n%s", unexpected, diff)
		}
	}
}

// A diff of a large file must not cost memory quadratic in its line count:
// a full LCS table for a 30k-line file is several gigabytes.
func TestGenerateDiffStringLargeFileStaysLinear(t *testing.T) {
	lines := make([]string, 5000)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	oldContent := strings.Join(lines, "\n")
	lines[10] = "changed near top"
	lines[4990] = "changed near bottom"
	newContent := strings.Join(lines, "\n")

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	diff, first := generateDiffString(oldContent, newContent)
	runtime.ReadMemStats(&after)

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 20<<20 {
		t.Fatalf("diff allocated %d MiB for a 5000-line file", alloc>>20)
	}
	if first != 11 {
		t.Fatalf("first changed line = %d, want 11", first)
	}
	for _, want := range []string{"-  11 line 10", "+  11 changed near top", "-4991 line 4990", "+4991 changed near bottom", "  12 line 11"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q", want)
		}
	}
	if n := strings.Count(diff, "\n") + 1; n > 25 {
		t.Fatalf("diff has %d lines, want bounded context", n)
	}
}

func TestApplyEditsLegacyReplaceAllDefault(t *testing.T) {
	// Legacy old_string/new_string callers get replaceAll from the top-level
	// replace_all flag (default false on the struct literal). Verify the
	// EditEdit.ReplaceAll value flows through.
	edit := EditEdit{OldText: "foo", NewText: "FOO", ReplaceAll: true}
	if !edit.ReplaceAll {
		t.Error("ReplaceAll not set on struct literal")
	}
}

func TestSampleLineNumbers(t *testing.T) {
	content := "a\nb\nc\nfoo\ne\nf\nfoo\n"
	// 1-based line numbers of "foo" occurrences: first 'foo' is on
	// line 4 (after 3 newlines), second 'foo' is on line 7 (after 6
	// newlines, the last one being the '\n' after "f").
	// Occurrence byte offsets point to the start of each "foo".
	occurrences := []int{6, 12}
	lines := sampleLineNumbers(content, occurrences, 5)
	// Note: offset 12 is the 'f' of "f\nfoo" — which is on line 6
	// (5 newlines precede it). The "foo" text itself begins at offset
	// 13, which is on line 7. The function returns the line of the
	// offset, so the second match is reported as line 6.
	want := []int{4, 6}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i, l := range lines {
		if l != want[i] {
			t.Errorf("lines[%d] = %d, want %d", i, l, want[i])
		}
	}
}

func TestSampleLineNumbersLimit(t *testing.T) {
	content := "a\nb\nc\n"
	occurrences := []int{0, 2, 4}
	lines := sampleLineNumbers(content, occurrences, 2)
	if len(lines) != 2 {
		t.Errorf("len(lines) = %d, want 2", len(lines))
	}
}

func TestGrepParityRipgrepVsPureGo(t *testing.T) {
	// Create a temp directory with a fixed corpus
	tmp := t.TempDir()

	files := map[string]string{
		"file1.txt":     "hello world\nfoo bar\nbaz qux\n",
		"file2.txt":     "Hello World\nfoo bar\nanother line\n",
		"file3.txt":     "no matches here\njust some text\n",
		"sub/file4.txt": "hello again\nfoo BAR\n",
	}

	for name, content := range files {
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testCases := []struct {
		name            string
		pattern         string
		glob            string
		caseInsensitive bool
	}{
		{"literal match", "foo bar", "", false},
		{"case insensitive", "hello", "", true},
		{"substring", "bar", "", false},
		{"no match", "notexist", "", false},
		{"with glob", "foo", "*.txt", false},
		{"regex", "hel+o", "", false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rgResult, _ := grepWithRG(t.Context(), "rg", tc.pattern, tmp, tc.glob, tc.caseInsensitive, 200, 0)
			pureResult, _ := grepPureGo(t.Context(), tc.pattern, tmp, tc.glob, tc.caseInsensitive, 200, 0)

			// Both should succeed
			if rgResult.IsError && !strings.Contains(rgResult.Content, "rg") {
				// rg not found is ok, skip this test case
				t.Skip("ripgrep not available")
			}

			// Normalize output for comparison
			rgNorm := normalizeGrepOutput(rgResult.Content)
			pureNorm := normalizeGrepOutput(pureResult.Content)

			if rgNorm != pureNorm {
				t.Errorf("ripgrep and pure-go disagree:\nripgrep:\n%s\npure-go:\n%s", rgResult.Content, pureResult.Content)
			}
		})
	}
}

func TestGrepParityWithContext(t *testing.T) {
	tmp := t.TempDir()

	content := `line1: foo bar
line2: baz qux
line3: foo bar again
line4: something else
line5: foo bar end`
	if err := os.WriteFile(filepath.Join(tmp, "test.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, context := range []int{0, 1, 2} {
		t.Run(contextName(context), func(t *testing.T) {
			rgResult, _ := grepWithRG(t.Context(), "rg", "foo bar", tmp, "", false, 200, context)
			t.Logf("context=%d: ripgrep output:\n%s", context, rgResult.Content)
			pureResult, _ := grepPureGo(t.Context(), "foo bar", tmp, "", false, 200, context)
			t.Logf("context=%d: pure-go output:\n%s", context, pureResult.Content)

			if rgResult.IsError && !strings.Contains(rgResult.Content, "rg") {
				t.Skip("ripgrep not available")
			}

			// Compare only match lines (lines without context prefix)
			rgMatches := extractMatchLines(rgResult.Content)
			pureMatches := extractMatchLines(pureResult.Content)

			if rgMatches != pureMatches {
				t.Errorf("context=%d: ripgrep and pure-go have different match lines:\nripgrep matches:\n%s\npure-go matches:\n%s", context, rgMatches, pureMatches)
			}
		})
	}
}

func TestGrepParityMaxResults(t *testing.T) {
	tmp := t.TempDir()

	// Create file with many matches
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("match\n")
	}
	if err := os.WriteFile(filepath.Join(tmp, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, max := range []int{5, 10, 50} {
		t.Run(contextName(max), func(t *testing.T) {
			rgResult, _ := grepWithRG(t.Context(), "rg", "match", tmp, "", false, max, 0)
			pureResult, _ := grepPureGo(t.Context(), "match", tmp, "", false, max, 0)

			if rgResult.IsError && !strings.Contains(rgResult.Content, "rg") {
				t.Skip("ripgrep not available")
			}

			// Normalize both outputs for comparison
			rgNorm := normalizeGrepOutput(rgResult.Content)
			pureNorm := normalizeGrepOutput(pureResult.Content)

			if rgNorm != pureNorm {
				t.Errorf("max=%d: ripgrep and pure-go disagree:\nripgrep:\n%s\npure-go:\n%s", max, rgResult.Content, pureResult.Content)
			}
		})
	}
}

// extractMatchLines extracts only the actual match lines (lines where the content
// after the line number doesn't start with "-", which is the context prefix).
// It also filters out ripgrep's context format: /path/file:line-content:content
func extractMatchLines(output string) string {
	if output == "No matches found" || strings.TrimSpace(output) == "" {
		return output
	}

	lines := strings.Split(output, "\n")
	var matchLines []string

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Parse file:line:content to check for context prefix in content
		colonIdx := strings.LastIndex(line, ":")
		if colonIdx < 0 {
			continue
		}
		content := line[colonIdx+1:]

		// Skip context lines (content starts with "-" for pure-go format)
		if strings.HasPrefix(content, "-") {
			continue
		}

		// Skip ripgrep context lines: filename contains "-<digit>-" pattern
		// e.g., /path/test.txt-2-line2 or /path/test.txt:1-3:content
		if isRipgrepContextLine(line) {
			continue
		}

		// Skip pure-go context lines: fileAndLine contains ":-" pattern
		// Pure-go format is "file:line:-content" for context lines
		if isPureGoContextLine(line) {
			continue
		}

		matchLines = append(matchLines, line)
	}

	slices.Sort(matchLines)
	return strings.Join(matchLines, "\n")
}

// isPureGoContextLine detects pure-go context line format.
// Pure-go outputs context lines with a "-" prefix after the line number.
// Format: "file:lineNumber:-lineContent" where the ":-" indicates context.
// This function parses the output line to detect this format.
func isPureGoContextLine(fullLine string) bool {
	// For pure-go format: file:lineNumber:[prefix]content
	// where [prefix] is "" for match lines and "-" for context lines.
	// Content itself may contain colons, so we need to parse carefully.
	//
	// Approach: Find the line number part and check if content prefix starts with "-"
	//
	// Example: /path/file:3:-line3: content
	// - Last colon splits: "/path/file:3:-line3" | "content"
	// - Need to check if "-line3" starts with "-" (it does, so context)
	//
	// Example: /path/file:3:line3: content
	// - Last colon splits: "/path/file:3:line3" | "content"
	// - Need to check if "line3" starts with "-" (it doesn't, so match)

	// Find the last colon (before content)
	lastColon := strings.LastIndex(fullLine, ":")
	if lastColon <= 0 {
		return false
	}

	fileAndLine := fullLine[:lastColon]
	// content is the part after last colon, but we need to look at the part
	// between line number and content (the prefix)

	// Now find the first colon in fileAndLine (after the filename)
	// This separates file from line:content
	firstColonAfterFile := strings.Index(fileAndLine, ":")
	if firstColonAfterFile < 0 {
		return false
	}

	// The part after this colon is "lineNumber:[prefix]content"
	// We need to find where lineNumber ends
	rest := fileAndLine[firstColonAfterFile+1:]

	// The line number is the first sequence of digits
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}

	if i == 0 {
		// No line number found
		return false
	}

	// Now check if what follows the line number starts with "-" (context prefix)
	// After extracting line number, rest[i:] is either ":" (match) or ":-" (context)
	// We need to skip the ":" and check if next char is "-"
	remaining := rest[i:]
	if len(remaining) > 0 && remaining[0] == ':' {
		remaining = remaining[1:]
	}
	return len(remaining) > 0 && remaining[0] == '-'
}

// isRipgrepContextLine detects ripgrep context line format in the file:line part.
// Ripgrep outputs context lines like:
//   - /path/file-2-line2:content (filename has -lineNumber suffix)
//   - /path/file:1-3:content (line number has range like 1-3)
func isRipgrepContextLine(fileAndLine string) bool {
	// Find the line number part (after last colon before content would be)
	// First, find the last colon to get file:line
	lastColon := strings.LastIndex(fileAndLine, ":")
	if lastColon < 0 {
		// No colon in fileAndLine - check if it's a ripgrep context line
		// Ripgrep context format: /path/file-line:content
		// where filename ends with "-<number>-<text>" pattern like "test.txt-2-line2"
		return isRipgrepContextFilename(fileAndLine)
	}
	filePart := fileAndLine[:lastColon]
	linePart := fileAndLine[lastColon+1:]

	// Check for ripgrep range format: "1-3" means lines 1 through 3
	if strings.Contains(linePart, "-") {
		parts := strings.Split(linePart, "-")
		if len(parts) >= 2 && isNumeric(parts[0]) && isNumeric(parts[1]) {
			return true // Range like "1-3"
		}
	}

	// Check filename part for ripgrep context pattern
	return isRipgrepContextFilename(filePart)
}

// isRipgrepContextFilename checks if the path/filename contains ripgrep's
// context marker pattern: "-<number>-<text>" like "file.txt-2-line2"
func isRipgrepContextFilename(path string) bool {
	// Get the filename (after last slash)
	lastSlash := strings.LastIndex(path, "/")
	filename := path
	if lastSlash >= 0 {
		filename = path[lastSlash+1:]
	}

	// Look for pattern where dash-digit-dash-letters appears
	// e.g., "test.txt-2-line2" has "-2-line2"
	for i := 0; i < len(filename); i++ {
		if filename[i] == '-' && i+1 < len(filename) {
			// Check if followed by digit(s)
			j := i + 1
			for j < len(filename) && filename[j] >= '0' && filename[j] <= '9' {
				j++
			}
			if j > i+1 { // Found at least one digit
				// Check if followed by dash-letter pattern
				if j < len(filename) && filename[j] == '-' && j+1 < len(filename) {
					// Check if what follows starts with a letter
					if (filename[j+1] >= 'a' && filename[j+1] <= 'z') ||
						(filename[j+1] >= 'A' && filename[j+1] <= 'Z') {
						return true
					}
				}
			}
		}
	}

	return false
}

// isNumeric checks if a string is purely numeric
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// normalizeGrepOutput removes file ordering and path differences while
// preserving the structural content of grep output.
func normalizeGrepOutput(output string) string {
	// Handle "No matches found" case
	if output == "No matches found" || strings.TrimSpace(output) == "" {
		return output
	}

	// Parse each line as file:linenum:content
	lines := strings.Split(output, "\n")
	var entries []string

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Parse file:line:content pattern
		file, linenum, content := parseGrepLine(line)
		if file != "" {
			entries = append(entries, file+":"+linenum+":"+content)
		}
	}

	// Sort to normalize ordering
	slices.Sort(entries)

	// Rebuild output with newlines between entries
	normalized := strings.Join(entries, "\n")
	return normalized
}

// parseGrepLine parses a grep output line into (file, line, content).
// Handles ripgrep format: /path/file:line:content
func parseGrepLine(line string) (file, linenum, content string) {
	// Find the last colon to separate line number from content
	// Format is: /path/to/file:line:content where content may contain colons
	colonIdx := strings.LastIndex(line, ":")
	if colonIdx < 0 {
		return "", "", line
	}
	content = line[colonIdx+1:]
	remaining := line[:colonIdx]

	// Now find the line number (second to last colon)
	colonIdx = strings.LastIndex(remaining, ":")
	if colonIdx < 0 {
		return remaining, "", content
	}
	linenum = remaining[colonIdx+1:]
	file = remaining[:colonIdx]

	return file, linenum, content
}

func contextName(n int) string {
	return "limit_" + string(rune('0'+n%10))
}

func TestReadToolPublishesOffsetAndRejectsPastEOF(t *testing.T) {
	tool := ReadTool(ToolConfig{})
	properties := tool.Parameters["properties"].(map[string]any)
	if _, ok := properties["offset"]; !ok {
		t.Error("offset is accepted at execution but missing from the schema")
	}
	for _, tc := range []struct {
		body    string
		offset  float64
		want    string
		isError bool
	}{
		{"one\ntwo", 2, "two", false},
		{"one\ntwo", 3, "Offset 3 is beyond end of file (2 lines total)", true},
		{"one\ntwo\n", 1, "one\ntwo\n", false},
		{"one\ntwo\n", 3, "", false},
		{"", 1, "", false},
		{"", 2, "Offset 2 is beyond end of file (1 lines total)", true},
	} {
		path := filepath.Join(t.TempDir(), "input.txt")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := tool.Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "offset": tc.offset}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Content != tc.want || result.IsError != tc.isError {
			t.Errorf("read %q at %v = (%q,error=%v), want (%q,error=%v)", tc.body, tc.offset, result.Content, result.IsError, tc.want, tc.isError)
		}
	}
}

func TestReadToolAlwaysBoundsSelectedText(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		args             map[string]any
		cap              int
		want, notice, by string
		outputLines      int
	}{
		{"explicit offset", "a\nb\nc\nd", map[string]any{"offset": float64(2)}, 2, "b\nc", "Use offset=4 to continue.", "lines", 2},
		{"explicit end", "a\nb\nc\nd", map[string]any{"end_line": float64(4)}, 2, "a\nb", "Use offset=3 to continue.", "lines", 2},
		{"large user limit", "a\nb\nc\nd", map[string]any{"limit": float64(4)}, 2, "a\nb", "Use offset=3 to continue.", "lines", 2},
		{"byte ceiling", "a\n" + strings.Repeat("界", 18000) + "\nend", nil, 2000, "a", "50.0KB limit", "bytes", 1},
		{"oversized first line", strings.Repeat("x", DefaultMaxBytes+1), nil, 2000, "", "Use bash:", "bytes", 0},
		{"user limit", "a\nb\nc", map[string]any{"limit": float64(1)}, 2000, "a", "[2 more lines in file. Use offset=2 to continue.]", "", 0},
		{"zero limit", "a\nb\nc", map[string]any{"limit": float64(0)}, 2000, "", "[3 more lines in file. Use offset=1 to continue.]", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "read.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"path": path}
			for k, v := range tc.args {
				args[k] = v
			}
			result, err := ReadTool(ToolConfig{ReadLineCap: tc.cap}).Execute(ToolContext{Context: t.Context(), Args: args})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || !strings.HasPrefix(result.Content, tc.want) || !strings.Contains(result.Content, tc.notice) {
				t.Errorf("unexpected read result %q, error=%v", result.Content, result.IsError)
			}
			if len(result.Content) > DefaultMaxBytes+512 {
				t.Errorf("read output has %d bytes", len(result.Content))
			}
			if tc.by != "" {
				details, ok := result.Details.(map[string]any)
				if !ok {
					t.Fatalf("missing truncation details: %+v", result.Details)
				}
				truncation, ok := details["truncation"].(*TruncationResult)
				if !ok {
					t.Fatalf("unexpected truncation details: %+v", details)
				}
				if !truncation.Truncated || truncation.TruncatedBy != tc.by || truncation.OutputLines != tc.outputLines || truncation.MaxBytes != DefaultMaxBytes {
					t.Errorf("truncation = %+v", truncation)
				}
			}
		})
	}
}

func TestTruncateReadHeadKeepsWholeLinesAndExactByteBoundary(t *testing.T) {
	for _, content := range []string{"", "a\n", strings.Repeat("x", DefaultMaxBytes), strings.Repeat("x", DefaultMaxBytes-1) + "\n"} {
		got := truncateReadHead(content, 2000)
		if got.Truncated || got.Content != content || got.OutputBytes != len(content) {
			t.Errorf("unchanged %d-byte input: %+v", len(content), got)
		}
	}
	got := truncateReadHead("界\n"+strings.Repeat("x", DefaultMaxBytes), 2000)
	if got.Content != "界" || got.OutputBytes != 3 || got.OutputLines != 1 || got.TotalLines != 2 || got.TotalBytes != DefaultMaxBytes+4 || got.TruncatedBy != "bytes" {
		t.Errorf("whole UTF-8 lines: %+v", got)
	}
	got = truncateReadHead(strings.Repeat("x", DefaultMaxBytes+1), 2000)
	if !got.FirstLineExceedsLimit || got.Content != "" || got.OutputLines != 0 || got.OutputBytes != 0 {
		t.Errorf("oversized first line: %+v", got)
	}
	got = truncateReadHead("a\nb\n", 1)
	if got.Content != "a" || got.TotalLines != 2 || got.OutputLines != 1 || got.TruncatedBy != "lines" {
		t.Errorf("line limit: %+v", got)
	}
}

func TestReadToolReturnsStructuredContent(t *testing.T) {
	tool := ReadTool(ToolConfig{ReadLineCap: 1})
	if tool.OutputSchema == nil {
		t.Error("read must publish its output schema")
	}
	dir := t.TempDir()
	textPath := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(textPath, []byte("first\nsecond"), 0o600); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "image.png")
	imageData := makeTestPNG(t, 2, 2)
	if err := os.WriteFile(imagePath, imageData, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		cfg        ToolConfig
		vision     bool
		wantError  bool
	}{
		{"text", textPath, ToolConfig{}, false, false},
		{"truncated text", textPath, ToolConfig{ReadLineCap: 1}, false, false},
		{"directory", dir, ToolConfig{}, false, false},
		{"image", imagePath, ToolConfig{}, true, false},
		{"blocked image", imagePath, ToolConfig{BlockImages: true}, true, false},
		{"missing", filepath.Join(dir, "missing"), ToolConfig{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ReadTool(tc.cfg).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": tc.path}, Capabilities: ai.Capabilities{Vision: tc.vision}})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != tc.wantError {
				t.Fatalf("error=%v: %s", result.IsError, result.Content)
			}
			if tc.wantError {
				if result.StructuredContent != nil {
					t.Fatalf("failed read returned structured content: %s", result.StructuredContent)
				}
				return
			}
			var output any
			if err := json.Unmarshal(result.StructuredContent, &output); err != nil {
				t.Fatalf("invalid structured content: %v", err)
			}
			if tc.name == "image" {
				image, ok := output.(map[string]any)
				if !ok {
					t.Fatalf("image output = %#v", output)
				}
				if image["type"] != "image" || image["mimeType"] != "image/png" || image["data"] != base64.StdEncoding.EncodeToString(imageData) || image["note"] != result.Content {
					t.Errorf("image output = %#v", image)
				}
			} else if output != result.Content {
				t.Errorf("structured output = %#v, want %q", output, result.Content)
			}
		})
	}
}

type heldWriteOperations struct {
	DefaultWriteOperations
	entered chan struct{}
	release chan struct{}
}

func (o heldWriteOperations) WriteFile(path string, content []byte, perm fs.FileMode) error {
	close(o.entered)
	<-o.release
	return o.DefaultWriteOperations.WriteFile(path, content, perm)
}

type observedEditOperations struct {
	DefaultEditOperations
	read chan struct{}
}

func (o observedEditOperations) ReadFile(path string) ([]byte, error) {
	close(o.read)
	return o.DefaultEditOperations.ReadFile(path)
}

func TestDefaultMutationQueueSerializesWriteAndEditAcrossFactories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered, release, read := make(chan struct{}), make(chan struct{}), make(chan struct{})
	writer := WriteTool(ToolConfig{WriteOps: heldWriteOperations{entered: entered, release: release}})
	editor := EditTool(ToolConfig{EditOps: observedEditOperations{read: read}})
	var wg sync.WaitGroup
	defer func() {
		close(release)
		wg.Wait()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		} else if string(content) != "edited" {
			t.Errorf("final content = %q, want edited", content)
		}
	}()
	wg.Go(func() {
		result, err := writer.Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "content": "written"}})
		if err != nil {
			t.Errorf("write: %v", err)
		} else if result.IsError {
			t.Errorf("write: %s", result.Content)
		}
	})
	<-entered
	wg.Go(func() {
		result, err := editor.Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "old_string": "written", "new_string": "edited"}})
		if err != nil {
			t.Errorf("edit: %v", err)
		} else if result.IsError {
			t.Errorf("edit: %s", result.Content)
		}
	})
	select {
	case <-read:
		t.Error("edit read the file before the in-flight write settled")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestToolConfigSharesDefaultMutationQueue(t *testing.T) {
	if (ToolConfig{}).mutationQueue() != (ToolConfig{}).mutationQueue() {
		t.Error("default tools must share a mutation queue across configuration rebuilds")
	}
	explicit := fmqueue.New()
	if (ToolConfig{MutationQueue: explicit}).mutationQueue() != explicit {
		t.Error("explicit queue was replaced")
	}
}

type cancellingMutationQueue struct{ cancel context.CancelFunc }

func (q cancellingMutationQueue) WithLock(_ string, fn func() error) error { q.cancel(); return fn() }

type cancellingEditOperations struct {
	DefaultEditOperations
	cancel context.CancelFunc
}

func (o cancellingEditOperations) ReadFile(path string) ([]byte, error) {
	data, err := o.DefaultEditOperations.ReadFile(path)
	o.cancel()
	return data, err
}

func TestMutationsObserveCancellationInsideTheQueue(t *testing.T) {
	for _, name := range []string{"write", "edit", "edit after read"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := ToolConfig{MutationQueue: cancellingMutationQueue{cancel: cancel}}
			tool := WriteTool(cfg)
			args := map[string]any{"path": path, "content": "changed"}
			if name != "write" {
				if name == "edit after read" {
					cfg = ToolConfig{EditOps: cancellingEditOperations{cancel: cancel}}
				}
				tool = EditTool(cfg)
				args = map[string]any{"path": path, "old_string": "original", "new_string": "changed"}
			}
			_, err := tool.Execute(ToolContext{Context: ctx, Args: args})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("mutation returned %v, want cancellation", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "original" {
				t.Errorf("cancelled mutation wrote %q", data)
			}
		})
	}
}

type cancellingWriteOperations struct {
	DefaultWriteOperations
	cancel context.CancelFunc
}

func (o cancellingWriteOperations) WriteFile(path string, data []byte, perm fs.FileMode) error {
	err := o.DefaultWriteOperations.WriteFile(path, data, perm)
	o.cancel()
	return err
}

type cancellingEditWriteOperations struct {
	DefaultEditOperations
	cancel context.CancelFunc
}

func (o cancellingEditWriteOperations) WriteFile(path string, data []byte, perm fs.FileMode) error {
	err := o.DefaultEditOperations.WriteFile(path, data, perm)
	o.cancel()
	return err
}

func TestMutationsReportCancellationAfterSettledWrite(t *testing.T) {
	for _, name := range []string{"write", "edit"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := ToolConfig{WriteOps: cancellingWriteOperations{cancel: cancel}, EditOps: cancellingEditWriteOperations{cancel: cancel}}
			tool := WriteTool(cfg)
			args := map[string]any{"path": path, "content": "changed"}
			if name == "edit" {
				tool = EditTool(cfg)
				args = map[string]any{"path": path, "old_string": "original", "new_string": "changed"}
			}
			_, err := tool.Execute(ToolContext{Context: ctx, Args: args})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("mutation error = %v, want cancellation", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "changed" {
				t.Errorf("already-settled write was rolled back: %q", data)
			}
		})
	}
}

func TestEditToolPreparesModelArgumentVariants(t *testing.T) {
	cases := []struct {
		name   string
		extra  map[string]any
		want   string
		reject bool
	}{
		{name: "array", extra: map[string]any{"edits": []any{map[string]any{"oldText": "first", "newText": "FIRST"}}}, want: "FIRST second\n"},
		{name: "string array", extra: map[string]any{"edits": `[{"oldText":"first","newText":"FIRST"}]`}, want: "FIRST second\n"},
		{name: "single object", extra: map[string]any{"edits": map[string]any{"oldText": "first", "newText": "FIRST"}}, want: "FIRST second\n"},
		{name: "string object", extra: map[string]any{"edits": `{"oldText":"first","newText":"FIRST"}`}, want: "FIRST second\n"},
		{name: "top level", extra: map[string]any{"oldText": "first", "newText": "FIRST"}, want: "FIRST second\n"},
		{name: "append top level", extra: map[string]any{"edits": []any{map[string]any{"oldText": "first", "newText": "FIRST"}}, "oldText": "second", "newText": "SECOND"}, want: "FIRST SECOND\n"},
		{name: "append to string array", extra: map[string]any{"edits": `[{"oldText":"first","newText":"FIRST"}]`, "oldText": "second", "newText": "SECOND"}, want: "FIRST SECOND\n"},
		{name: "tau legacy", extra: map[string]any{"old_string": "first", "new_string": "FIRST"}, want: "FIRST second\n"},
		{name: "malformed string", extra: map[string]any{"edits": "not json"}, reject: true},
		{name: "string scalar", extra: map[string]any{"edits": "42"}, reject: true},
		{name: "invalid single object", extra: map[string]any{"edits": map[string]any{"oldText": "first", "newText": 42}}, reject: true},
		{name: "invalid array entry", extra: map[string]any{"edits": `[{"oldText":"first","newText":{}}]`}, reject: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("first second\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := maps.Clone(tt.extra)
			args["path"] = path
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			var admitted int
			tool := EditTool(ToolConfig{})
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{ID: "edit", Name: "edit", Arguments: raw}, AgentLoopConfig{
				BeforeToolCall: func(_ context.Context, call ToolCallContext) (BeforeToolCallResult, error) {
					admitted++
					if _, ok := call.Args["oldText"]; ok {
						t.Error("top-level oldText reached the hook")
					}
					if _, ok := call.Args["newText"]; ok {
						t.Error("top-level newText reached the hook")
					}
					if _, legacy := call.Args["old_string"]; !legacy {
						if _, ok := call.Args["edits"].([]any); !ok {
							t.Errorf("hook edits = %T, want array", call.Args["edits"])
						}
					}
					return BeforeToolCallResult{}, nil
				},
			})
			if tt.reject {
				if !result.IsError || admitted != 0 {
					t.Errorf("invalid arguments admitted: %+v, hooks %d", result, admitted)
				}
			} else if result.IsError || result.Rejection != nil || admitted != 1 {
				t.Errorf("edit failed: %+v, hooks %d", result, admitted)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := tt.want
			if tt.reject {
				want = "first second\n"
			}
			if string(data) != want {
				t.Errorf("file = %q, want %q", data, want)
			}
			after, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, after) {
				t.Error("source arguments changed")
			}
		})
	}
}

func TestPrepareEditArgumentsDoesNotModifyTheSource(t *testing.T) {
	original := []any{map[string]any{"oldText": "a", "newText": "b"}, "sentinel"}
	args := map[string]any{"edits": original[:1], "oldText": "c", "newText": "d"}
	prepared := prepareEditArguments(args)
	if original[1] != "sentinel" {
		t.Error("preparation changed the source array backing store")
	}
	if args["oldText"] != "c" || args["newText"] != "d" || len(args["edits"].([]any)) != 1 {
		t.Errorf("source arguments changed: %#v", args)
	}
	if len(prepared["edits"].([]any)) != 2 {
		t.Errorf("prepared edits = %#v", prepared["edits"])
	}
	for _, value := range []any{"not json", "42", `{"oldText":"a"}`, nil} {
		result := prepareEditArguments(map[string]any{"edits": value})
		if result["edits"] != value {
			t.Errorf("invalid input %v was changed to %v", value, result["edits"])
		}
	}
}

func TestResolvePathNormalizesToolInputs(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	cwd := t.TempDir()
	for _, tt := range []struct{ input, want string }{
		{"@file.txt", filepath.Join(cwd, "file.txt")},
		{"@@file.txt", filepath.Join(cwd, "@file.txt")},
		{"~", home},
		{"@~", home},
		{"@~/file.txt", filepath.Join(home, "file.txt")},
		{"~draft.md", filepath.Join(cwd, "~draft.md")},
		{"@~draft.md", filepath.Join(cwd, "~draft.md")},
		{"./dir/../file.txt", filepath.Join(cwd, "file.txt")},
		{filepath.Join(cwd, "dir") + string(filepath.Separator) + ".." + string(filepath.Separator) + "file.txt", filepath.Join(cwd, "file.txt")},
		{" file.txt ", filepath.Join(cwd, " file.txt ")},
		{"file\tname.txt", filepath.Join(cwd, "file\tname.txt")},
		{"file\u1680name.txt", filepath.Join(cwd, "file\u1680name.txt")},
	} {
		if got, err := resolvePath(cwd, tt.input); err != nil || got != tt.want {
			t.Errorf("resolvePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
	for _, r := range "\u00a0\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u202f\u205f\u3000" {
		input := "file" + string(r) + "name.txt"
		if got, err := resolvePath(cwd, input); err != nil || got != filepath.Join(cwd, "file name.txt") {
			t.Errorf("resolvePath(%q) = %q", input, got)
		}
	}
}

func TestFileToolsShareInputPathNormalization(t *testing.T) {
	cwd := t.TempDir()
	write := WriteTool(ToolConfig{})
	result, err := write.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: map[string]any{"path": "@file\u00a0name.txt", "content": "before\n"}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("write = %+v, %v", result, err)
	}
	edit := EditTool(ToolConfig{})
	result, err = edit.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: map[string]any{"path": "@file\u2003name.txt", "old_string": "before", "new_string": "after"}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("edit = %+v, %v", result, err)
	}
	read := ReadTool(ToolConfig{})
	result, err = read.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: map[string]any{"path": "@file\u202fname.txt"}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("read = %+v, %v", result, err)
	}
	if result.Content != "after\n" {
		t.Errorf("read = %q", result.Content)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "file name.txt" {
		t.Errorf("unexpected files: %v", entries)
	}
}

func TestReadToolFindsFilenameVariantsInUpstreamOrder(t *testing.T) {
	cases := []struct {
		name, input string
		files       map[string]string
		want        string
		reject      bool
	}{
		{name: "exact", input: "file.txt", files: map[string]string{"file.txt": "exact"}, want: "exact"},
		{name: "NFD", input: "file\u00e9.txt", files: map[string]string{"filee\u0301.txt": "decomposed"}, want: "decomposed"},
		{name: "curly quote", input: "Capture d'ecran.txt", files: map[string]string{"Capture d\u2019ecran.txt": "curly"}, want: "curly"},
		{name: "combined", input: "Capture d'\u00e9cran.txt", files: map[string]string{"Capture d\u2019e\u0301cran.txt": "combined"}, want: "combined"},
		{name: "screenshot AM", input: "Screenshot 10.00.00 AM.txt", files: map[string]string{"Screenshot 10.00.00\u202fAM.txt": "morning"}, want: "morning"},
		{name: "screenshot pm", input: "Screenshot 10.00.00 pm.txt", files: map[string]string{"Screenshot 10.00.00\u202fpm.txt": "evening"}, want: "evening"},
		{name: "exact before curly", input: "Capture d'ecran.txt", files: map[string]string{"Capture d'ecran.txt": "exact", "Capture d\u2019ecran.txt": "curly"}, want: "exact"},
		{name: "screenshot before curly", input: "Capture d'ecran AM.txt", files: map[string]string{"Capture d'ecran\u202fAM.txt": "screenshot", "Capture d\u2019ecran AM.txt": "curly"}, want: "screenshot"},
		{name: "NFD before curly", input: "Capture d'\u00e9cran.txt", files: map[string]string{"Capture d'e\u0301cran.txt": "decomposed", "Capture d\u2019\u00e9cran.txt": "curly"}, want: "decomposed"},
		{name: "missing", input: "missing.txt", reject: true},
		{name: "no suffix match", input: "Screenshot AM", files: map[string]string{"Screenshot\u202fAM": "not a screenshot suffix"}, reject: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tool := ReadTool(ToolConfig{})
			result, err := tool.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: map[string]any{"path": tt.input}})
			if err != nil {
				t.Fatal(err)
			}
			if tt.reject {
				if !result.IsError {
					t.Errorf("unexpected read success: %+v", result)
				}
				return
			}
			if result.IsError || result.Content != tt.want {
				t.Errorf("read = %+v, want %q", result, tt.want)
			}
		})
	}
}

type checkedEditOperations struct {
	DefaultEditOperations
	calls     []string
	accessErr error
	cancel    context.CancelFunc
}

func (o *checkedEditOperations) Access(path string) error {
	o.calls = append(o.calls, "access")
	if o.cancel != nil {
		o.cancel()
	}
	return o.accessErr
}

func (o *checkedEditOperations) ReadFile(path string) ([]byte, error) {
	o.calls = append(o.calls, "read")
	return o.DefaultEditOperations.ReadFile(path)
}

func (o *checkedEditOperations) WriteFile(path string, data []byte, perm fs.FileMode) error {
	o.calls = append(o.calls, "write")
	return o.DefaultEditOperations.WriteFile(path, data, perm)
}

func TestEditToolChecksInjectedAccessBeforeReading(t *testing.T) {
	for _, outcome := range []string{"success", "denied", "cancelled", "denied and cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			denied := errors.New("backend access denied")
			ops := &checkedEditOperations{}
			if strings.Contains(outcome, "denied") {
				ops.accessErr = denied
			}
			if strings.Contains(outcome, "cancelled") {
				ops.cancel = cancel
			}
			tool := EditTool(ToolConfig{EditOps: ops})
			result, err := tool.Execute(ToolContext{Context: ctx, Args: map[string]any{"path": path, "old_string": "before", "new_string": "after"}})
			wantCalls := []string{"access"}
			wantText := "before"
			if outcome == "success" {
				wantCalls = append(wantCalls, "read", "write")
				wantText = "after"
				if err != nil || result == nil || result.IsError {
					t.Errorf("edit = %+v, %v", result, err)
				}
			} else if strings.Contains(outcome, "cancelled") {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("edit error = %v, want cancellation", err)
				}
				if ops.accessErr != nil && !errors.Is(err, denied) {
					t.Errorf("lost access error: %v", err)
				}
			} else if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content, denied.Error()) {
				t.Errorf("access error lost: %+v, %v", result, err)
			}
			if !slices.Equal(ops.calls, wantCalls) {
				t.Errorf("operations = %v, want %v", ops.calls, wantCalls)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != wantText {
				t.Errorf("file = %q, want %q", data, wantText)
			}
		})
	}
}

type cancellingReadOperations struct {
	DefaultReadOperations
	phase   string
	cancel  context.CancelFunc
	failure error
	calls   []string
}

func (o *cancellingReadOperations) Access(path string) (os.FileInfo, error) {
	o.calls = append(o.calls, "access")
	if o.phase == "access" {
		info, err := o.DefaultReadOperations.Access(path)
		o.cancel()
		return info, errors.Join(err, o.failure)
	}
	return o.DefaultReadOperations.Access(path)
}

func (o *cancellingReadOperations) ReadFile(path string) ([]byte, error) {
	o.calls = append(o.calls, "read")
	o.cancel()
	return []byte("late data"), o.failure
}

func (o *cancellingReadOperations) Ls(path string) ([]os.DirEntry, error) {
	o.calls = append(o.calls, "ls")
	o.cancel()
	return nil, o.failure
}

func TestReadToolStopsAfterCancellation(t *testing.T) {
	for _, phase := range []string{"before", "access", "read", "ls"} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/error=%v", phase, failed), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ops := &cancellingReadOperations{phase: phase, cancel: cancel}
				failure := errors.New("backend failed")
				if failed {
					ops.failure = failure
				}
				path := t.TempDir()
				if phase != "ls" {
					path = filepath.Join(path, "file.txt")
					if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before" {
					cancel()
				}
				tool := ReadTool(ToolConfig{ReadOps: ops})
				result, err := tool.Execute(ToolContext{Context: ctx, Args: map[string]any{"path": path}})
				if !errors.Is(err, context.Canceled) {
					t.Errorf("read = %+v, %v, want cancellation", result, err)
				}
				if failed && phase != "before" && !errors.Is(err, failure) {
					t.Errorf("lost backend failure: %v", err)
				}
				if result != nil {
					t.Errorf("cancelled read returned a result: %+v", result)
				}
				var want []string
				if phase != "before" {
					want = []string{"access"}
					if phase != "access" {
						want = append(want, phase)
					}
				}
				if !slices.Equal(ops.calls, want) {
					t.Errorf("operations = %v, want %v", ops.calls, want)
				}
			})
		}
	}
}

func TestEditToolPublishesPerEntryReplaceAll(t *testing.T) {
	tool := EditTool(ToolConfig{})
	properties := tool.Parameters["properties"].(map[string]any)
	items := properties["edits"].(map[string]any)["items"].(map[string]any)
	fields := items["properties"].(map[string]any)
	option, ok := fields["replaceAll"].(map[string]any)
	if !ok || option["type"] != "boolean" {
		t.Errorf("per-edit replaceAll schema = %#v", fields["replaceAll"])
	}
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("same same"), 0o600); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"path": path, "edits": []any{map[string]any{"oldText": "same", "newText": "changed", "replaceAll": all}}})
			if err != nil {
				t.Fatal(err)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{ID: "edit", Name: "edit", Arguments: raw}, AgentLoopConfig{})
			if result.IsError == all {
				t.Errorf("replaceAll=%v result = %+v", all, result)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "same same"
			if all {
				want = "changed changed"
			}
			if string(data) != want {
				t.Errorf("file = %q, want %q", data, want)
			}
		})
	}
}

func TestReadToolImageNoteSurvivesTranscriptFinalization(t *testing.T) {
	for _, width := range []int{2, 8} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.png")
			if err := os.WriteFile(path, makeTestPNG(t, width, 2), 0o600); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{ReadTool(ToolConfig{MaxImageWidth: 4, MaxImageHeight: 4})}, Capabilities: ai.Capabilities{Vision: true}}, nil, ai.ToolCall{ID: "image", Name: "read", Arguments: raw}, AgentLoopConfig{})
			if result.IsError {
				t.Fatal(result.Content)
			}
			result = finalizeToolResultMessage(ai.ToolCall{ID: "image", Name: "read"}, result, func(Event) {})
			if result.Message == nil {
				t.Fatal("read was not finalized")
			}
			blocks := result.Message.Content
			if len(blocks) != 2 {
				t.Fatalf("transcript content = %#v, want note then image", result.Message.Content)
			}
			if blocks[0].Type != ai.ContentBlockText || blocks[0].Text != result.Content || blocks[1].Type != ai.ContentBlockImage {
				t.Errorf("transcript blocks = %+v", blocks)
			}
			if width == 8 && !strings.Contains(blocks[0].Text, "resized from 8x2 to 4x1") {
				t.Errorf("resize note missing: %q", blocks[0].Text)
			}
			var output map[string]string
			if err := json.Unmarshal(result.StructuredContent, &output); err != nil {
				t.Fatal(err)
			}
			if output["note"] != blocks[0].Text || output["data"] != blocks[1].Data || output["mimeType"] != blocks[1].MimeType {
				t.Errorf("structured image differs from transcript: %#v", output)
			}
		})
	}
}

func TestEditToolPreservesBOMAndLineEndings(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bom.txt")
			original := "\ufefffirst" + ending + "second" + ending
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := EditTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "old_string": "first", "new_string": "changed"}})
			if err != nil || result.IsError {
				t.Fatalf("edit = %+v, %v", result, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "\ufeffchanged" + ending + "second" + ending
			if string(data) != want {
				t.Errorf("file = %q, want %q", data, want)
			}
		})
	}
}

func TestFuzzyEditsPreserveUntouchedLines(t *testing.T) {
	for _, tt := range []struct {
		name, content, want string
		edits               []EditEdit
	}{
		{"separate lines", "keep ‘one’  \ntarget ‘two’  \nkeep ‘three’\t\n", "keep ‘one’  \nchanged\nkeep ‘three’\t\n", []EditEdit{{OldText: "target 'two'", NewText: "changed"}}},
		{"same line", "keep ‘one’  \na ‘two’ b ‘three’  \ntail  ", "keep ‘one’  \na TWO b THREE\ntail  ", []EditEdit{{OldText: "‘two’", NewText: "TWO"}, {OldText: "'three'", NewText: "THREE"}}},
		{"multiline", "head\t\nfirst ‘one’  \nsecond ‘two’\t\ntail  \n", "head\t\nreplacement\nextra\ntail  \n", []EditEdit{{OldText: "first 'one'\nsecond 'two'", NewText: "replacement\nextra"}}},
		{"duplicate normalized neighbor", "same ‘quote’  \nmarker ‘start’\nsame ‘quote’\t\ntail  \n", "same ‘quote’  \nmarker 'start'\nchanged\ntail  \n", []EditEdit{{OldText: "marker 'start'\nsame 'quote'", NewText: "marker 'start'\nchanged"}}},
		{"adjacent touched lines", "head  \na ‘one’  \nb ‘two’\t\ntail  ", "head  \na ONE\nb TWO\ntail  ", []EditEdit{{OldText: "'one'", NewText: "ONE"}, {OldText: "'two'", NewText: "TWO"}}},
		{"replace all", "head  \n‘one’\t\nmiddle  \n‘one’  ", "head  \nONE\nmiddle  \nONE", []EditEdit{{OldText: "'one'", NewText: "ONE", ReplaceAll: true}}},
		{"delete newline", "head  \na ‘one’  \nb ‘two’  \ntail  \n", "head  \na ONEb ‘two’  \ntail  \n", []EditEdit{{OldText: "'one'\n", NewText: "ONE"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, got, err := applyEditsToNormalizedContent(tt.content, tt.edits, "test.txt")
			if err != nil {
				t.Fatal(err)
			}
			if base != tt.content {
				t.Errorf("diff base = %q, want original %q", base, tt.content)
			}
			if got != tt.want {
				t.Errorf("edited = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEditToolFuzzyMatchKeepsUnchangedFileBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	original := "\ufeffhead ‘quote’  \r\nchange ‘this’  \r\ntail\t\r\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := EditTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "old_string": "change 'this'", "new_string": "changed"}})
	if err != nil || result.IsError {
		t.Fatalf("edit = %+v, %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "\ufeffhead ‘quote’  \r\nchanged\r\ntail\t\r\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q", data, want)
	}
}

func TestFuzzyEditNormalizationMatchesUpstream(t *testing.T) {
	for _, tt := range []struct{ name, source, old, want string }{
		{"compatibility ligature", "head  \nﬁle  \ntail\t", "file", "head  \nchanged\ntail\t"},
		{"fullwidth", "head  \nＦＯＯ\ntail  ", "FOO", "head  \nchanged\ntail  "},
		{"composed accents", "head  \ncafe\u0301\ntail  ", "café", "head  \nchanged\ntail  "},
		{"vertical trailing space", "head  \nfirst\v\nsecond\ntail  ", "first\nsecond", "head  \nchanged\ntail  "},
		{"BOM trailing space", "head  \nfirst\ufeff\nsecond\ntail  ", "first\nsecond", "head  \nchanged\ntail  "},
		{"thin trailing space", "head  \nfirst\u2009\nsecond\ntail  ", "first\nsecond", "head  \nchanged\ntail  "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, got, err := applyEditsToNormalizedContent(tt.source, []EditEdit{{OldText: tt.old, NewText: "changed"}}, "file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if base != tt.source || got != tt.want {
				t.Errorf("base=%q, edited=%q, want %q", base, got, tt.want)
			}
		})
	}
	if got := normalizeForFuzzyMatch("one\u0085\ntwo"); got != "one\u0085\ntwo" {
		t.Errorf("non-JavaScript whitespace trimmed: %q", got)
	}
}

func TestEditToolReturnsApplicableUnifiedPatch(t *testing.T) {
	for _, ending := range []string{"\n", ""} {
		t.Run(fmt.Sprintf("ending=%q", ending), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("before"+ending), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := EditTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path, "old_string": "before", "new_string": "after"}})
			if err != nil || result.IsError {
				t.Fatalf("edit = %+v, %v", result, err)
			}
			details, ok := result.Details.(map[string]any)
			if !ok {
				t.Fatalf("details = %#v", result.Details)
			}
			patch, ok := details["patch"].(string)
			if !ok {
				t.Fatalf("missing patch: %#v", details)
			}
			missing := ""
			if ending == "" {
				missing = "\\ No newline at end of file\n"
			}
			want := "--- " + path + "\n+++ " + path + "\n@@ -1 +1 @@\n-before\n" + missing + "+after\n" + missing
			if patch != want {
				t.Errorf("patch = %q, want %q", patch, want)
			}
		})
	}
}

func TestGenerateDiffStringBoundsContext(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	old := strings.Join(lines, "\n") + "\n"
	lines[9] = "first change"
	lines[24] = "second change"
	diff, first := generateDiffString(old, strings.Join(lines, "\n")+"\n")
	want := strings.Join([]string{
		"    ...", "  6 line 6", "  7 line 7", "  8 line 8", "  9 line 9", "-10 line 10", "+10 first change",
		" 11 line 11", " 12 line 12", " 13 line 13", " 14 line 14", "    ...",
		" 21 line 21", " 22 line 22", " 23 line 23", " 24 line 24", "-25 line 25", "+25 second change",
		" 26 line 26", " 27 line 27", " 28 line 28", " 29 line 29", "    ...",
	}, "\n")
	if first != 10 || diff != want {
		t.Errorf("first=%d, diff:\n%s\nwant:\n%s", first, diff, want)
	}
	if got, _ := generateDiffString("before\n", "after\n"); got != "-1 before\n+1 after" {
		t.Errorf("phantom final line: %q", got)
	}
	if got, _ := generateDiffString(old, old); got != "" {
		t.Errorf("unchanged diff: %q", got)
	}
}

type cancellingLsOperations struct {
	DefaultLsOperations
	cancel  context.CancelFunc
	phase   string
	failure error
	calls   []string
}

func (o *cancellingLsOperations) Ls(path string) ([]os.DirEntry, error) {
	o.calls = append(o.calls, "ls")
	entries, err := o.DefaultLsOperations.Ls(path)
	if o.phase == "ls" {
		o.cancel()
		return entries, errors.Join(err, o.failure)
	}
	return entries, err
}

func (o *cancellingLsOperations) Stat(path string) (os.FileInfo, error) {
	o.calls = append(o.calls, "stat")
	info, err := o.DefaultLsOperations.Stat(path)
	o.cancel()
	return info, errors.Join(err, o.failure)
}

func TestLSToolStopsAfterCancellation(t *testing.T) {
	for _, phase := range []string{"before", "ls", "stat"} {
		for _, failure := range []error{nil, errors.New("backend failed")} {
			t.Run(fmt.Sprintf("%s/%v", phase, failure), func(t *testing.T) {
				dir := t.TempDir()
				for _, name := range []string{"a", "b"} {
					if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ops := &cancellingLsOperations{phase: phase, cancel: cancel, failure: failure}
				if phase == "before" {
					cancel()
				}
				result, err := LSTool(ToolConfig{LsOps: ops}).Execute(ToolContext{Context: ctx, SessionCWD: dir, Args: map[string]any{}})
				if !errors.Is(err, context.Canceled) || result != nil {
					t.Errorf("ls = %+v, %v", result, err)
				}
				if phase != "before" && failure != nil && !errors.Is(err, failure) {
					t.Errorf("lost backend error: %v", err)
				}
				var want []string
				if phase != "before" {
					want = []string{"ls"}
					if phase == "stat" {
						want = append(want, "stat")
					}
				}
				if !slices.Equal(ops.calls, want) {
					t.Errorf("operations = %v, want %v", ops.calls, want)
				}
			})
		}
	}
}

func TestLSToolSortsNamesCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "B", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			result, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{"reverse": reverse}})
			if err != nil || result.IsError {
				t.Fatalf("ls = %+v, %v", result, err)
			}
			var got []string
			for _, line := range strings.Split(result.Content, "\n") {
				fields := strings.Fields(line)
				got = append(got, fields[len(fields)-1])
			}
			want := []string{"a", "B", "c"}
			if reverse {
				slices.Reverse(want)
			}
			if !slices.Equal(got, want) {
				t.Errorf("names = %v, want %v", got, want)
			}
		})
	}
}

func TestLSToolKeepsEqualSortKeysStable(t *testing.T) {
	dir := t.TempDir()
	names := []string{"A", "B", "a", "c"}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(100, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, sortBy := range []string{"size", "mtime"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", sortBy, reverse), func(t *testing.T) {
				result, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{"sort_by": sortBy, "reverse": reverse}})
				if err != nil || result.IsError {
					t.Fatalf("ls = %+v, %v", result, err)
				}
				var got []string
				for _, line := range strings.Split(result.Content, "\n") {
					fields := strings.Fields(line)
					got = append(got, fields[len(fields)-1])
				}
				if !slices.Equal(got, names) {
					t.Errorf("equal keys reordered = %v, want %v", got, names)
				}
			})
		}
	}
}

type cancellingGrepOperations struct {
	ctx    context.Context
	cancel context.CancelFunc
	err    error
	called bool
}

func (o *cancellingGrepOperations) Grep(ctx context.Context, _, _, _ string, _ bool, _, _ int) (*ToolResult, error) {
	o.called = true
	o.ctx = ctx
	o.cancel()
	return &ToolResult{Content: "late match"}, o.err
}

type cancellingFindOperations struct {
	ctx    context.Context
	cancel context.CancelFunc
	err    error
	called bool
}

func (o *cancellingFindOperations) Find(ctx context.Context, _, _, _ string, _ int, _, _ bool, _ int) ([]string, error) {
	o.called = true
	o.ctx = ctx
	o.cancel()
	return []string{"late match"}, o.err
}

func TestSearchToolsDiscardCancelledResults(t *testing.T) {
	for _, name := range []string{"grep", "find"} {
		for _, phase := range []string{"before", "during"} {
			for _, failure := range []error{nil, errors.New("backend failed")} {
				t.Run(fmt.Sprintf("%s/%s/%v", name, phase, failure), func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					grep := &cancellingGrepOperations{cancel: cancel, err: failure}
					find := &cancellingFindOperations{cancel: cancel, err: failure}
					cfg := ToolConfig{GrepOps: grep, FindOps: find}
					tool := GrepTool(cfg)
					args := map[string]any{"pattern": "needle"}
					if name == "find" {
						tool = FindTool(cfg)
						args = map[string]any{"query": "needle"}
					}
					if phase == "before" {
						cancel()
					}
					result, err := tool.Execute(ToolContext{Context: ctx, Args: args})
					if result != nil || !errors.Is(err, context.Canceled) {
						t.Errorf("search = %+v, %v", result, err)
					}
					if phase == "during" && failure != nil && !errors.Is(err, failure) {
						t.Errorf("lost backend failure: %v", err)
					}
					if phase == "during" && grep.ctx != ctx && find.ctx != ctx {
						t.Error("backend did not receive caller context")
					}
					if called := grep.called || find.called; called != (phase == "during") {
						t.Errorf("backend called = %v", called)
					}
				})
			}
		}
	}
}

type searchCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *searchCancelContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestSearchBackendsObserveCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("needle\nneedle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"find", "grep-go", "grep-rg"} {
		for _, checks := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/check=%d", name, checks), func(t *testing.T) {
				base, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := &searchCancelContext{Context: base, cancel: cancel, remaining: checks}
				switch name {
				case "find":
					result, err := (DefaultFindOperations{}).Find(ctx, "*", dir, "glob", -1, true, true, 100)
					if result != nil || !errors.Is(err, context.Canceled) {
						t.Errorf("find = %v, %v", result, err)
					}
				case "grep-go":
					result, err := grepPureGo(ctx, "needle", dir, "", false, 100, 0)
					if result != nil || !errors.Is(err, context.Canceled) {
						t.Errorf("grep = %+v, %v", result, err)
					}
				case "grep-rg":
					// Cancellation at entry must not try to launch even a missing executable.
					if checks != 1 {
						return
					}
					result, err := grepWithRG(ctx, filepath.Join(dir, "missing-rg"), "needle", dir, "", false, 100, 0)
					if result != nil || !errors.Is(err, context.Canceled) {
						t.Errorf("rg = %+v, %v", result, err)
					}
				}
			})
		}
	}
}

func TestGrepWithRGMissingExecutableReturnsError(t *testing.T) {
	result, err := grepWithRG(t.Context(), filepath.Join(t.TempDir(), "missing-rg"), "needle", ".", "", false, 10, 0)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("rg = %+v, %v", result, err)
	}
}

func TestReadToolDecodesMalformedUTF8BeforeTruncation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"invalid leads", []byte{'a', 0xff, 0xff, 'b'}, "a\ufffd\ufffdb"},
		{"truncated prefix", []byte{'a', 0xe2, 0x82}, "a\ufffd"},
		{"bad continuation", []byte{0xe2, 0x82, 'x', 0xff}, "\ufffdx\ufffd"},
		{"valid text", []byte("café\n"), "café\n"},
		{"embedded BOM", []byte("a\ufeffb"), "a\ufeffb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "text.txt")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := ReadTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path}})
			if err != nil || result.IsError {
				t.Fatalf("read = %+v, %v", result, err)
			}
			if result.Content != tc.want {
				t.Errorf("content = %q, want %q", result.Content, tc.want)
			}
			var structured string
			if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
				t.Fatal(err)
			}
			if structured != tc.want {
				t.Errorf("structured = %q, want %q", structured, tc.want)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "large.txt")
	data := bytes.Repeat([]byte{0xff}, DefaultMaxBytes/3+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": path}})
	if err != nil || result.IsError {
		t.Fatalf("read = %+v, %v", result, err)
	}
	if !strings.Contains(result.Content, "exceeds") {
		t.Errorf("decoded byte ceiling not applied: %q", result.Content)
	}
}

func TestReadToolKeepsImagesForLaterVisionReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, makeTestPNG(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	for _, vision := range []bool{false, true} {
		t.Run(fmt.Sprint(vision), func(t *testing.T) {
			model := &ai.Model{Input: []ai.InputType{ai.InputText}}
			if vision {
				model.Input = append(model.Input, ai.InputImage)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{ReadTool(ToolConfig{})}, Model: model, Capabilities: ai.Capabilities{Vision: vision}}, nil, ai.ToolCall{ID: "read-image", Name: "read", Arguments: raw}, AgentLoopConfig{})
			if result.IsError {
				t.Fatalf("read failed: %s", result.Content)
			}
			result = finalizeToolResultMessage(ai.ToolCall{ID: "read-image", Name: "read"}, result, func(Event) {})
			if len(result.Message.Content) != 2 || result.Message.Content[1].Type != ai.ContentBlockImage {
				t.Fatalf("transcript = %+v", result.Message.Content)
			}
			note := "[Current model does not support images. The image will be omitted from this request.]"
			if strings.Contains(result.Content, note) == vision {
				t.Errorf("wrong model note: %q", result.Content)
			}
			var output map[string]string
			if err := json.Unmarshal(result.StructuredContent, &output); err != nil {
				t.Fatal(err)
			}
			if output["type"] != "image" || output["data"] != result.Message.Content[1].Data || output["note"] != result.Content {
				t.Errorf("structured = %v", output)
			}
			history := &ai.Context{Messages: []ai.Message{{Role: "toolResult", ToolCallID: "read-image", Content: result.Message.Content}}}
			textContext, err := ai.TransformContext(history, &ai.Model{Input: []ai.InputType{ai.InputText}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			blocks := textContext.Messages[0].Content.([]ai.ContentBlock)
			if len(blocks) != 2 || blocks[1].Type != ai.ContentBlockText {
				t.Errorf("nonvision request = %+v", blocks)
			}
			visionContext, err := ai.TransformContext(history, &ai.Model{Input: []ai.InputType{ai.InputText, ai.InputImage}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if visionContext.Messages[0].Content.([]ai.ContentBlock)[1].Type != ai.ContentBlockImage {
				t.Error("image lost on model switch")
			}
		})
	}
}

type cancellingImageReadOperations struct {
	DefaultReadOperations
	cancel context.CancelFunc
}

func (o cancellingImageReadOperations) DetectImageMimeType(string) string {
	o.cancel()
	return "image/png"
}

func TestReadToolStopsAfterImageDetectionCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, makeTestPNG(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := ReadTool(ToolConfig{ReadOps: cancellingImageReadOperations{cancel: cancel}}).Execute(ToolContext{Context: ctx, Args: map[string]any{"path": path}, Capabilities: ai.Capabilities{Vision: true}})
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("read = %+v, %v", result, err)
	}
}

func TestLSToolBoundsOutputBytes(t *testing.T) {
	dir := t.TempDir()
	for i := range 300 {
		name := fmt.Sprintf("%03d-%s", i, strings.Repeat("é", 100))
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("ls = %+v, %v", result, err)
	}
	if len(result.Content) > DefaultMaxBytes+100 {
		t.Errorf("listing has %d bytes", len(result.Content))
	}
	details, ok := result.Details.(map[string]any)
	if !ok {
		t.Fatalf("missing truncation details: %#v", result.Details)
	}
	truncation, ok := details["truncation"].(*TruncationResult)
	if !ok {
		t.Fatalf("truncation = %#v", details)
	}
	if !truncation.Truncated || truncation.TruncatedBy != "bytes" || truncation.OutputBytes > DefaultMaxBytes || truncation.OutputLines >= 300 {
		t.Errorf("truncation = %+v", truncation)
	}
	if !strings.HasPrefix(result.Content, truncation.Content) || !strings.HasSuffix(truncation.Content, strings.Repeat("é", 100)) {
		t.Error("listing split a filename or lost truncated content")
	}
	limited, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{"limit": float64(2)}})
	if err != nil || limited.IsError {
		t.Fatalf("limited ls = %+v, %v", limited, err)
	}
	if got := limited.Details.(map[string]any)["entryLimitReached"]; got != 2 {
		t.Errorf("entry limit = %v", got)
	}
}

func TestFindToolBoundsOutputAndReportsLimits(t *testing.T) {
	matches := make([]string, 300)
	for i := range matches {
		matches[i] = fmt.Sprintf("%03d-%s", i, strings.Repeat("é", 100))
	}
	for _, tc := range []struct {
		name    string
		matches []string
		limit   float64
		want    string
		reached int
		bounded bool
	}{
		{name: "byte limit", matches: matches, bounded: true},
		{name: "result limit", matches: matches[:2], limit: 2, reached: 2},
		{name: "empty", want: "No files found matching pattern"},
		{name: "ordinary", matches: []string{"a.txt", "b.txt"}, want: "a.txt\nb.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &stubFindOps{matches: tc.matches}
			args := map[string]any{"query": "*"}
			if tc.limit > 0 {
				args["max_results"] = tc.limit
			}
			result, err := FindTool(ToolConfig{FindOps: ops}).Execute(ToolContext{Context: t.Context(), Args: args})
			if err != nil || result.IsError {
				t.Fatalf("find = %+v, %v", result, err)
			}
			if tc.want != "" && result.Content != tc.want {
				t.Errorf("content = %q, want %q", result.Content, tc.want)
			}
			if len(result.Content) > DefaultMaxBytes+100 {
				t.Errorf("output has %d bytes", len(result.Content))
			}
			details, _ := result.Details.(map[string]any)
			if tc.reached > 0 && details["resultLimitReached"] != tc.reached {
				t.Errorf("result limit details = %#v", details)
			}
			if tc.bounded {
				truncation, ok := details["truncation"].(*TruncationResult)
				if !ok {
					t.Fatalf("missing truncation: %#v", details)
				}
				if !truncation.Truncated || truncation.TruncatedBy != "bytes" || truncation.OutputBytes > DefaultMaxBytes {
					t.Errorf("truncation = %+v", truncation)
				}
				if !strings.HasSuffix(truncation.Content, strings.Repeat("é", 100)) || !strings.HasPrefix(result.Content, truncation.Content) {
					t.Error("split a filename or lost retained output")
				}
			} else if tc.reached == 0 && result.Details != nil {
				t.Errorf("unexpected details: %#v", result.Details)
			}
		})
	}
}

func TestFileToolsAcceptFileURLs(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "file #é.txt")
	urlPath := filepath.ToSlash(path)
	urlDir := filepath.ToSlash(cwd)
	if runtime.GOOS == "windows" {
		urlPath = "/" + urlPath
		urlDir = "/" + urlDir
	}
	fileURL := (&url.URL{Scheme: "file", Path: urlPath}).String()
	for _, step := range []struct {
		tool Tool
		args map[string]any
	}{
		{WriteTool(ToolConfig{}), map[string]any{"path": "@" + fileURL, "content": "before"}},
		{EditTool(ToolConfig{}), map[string]any{"path": fileURL, "old_string": "before", "new_string": "after"}},
		{ReadTool(ToolConfig{}), map[string]any{"path": fileURL}},
		{LSTool(ToolConfig{}), map[string]any{"path": (&url.URL{Scheme: "file", Path: urlDir}).String()}},
	} {
		result, err := step.tool.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: step.args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("%s = %+v, %v", step.tool.Name, result, err)
		}
		if step.tool.Name == "read" && result.Content != "after" {
			t.Errorf("read = %q", result.Content)
		}
	}
}

func TestToolsRejectInvalidFileURLsBeforeIO(t *testing.T) {
	for _, input := range []string{"file:///tmp/bad%escape", "file:///tmp/%FF.txt", "file:///tmp/a%2Fb", "file://other-host/tmp/file", "file://user@localhost/tmp/file", "file://localhost:123/tmp/file"} {
		t.Run(input, func(t *testing.T) {
			for _, tool := range []Tool{ReadTool(ToolConfig{}), WriteTool(ToolConfig{}), EditTool(ToolConfig{}), BashTool(ToolConfig{BashOps: &captureBashOps{}}), GrepTool(ToolConfig{GrepOps: &stubGrepOps{}}), FindTool(ToolConfig{}), LSTool(ToolConfig{})} {
				if runtime.GOOS == "windows" && strings.Contains(input, "other-host") {
					continue
				}
				args := map[string]any{"path": input, "cwd": input, "content": "text", "old_string": "before", "new_string": "after", "command": "must not execute", "pattern": "text", "query": "*"}
				result, err := tool.Execute(ToolContext{Context: t.Context(), SessionCWD: t.TempDir(), Args: args})
				if err == nil || result != nil {
					t.Errorf("%s accepted invalid URL: %+v, %v", tool.Name, result, err)
				}
			}
		})
	}
}

func TestResolveToolPathFileURLsAndBases(t *testing.T) {
	cwd := t.TempDir()
	urlPath := filepath.ToSlash(cwd)
	if runtime.GOOS == "windows" {
		urlPath = "/" + urlPath
	}
	baseURL := (&url.URL{Scheme: "file", Path: urlPath}).String()
	for _, tc := range []struct{ input, base, want string }{
		{"file%20name.txt", cwd, filepath.Join(cwd, "file%20name.txt")},
		{"file.txt", baseURL, filepath.Join(cwd, "file.txt")},
		{baseURL + "/file%20%23%C3%A9.txt?ignored#ignored", cwd, filepath.Join(cwd, "file #é.txt")},
		{baseURL + "/file%E2%80%AFname.txt", cwd, filepath.Join(cwd, "file\u202fname.txt")},
		{"file://localhost" + urlPath + "/file.txt", cwd, filepath.Join(cwd, "file.txt")},
	} {
		got, err := resolvePath(tc.base, tc.input)
		if err != nil || got != tc.want {
			t.Errorf("resolvePath(%q,%q) = %q, %v, want %q", tc.base, tc.input, got, err, tc.want)
		}
	}
	if _, err := resolvePath("file:///bad%escape", "relative.txt"); err == nil {
		t.Error("accepted invalid base URL")
	}
}

func TestResolveToolPathWindowsShellPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows path handling")
	}
	for _, input := range []string{"/c/Users/me/file.txt", "/mnt/c/Users/me/file.txt", "/cygdrive/c/Users/me/file.txt"} {
		got, err := resolvePath(t.TempDir(), input)
		if err != nil || got != `C:\Users\me\file.txt` {
			t.Errorf("resolvePath(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"file:///c:/a%5Cb", "file:///relative.txt"} {
		if _, err := resolvePath(t.TempDir(), input); err == nil {
			t.Errorf("accepted invalid Windows file URL %q", input)
		}
	}
}

func TestSearchAndShellToolsResolveFileURLs(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, "search dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	urlPath := filepath.ToSlash(dir)
	if runtime.GOOS == "windows" {
		urlPath = "/" + urlPath
	}
	input := (&url.URL{Scheme: "file", Path: urlPath}).String()
	bash := &captureBashOps{}
	grep := &stubGrepOps{}
	cfg := ToolConfig{BashOps: bash, GrepOps: grep}
	for _, tool := range []Tool{BashTool(cfg), GrepTool(cfg), FindTool(cfg)} {
		args := map[string]any{"path": input, "cwd": input, "command": "ignored", "pattern": "needle"}
		if tool.Name == "find" {
			args["pattern"] = "file.txt"
		}
		result, err := tool.Execute(ToolContext{Context: t.Context(), SessionCWD: cwd, Args: args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("%s = %+v, %v", tool.Name, result, err)
		}
		if tool.Name == "find" && result.Content != "file.txt" {
			t.Errorf("find = %q, want file.txt", result.Content)
		}
	}
	if bash.last.Dir != dir || grep.path != dir {
		t.Errorf("resolved directories: bash=%q, grep=%q, want %q", bash.last.Dir, grep.path, dir)
	}
}

func TestLSToolReportsEmptyDirectory(t *testing.T) {
	result, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: t.TempDir(), Args: map[string]any{}})
	if err != nil || result.IsError || result.Content != "(empty directory)" || result.Details != nil {
		t.Errorf("ls = %+v, %v", result, err)
	}
}

func TestLSToolRecognizesSymlinkedDirectories(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	result, err := LSTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("ls = %+v, %v", result, err)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "d" {
			t.Errorf("directory row = %q", line)
		}
	}
	if !strings.Contains(result.Content, "linked") {
		t.Error("symlink omitted")
	}
}

func TestWriteToolCreatesParentsByDefault(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parents   any
		wantError bool
	}{
		{name: "default"}, {name: "explicit true", parents: true}, {name: "explicit false", parents: false, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			path := filepath.Join(cwd, "new", "nested", "file.txt")
			args := map[string]any{"path": path, "content": "created"}
			if tc.parents != nil {
				args["create_parents"] = tc.parents
			}
			result, err := WriteTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: args})
			if err != nil || result == nil || result.IsError != tc.wantError {
				t.Fatalf("write = %+v, %v", result, err)
			}
			data, err := os.ReadFile(path)
			if tc.wantError {
				if !os.IsNotExist(err) {
					t.Errorf("denied creation left file: %q, %v", data, err)
				}
			} else if err != nil || string(data) != "created" {
				t.Errorf("file = %q, %v", data, err)
			}
		})
	}
}

type recordingWriteOperations struct {
	DefaultWriteOperations
	called bool
}

func (o *recordingWriteOperations) WriteFile(string, []byte, fs.FileMode) error {
	o.called = true
	return nil
}

func TestWriteToolRejectsNonDirectoryParentBeforeBackend(t *testing.T) {
	cwd := t.TempDir()
	parent := filepath.Join(cwd, "not-a-directory")
	if err := os.WriteFile(parent, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := &recordingWriteOperations{}
	result, err := WriteTool(ToolConfig{WriteOps: ops}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": filepath.Join(parent, "file.txt"), "content": "changed"}})
	if err != nil || result == nil || !result.IsError || ops.called {
		t.Errorf("write = %+v, %v; backend called = %v", result, err, ops.called)
	}
}

type virtualWriteOperations struct {
	DefaultWriteOperations
	cancel   context.CancelFunc
	mkdirErr error
	statErr  error
	statName string
	calls    []string
}

func (o *virtualWriteOperations) Stat(path string) (fs.FileInfo, error) {
	o.calls = append(o.calls, "stat:"+filepath.Base(path))
	if filepath.Base(path) == o.statName {
		return nil, o.statErr
	}
	return nil, fs.ErrNotExist
}

func (o *virtualWriteOperations) Mkdir(path string) error {
	o.calls = append(o.calls, "mkdir:"+filepath.Base(path))
	if o.cancel != nil {
		o.cancel()
	}
	return o.mkdirErr
}

func (o *virtualWriteOperations) WriteFile(path string, _ []byte, _ fs.FileMode) error {
	o.calls = append(o.calls, "write:"+filepath.Base(path))
	return nil
}

func TestWriteToolUsesInjectedParentOperations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mkdirErr error
		cancel   bool
	}{
		{name: "success"}, {name: "mkdir failure", mkdirErr: fs.ErrPermission}, {name: "cancel after mkdir", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ops := &virtualWriteOperations{mkdirErr: tc.mkdirErr}
			if tc.cancel {
				ops.cancel = cancel
			}
			path := filepath.Join(cwd, "virtual", "file.txt")
			result, err := WriteTool(ToolConfig{WriteOps: ops}).Execute(ToolContext{Context: ctx, Args: map[string]any{"path": path, "content": "text"}})
			want := []string{"stat:virtual", "mkdir:virtual"}
			if tc.mkdirErr == nil && !tc.cancel {
				want = append(want, "stat:file.txt")
			}
			if tc.cancel {
				if !errors.Is(err, context.Canceled) || result != nil {
					t.Errorf("cancelled write = %+v, %v", result, err)
				}
			} else {
				if tc.mkdirErr == nil {
					want = append(want, "write:file.txt")
				}
				if err != nil || result == nil || result.IsError != (tc.mkdirErr != nil) {
					t.Errorf("write = %+v, %v", result, err)
				}
			}
			if !slices.Equal(ops.calls, want) {
				t.Errorf("calls = %v, want %v", ops.calls, want)
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Errorf("injected mkdir changed local filesystem: %v", err)
			}
		})
	}
}

func TestWriteToolStopsAfterInjectedStatFailure(t *testing.T) {
	for _, name := range []string{"virtual", "file.txt"} {
		t.Run(name, func(t *testing.T) {
			ops := &virtualWriteOperations{statName: name, statErr: fs.ErrPermission}
			result, err := WriteTool(ToolConfig{WriteOps: ops}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"path": filepath.Join(t.TempDir(), "virtual", "file.txt"), "content": "text"}})
			if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content, fs.ErrPermission.Error()) {
				t.Errorf("write = %+v, %v", result, err)
			}
			want := []string{"stat:virtual"}
			if name == "file.txt" {
				want = append(want, "mkdir:virtual", "stat:file.txt")
			}
			if !slices.Equal(ops.calls, want) {
				t.Errorf("calls = %v, want %v", ops.calls, want)
			}
		})
	}
}

func TestFindToolRelativizesBackendPaths(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	for _, tc := range []struct {
		name, base string
		matches    []string
		want       string
	}{
		{name: "root", base: root, matches: []string{filepath.Join(root, "home", "project") + string(filepath.Separator), filepath.Join(root, "home", "project", "file.txt")}, want: "home/project/\nhome/project/file.txt"},
		{name: "sibling prefix", base: filepath.Join(root, "home", "project"), matches: []string{filepath.Join(root, "home", "project2", "file.txt")}, want: "../project2/file.txt"},
		{name: "relative", base: root, matches: []string{filepath.Join("sub", "dir") + string(filepath.Separator), filepath.Join("sub", "file.txt")}, want: "sub/dir/\nsub/file.txt"},
		{name: "outside", base: filepath.Join(root, "workspace", "project"), matches: []string{filepath.Join(root, "tmp", "dir") + string(filepath.Separator)}, want: "../../tmp/dir/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slices.Clone(tc.matches)
			result, err := FindTool(ToolConfig{FindOps: &stubFindOps{matches: tc.matches}}).Execute(ToolContext{Context: t.Context(), SessionCWD: tc.base, Args: map[string]any{"query": "*"}})
			if err != nil || result.IsError || result.Content != tc.want {
				t.Errorf("find = %+v, %v, want %q", result, err, tc.want)
			}
			if !slices.Equal(tc.matches, original) {
				t.Errorf("mutated backend paths: %v", tc.matches)
			}
		})
	}
	if runtime.GOOS != "windows" {
		result, err := FindTool(ToolConfig{FindOps: &stubFindOps{matches: []string{`/home/file\`}}}).Execute(ToolContext{Context: t.Context(), SessionCWD: "/home", Args: map[string]any{"query": "*"}})
		if err != nil || result.Content != `file\` {
			t.Errorf("POSIX backslash = %+v, %v", result, err)
		}
	}
}

type recordingFindOperations struct {
	stubFindOps
	query  string
	hidden bool
	limit  int
}

func (o *recordingFindOperations) Find(_ context.Context, query, _, _ string, _ int, hidden, _ bool, limit int) ([]string, error) {
	o.called = true
	o.query = query
	o.hidden = hidden
	o.limit = limit
	return []string{"found.txt"}, nil
}

func TestFindToolAdmitsUpstreamAndLegacyArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]any
		query  string
		limit  int
		hidden bool
	}{
		{name: "upstream defaults", args: map[string]any{"pattern": "*.txt"}, query: "*.txt", limit: 1000, hidden: true},
		{name: "upstream limit", args: map[string]any{"pattern": "*.go", "limit": float64(7)}, query: "*.go", limit: 7, hidden: true},
		{name: "legacy", args: map[string]any{"query": "*.md", "max_results": float64(9), "include_hidden": false}, query: "*.md", limit: 9},
		{name: "canonical wins", args: map[string]any{"pattern": "*.go", "query": "*.md", "limit": float64(7), "max_results": float64(9)}, query: "*.go", limit: 7, hidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &recordingFindOperations{}
			tool := FindTool(ToolConfig{FindOps: ops})
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{ID: "find", Name: "find", Arguments: raw}, AgentLoopConfig{})
			if result.IsError || result.Rejection != nil || result.Content != "found.txt" {
				t.Fatalf("find = %+v", result)
			}
			if !ops.called || ops.query != tc.query || ops.limit != tc.limit || ops.hidden != tc.hidden {
				t.Errorf("backend = %+v", ops)
			}
		})
	}
}

func TestFindToolIncludesHiddenFilesByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".hidden.txt"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: dir, Args: map[string]any{"pattern": "*.txt"}})
	if err != nil || result == nil || result.IsError || result.Content != ".hidden.txt" {
		t.Errorf("find = %+v, %v", result, err)
	}
}

func TestBuiltinNumericOptionsSurviveArgumentValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	bash := &captureBashOps{}
	grep := &numericGrepOperations{}
	for _, tc := range []struct {
		name  string
		tool  Tool
		args  map[string]any
		check func(*testing.T, *ToolResult)
	}{
		{name: "read", tool: ReadTool(ToolConfig{}), args: map[string]any{"path": path, "offset": 2, "limit": 1}, check: func(t *testing.T, result *ToolResult) {
			if !strings.HasPrefix(result.Content, "second") || strings.Contains(result.Content, "first") || strings.Contains(result.Content, "third") {
				t.Errorf("read = %q", result.Content)
			}
		}},
		{name: "ls", tool: LSTool(ToolConfig{}), args: map[string]any{"path": dir, "limit": 1}, check: func(t *testing.T, result *ToolResult) {
			details, _ := result.Details.(map[string]any)
			if details["entryLimitReached"] != 1 {
				t.Errorf("ls = %+v", result)
			}
		}},
		{name: "bash", tool: BashTool(ToolConfig{BashOps: bash}), args: map[string]any{"command": "ignored", "timeout": 7}, check: func(t *testing.T, _ *ToolResult) {
			if bash.last.Timeout != 7*time.Second {
				t.Errorf("timeout = %v", bash.last.Timeout)
			}
		}},
		{name: "grep", tool: GrepTool(ToolConfig{GrepOps: grep}), args: map[string]any{"pattern": "needle", "max_results": 7, "context": 2}, check: func(t *testing.T, _ *ToolResult) {
			if grep.limit != 7 || grep.lines != 2 {
				t.Errorf("grep limit/context = %d/%d", grep.limit, grep.lines)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tc.tool}}, nil, ai.ToolCall{ID: tc.name, Name: tc.tool.Name, Arguments: raw}, AgentLoopConfig{})
			if result.IsError || result.Rejection != nil {
				t.Fatalf("%s = %+v", tc.name, result)
			}
			tc.check(t, result)
		})
	}
}

type numericGrepOperations struct {
	limit, lines  int
	pattern, path string
	insensitive   bool
}

func (o *numericGrepOperations) Grep(_ context.Context, pattern, path, _ string, insensitive bool, limit, lines int) (*ToolResult, error) {
	o.limit = limit
	o.lines = lines
	o.pattern = pattern
	o.path = path
	o.insensitive = insensitive
	return &ToolResult{Content: "found"}, nil
}

func TestFindToolScopesNestedIgnoreRules(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string]string{
		".gitignore":        "*.drop.txt\n",
		"a/.gitignore":      "ignored.txt\n",
		"a/deep/.gitignore": "secret.txt\n!keep.drop.txt\n",
		"a/deep/.ignore":    "!secret.txt\n",
		"a/deep/.fdignore":  "secret.txt\n",
		"a/ignored.txt":     "", "a/kept.txt": "", "a/deep/ignored.txt": "", "a/deep/secret.txt": "", "a/deep/kept.txt": "", "a/deep/keep.drop.txt": "",
		"b/ignored.txt": "", "b/kept.txt": "", "b/remove.drop.txt": "", "root.txt": "",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, noIgnore := range []bool{false, true} {
		t.Run(fmt.Sprintf("no_ignore=%v", noIgnore), func(t *testing.T) {
			result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: root, Args: map[string]any{"pattern": "*.txt", "no_ignore": noIgnore}})
			if err != nil || result.IsError {
				t.Fatalf("find = %+v, %v", result, err)
			}
			want := []string{"a/deep/keep.drop.txt", "a/deep/kept.txt", "a/kept.txt", "b/ignored.txt", "b/kept.txt", "root.txt"}
			if noIgnore {
				want = []string{"a/deep/ignored.txt", "a/deep/keep.drop.txt", "a/deep/kept.txt", "a/deep/secret.txt", "a/ignored.txt", "a/kept.txt", "b/ignored.txt", "b/kept.txt", "b/remove.drop.txt", "root.txt"}
			}
			if result.Content != strings.Join(want, "\n") {
				t.Errorf("find = %q, want %v", result.Content, want)
			}
		})
	}
}

func TestFindToolStopsParentGitignoreAtNestedRepository(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "nested/.git", "ordinary"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{".gitignore": "*.txt\n", "nested/keep.txt": "", "ordinary/drop.txt": ""} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: root, Args: map[string]any{"pattern": "*.txt"}})
	if err != nil || result.IsError || result.Content != "nested/keep.txt" {
		t.Errorf("find = %+v, %v", result, err)
	}
}

func TestFindToolMatchesPathGlobs(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"some/parent/child/file.ext", "some/parent/child/test.spec.ts", "src/foo/bar/example.spec.ts", "src/direct.spec.ts", "other/src/deep/also.spec.ts"} {
		full := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"*.spec.ts", []string{"other/src/deep/also.spec.ts", "some/parent/child/test.spec.ts", "src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
		{"some/parent/child/**", []string{"some/parent/child/file.ext", "some/parent/child/test.spec.ts"}},
		{"**/parent/child/*", []string{"some/parent/child/file.ext", "some/parent/child/test.spec.ts"}},
		{"src/**/*.spec.ts", []string{"other/src/deep/also.spec.ts", "src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
		{"src/*/*.spec.ts", []string{"other/src/deep/also.spec.ts"}},
		{"src/**/[de]*.spec.ts", []string{"src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
		{filepath.ToSlash(filepath.Join(root, "src")) + "/**/*.spec.ts", []string{"src/direct.spec.ts", "src/foo/bar/example.spec.ts"}},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: root, Args: map[string]any{"pattern": tc.pattern}})
			if err != nil || result.IsError {
				t.Fatalf("find = %+v, %v", result, err)
			}
			var files []string
			for _, line := range strings.Split(result.Content, "\n") {
				if strings.Contains(line, ".") {
					files = append(files, line)
				}
			}
			if !slices.Equal(files, tc.want) {
				t.Errorf("files = %v, want %v", files, tc.want)
			}
		})
	}
}

func TestFindToolListsChildrenAndMarksDirectories(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, ".root")
	if err := os.MkdirAll(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []bool{false, true} {
		result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: root, Args: map[string]any{"pattern": "*", "include_hidden": hidden}})
		if err != nil || result.IsError || result.Content != "child/\nfile.txt" {
			t.Errorf("find hidden=%v = %+v, %v", hidden, result, err)
		}
	}
}

func TestFindToolReportsMissingSearchRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), Args: map[string]any{"pattern": "*", "path": path}})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content, path) {
		t.Errorf("find = %+v, %v", result, err)
	}
}

func TestFindToolLoadsAncestorIgnoreRulesWithinRepository(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "sub/deep"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{".gitignore": "drop.txt\n", "sub/deep/drop.txt": "", "sub/deep/keep.txt": ""} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	search := filepath.Join(root, "sub", "deep")
	result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: search, Args: map[string]any{"pattern": "*.txt"}})
	if err != nil || result.IsError || result.Content != "keep.txt" {
		t.Errorf("find = %+v, %v", result, err)
	}
	if err := os.Mkdir(filepath.Join(search, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err = FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: search, Args: map[string]any{"pattern": "*.txt"}})
	if err != nil || result.IsError || result.Content != "drop.txt\nkeep.txt" {
		t.Errorf("nested-root find = %+v, %v", result, err)
	}
}

func TestFindToolDoesNotInventIgnoreRules(t *testing.T) {
	root := t.TempDir()
	names := []string{".git", ".venv", "__pycache__", ".pytest_cache", "build", "dist", "node_modules", "target", "vendor"}
	var want []string
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("text"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != ".git" {
			want = append(want, name+"/keep.txt")
		}
	}
	slices.Sort(want)
	result, err := FindTool(ToolConfig{}).Execute(ToolContext{Context: t.Context(), SessionCWD: root, Args: map[string]any{"pattern": "*.txt"}})
	if err != nil || result.IsError || result.Content != strings.Join(want, "\n") {
		t.Errorf("find = %+v, %v, want %v", result, err, want)
	}
}

func TestGrepToolAdmitsUpstreamAndLegacyArguments(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        map[string]any
		pattern     string
		limit       int
		insensitive bool
	}{
		{name: "defaults", args: map[string]any{"pattern": "a.b"}, pattern: "a.b", limit: 100},
		{name: "canonical", args: map[string]any{"pattern": "a.b", "literal": true, "ignoreCase": true, "limit": 7}, pattern: `a\.b`, limit: 7, insensitive: true},
		{name: "legacy", args: map[string]any{"pattern": "a.b", "case_insensitive": true, "max_results": 9}, pattern: "a.b", limit: 9, insensitive: true},
		{name: "canonical wins", args: map[string]any{"pattern": "a.b", "ignoreCase": false, "case_insensitive": true, "limit": 1201, "max_results": 9}, pattern: "a.b", limit: 1201},
		{name: "minimum", args: map[string]any{"pattern": "a.b", "limit": 0}, pattern: "a.b", limit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &numericGrepOperations{}
			tool := GrepTool(ToolConfig{GrepOps: ops})
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			result := RunToolCall(t.Context(), &AgentContext{Tools: []Tool{tool}}, nil, ai.ToolCall{ID: tc.name, Name: tool.Name, Arguments: raw}, AgentLoopConfig{})
			if result.IsError || result.Rejection != nil {
				t.Fatalf("grep = %+v", result)
			}
			if ops.limit != tc.limit || ops.pattern != tc.pattern || ops.insensitive != tc.insensitive || !filepath.IsAbs(ops.path) {
				t.Errorf("backend = %+v", ops)
			}
		})
	}
}

func TestGrepPureGoReportsInvalidRegexAndMissingRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("[\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, pattern, path string }{
		{"invalid regex", "[", root}, {"missing root", "needle", filepath.Join(root, "absent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := grepPureGo(t.Context(), tc.pattern, tc.path, "", false, 100, 0)
			if err != nil || result == nil || !result.IsError || result.Content == "" {
				t.Errorf("grep = %+v, %v", result, err)
			}
		})
	}
	result, err := grepPureGo(t.Context(), regexp.QuoteMeta("["), root, "", false, 100, 0)
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Content, "[") {
		t.Errorf("literal grep = %+v, %v", result, err)
	}
}
