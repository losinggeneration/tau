package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
			rgResult, _ := grepWithRG("rg", tc.pattern, tmp, tc.glob, tc.caseInsensitive, 200, 0)
			pureResult, _ := grepPureGo(tc.pattern, tmp, tc.glob, tc.caseInsensitive, 200, 0)

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
			rgResult, _ := grepWithRG("rg", "foo bar", tmp, "", false, 200, context)
			t.Logf("context=%d: ripgrep output:\n%s", context, rgResult.Content)
			pureResult, _ := grepPureGo("foo bar", tmp, "", false, 200, context)
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
			rgResult, _ := grepWithRG("rg", "match", tmp, "", false, max, 0)
			pureResult, _ := grepPureGo("match", tmp, "", false, max, 0)

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
