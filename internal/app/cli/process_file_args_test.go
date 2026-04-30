package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProcessFileArguments_TextFile is the baseline: @<file> embeds the
// file contents in the prompt wrapped with the file's absolute path.
func TestProcessFileArguments_TextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, images, err := processFileArguments("look at @" + path)
	if err != nil {
		t.Fatalf("processFileArguments: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("text missing body: %q", out)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("text missing file path %q: %q", path, out)
	}
}

// TestProcessFileArguments_DirectoryDoesNotWalk is the regression
// test for escaped @ prompts accidentally trying to enumerate /.
func TestProcessFileArguments_DirectoryDoesNotWalk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	out, images, err := processFileArguments("list @" + dir)
	if err != nil {
		t.Fatalf("processFileArguments: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	want := "list <directory name=\"" + dir + "\">\n</directory>"
	if out != want {
		t.Fatalf("directory expansion = %q, want %q", out, want)
	}
	for _, banned := range []string{"a.go", "sub", "alpha"} {
		if strings.Contains(out, banned) {
			t.Fatalf("@dir walked/listed file content %q: %q", banned, out)
		}
	}
}

// TestProcessFileArguments_DirectoryOutsideCWD covers the parallel
// regression: @<dir> must accept paths outside cwd. Previously the
// CLI flow in processFileArguments could read a file outside cwd but
// had no notion of "directory", so the regression in this package
// was mostly the bug above. This test pins the behavior so neither
// path slips back to the broken state.
func TestProcessFileArguments_DirectoryOutsideCWD(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.md"), []byte("contents"), 0o644); err != nil {
		t.Fatalf("write shared.md: %v", err)
	}
	cwd := filepath.Join(project, "pkg", "internal")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	out, _, err := processFileArguments("list @" + dir)
	if err != nil {
		t.Fatalf("processFileArguments(@outside dir): %v", err)
	}
	want := "list <directory name=\"" + dir + "\">\n</directory>"
	if out != want {
		t.Fatalf("@outside dir expansion = %q, want %q", out, want)
	}
}

// TestProcessFileArguments_FileOutsideCWD verifies @<file> works for
// paths outside the process cwd, matching the spec request that
// @../some-file should work as expected. The "outside" here means
// paths the shell would also resolve from the working directory:
// @../file from cwd=project/sub/ resolves to project/file the same
// way `cat ../file` does.
func TestProcessFileArguments_FileOutsideCWD(t *testing.T) {
	project := t.TempDir()
	cwd := filepath.Join(project, "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	// Place the sibling file in the cwd's parent so @../sibling.txt
	// (read from cwd) lands on it — same semantics as a shell
	// relative path.
	sibling := filepath.Join(project, "sibling.txt")
	if err := os.WriteFile(sibling, []byte("sibling body"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	out, _, err := processFileArguments("look at @../sibling.txt")
	if err != nil {
		t.Fatalf("processFileArguments(@../sibling.txt): %v", err)
	}
	if !strings.Contains(out, "sibling body") {
		t.Fatalf("@../sibling.txt did not expand: %q", out)
	}
	if !strings.Contains(out, sibling) {
		t.Fatalf("@../sibling.txt missing absolute path %q: %q", sibling, out)
	}
}

// TestProcessFileArguments_DirectoryDoesNotListDotfiles verifies directories
// are wrapped without traversal, so dotfile trees cannot leak into prompts.
func TestProcessFileArguments_DirectoryDoesNotListDotfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644); err != nil {
		t.Fatalf("write .git/HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	out, _, err := processFileArguments("ls @" + dir)
	if err != nil {
		t.Fatalf("processFileArguments: %v", err)
	}
	if strings.Contains(out, "README.md") || strings.Contains(out, ".git") || strings.Contains(out, "HEAD") {
		t.Fatalf("directory expansion listed contents: %q", out)
	}
}
