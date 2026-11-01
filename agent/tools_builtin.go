package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/losinggeneration/tau/agent/fmqueue"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/internal/ignore"
	textunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/unicode/norm"
)

// ============================================================================
// Static Errors
// ============================================================================

// Static errors for tool validation.
var (
	ErrPathMustBeString    = errors.New("path must be a string")
	ErrContentMustBeString = errors.New("content must be a string")
	ErrCommandMustBeString = errors.New("command must be a string")
	ErrPatternMustBeString = errors.New("pattern must be a string")

	// ErrBinaryFile is returned when read encounters a binary file.
	ErrBinaryFile = errors.New("binary file")

	// ErrEditNotFound is returned when the old string is not found in the file.
	ErrEditNotFound = errors.New("edit: old string not found")

	// ErrEditAmbiguous is returned when the old string matches multiple locations
	// without replace_all.
	ErrEditAmbiguous = errors.New("edit: old string matches multiple locations")
)

// ErrEditAmbiguousPayload provides structured error details when edit matches
// multiple locations without replace_all.
type ErrEditAmbiguousPayload struct {
	Path        string
	MatchCount  int
	SampleLines []int
}

func (e *ErrEditAmbiguousPayload) Error() string {
	return fmt.Sprintf("edit: old string matches %d locations in %s", e.MatchCount, e.Path)
}

// ============================================================================
// Default Constants
// ============================================================================

// Default tool settings.
const (
	defaultReadLineCap    = 2000
	defaultBashTimeout    = 120 * time.Second
	maxBashTimeout        = 600 * time.Second
	defaultBashOutputCap  = 100 * 1024 // 100 KiB
	defaultGrepMaxResults = 100
	maxGrepMaxResults     = 1000
	defaultGrepOutputCap  = 100 * 1024 // 100 KiB
	defaultFindMaxResults = 1000
	maxFindMaxResults     = 5000
	defaultLSMaxEntries   = 1000
)

// Image magic bytes.
var (
	pngMagic  = []byte{0x89, 0x50, 0x4E, 0x47}
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	gifMagic  = []byte{0x47, 0x49, 0x46}
	webpMagic = []byte{0x52, 0x49, 0x46, 0x46} // RIFF header (WebP starts with RIFF...WEBP)
)

// ============================================================================
// Operations Interfaces (for pluggable operations pattern)
// ============================================================================

// ReadOperations is the injection seam for file reading operations.
// Default impl uses os.ReadFile; tests/extensions use a mock.
type ReadOperations interface {
	// ReadFile reads a file at the given absolute path.
	ReadFile(path string) ([]byte, error)
	// Access checks if a file is readable and returns file info.
	Access(path string) (os.FileInfo, error)
	// Ls lists directory entries at the given path.
	Ls(path string) ([]os.DirEntry, error)
	// Stat returns file info for the given path.
	Stat(path string) (os.FileInfo, error)
	// DetectImageMimeType detects the MIME type of an image file.
	// Returns empty string if not an image.
	DetectImageMimeType(path string) string
}

// DefaultReadOperations uses the local filesystem.
type DefaultReadOperations struct{}

func (d DefaultReadOperations) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (d DefaultReadOperations) Access(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (d DefaultReadOperations) DetectImageMimeType(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return detectImageMIME(data)
}

func (d DefaultReadOperations) Ls(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (d DefaultReadOperations) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// WriteOperations is the injection seam for file writing operations.
// Default impl uses os.WriteFile; tests/extensions use a mock.
type WriteOperations interface {
	// Mkdir creates a directory and any missing parents.
	Mkdir(path string) error
	// WriteFile writes content to a file at the given absolute path.
	WriteFile(path string, content []byte, perm fs.FileMode) error
	// Access checks if a file is readable and writable.
	Access(path string) error
	// Ls lists directory entries at the given path.
	Ls(path string) ([]os.DirEntry, error)
	// Stat returns file info for the given path.
	Stat(path string) (os.FileInfo, error)
}

// DefaultWriteOperations uses the local filesystem.
type DefaultWriteOperations struct{}

func (d DefaultWriteOperations) Mkdir(path string) error { return os.MkdirAll(path, 0o755) }

func (d DefaultWriteOperations) WriteFile(path string, content []byte, perm fs.FileMode) error {
	return os.WriteFile(path, content, perm)
}

func (d DefaultWriteOperations) Access(path string) error {
	_, err := os.Stat(path)
	return err
}

func (d DefaultWriteOperations) Ls(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (d DefaultWriteOperations) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// EditOperations is the injection seam for file editing operations.
// Default impl uses os.ReadFile + os.WriteFile; tests/extensions use a mock.
type EditOperations interface {
	// ReadFile reads a file at the given absolute path.
	ReadFile(path string) ([]byte, error)
	// WriteFile writes content to a file at the given absolute path.
	WriteFile(path string, content []byte, perm fs.FileMode) error
	// Access checks if a file is readable and writable.
	Access(path string) error
}

// DefaultEditOperations uses the local filesystem.
type DefaultEditOperations struct{}

func (d DefaultEditOperations) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (d DefaultEditOperations) WriteFile(path string, content []byte, perm fs.FileMode) error {
	return os.WriteFile(path, content, perm)
}

func (d DefaultEditOperations) Access(path string) error {
	_, err := os.Stat(path)
	return err
}

// GrepOperations is the injection seam for grep operations.
// Default impl uses ripgrep; tests/extensions use a mock.
type GrepOperations interface {
	// Grep searches for pattern in path with optional glob filter. The
	// returned *ToolResult carries the formatted output, including any
	// error context (IsError) and truncation notes. A non-nil error is
	// reserved for unexpected I/O failures; "no matches" and similar
	// tool-level results stay inside the *ToolResult.
	Grep(ctx context.Context, pattern, path, glob string, caseInsensitive bool, maxResults, contextLines int) (*ToolResult, error)
}

// FindOperations is the injection seam for find operations.
// Default impl uses filepath.Walk; tests/extensions use a mock.
type FindOperations interface {
	// Find searches for files matching the query pattern. The returned
	// slice contains absolute or search-root-relative paths; an empty
	// slice with a nil error means "no matches".
	Find(ctx context.Context, query, path string, mode string, maxDepth int, includeHidden, noIgnore bool, maxResults int) ([]string, error)
}

// LsOperations is the injection seam for ls operations.
// Default impl uses os.ReadDir; tests/extensions use a mock.
type LsOperations interface {
	// Ls lists directory entries at the given path.
	Ls(path string) ([]os.DirEntry, error)
	// Stat returns file info for the given path.
	Stat(path string) (fs.FileInfo, error)
}

// DefaultLsOperations uses the local filesystem.
type DefaultLsOperations struct{}

func (d DefaultLsOperations) Ls(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (d DefaultLsOperations) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

// DefaultGrepOperations tries ripgrep first, falling back to the pure-Go
// implementation when rg is not available. Both backends return *ToolResult
// directly (with IsError set on tool-level failures), so the seam never
// has to reconstruct a result from a string.
type DefaultGrepOperations struct{}

func (d DefaultGrepOperations) Grep(ctx context.Context, pattern, path, glob string, caseInsensitive bool, maxResults, contextLines int) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if rgPath, err := exec.LookPath("rg"); err == nil {
		return grepWithRG(ctx, rgPath, pattern, path, glob, caseInsensitive, maxResults, contextLines)
	}
	return grepPureGo(ctx, pattern, path, glob, caseInsensitive, maxResults, contextLines)
}

// DefaultFindOperations walks the filesystem with filepath.Walk, applying the
// same matcher / hidden / depth / ignore rules that the original inline
// FindTool.Execute used. Extracted to back the FindOperations seam so the
// tool does not carry the policy logic inline.
type DefaultFindOperations struct{}

func (d DefaultFindOperations) Find(ctx context.Context, query, searchPath, mode string, maxDepth int, includeHidden, noIgnore bool, maxResults int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Build matcher
	var matcher func(string) bool
	switch mode {
	case "regex":
		re, err := regexp.Compile(query)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		matcher = re.MatchString
	case "substring":
		matcher = func(name string) bool { return strings.Contains(name, query) }
	default: // glob
		if _, err := filepath.Match(query, query); err != nil {
			return nil, fmt.Errorf("invalid glob: %w", err)
		}
		matcher = func(name string) bool {
			matched, _ := filepath.Match(query, name)
			return matched
		}
	}

	ignoreRoot := searchPath
	if !noIgnore {
		for current := searchPath; ; current = filepath.Dir(current) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ignoreRoot = current
			if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("find repository root: %w", err)
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	}
	var matches []string
	type directoryRules struct {
		root    string
		matcher *ignore.Matcher
	}
	rulesByDir := make(map[string]directoryRules)
	rootDepth := len(strings.Split(filepath.ToSlash(searchPath), "/"))

	walkErr := filepath.Walk(searchPath, func(current string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if err != nil {
			if current == searchPath {
				return err
			}
			return nil
		}

		if maxDepth > 0 {
			depth := len(strings.Split(filepath.ToSlash(current), "/")) - rootDepth
			if depth > maxDepth {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		name := info.Name()
		if current != searchPath && !includeHidden && strings.HasPrefix(name, ".") && name != "." && name != ".." {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current != searchPath && !noIgnore && name == ".git" {
			return filepath.SkipDir
		}

		if !noIgnore {
			parent := rulesByDir[filepath.Dir(current)]
			if parent.matcher.Ignored(current, info.IsDir()) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				ruleRoot := parent.root
				if current == searchPath {
					ruleRoot = ignoreRoot
				} else {
					if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
						ruleRoot = current
					} else if !os.IsNotExist(err) {
						return fmt.Errorf("check nested repository: %w", err)
					}
				}
				rules, err := ignore.New(current, ruleRoot)
				if err != nil {
					return err
				}
				rulesByDir[current] = directoryRules{root: ruleRoot, matcher: rules}
			}
		}
		if current == searchPath {
			return nil
		}

		matched := matcher(name)
		if mode != "regex" && mode != "substring" && strings.Contains(query, "/") {
			pattern := query
			if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && !filepath.IsAbs(filepath.FromSlash(pattern)) {
				pattern = "**/" + pattern
			}
			var matchErr error
			matched, matchErr = matchFindPathGlob(pattern, filepath.ToSlash(current))
			if matchErr != nil {
				return matchErr
			}
		}
		if matched {
			resultPath := current
			if info.IsDir() {
				resultPath += string(filepath.Separator)
			}
			matches = append(matches, resultPath)
			if len(matches) >= maxResults {
				return errMaxResultsReached
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, errors.Join(walkErr, ctx.Err())
	}
	if walkErr != nil && !errors.Is(walkErr, errMaxResultsReached) {
		return matches, fmt.Errorf("search %s: %w", searchPath, walkErr)
	}
	return matches, nil
}

// errMaxResultsReached is a sentinel returned from filepath.Walk callbacks
// to short-circuit a Find walk once the result cap is hit. It is unexported
// because callers must use errors.Is to detect it.
var errMaxResultsReached = errors.New("max results reached")

// ============================================================================
// ToolConfig
// ============================================================================

// ToolConfig holds configuration for built-in tools.
type ToolConfig struct {
	// BashOps is the bash operations backend. If nil, DefaultBashOps is used.
	BashOps BashOperations
	// MutationQueue serializes file mutations. If nil, tools share the process-wide queue.
	MutationQueue fmqueue.MutationQueue

	// ReadOps is the read operations backend. If nil, DefaultReadOperations is used.
	ReadOps ReadOperations
	// WriteOps is the write operations backend. If nil, DefaultWriteOperations is used.
	WriteOps WriteOperations
	// EditOps is the edit operations backend. If nil, DefaultEditOperations is used.
	EditOps EditOperations
	// GrepOps is the grep operations backend. If nil, uses ripgrep or pure Go.
	GrepOps GrepOperations
	// FindOps is the find operations backend. If nil, uses filepath.Walk.
	FindOps FindOperations
	// LsOps is the ls operations backend. If nil, uses os.ReadDir.
	LsOps LsOperations

	// Bash settings - applied when BashOps is nil
	BashShell        string            // shell path (e.g., "/bin/bash")
	BashOutputCap    int               // max output bytes
	BashEnvAllowlist []string          // allowed env vars
	BashEnvExtra     map[string]string // extra env vars
	BashStreamWindow time.Duration     // coalescing window for streamed output
	BashKillGrace    time.Duration     // grace period before SIGKILL on cancellation
	CommandPrefix    string            // prefix prepended to each bash command

	// ReadLineCap is the max lines returned by read. Default 2000.
	ReadLineCap int

	// AutoResizeImages enables automatic image resizing to max dimensions.
	// Default: true unless AutoResizeImagesSet is true and AutoResizeImages is false.
	AutoResizeImages    bool
	AutoResizeImagesSet bool
	// MaxImageWidth is the maximum width for image resizing. Default: 2000.
	MaxImageWidth int
	// MaxImageHeight is the maximum height for image resizing. Default: 2000.
	MaxImageHeight int
	// BlockImages returns text placeholders instead of image content blocks.
	BlockImages bool

	// BashTempFile enables saving full output to temp file when truncated.
	// Default: true
	BashTempFile bool
}

// BashOperations returns the configured shell backend, or a DefaultBashOps
// built from the shell settings.
func (c ToolConfig) BashOperations() BashOperations {
	if c.BashOps != nil {
		return c.BashOps
	}
	ops := NewDefaultBashOps()
	if c.BashShell != "" {
		ops.Shell = c.BashShell
	}
	if c.BashOutputCap > 0 {
		ops.OutputCap = int64(c.BashOutputCap)
	}
	if len(c.BashEnvAllowlist) > 0 {
		ops.EnvAllowlist = c.BashEnvAllowlist
	}
	if len(c.BashEnvExtra) > 0 {
		ops.EnvExtra = c.BashEnvExtra
	}
	if c.BashStreamWindow > 0 {
		ops.StreamWindow = c.BashStreamWindow
	}
	if c.BashKillGrace > 0 {
		ops.KillGrace = c.BashKillGrace
	}
	if c.BashTempFile {
		ops.TempFile = true
	}
	return ops
}

var defaultMutationQueue = fmqueue.New()

func (c ToolConfig) mutationQueue() fmqueue.MutationQueue {
	if c.MutationQueue != nil {
		return c.MutationQueue
	}
	return defaultMutationQueue
}

func (c ToolConfig) readLineCap() int {
	if c.ReadLineCap > 0 {
		return c.ReadLineCap
	}
	return defaultReadLineCap
}

func (c ToolConfig) readOps() ReadOperations {
	if c.ReadOps != nil {
		return c.ReadOps
	}
	return DefaultReadOperations{}
}

func (c ToolConfig) writeOps() WriteOperations {
	if c.WriteOps != nil {
		return c.WriteOps
	}
	return DefaultWriteOperations{}
}

func (c ToolConfig) editOps() EditOperations {
	if c.EditOps != nil {
		return c.EditOps
	}
	return DefaultEditOperations{}
}

func (c ToolConfig) lsOps() LsOperations {
	if c.LsOps != nil {
		return c.LsOps
	}
	return DefaultLsOperations{}
}

func (c ToolConfig) grepOps() GrepOperations {
	if c.GrepOps != nil {
		return c.GrepOps
	}
	return DefaultGrepOperations{}
}

func (c ToolConfig) findOps() FindOperations {
	if c.FindOps != nil {
		return c.FindOps
	}
	return DefaultFindOperations{}
}

func (c ToolConfig) autoResizeImages() bool {
	if c.AutoResizeImagesSet {
		return c.AutoResizeImages
	}
	return true
}

func (c ToolConfig) maxImageWidth() int {
	if c.MaxImageWidth > 0 {
		return c.MaxImageWidth
	}
	return 2000
}

func (c ToolConfig) maxImageHeight() int {
	if c.MaxImageHeight > 0 {
		return c.MaxImageHeight
	}
	return 2000
}

func (c ToolConfig) bashTempFile() bool {
	// Default to true for temp file behavior
	return c.BashTempFile
}

// ============================================================================
// Path Resolution
// ============================================================================

// resolvePath resolves tool input paths against the session CWD.
func resolvePath(sessionCWD, path string) (string, error) {
	path = strings.Map(func(r rune) rune {
		if r == '\u00a0' || r >= '\u2000' && r <= '\u200a' || r == '\u202f' || r == '\u205f' || r == '\u3000' {
			return ' '
		}
		return r
	}, strings.TrimPrefix(path, "@"))
	path, err := normalizeToolPath(path)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base, err := normalizeToolPath(sessionCWD)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(filepath.Join(base, path))
	if err != nil {
		return "", fmt.Errorf("resolve tool path: %w", err)
	}
	return absolute, nil
}

var windowsShellPath = regexp.MustCompile(`(?i)^/(?:mnt/|cygdrive/)?([a-z])(?:/(.*))?$`)

func normalizeToolPath(path string) (string, error) {
	if runtime.GOOS == "windows" && !strings.HasPrefix(path, "//") && !strings.Contains(path, `\`) {
		if parts := windowsShellPath.FindStringSubmatch(path); parts != nil {
			path = strings.ToUpper(parts[1]) + `:\` + strings.ReplaceAll(parts[2], "/", `\`)
		}
	}
	if path == "~" || strings.HasPrefix(path, "~/") || runtime.GOOS == "windows" && strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	if !strings.HasPrefix(path, "file://") {
		return path, nil
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse file URL: %w", err)
	}
	escaped := strings.ToLower(parsed.EscapedPath())
	if parsed.User != nil || parsed.Port() != "" || strings.Contains(escaped, "%2f") || !utf8.ValidString(parsed.Path) {
		return "", fmt.Errorf("invalid file URL %q", path)
	}
	host := parsed.Hostname()
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if runtime.GOOS == "windows" {
		if strings.Contains(escaped, "%5c") {
			return "", fmt.Errorf("encoded separator in file URL %q", path)
		}
		if host != "" && !strings.EqualFold(host, "localhost") {
			return filepath.FromSlash("//" + host + parsed.Path), nil
		}
		if len(parsed.Path) < 3 || parsed.Path[0] != '/' || parsed.Path[2] != ':' || (parsed.Path[1] < 'a' || parsed.Path[1] > 'z') && (parsed.Path[1] < 'A' || parsed.Path[1] > 'Z') {
			return "", fmt.Errorf("file URL must contain an absolute drive path: %q", path)
		}
		return filepath.FromSlash(parsed.Path[1:]), nil
	}
	if host != "" && !strings.EqualFold(host, "localhost") {
		return "", fmt.Errorf("file URL host must be localhost: %q", path)
	}
	return parsed.Path, nil
}

var screenshotTimeSuffix = regexp.MustCompile(`(?i) (AM|PM)\.`)

func resolveReadPath(sessionCWD, path string) (string, error) {
	resolved, err := resolvePath(sessionCWD, path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(resolved); err == nil {
		return resolved, nil
	}
	decomposed := norm.NFD.String(resolved)
	for _, candidate := range []string{
		screenshotTimeSuffix.ReplaceAllString(resolved, "\u202f$1."),
		decomposed,
		strings.ReplaceAll(resolved, "'", "\u2019"),
		strings.ReplaceAll(decomposed, "'", "\u2019"),
	} {
		if candidate != resolved {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return resolved, nil
}

// ============================================================================
// Read Tool
// ============================================================================

// ReadTool returns a tool for reading files.
func ReadTool(cfg ToolConfig) Tool {
	lineCap := cfg.readLineCap()
	ops := cfg.readOps()
	autoResize := cfg.autoResizeImages()
	maxWidth := cfg.maxImageWidth()
	maxHeight := cfg.maxImageHeight()

	return Tool{
		Name:        "read",
		Description: fmt.Sprintf("Read contents of a file or directory listing. Text output is limited to %d lines or 50KB. Use offset/limit to continue large reads.", lineCap),
		OutputSchema: map[string]any{"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "object", "properties": map[string]any{
				"type":     map[string]any{"const": "image"},
				"data":     map[string]any{"type": "string"},
				"mimeType": map[string]any{"type": "string"},
				"note":     map[string]any{"type": "string"},
			}, "required": []string{"type", "data", "mimeType", "note"}},
		}},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file or directory",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Starting line number (1-indexed)",
				},
				"start_line": map[string]any{
					"type":        "integer",
					"description": "Starting line number (1-indexed)",
				},
				"end_line": map[string]any{
					"type":        "integer",
					"description": "Ending line number (inclusive)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to read (alternative to end_line)",
				},
			},
			"required": []string{"path"},
		},
		Execute: func(ctx ToolContext) (result *ToolResult, err error) {
			defer func() {
				if err != nil || result == nil || result.IsError {
					return
				}
				var output any = result.Content
				for _, block := range result.ContentBlocks {
					if block.Type == ai.ContentBlockImage {
						output = map[string]string{"type": "image", "data": block.Data, "mimeType": block.MimeType, "note": result.Content}
						break
					}
				}
				result.StructuredContent, err = json.Marshal(output)
				if err != nil {
					err = fmt.Errorf("encode read output: %w", err)
				}
			}()

			if ctx.Context != nil && ctx.Context.Err() != nil {
				return nil, ctx.Context.Err()
			}
			path, ok := ctx.Args["path"].(string)
			if !ok {
				return nil, ErrPathMustBeString
			}
			path, err = resolveReadPath(ctx.SessionCWD, path)
			if err != nil {
				return nil, err
			}

			// Use mutation queue for realpath resolution
			realpath, err := fmqueue.Realpath(path)
			if err != nil {
				return &ToolResult{
					Content: fmt.Sprintf("Error reading %s: %v", path, err),
					IsError: true,
				}, nil
			}

			info, err := ops.Access(realpath)
			if ctx.Context != nil && ctx.Context.Err() != nil {
				return nil, errors.Join(err, ctx.Context.Err())
			}
			if err != nil {
				return &ToolResult{
					Content: fmt.Sprintf("Error reading %s: %v", path, err),
					IsError: true,
				}, nil
			}

			// Directory: brief listing pointing at ls
			if info.IsDir() {
				entries, err := ops.Ls(realpath)
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return nil, errors.Join(err, ctx.Context.Err())
				}
				if err != nil {
					return &ToolResult{
						Content: fmt.Sprintf("Error reading directory %s: %v", path, err),
						IsError: true,
					}, nil
				}
				limit := 50
				if len(entries) < limit {
					limit = len(entries)
				}
				lines := make([]string, 0, limit)
				for _, entry := range entries[:limit] {
					name := entry.Name()
					if entry.IsDir() {
						name += "/"
					}
					lines = append(lines, name)
				}
				result := fmt.Sprintf("Directory %s (%d entries)\n%s", path, len(entries), strings.Join(lines, "\n"))
				if len(entries) > 50 {
					result += fmt.Sprintf("\n... and %d more. Use `ls` for full listing.", len(entries)-50)
				}
				return &ToolResult{Content: result}, nil
			}

			// Read file content
			data, err := ops.ReadFile(realpath)
			if ctx.Context != nil && ctx.Context.Err() != nil {
				return nil, errors.Join(err, ctx.Context.Err())
			}
			if err != nil {
				return &ToolResult{
					Content: fmt.Sprintf("Error reading %s: %v", path, err),
					IsError: true,
				}, nil
			}

			// Image detection by magic bytes
			if isImageFile(data, realpath) {
				if cfg.BlockImages {
					return &ToolResult{Content: fmt.Sprintf("Image file %s (%d bytes, %s) omitted by image settings.", path, len(data), detectImageMIME(data))}, nil
				}
				mimeType := ops.DetectImageMimeType(realpath)
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return nil, ctx.Context.Err()
				}
				if mimeType == "" {
					mimeType = detectImageMIME(data)
				}

				// Auto-resize image if enabled and needed
				imageData := data
				var resizeNote string
				displayDims := fmt.Sprintf("%d bytes", len(imageData))
				if autoResize {
					r := resizeImageData(imageData, mimeType, maxWidth, maxHeight)
					if r.Data != nil {
						imageData = r.Data
					}
					if r.OrigW > 0 && r.OrigH > 0 {
						displayDims = fmt.Sprintf("%dx%d", r.NewW, r.NewH)
					}
					resizeNote = r.Note
				}
				if ctx.Model != nil && ctx.Model.InputLimits != nil && ctx.Model.InputLimits.Images != nil {
					block, err := ResizeImageBlock(ai.ContentBlock{Type: ai.ContentBlockImage, Data: base64.StdEncoding.EncodeToString(imageData), MimeType: mimeType}, ctx.Model.InputLimits.Images.Resize)
					if err != nil {
						return &ToolResult{Content: err.Error(), IsError: true}, nil
					}
					imageData, err = base64.StdEncoding.DecodeString(block.Data)
					if err != nil {
						return nil, fmt.Errorf("decode resized image: %w", err)
					}
				}
				encoded := base64.StdEncoding.EncodeToString(imageData)
				content := resizeNote
				if content != "" {
					content += "\n"
				}
				content += fmt.Sprintf("[Image file %s (%s)]", path, displayDims)
				if ctx.Model != nil && !ctx.Model.SupportsInput(ai.InputImage) {
					content += "\n[Current model does not support images. The image will be omitted from this request.]"
				}
				return &ToolResult{
					ContentBlocks: []ai.ContentBlock{
						{Type: ai.ContentBlockText, Text: content},
						{
							Type:     ai.ContentBlockImage,
							Data:     encoded,
							MimeType: mimeType,
						},
					},
					Content: content,
				}, nil
			}

			// Binary detection: check first 8 KiB for NUL bytes or bad UTF-8
			checkSize := len(data)
			if checkSize > 8192 {
				checkSize = 8192
			}
			check := data[:checkSize]
			if !isTextData(check) {
				mimeGuess := "application/octet-stream"
				return &ToolResult{
					Content: fmt.Sprintf("Binary file: %s (%d bytes, %s). Use bash with `file`, `xxd`, or `hexdump` to inspect.", path, len(data), mimeGuess),
					IsError: true,
					Details: map[string]any{
						"error":      "binary_file",
						"path":       path,
						"size":       len(data),
						"mime_guess": mimeGuess,
					},
				}, nil
			}

			content, decodeErr := textunicode.UTF8.NewDecoder().String(string(data))
			if decodeErr != nil {
				return nil, fmt.Errorf("decode file text: %w", decodeErr)
			}
			lines := strings.Split(content, "\n")
			totalLines := len(lines)

			// Handle line range (support both start_line/end_line and offset/limit)
			startLine := 1
			endLine := totalLines

			// Check for offset (1-indexed start)
			if sl, ok := toolNumber(ctx.Args["start_line"]); ok && int(sl) > 0 {
				startLine = int(sl)
			}
			if sl, ok := toolNumber(ctx.Args["offset"]); ok && int(sl) > 0 {
				startLine = int(sl)
			}

			if startLine > totalLines {
				return &ToolResult{Content: fmt.Sprintf("Offset %d is beyond end of file (%d lines total)", startLine, totalLines), IsError: true}, nil
			}

			if el, ok := toolNumber(ctx.Args["end_line"]); ok && int(el) > 0 {
				endLine = min(int(el), totalLines)
			}
			limit, hasLimit := toolNumber(ctx.Args["limit"])
			if hasLimit {
				endLine = startLine - 1 + min(max(0, int(limit)), totalLines-startLine+1)
			}
			start := startLine - 1
			var selected string
			if start < endLine {
				selected = joinLines(lines[start:endLine])
			}
			truncation := truncateReadHead(selected, lineCap)
			text := truncation.Content
			var details map[string]any
			switch {
			case truncation.FirstLineExceedsLimit:
				text = fmt.Sprintf("[Line %d is %.1fKB, exceeds 50.0KB limit. Use bash: sed -n '%dp' %s | head -c %d]", startLine, float64(len(lines[start]))/1024, startLine, path, DefaultMaxBytes)
			case truncation.Truncated:
				end := startLine + truncation.OutputLines - 1
				var byteNote string
				if truncation.TruncatedBy == "bytes" {
					byteNote = " (50.0KB limit)"
				}
				text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d%s. Use offset=%d to continue.]", startLine, end, totalLines, byteNote, end+1)
			case hasLimit && endLine < totalLines:
				text += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", totalLines-endLine, endLine+1)
			}
			if truncation.Truncated {
				details = map[string]any{"truncation": truncation}
			}
			return &ToolResult{Content: text, Details: details}, nil
		},
	}
}

func truncateReadHead(content string, maxLines int) *TruncationResult {
	lines := splitLines(content)
	result := &TruncationResult{Content: content, TotalLines: len(lines), TotalBytes: len(content), OutputLines: len(lines), OutputBytes: len(content), MaxLines: maxLines, MaxBytes: DefaultMaxBytes}
	if len(lines) <= maxLines && len(content) <= DefaultMaxBytes {
		return result
	}
	result.Truncated = true
	result.TruncatedBy = "lines"
	result.OutputLines = 0
	result.OutputBytes = 0
	if len(lines) > 0 && len(lines[0]) > DefaultMaxBytes {
		result.FirstLineExceedsLimit = true
		result.TruncatedBy = "bytes"
	} else {
		for _, line := range lines[:min(len(lines), maxLines)] {
			bytes := len(line)
			if result.OutputLines > 0 {
				bytes++
			}
			if result.OutputBytes+bytes > DefaultMaxBytes {
				result.TruncatedBy = "bytes"
				break
			}
			result.OutputBytes += bytes
			result.OutputLines++
		}
	}
	result.Content = joinLines(lines[:result.OutputLines])
	return result
}

// ============================================================================
// Write Tool
// ============================================================================

// WriteTool returns a tool for writing files.
func WriteTool(cfg ToolConfig) Tool {
	mq := cfg.mutationQueue()
	ops := cfg.writeOps()
	return Tool{
		Name:        "write",
		Description: "Write content to a file, creating missing parent directories and replacing existing content",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to write",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Content to write to the file",
				},
				"create_parents": map[string]any{
					"type":        "boolean",
					"description": "Create missing parent directories (default true)",
				},
			},
			"required": []string{"path", "content"},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			path, ok := ctx.Args["path"].(string)
			if !ok {
				return nil, ErrPathMustBeString
			}
			content, ok := ctx.Args["content"].(string)
			if !ok {
				return nil, ErrContentMustBeString
			}
			var err error
			path, err = resolvePath(ctx.SessionCWD, path)
			if err != nil {
				return nil, err
			}

			createParents := true
			if requested, ok := ctx.Args["create_parents"].(bool); ok {
				createParents = requested
			}

			// Resolve realpath for the mutation queue
			realpath, err := fmqueue.Realpath(path)
			if err != nil {
				return nil, fmt.Errorf("resolving path: %w", err)
			}

			var result *ToolResult
			writeErr := mq.WithLock(realpath, func() error {
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				// Check parent exists
				dir := filepath.Dir(path)
				parentInfo, statErr := ops.Stat(dir)
				if statErr != nil && !os.IsNotExist(statErr) {
					result = &ToolResult{Content: fmt.Sprintf("Error checking parent directory %s: %v", dir, statErr), IsError: true}
					return nil
				}
				if statErr == nil && !parentInfo.IsDir() {
					result = &ToolResult{Content: fmt.Sprintf("Error: parent path %s is not a directory", dir), IsError: true}
					return nil
				}
				if os.IsNotExist(statErr) {
					if !createParents {
						result = &ToolResult{
							Content: fmt.Sprintf("Error: parent directory %s does not exist. Use create_parents=true to create it.", dir),
							IsError: true,
						}
						return nil
					}
					if mkdirErr := ops.Mkdir(dir); mkdirErr != nil {
						result = &ToolResult{
							Content: fmt.Sprintf("Error creating directories: %v", mkdirErr),
							IsError: true,
						}
						return nil
					}
				}

				// Preserve file mode on existing files, use 0644 for new
				perm := fs.FileMode(0o644)
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				if info, statErr := ops.Stat(path); statErr == nil {
					perm = info.Mode().Perm()
				} else if !os.IsNotExist(statErr) {
					result = &ToolResult{Content: fmt.Sprintf("Error checking file %s: %v", path, statErr), IsError: true}
					return nil
				}

				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				if writeErr := ops.WriteFile(realpath, []byte(content), perm); writeErr != nil {
					result = &ToolResult{
						Content: fmt.Sprintf("Error writing %s: %v", path, writeErr),
						IsError: true,
					}
					return nil
				}

				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				result = &ToolResult{Content: fmt.Sprintf("Wrote %d bytes to %s", len(content), path)}
				return nil
			})
			if writeErr != nil {
				return nil, writeErr
			}
			return result, nil
		},
	}
}

// ============================================================================
// Edit Tool
// ============================================================================

// EditConfig holds configuration for edit operations.
type EditConfig struct {
	Operations    EditOperations
	MutationQueue fmqueue.MutationQueue
}

// EditEdit represents a single edit operation.
type EditEdit struct {
	OldText    string
	NewText    string
	ReplaceAll bool
}

// EditToolDetails contains additional information about the edit result.
type EditToolDetails struct {
	// Diff is the unified diff of the changes made.
	Diff string
	// Patch is a standard unified patch.
	Patch string
	// FirstChangedLine is the line number of the first change.
	FirstChangedLine int
}

func prepareEditArguments(args map[string]any) map[string]any {
	prepared := maps.Clone(args)
	edits := prepared["edits"]
	if text, ok := edits.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			edits = parsed
		}
	}
	if edit, ok := edits.(map[string]any); ok {
		_, hasOld := edit["oldText"].(string)
		_, hasNew := edit["newText"].(string)
		if hasOld && hasNew {
			edits = []any{edit}
		}
	}
	if array, ok := edits.([]any); ok {
		prepared["edits"] = array
	}
	oldText, hasOld := prepared["oldText"].(string)
	newText, hasNew := prepared["newText"].(string)
	if hasOld && hasNew {
		array, _ := prepared["edits"].([]any)
		prepared["edits"] = append(slices.Clone(array), map[string]any{"oldText": oldText, "newText": newText})
		delete(prepared, "oldText")
		delete(prepared, "newText")
	}
	return prepared
}

// EditTool returns a tool for editing files with multi-edit support.
func EditTool(cfg ToolConfig) Tool {
	mq := cfg.mutationQueue()
	ops := cfg.editOps()

	return Tool{
		Name:             "edit",
		PrepareArguments: prepareEditArguments,
		Description:      "Edit a file using exact text replacement. Supports multiple edits via edits[] array.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to edit",
				},
				"edits": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"oldText": map[string]any{
								"type":        "string",
								"description": "Exact text to replace",
							},
							"replaceAll": map[string]any{"type": "boolean", "description": "Replace every occurrence of this edit's oldText (default false)"},
							"newText": map[string]any{
								"type":        "string",
								"description": "Replacement text",
							},
						},
						"required": []string{"oldText", "newText"},
					},
					"description": "One or more targeted replacements",
				},
				// Legacy support: single edit via old_string/new_string
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to replace (legacy parameter)",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text (legacy parameter)",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace all occurrences (legacy parameter, use edits[] for multi-edit)",
				},
			},
			"required": []string{"path"},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			path, ok := ctx.Args["path"].(string)
			if !ok {
				return nil, ErrPathMustBeString
			}
			var err error
			path, err = resolvePath(ctx.SessionCWD, path)
			if err != nil {
				return nil, err
			}

			// Parse edits from args
			var edits []EditEdit

			// Top-level replace_all flag seeds the legacy single-edit
			// replaceAll and is overridden per-edit by an explicit
			// "replaceAll" field inside edits[] entries.
			legacyReplaceAll, _ := ctx.Args["replace_all"].(bool)

			// Check for new edits[] format
			if editsArg, ok := ctx.Args["edits"].([]any); ok {
				for i, e := range editsArg {
					editMap, ok := e.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("edits[%d] must be an object", i)
					}
					oldText, ok := editMap["oldText"].(string)
					if !ok {
						return nil, fmt.Errorf("edits[%d].oldText must be a string", i)
					}
					newText, ok := editMap["newText"].(string)
					if !ok {
						return nil, fmt.Errorf("edits[%d].newText must be a string", i)
					}
					replaceAll, _ := editMap["replaceAll"].(bool)
					edits = append(edits, EditEdit{
						OldText:    oldText,
						NewText:    newText,
						ReplaceAll: replaceAll,
					})
				}
			}

			// Support legacy old_string/new_string format
			if len(edits) == 0 {
				oldText, hasOld := ctx.Args["old_string"].(string)
				newText, hasNew := ctx.Args["new_string"].(string)
				if hasOld && hasNew {
					edits = append(edits, EditEdit{OldText: oldText, NewText: newText, ReplaceAll: legacyReplaceAll})
				}
			}

			if len(edits) == 0 {
				return nil, fmt.Errorf("edit tool requires edits[] array or old_string/new_string parameters")
			}

			// Normalize edits: normalize line endings
			for i := range edits {
				edits[i].OldText = normalizeToLF(edits[i].OldText)
				edits[i].NewText = normalizeToLF(edits[i].NewText)
			}

			// Resolve realpath for mutation queue
			realpath, err := fmqueue.Realpath(path)
			if err != nil {
				return nil, fmt.Errorf("resolving path: %w", err)
			}

			var result *ToolResult
			var details *EditToolDetails

			writeErr := mq.WithLock(realpath, func() error {
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				accessErr := ops.Access(realpath)
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return errors.Join(accessErr, ctx.Context.Err())
				}
				if accessErr != nil {
					result = &ToolResult{Content: fmt.Sprintf("Could not edit file %s: %v", path, accessErr), IsError: true}
					return nil
				}
				data, err := ops.ReadFile(realpath)
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return errors.Join(err, ctx.Context.Err())
				}
				if err != nil {
					result = &ToolResult{
						Content: fmt.Sprintf("Error reading %s: %v", path, err),
						IsError: true,
					}
					return nil
				}

				content := string(data)

				// Strip BOM before matching
				hasBOM, content := stripBom(content)

				// Detect original line ending
				originalEnding := detectLineEnding(content)

				// Normalize content to LF for processing
				normalizedContent := normalizeToLF(content)

				// Apply edits to normalized content
				baseContent, newContent, err := applyEditsToNormalizedContent(normalizedContent, edits, path)
				if err != nil {
					result = &ToolResult{
						Content: err.Error(),
						IsError: true,
					}
					return nil
				}

				// Restore line endings and BOM
				finalContent := restoreLineEndings(newContent, originalEnding)
				if hasBOM {
					finalContent = "\uFEFF" + finalContent
				}

				// Preserve file mode
				perm := fs.FileMode(0o644)
				if info, statErr := os.Stat(path); statErr == nil {
					perm = info.Mode().Perm()
				}

				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				if writeErr := ops.WriteFile(realpath, []byte(finalContent), perm); writeErr != nil {
					result = &ToolResult{
						Content: fmt.Sprintf("Error writing %s: %v", path, writeErr),
						IsError: true,
					}
					return nil
				}

				if ctx.Context != nil && ctx.Context.Err() != nil {
					return ctx.Context.Err()
				}
				// Generate diff
				diff, firstLine := generateDiffString(baseContent, newContent)
				patch, err := generateUnifiedPatch(path, baseContent, newContent)
				if err != nil {
					return fmt.Errorf("generate edit patch: %w", err)
				}
				details = &EditToolDetails{
					Patch:            patch,
					Diff:             diff,
					FirstChangedLine: firstLine,
				}

				result = &ToolResult{
					Content: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), path),
				}
				return nil
			})

			if writeErr != nil {
				return nil, writeErr
			}

			if details != nil {
				result.Details = map[string]any{
					"patch":            details.Patch,
					"diff":             details.Diff,
					"firstChangedLine": details.FirstChangedLine,
				}
			}

			return result, nil
		},
	}
}

// ============================================================================
// Bash Tool
// ============================================================================

// BashToolConfig holds additional configuration for the bash tool.
type BashToolConfig struct {
	// Shell is the shell path to use.
	Shell string
	// OutputCap is the maximum output bytes before truncation.
	OutputCap int64
	// TempFile enables saving full output to temp file when truncated.
	TempFile bool
	// EnvAllowlist allows only specified environment variables.
	EnvAllowlist []string
	// EnvExtra adds extra environment variables.
	EnvExtra map[string]string
}

// PrefixBashCommand applies the configured shell prefix once.
func PrefixBashCommand(prefix, command string) string {
	if prefix == "" || strings.HasPrefix(command, prefix) {
		return command
	}
	return prefix + command
}

func bashResultIsError(res BashResult) bool {
	return res.ExitCode != 0 || (res.CancelReason != "" && res.CancelReason != BashCancelNone)
}

// BashTool returns a tool for executing shell commands.
func BashTool(cfg ToolConfig) Tool {
	ops := cfg.BashOperations()
	_ = cfg.bashTempFile() // ensure config is consulted
	return Tool{
		Name:        "bash",
		Description: "Execute a shell command in the terminal",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Working directory for the command (overrides session CWD)",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Maximum execution time in seconds (default 120, max 600)",
				},
				"exclude_from_context": map[string]any{
					"type":        "boolean",
					"description": "Keep output in the transcript but omit it from future model context",
				},
			},
			"required": []string{"command"},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			command, ok := ctx.Args["command"].(string)
			if !ok {
				return nil, ErrCommandMustBeString
			}
			command = PrefixBashCommand(cfg.CommandPrefix, command)

			cmdCtx := ctx.Context
			if cmdCtx == nil {
				cmdCtx = context.Background()
			}

			// Determine working directory
			dir := ctx.SessionCWD
			if cwd, ok := ctx.Args["cwd"].(string); ok && cwd != "" {
				var err error
				dir, err = resolvePath(ctx.SessionCWD, cwd)
				if err != nil {
					return nil, err
				}
			}

			// Determine timeout
			timeout := defaultBashTimeout
			if ts, ok := toolNumber(ctx.Args["timeout"]); ok && ts > 0 {
				timeout = time.Duration(ts * float64(time.Second))
				if timeout > maxBashTimeout {
					timeout = maxBashTimeout
				}
			}

			excludeFromContext, _ := ctx.Args["exclude_from_context"].(bool)

			req := BashRequest{
				Command: command,
				Dir:     dir,
				Timeout: timeout,
			}
			if ctx.OnUpdate != nil {
				req.OnOutput = func(chunk string) { ctx.OnUpdate(&ToolResult{Content: chunk}) }
			}

			res, runErr := ops.Run(cmdCtx, req)

			if runErr != nil {
				return nil, runErr
			}

			result := &ToolResult{
				Content:            res.Output,
				IsError:            bashResultIsError(res),
				ExcludeFromContext: excludeFromContext,
			}

			if res.CancelReason != BashCancelNone {
				result.Details = map[string]any{"cancel_reason": string(res.CancelReason)}
				if res.CancelReason == BashCancelReload {
					result.Content = "cancelled by /reload"
				}
			}

			// Include full output path if truncation occurred
			if res.FullOutputPath != "" {
				if result.Details == nil {
					result.Details = map[string]any{}
				}
				if details, ok := result.Details.(map[string]any); ok {
					details["full_output_path"] = res.FullOutputPath
				}
			}

			return result, nil
		},
	}
}

// ============================================================================
// Grep Tool
// ============================================================================
// GrepTool returns a tool for searching file contents. The ToolConfig's
// GrepOps field selects the search backend (ripgrep / pure-Go / mock);
// pass ToolConfig{} for the default ripgrep-or-pureGo behavior.
func GrepTool(cfg ToolConfig) Tool {
	return Tool{
		Name:        "grep",
		Description: "Search for text within files under a directory",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Pattern to search for (literal or regex)",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory or file path to search (default: session CWD)",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "File include pattern (e.g. '*.go')",
				},
				"ignoreCase": map[string]any{"type": "boolean", "description": "Case-insensitive search (default false)"},
				"literal":    map[string]any{"type": "boolean", "description": "Treat the pattern as literal text (default false)"},
				"limit":      map[string]any{"type": "integer", "description": "Maximum matches (default 100)"},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Legacy alias for ignoreCase",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Legacy alias for limit (max 1000)",
				},
				"context": map[string]any{
					"type":        "integer",
					"description": "Lines of context before/after each match (default 0)",
				},
			},
			"required": []string{"pattern"},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			searchCtx := ctx.Context
			if searchCtx == nil {
				searchCtx = context.Background()
			}
			if err := searchCtx.Err(); err != nil {
				return nil, err
			}
			pattern, ok := ctx.Args["pattern"].(string)
			if !ok {
				return nil, ErrPatternMustBeString
			}

			searchDir := "."
			if p, ok := ctx.Args["path"].(string); ok && p != "" {
				searchDir = p
			}
			searchPath, err := resolvePath(ctx.SessionCWD, searchDir)
			if err != nil {
				return nil, err
			}
			caseInsensitive, _ := ctx.Args["case_insensitive"].(bool)
			if canonical, ok := ctx.Args["ignoreCase"].(bool); ok {
				caseInsensitive = canonical
			}
			if literal, _ := ctx.Args["literal"].(bool); literal {
				pattern = regexp.QuoteMeta(pattern)
			}
			maxResults := defaultGrepMaxResults
			if mr, ok := toolNumber(ctx.Args["max_results"]); ok && int(mr) > 0 {
				maxResults = int(mr)
				if maxResults > maxGrepMaxResults {
					maxResults = maxGrepMaxResults
				}
			}
			if limit, ok := toolNumber(ctx.Args["limit"]); ok {
				maxResults = max(1, int(limit))
			}
			contextLines := 0
			if cl, ok := toolNumber(ctx.Args["context"]); ok && int(cl) > 0 {
				contextLines = int(cl)
			}
			globPattern, _ := ctx.Args["glob"].(string)

			result, err := cfg.grepOps().Grep(searchCtx, pattern, searchPath, globPattern, caseInsensitive, maxResults, contextLines)
			if searchCtx.Err() != nil {
				return nil, errors.Join(err, searchCtx.Err())
			}
			return result, err
		},
	}
}

// ============================================================================
// Find Tool
// ============================================================================

// FindTool returns a tool for finding files. The ToolConfig's FindOps field
// selects the walk backend; pass ToolConfig{} for the default filepath.Walk
// behavior with ripgrep-style filtering.
func FindTool(cfg ToolConfig) Tool {
	return Tool{
		Name:        "find",
		Description: "Find files or directories by name pattern. Returns paths relative to the search directory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern to match files"},
				"limit":   map[string]any{"type": "integer", "description": "Maximum results (default 1000)"},
				"query": map[string]any{
					"type":        "string",
					"description": "Search query",
				},
				"mode": map[string]any{
					"type":        "string",
					"description": "Search mode: 'glob', 'substring', or 'regex' (default 'glob')",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Root directory to search (default: session CWD)",
				},
				"max_depth": map[string]any{
					"type":        "integer",
					"description": "Maximum directory depth",
				},
				"include_hidden": map[string]any{
					"type":        "boolean",
					"description": "Include hidden files (default true)",
				},
				"no_ignore": map[string]any{
					"type":        "boolean",
					"description": "Don't skip default ignore directories (default false)",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum results (legacy alias for limit, max 5000)",
				},
			},
			"anyOf": []any{map[string]any{"required": []string{"pattern"}}, map[string]any{"required": []string{"query"}}},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			searchCtx := ctx.Context
			if searchCtx == nil {
				searchCtx = context.Background()
			}
			if err := searchCtx.Err(); err != nil {
				return nil, err
			}
			queryArg, present := ctx.Args["pattern"]
			if !present {
				queryArg = ctx.Args["query"]
			}
			query, ok := queryArg.(string)
			if !ok {
				return nil, ErrPatternMustBeString
			}

			searchDir := "."
			if p, ok := ctx.Args["path"].(string); ok && p != "" {
				searchDir = p
			}
			searchPath, err := resolvePath(ctx.SessionCWD, searchDir)
			if err != nil {
				return nil, err
			}

			mode := "glob"
			if m, ok := ctx.Args["mode"].(string); ok && m != "" {
				mode = m
			}

			maxDepth := -1
			if md, ok := toolNumber(ctx.Args["max_depth"]); ok && int(md) > 0 {
				maxDepth = int(md)
			}

			includeHidden := true
			if hidden, ok := ctx.Args["include_hidden"].(bool); ok {
				includeHidden = hidden
			}
			noIgnore, _ := ctx.Args["no_ignore"].(bool)

			maxResults := defaultFindMaxResults
			if mr, ok := toolNumber(ctx.Args["max_results"]); ok && int(mr) > 0 {
				maxResults = int(mr)
				if maxResults > maxFindMaxResults {
					maxResults = maxFindMaxResults
				}
			}

			if value, present := ctx.Args["limit"]; present {
				maxResults = defaultFindMaxResults
				if limit, ok := toolNumber(value); ok && limit > 0 {
					maxResults = int(limit)
				}
			}

			matches, err := cfg.findOps().Find(searchCtx, query, searchPath, mode, maxDepth, includeHidden, noIgnore, maxResults)
			if searchCtx.Err() != nil {
				return nil, errors.Join(err, searchCtx.Err())
			}
			if err != nil {
				return &ToolResult{
					Content: err.Error(),
					IsError: true,
				}, nil
			}

			if len(matches) == 0 {
				return &ToolResult{Content: "No files found matching pattern"}, nil
			}
			relative := make([]string, len(matches))
			for i, match := range matches {
				trailing := strings.HasSuffix(match, string(filepath.Separator)) || runtime.GOOS == "windows" && strings.HasSuffix(match, "/")
				if filepath.IsAbs(match) {
					match, err = filepath.Rel(searchPath, match)
					if err != nil {
						return nil, fmt.Errorf("relativize find result: %w", err)
					}
				}
				relative[i] = filepath.ToSlash(match)
				if trailing && !strings.HasSuffix(relative[i], "/") {
					relative[i] += "/"
				}
			}
			bounded := truncateReadHead(strings.Join(relative, "\n"), len(relative))
			result := &ToolResult{Content: bounded.Content}
			details := make(map[string]any)
			var notices []string
			if len(matches) >= maxResults {
				details["resultLimitReached"] = maxResults
				notices = append(notices, fmt.Sprintf("%d results limit reached", maxResults))
			}
			if bounded.Truncated {
				details["truncation"] = bounded
				notices = append(notices, "50.0KB limit reached")
			}
			if len(notices) > 0 {
				result.Content += "\n\n[" + strings.Join(notices, ". ") + "]"
				result.Details = details
			}
			return result, nil
		},
	}
}

// ============================================================================
// LS Tool
// ============================================================================

// LSTool returns a tool for listing directory contents. The ToolConfig's
// LsOps field selects the directory-read backend; pass ToolConfig{} for the
// default os.ReadDir behavior.
func LSTool(cfg ToolConfig) Tool {
	return Tool{
		Name:        "ls",
		Description: "List directory contents",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path to list (default: session CWD)",
				},
				"sort_by": map[string]any{
					"type":        "string",
					"description": "Sort by: 'name', 'mtime', or 'size' (default 'name')",
				},
				"reverse": map[string]any{
					"type":        "boolean",
					"description": "Reverse sort order (default false)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of entries to return (default 1000)",
				},
			},
		},
		Execute: func(ctx ToolContext) (*ToolResult, error) {
			if ctx.Context != nil && ctx.Context.Err() != nil {
				return nil, ctx.Context.Err()
			}
			dirPath := ctx.SessionCWD
			if p, ok := ctx.Args["path"].(string); ok && p != "" {
				var err error
				dirPath, err = resolvePath(ctx.SessionCWD, p)
				if err != nil {
					return nil, err
				}
			}

			sortBy := "name"
			if s, ok := ctx.Args["sort_by"].(string); ok && s != "" {
				sortBy = s
			}
			reverse, _ := ctx.Args["reverse"].(bool)

			// Parse limit parameter
			limit := defaultLSMaxEntries
			if lim, ok := toolNumber(ctx.Args["limit"]); ok && int(lim) > 0 {
				limit = int(lim)
			}

			ops := cfg.lsOps()
			entries, err := ops.Ls(dirPath)
			if ctx.Context != nil && ctx.Context.Err() != nil {
				return nil, errors.Join(err, ctx.Context.Err())
			}
			if err != nil {
				return &ToolResult{
					Content: fmt.Sprintf("Error listing %s: %v", dirPath, err),
					IsError: true,
				}, nil
			}

			// Build entry info
			type entryInfo struct {
				name  string
				typ   string // f, d, l
				size  int64
				mtime time.Time
			}

			infos := make([]entryInfo, 0, len(entries))
			for _, entry := range entries {
				info, err := ops.Stat(filepath.Join(dirPath, entry.Name()))
				if ctx.Context != nil && ctx.Context.Err() != nil {
					return nil, errors.Join(err, ctx.Context.Err())
				}
				if err != nil {
					continue
				}
				typ := "f"
				if info.IsDir() {
					typ = "d"
				} else if info.Mode()&os.ModeSymlink != 0 {
					typ = "l"
				}
				infos = append(infos, entryInfo{
					name:  entry.Name(),
					typ:   typ,
					size:  info.Size(),
					mtime: info.ModTime(),
				})
			}

			if len(infos) == 0 {
				return &ToolResult{Content: "(empty directory)"}, nil
			}

			// Sort
			slices.SortStableFunc(infos, func(a, b entryInfo) int {
				var order int
				switch sortBy {
				case "mtime":
					order = a.mtime.Compare(b.mtime)
				case "size":
					order = cmp.Compare(a.size, b.size)
				default:
					order = strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
				}
				if reverse {
					return -order
				}
				return order
			})

			// Apply limit and check for truncation
			truncated := false
			totalEntries := len(infos)
			if len(infos) > limit {
				infos = infos[:limit]
				truncated = true
			}

			lines := make([]string, 0, len(infos))
			for _, info := range infos {
				lines = append(lines, fmt.Sprintf("%s %8d %s %s", info.typ, info.size, info.mtime.Format("Jan 02 15:04"), info.name))
			}

			bounded := truncateReadHead(strings.Join(lines, "\n"), len(lines))
			result := bounded.Content
			details := make(map[string]any)
			if truncated {
				result += fmt.Sprintf("\n... [truncated, showing first %d of %d entries]", limit, totalEntries)
				details["entryLimitReached"] = limit
			}
			if bounded.Truncated {
				result += "\n\n[50.0KB limit reached]"
				details["truncation"] = bounded
			}
			output := &ToolResult{Content: result}
			if len(details) > 0 {
				output.Details = details
			}
			return output, nil
		},
	}
}

// ============================================================================
// BuiltInTools Factory
// ============================================================================

// BuiltInTools returns all built-in tools with default configuration.
func BuiltInTools() []Tool {
	return BuiltInToolsWithConfig(ToolConfig{})
}

// BuiltInToolsWithConfig returns all built-in tools with the given configuration.
func BuiltInToolsWithConfig(cfg ToolConfig) []Tool {
	return []Tool{
		ReadTool(cfg),
		WriteTool(cfg),
		EditTool(cfg),
		BashTool(cfg),
		GrepTool(cfg),
		FindTool(cfg),
		LSTool(cfg),
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func joinLines(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

func isTextData(data []byte) bool {
	// Check for NUL bytes
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	// Check UTF-8 validity with small tolerance
	if !utf8.Valid(data) {
		// Allow a small number of invalid sequences
		invalid := 0
		for len(data) > 0 {
			_, sz := utf8.DecodeRune(data)
			if sz == 1 && data[0] == 0xEF { // could be BOM-ish
				invalid++
			}
			data = data[sz:]
		}
		return invalid < 3
	}
	return true
}

func isImageFile(data []byte, path string) bool {
	if len(data) < 12 {
		return false
	}
	// Check magic bytes
	if bytesHasPrefix(data, pngMagic) {
		return true
	}
	if bytesHasPrefix(data, jpegMagic) {
		return true
	}
	if bytesHasPrefix(data, gifMagic) {
		return true
	}
	// WebP: RIFF....WEBP
	if bytesHasPrefix(data, webpMagic) && len(data) >= 12 && string(data[8:12]) == "WEBP" {
		return true
	}
	return false
}

func bytesHasPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if data[i] != b {
			return false
		}
	}
	return true
}

func detectImageMIME(data []byte) string {
	if bytesHasPrefix(data, pngMagic) {
		return "image/png"
	}
	if bytesHasPrefix(data, jpegMagic) {
		return "image/jpeg"
	}
	if bytesHasPrefix(data, gifMagic) {
		return "image/gif"
	}
	if bytesHasPrefix(data, webpMagic) && len(data) >= 12 && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "application/octet-stream"
}

// ============================================================================
// Line Ending & BOM Utilities
// ============================================================================

// detectLineEnding detects the original line ending style.
func detectLineEnding(content string) string {
	crlfIdx := strings.Index(content, "\r\n")
	lfIdx := strings.Index(content, "\n")
	if lfIdx == -1 {
		return "\n"
	}
	if crlfIdx == -1 {
		return "\n"
	}
	if crlfIdx < lfIdx {
		return "\r\n"
	}
	return "\n"
}

// normalizeToLF normalizes all line endings to LF.
func normalizeToLF(text string) string {
	// First normalize CRLF to LF, then normalize any remaining CR to LF
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// restoreLineEndings restores the original line ending style.
func restoreLineEndings(text string, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// stripBom strips the UTF-8 BOM if present.
// Returns (hasBOM, contentWithoutBOM).
func stripBom(content string) (bool, string) {
	if strings.HasPrefix(content, "\uFEFF") {
		return true, strings.TrimPrefix(content, "\uFEFF")
	}
	return false, content
}

// ============================================================================
// Edit Diff Utilities
// ============================================================================

// applyEditsToNormalizedContent applies edits to LF-normalized content.
// All edits are matched against the same original content. Replacements are
// applied in reverse order so offsets remain stable.
//
// Each EditEdit may opt into ReplaceAll mode, which replaces every
// occurrence of OldText in the content rather than erroring on ambiguity.
// Ambiguous non-replaceAll matches return a structured *ErrEditAmbiguousPayload
// so callers can surface match counts and sample line numbers.
func applyEditsToNormalizedContent(content string, edits []EditEdit, path string) (string, string, error) {
	for i, edit := range edits {
		if edit.OldText == "" {
			return "", "", fmt.Errorf("edits[%d].oldText must not be empty in %s", i, path)
		}
	}

	// Decide the match basis up front. If any edit fails exact matching,
	// fall back to a fuzzy-normalized view for ALL edits. This avoids
	// mutating the basis halfway through the loop when some edits succeed
	// on the original content and others need fuzzy normalization.
	basis := content
	usedFuzzy := false
	for _, edit := range edits {
		if !strings.Contains(basis, edit.OldText) {
			fuzzyContent := normalizeForFuzzyMatch(content)
			if strings.Contains(fuzzyContent, normalizeForFuzzyMatch(edit.OldText)) {
				basis = fuzzyContent
				usedFuzzy = true
			}
		}
	}

	// findAllOccurrences returns every non-overlapping occurrence of needle
	// in haystack, in left-to-right order. Used by replace-all and by the
	// ambiguity check for non-replaceAll edits.
	findAllOccurrences := func(haystack, needle string) []int {
		var positions []int
		if needle == "" {
			return positions
		}
		start := 0
		for {
			idx := strings.Index(haystack[start:], needle)
			if idx == -1 {
				return positions
			}
			positions = append(positions, start+idx)
			start += idx + len(needle)
		}
	}

	// matchedEdit captures one (position, replacement) pair. For
	// replace-all edits, the loop appends one entry per occurrence.
	type matchedEdit struct {
		index    int
		matchLen int
		newText  string
		editIdx  int // original edits[] index, for error messages
	}
	var matches []matchedEdit

	for editIdx, edit := range edits {
		needle := edit.OldText
		if usedFuzzy {
			needle = normalizeForFuzzyMatch(needle)
		}

		occurrences := findAllOccurrences(basis, needle)
		if len(occurrences) == 0 {
			if len(edits) == 1 {
				return "", "", fmt.Errorf("could not find the exact text in %s: the old text must match exactly including all whitespace and newlines", path)
			}
			return "", "", fmt.Errorf("could not find edits[%d] in %s: the oldText must match exactly", editIdx, path)
		}

		if len(occurrences) > 1 && !edit.ReplaceAll {
			// Ambiguous match: return a structured payload so the caller
			// can surface match count and sample line numbers.
			return "", "", &ErrEditAmbiguousPayload{
				Path:        path,
				MatchCount:  len(occurrences),
				SampleLines: sampleLineNumbers(basis, occurrences, 5),
			}
		}

		for _, idx := range occurrences {
			matches = append(matches, matchedEdit{
				index:    idx,
				matchLen: len(needle),
				newText:  edit.NewText,
				editIdx:  editIdx,
			})
		}
	}

	// Check for duplicate matches (only relevant when two distinct edits
	// happen to land on the same position).
	for i, m1 := range matches {
		for j, m2 := range matches {
			if i != j && m1.index == m2.index {
				if len(edits) == 1 {
					return "", "", fmt.Errorf("found multiple occurrences of the text in %s: the text must be unique", path)
				}
				return "", "", fmt.Errorf("found multiple occurrences of edits[%d] in %s: each oldText must be unique", m1.editIdx, path)
			}
		}
	}

	// Sort by index (ascending) so overlapping checks are deterministic.
	slices.SortFunc(matches, func(a, b matchedEdit) int {
		return cmp.Compare(a.index, b.index)
	})

	// Check for overlapping edits.
	for i := 1; i < len(matches); i++ {
		prev := matches[i-1]
		curr := matches[i]
		if prev.index+prev.matchLen > curr.index {
			return "", "", fmt.Errorf("edits overlap in %s: merge them into one edit or target disjoint regions", path)
		}
	}

	// Apply edits in reverse order so earlier offsets remain stable.
	baseContent := content
	newContent := basis
	if !usedFuzzy {
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			newContent = newContent[:m.index] + m.newText + newContent[m.index+m.matchLen:]
		}
	} else {
		// Fuzzy offsets refer to normalized bytes; only rewrite touched lines.

		originalLines := strings.SplitAfter(content, "\n")
		baseLines := strings.SplitAfter(basis, "\n")
		offsets := make([]int, len(baseLines)+1)
		for i, line := range baseLines {
			offsets[i+1] = offsets[i] + len(line)
		}
		var preserved strings.Builder
		var nextLine int
		for i := 0; i < len(matches); {
			startLine := strings.Count(basis[:matches[i].index], "\n")
			endLine := strings.Count(basis[:matches[i].index+matches[i].matchLen-1], "\n") + 1
			endMatch := i + 1
			for endMatch < len(matches) {
				m := matches[endMatch]
				if strings.Count(basis[:m.index], "\n") >= endLine {
					break
				}
				endLine = max(endLine, strings.Count(basis[:m.index+m.matchLen-1], "\n")+1)
				endMatch++
			}
			for _, line := range originalLines[nextLine:startLine] {
				preserved.WriteString(line)
			}
			startOffset := offsets[startLine]
			changed := basis[startOffset:offsets[endLine]]
			for j := endMatch - 1; j >= i; j-- {
				m := matches[j]
				index := m.index - startOffset
				changed = changed[:index] + m.newText + changed[index+m.matchLen:]
			}
			preserved.WriteString(changed)
			nextLine = endLine
			i = endMatch
		}
		for _, line := range originalLines[nextLine:] {
			preserved.WriteString(line)
		}
		newContent = preserved.String()
	}

	// Check for no-change.
	if baseContent == newContent {
		if len(edits) == 1 {
			return "", "", fmt.Errorf("no changes made to %s: the replacement produced identical content", path)
		}
		return "", "", fmt.Errorf("no changes made to %s: the replacements produced identical content", path)
	}

	return baseContent, newContent, nil
}

// sampleLineNumbers returns the 1-based line numbers of the first limit
// occurrences in content. Used by ErrEditAmbiguousPayload so the user
// sees where the ambiguous matches are.
func sampleLineNumbers(content string, occurrences []int, limit int) []int {
	if limit <= 0 || len(occurrences) == 0 {
		return nil
	}
	if limit > len(occurrences) {
		limit = len(occurrences)
	}
	lines := make([]int, 0, limit)
	for _, off := range occurrences[:limit] {
		// Count newlines before offset, then add 1 (1-based).
		line := 1
		for i := 0; i < off && i < len(content); i++ {
			if content[i] == '\n' {
				line++
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// normalizeForFuzzyMatch normalizes text for fuzzy matching.
// Strips trailing whitespace, normalizes smart quotes, etc.
func normalizeForFuzzyMatch(text string) string {
	lines := strings.Split(norm.NFKC.String(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], func(r rune) bool {
			return (unicode.IsSpace(r) && r != '\u0085') || r == '\ufeff'
		})
	}
	text = strings.Join(lines, "\n")

	// Normalize smart quotes to ASCII
	text = strings.ReplaceAll(text, "\u2018", "'")
	text = strings.ReplaceAll(text, "\u2019", "'")
	text = strings.ReplaceAll(text, "\u201A", "'")
	text = strings.ReplaceAll(text, "\u201B", "'")
	text = strings.ReplaceAll(text, "\u201C", "\"")
	text = strings.ReplaceAll(text, "\u201D", "\"")
	text = strings.ReplaceAll(text, "\u201E", "\"")
	text = strings.ReplaceAll(text, "\u201F", "\"")

	// Normalize dashes/hyphens
	dashes := []string{
		"\u2010", // hyphen
		"\u2011", // non-breaking hyphen
		"\u2012", // figure dash
		"\u2013", // en-dash
		"\u2014", // em-dash
		"\u2015", // horizontal bar
		"\u2212", // minus
	}
	for _, d := range dashes {
		text = strings.ReplaceAll(text, d, "-")
	}

	// Normalize special spaces to regular space
	specialSpaces := []string{
		"\u00A0", // NBSP
		"\u2002", // en space
		"\u2003", // em space
		"\u2004", // three-per-em space
		"\u2005", // four-per-em space
		"\u2006", // six-per-em space
		"\u2007", // figure space
		"\u2008", // punctuation space
		"\u2009", // thin space
		"\u200A", // hair space
		"\u202F", // narrow NBSP
		"\u205F", // medium math space
		"\u3000", // ideographic space
	}
	for _, s := range specialSpaces {
		text = strings.ReplaceAll(text, s, " ")
	}

	return text
}

func generateUnifiedPatch(path, oldContent, newContent string) (string, error) {
	lines := func(content string) []string {
		if content == "" {
			return nil
		}
		parts := strings.SplitAfter(content, "\n")
		if parts[len(parts)-1] == "" {
			return parts[:len(parts)-1]
		}
		parts[len(parts)-1] += "\n\\ No newline at end of file\n"
		return parts
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: lines(oldContent), B: lines(newContent), FromFile: path, ToFile: path, Context: 4})
}

// generateDiffString generates a unified diff string with line numbers.
func generateDiffString(oldContent, newContent string) (string, int) {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	maxLineNum := len(oldLines)
	if len(newLines) > maxLineNum {
		maxLineNum = len(newLines)
	}
	lineNumWidth := len(fmt.Sprintf("%d", maxLineNum))

	// difflib's matcher works in memory linear in the line counts; a full LCS
	// table is quadratic and runs to gigabytes on a large source file.
	var output []string
	firstChangedLine := -1
	line := func(sign byte, num int, text string) {
		output = append(output, fmt.Sprintf("%c%*d %s", sign, lineNumWidth, num, text))
	}
	split := func(content string) []string {
		if content == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	oldLines, newLines = split(oldContent), split(newContent)
	ops := difflib.NewMatcher(oldLines, newLines).GetOpCodes()
	for index, op := range ops {
		if op.Tag == 'e' {
			before, after := index > 0, index < len(ops)-1
			omitted := false
			for k := range op.I2 - op.I1 {
				if before && k < 4 || after && k >= op.I2-op.I1-4 {
					line(' ', op.I1+k+1, oldLines[op.I1+k])
					omitted = false
				} else if (before || after) && !omitted {
					output = append(output, fmt.Sprintf(" %*s ...", lineNumWidth, ""))
					omitted = true
				}
			}
			continue
		}
		if firstChangedLine == -1 {
			firstChangedLine = op.J1 + 1
		}
		for i := op.I1; i < op.I2; i++ {
			line('-', i+1, oldLines[i])
		}
		for j := op.J1; j < op.J2; j++ {
			line('+', j+1, newLines[j])
		}
	}

	return strings.Join(output, "\n"), firstChangedLine
}

// ============================================================================
// Image Resize Utilities
// ============================================================================
//
// resizeImageData is implemented in image_resize.go.

// ============================================================================
// Grep Implementation
// ============================================================================

func grepWithRG(ctx context.Context, rgPath, pattern, searchPath, globPattern string, caseInsensitive bool, maxResults, contextLines int) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	args := []string{"--no-heading", "--with-filename", "--line-number"}
	if caseInsensitive {
		args = append(args, "-i")
	}
	if globPattern != "" {
		args = append(args, "--glob", globPattern)
	}
	if maxResults > 0 {
		args = append(args, "--max-count", fmt.Sprintf("%d", maxResults))
	}
	if contextLines > 0 {
		args = append(args, "-C", fmt.Sprintf("%d", contextLines))
	}
	args = append(args, "--", pattern, searchPath)

	cmd := exec.CommandContext(ctx, rgPath, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if err != nil {
		// rg returns exit code 1 when no matches found
		if cmd.ProcessState.ExitCode() == 1 {
			return &ToolResult{Content: "No matches found"}, nil
		}
		return &ToolResult{
			Content: fmt.Sprintf("Error running rg: %v\n%s", err, string(output)),
			IsError: true,
		}, nil
	}

	result := string(output)
	if len(result) > defaultGrepOutputCap {
		result = result[:defaultGrepOutputCap] + fmt.Sprintf("\n... [truncated, %d bytes total]", len(result))
	}
	return &ToolResult{Content: result}, nil
}

func grepPureGo(ctx context.Context, pattern, searchPath, globPattern string, caseInsensitive bool, maxResults, contextLines int) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return &ToolResult{Content: fmt.Sprintf("Invalid regex: %v", err), IsError: true}, nil
	}
	if caseInsensitive {
		re = regexp.MustCompile("(?i)" + re.String())
	}

	var matches []string
	matchCount := 0

	walkErr := filepath.Walk(searchPath, func(current string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if err != nil {
			if current == searchPath {
				return err
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}

		// Glob filter
		if globPattern != "" {
			matched, _ := filepath.Match(globPattern, info.Name())
			if !matched {
				return nil
			}
		}

		data, err := os.ReadFile(current)
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if err != nil {
			return nil
		}

		lines := splitLines(string(data))
		outputLines := make(map[int]bool) // Track which lines have been output

		for i, line := range lines {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if re.MatchString(line) {
				matchCount++
				if matchCount > maxResults {
					return fmt.Errorf("max results reached")
				}

				// Output match line
				matches = append(matches, fmt.Sprintf("%s:%d:%s", current, i+1, line))
				outputLines[i] = true

				// Output context lines
				for c := 1; c <= contextLines; c++ {
					before := i - c
					after := i + c
					for _, idx := range []int{before, after} {
						if idx >= 0 && idx < len(lines) && !outputLines[idx] {
							// ripgrep uses no prefix for context lines, just the line
							matches = append(matches, fmt.Sprintf("%s-%d-line%d:%s", current, idx+1, idx+1, lines[idx]))
							outputLines[idx] = true
						}
					}
				}
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, errors.Join(walkErr, ctx.Err())
	}
	if walkErr != nil && walkErr.Error() != "max results reached" {
		return &ToolResult{
			Content: fmt.Sprintf("Error searching %s: %v", searchPath, walkErr),
			IsError: true,
		}, nil
	}

	if len(matches) == 0 {
		return &ToolResult{Content: "No matches found"}, nil
	}

	result := strings.Join(matches, "\n")
	if len(result) > defaultGrepOutputCap {
		result = result[:defaultGrepOutputCap] + fmt.Sprintf("\n... [truncated, %d bytes total]", len(result))
	}
	return &ToolResult{Content: result}, nil
}

// ============================================================================
// Shell Configuration (getShellConfig equivalent)
// ============================================================================

// cachedShellConfig holds the cached shell configuration.
var cachedShellConfig *shellConfigCache

// shellConfigCache holds the cached shell and arguments.
type shellConfigCache struct {
	shell string
	args  []string
}

// GetShellConfig returns the shell configuration based on platform and settings.
// Resolution order:
//  1. User-specified shellPath in settings (if provided)
//  2. TAU_SHELL environment variable
//  3. Unix: /bin/bash, then bash on PATH, then fallback to sh
//
// The result is cached after first call (unless settings is provided).
func GetShellConfig(settings *ai.Settings) (shell string, args []string) {
	// Check settings for custom shell path first
	if settings != nil && settings.Bash.Shell != "" {
		// Verify the shell path exists
		if exists(settings.Bash.Shell) {
			return settings.Bash.Shell, []string{"-c"}
		}
		// If shell doesn't exist, fall through to other options
	}

	// Check TAU_SHELL environment variable
	if s := os.Getenv("TAU_SHELL"); s != "" {
		return s, []string{"-c"}
	}

	// Check cached value (only use cache if no settings provided)
	if cachedShellConfig != nil && settings == nil {
		return cachedShellConfig.shell, cachedShellConfig.args
	}

	// Unix: try /bin/bash, then bash on PATH, then fallback to sh
	if exists("/bin/bash") {
		shell, args = "/bin/bash", []string{"-c"}
	} else if bashOnPath := findBashOnPath(); bashOnPath != "" {
		shell, args = bashOnPath, []string{"-c"}
	} else {
		shell, args = "/bin/sh", []string{"-c"}
	}

	// Cache result (only if no settings override was used)
	if settings == nil || settings.Bash.Shell == "" {
		cachedShellConfig = &shellConfigCache{shell: shell, args: args}
	}

	return shell, args
}

// findBashOnPath searches for bash executable on PATH using exec.LookPath.
// Returns empty string if not found.
func findBashOnPath() string {
	path, err := exec.LookPath("bash")
	if err != nil {
		return ""
	}
	return path
}

// GetShellEnv returns the environment for shell commands.
func GetShellEnv() []string {
	return os.Environ()
}

// exists checks if a file exists at the given path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// GenerateTempFilePath generates a unique temp file path for bash output.
func GenerateTempFilePath() string {
	b := make([]byte, 8)
	rand.Read(b)
	return filepath.Join(os.TempDir(), fmt.Sprintf("tau-bash-%x.log", b))
}

func toolNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func matchFindPathGlob(pattern, candidate string) (bool, error) {
	parts := strings.Split(candidate, "/")
	matched := make([]bool, len(parts)+1)
	matched[0] = true
	for _, segment := range strings.Split(pattern, "/") {
		next := make([]bool, len(matched))
		if segment == "**" {
			next[0] = matched[0]
			for i := range parts {
				next[i+1] = matched[i+1] || next[i]
			}
		} else {
			for i, part := range parts {
				matches, err := filepath.Match(segment, part)
				if err != nil {
					return false, fmt.Errorf("match find glob: %w", err)
				}
				next[i+1] = matched[i] && matches
			}
		}
		matched = next
	}
	return matched[len(parts)], nil
}
