// Package artifacts provides unified discovery for skills, prompt templates,
// and themes. The discovery walker scans source tiers and yields entries that
// each registry consumes and parses according to its schema.
package artifacts // import "github.com/losinggeneration/tau/agent/artifacts"

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/packages"
	"github.com/losinggeneration/tau/commands"
)

// SourceTier represents where an artifact was discovered.
type SourceTier int

const (
	// TierCLI is the highest tier: explicitly passed via CLI flags.
	TierCLI SourceTier = iota
	// TierProject is for project-local artifacts (.tau/...).
	TierProject
	// TierUser is for user-global artifacts (~/.tau/...).
	TierUser
	// TierSettings is for artifacts declared in settings.json.
	TierSettings
	// TierPackages is for package-installed artifacts.
	TierPackages
	// TierBuiltin is the lowest tier: built-in commands.
	TierBuiltin
)

// tierCount is the number of declared SourceTier values. It must be updated
// whenever a new tier is added to the iota block above.
const tierCount = int(TierBuiltin) + 1

// Precedence returns the precedence rank of the tier. Higher value = higher
// precedence (wins on collision). TierCLI has the highest precedence; the
// last iota (TierBuiltin) has the lowest. The tier with the smallest iota
// wins on collision, matching Pi's resource-discovery precedence.
//
// Use this when ordering entries for first-wins registration; do not
// rely on the raw iota value directly.
func (t SourceTier) Precedence() int {
	return tierCount - 1 - int(t)
}

// String returns a human-readable name for the tier.
func (t SourceTier) String() string {
	switch t {
	case TierCLI:
		return "cli"
	case TierProject:
		return "project"
	case TierUser:
		return "user"
	case TierSettings:
		return "settings"
	case TierPackages:
		return "packages"
	case TierBuiltin:
		return "builtin"
	default:
		return "unknown"
	}
}

// ParseTier parses a tier string into a SourceTier value.
func ParseTier(s string) (SourceTier, error) {
	switch s {
	case "cli":
		return TierCLI, nil
	case "project":
		return TierProject, nil
	case "user":
		return TierUser, nil
	case "settings":
		return TierSettings, nil
	case "packages":
		return TierPackages, nil
	case "builtin":
		return TierBuiltin, nil
	default:
		return TierBuiltin, fmt.Errorf("unknown tier %q", s)
	}
}

// ToCommandsTier converts this SourceTier to the commands.Tier equivalent.
// Both enums have the same six tiers but in different order; this maps between them.
func (t SourceTier) ToCommandsTier() commands.Tier {
	switch t {
	case TierCLI:
		return commands.TierCLI
	case TierProject:
		return commands.TierProject
	case TierUser:
		return commands.TierUser
	case TierSettings:
		return commands.TierSettings
	case TierPackages:
		return commands.TierPackages
	case TierBuiltin:
		return commands.TierBuiltin
	default:
		return commands.TierBuiltin
	}
}

// ArtifactKind represents the type of artifact.
type ArtifactKind string

const (
	KindSkill     ArtifactKind = "skill"
	KindPrompt    ArtifactKind = "prompt"
	KindTheme     ArtifactKind = "theme"
	KindExtension ArtifactKind = "extension"
)

// Entry represents a discovered artifact source.
type Entry struct {
	Tier       SourceTier
	Kind       ArtifactKind
	SourcePath string
	// RealPath is the resolved real path for duplicate detection via symlinks.
	// If empty, SourcePath is used as the real path.
	RealPath string
	Raw      []byte
}

// DiscoveryConfig holds configuration for the discovery walker.
type DiscoveryConfig struct {
	// HomeDir is the user's home directory.
	HomeDir string
	// CWD is the current working directory (for project-local discovery).
	CWD string
	// SettingsPaths are explicit paths from settings.json.
	SettingsPaths map[ArtifactKind][]string
	// CLIPaths are explicit paths from CLI flags.
	CLIPaths map[ArtifactKind][]string
	// PackagePaths are directories to scan for installed packages.
	PackagePaths []string
	// DisableSkills skips skill discovery.
	DisableSkills bool
	// DisablePrompts skips prompt template discovery.
	DisablePrompts bool
	// DisableThemes skips theme discovery.
	DisableThemes bool
	// DisableExtensions skips extension discovery.
	DisableExtensions bool
	// Log is used for warnings. If nil, warnings are discarded.
	Log *slog.Logger
}

// Walk discovers artifacts across all configured source tiers. It yields
// entries in precedence order (most specific first).
func Walk(cfg DiscoveryConfig) ([]Entry, error) {
	var entries []Entry

	// Ensure home dir is set
	if cfg.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("artifacts: cannot determine home directory: %w", err)
		}
		cfg.HomeDir = home
	}

	// 1. CLI paths (highest precedence)
	for _, path := range cfg.CLIPaths[KindSkill] {
		e, err := discoverArtifactPath(path, KindSkill, TierCLI)
		if err != nil {
			warn(cfg.Log, "skill", path, err)
			continue
		}
		entries = append(entries, e...)
	}
	for _, path := range cfg.CLIPaths[KindPrompt] {
		e, err := discoverArtifactPath(path, KindPrompt, TierCLI)
		if err != nil {
			warn(cfg.Log, "prompt", path, err)
			continue
		}
		entries = append(entries, e...)
	}
	for _, path := range cfg.CLIPaths[KindTheme] {
		e, err := discoverArtifactPath(path, KindTheme, TierCLI)
		if err != nil {
			warn(cfg.Log, "theme", path, err)
			continue
		}
		entries = append(entries, e...)
	}
	for _, path := range cfg.CLIPaths[KindExtension] {
		e, err := discoverArtifactPath(path, KindExtension, TierCLI)
		if err != nil {
			warn(cfg.Log, "extension", path, err)
			continue
		}
		entries = append(entries, e...)
	}

	// 2. Project-local paths (TierProject)
	tauDir := filepath.Join(cfg.CWD, ".tau")
	if cfg.CWD != "" {
		if !cfg.DisableSkills {
			skillsDir := filepath.Join(tauDir, "skills")
			e, _ := discoverDir(skillsDir, KindSkill, TierProject, false, cfg.CWD)
			entries = append(entries, e...)
		}
		if !cfg.DisablePrompts {
			promptsDir := filepath.Join(tauDir, "prompts")
			e, _ := discoverDir(promptsDir, KindPrompt, TierProject, false, cfg.CWD)
			entries = append(entries, e...)
		}
		if !cfg.DisableThemes {
			themesDir := filepath.Join(tauDir, "themes")
			e, _ := discoverDir(themesDir, KindTheme, TierProject, false, cfg.CWD)
			entries = append(entries, e...)
		}
		if !cfg.DisableExtensions {
			extensionsDir := filepath.Join(tauDir, "extensions")
			e, _ := discoverDir(extensionsDir, KindExtension, TierProject, false, cfg.CWD)
			entries = append(entries, e...)
		}

		// Project cross-harness skills
		if !cfg.DisableSkills {
			e := discoverCrossHarness(cfg.CWD, TierProject)
			entries = append(entries, e...)
		}
	}

	// 3. User-global paths (TierUser)
	userAgentDir := filepath.Join(cfg.HomeDir, ".tau", "agent")
	if !cfg.DisableSkills {
		skillsDir := filepath.Join(userAgentDir, "skills")
		e, _ := discoverDir(skillsDir, KindSkill, TierUser, false, cfg.HomeDir)
		entries = append(entries, e...)
	}
	if !cfg.DisablePrompts {
		promptsDir := filepath.Join(userAgentDir, "prompts")
		e, _ := discoverDir(promptsDir, KindPrompt, TierUser, false, cfg.HomeDir)
		entries = append(entries, e...)
	}
	if !cfg.DisableThemes {
		themesDir := filepath.Join(userAgentDir, "themes")
		e, _ := discoverDir(themesDir, KindTheme, TierUser, false, cfg.HomeDir)
		entries = append(entries, e...)
	}
	if !cfg.DisableExtensions {
		extensionsDir := filepath.Join(userAgentDir, "extensions")
		e, _ := discoverDir(extensionsDir, KindExtension, TierUser, false, cfg.HomeDir)
		entries = append(entries, e...)
	}

	// 4. Cross-harness user skills
	if !cfg.DisableSkills {
		agentsSkillsDir := filepath.Join(cfg.HomeDir, ".agents", "skills")
		e, _ := discoverCrossHarnessDir(agentsSkillsDir, TierUser)
		entries = append(entries, e...)
	}

	// 5. Settings paths
	if !cfg.DisableSkills {
		for _, path := range cfg.SettingsPaths[KindSkill] {
			es, tier := discoverArtifactPathWithSettingsTier(path, cfg.CWD, cfg.HomeDir, KindSkill)
			if len(es) > 0 {
				for i := range es {
					es[i].Kind = KindSkill
				}
				entries = append(entries, es...)
			} else if tier != TierBuiltin {
				warn(cfg.Log, "skill", path, errors.New("not found"))
			}
		}
	}
	if !cfg.DisablePrompts {
		for _, path := range cfg.SettingsPaths[KindPrompt] {
			es, tier := discoverArtifactPathWithSettingsTier(path, cfg.CWD, cfg.HomeDir, KindPrompt)
			if len(es) > 0 {
				for i := range es {
					es[i].Kind = KindPrompt
				}
				entries = append(entries, es...)
			} else if tier != TierBuiltin {
				warn(cfg.Log, "prompt", path, errors.New("not found"))
			}
		}
	}
	if !cfg.DisableThemes {
		for _, path := range cfg.SettingsPaths[KindTheme] {
			es, tier := discoverArtifactPathWithSettingsTier(path, cfg.CWD, cfg.HomeDir, KindTheme)
			if len(es) > 0 {
				for i := range es {
					es[i].Kind = KindTheme
				}
				entries = append(entries, es...)
			} else if tier != TierBuiltin {
				warn(cfg.Log, "theme", path, errors.New("not found"))
			}
		}
	}
	if !cfg.DisableExtensions {
		for _, path := range cfg.SettingsPaths[KindExtension] {
			es, tier := discoverArtifactPathWithSettingsTier(path, cfg.CWD, cfg.HomeDir, KindExtension)
			if len(es) > 0 {
				for i := range es {
					es[i].Kind = KindExtension
				}
				entries = append(entries, es...)
			} else if tier != TierBuiltin {
				warn(cfg.Log, "extension", path, errors.New("not found"))
			}
		}
	}

	// 6. Packages (TierPackages)
	entries = append(entries, discoverPackageArtifacts(cfg)...)

	// 7. Built-in themes (TierBuiltin)
	if !cfg.DisableThemes {
		entries = append(entries, BuiltinThemes()...)
	}

	// Sort entries within each tier by source path for deterministic first-load-wins ordering
	entries = sortEntriesByTier(entries)

	// Suppress duplicates: if multiple entries resolve to the same real path,
	// keep only the first (highest precedence). This matches TS behavior where
	// realpathSync is used to detect symlink duplicates.
	entries = deduplicateByRealPath(entries)

	return entries, nil
}

// discoverPackageArtifacts discovers artifacts from installed packages.
func discoverPackageArtifacts(cfg DiscoveryConfig) []Entry {
	var entries []Entry

	for _, basePath := range cfg.PackagePaths {
		basePath = expandPath(basePath)
		pkgDirs, err := os.ReadDir(basePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			warn(cfg.Log, "packages", basePath, err)
			continue
		}

		for _, pkgEntry := range pkgDirs {
			// Skip non-directory entries unless it's a symlink (which may point to a directory).
			if !pkgEntry.IsDir() && pkgEntry.Type()&os.ModeSymlink == 0 {
				continue
			}

			pkgName := pkgEntry.Name()
			pkgRoot := filepath.Join(basePath, pkgName)

			// Resolve symlinks so we can read the actual package directory.
			pkgRoot, _ = filepath.EvalSymlinks(pkgRoot)
			manifestPath := filepath.Join(pkgRoot, "tau-package.toml")

			// Try to load manifest; if missing, fall back to convention-based discovery.
			manifest, err := packages.Load(manifestPath)
			if err != nil {
				// Convention fallback: missing manifest with skills/prompts/themes at root
				// auto-discovers as if declared. Plugins require a manifest (security).
				if !discoverConventionFallback(pkgRoot, cfg, &entries) {
					continue
				}
				// Artifacts were discovered via convention fallback; continue to next package.
				continue
			}

			skills := manifest.AllSkills()
			if !cfg.DisableSkills && len(skills) > 0 {
				for _, skill := range skills {
					skillPath := filepath.Join(pkgRoot, skill.Path)
					e, err := discoverArtifactPath(skillPath, KindSkill, TierPackages)
					if err != nil {
						warn(cfg.Log, "skill", skillPath, err)
						continue
					}
					entries = append(entries, e...)
				}
			} else if !cfg.DisableSkills {
				skillPath := filepath.Join(pkgRoot, "skills")
				e, _ := discoverDir(skillPath, KindSkill, TierPackages, false, pkgRoot)
				entries = append(entries, e...)
			}

			prompts := manifest.AllPrompts()
			if !cfg.DisablePrompts && len(prompts) > 0 {
				for _, prompt := range prompts {
					promptPath := filepath.Join(pkgRoot, prompt.Path)
					e, err := discoverArtifactPath(promptPath, KindPrompt, TierPackages)
					if err != nil {
						warn(cfg.Log, "prompt", promptPath, err)
						continue
					}
					entries = append(entries, e...)
				}
			} else if !cfg.DisablePrompts {
				promptPath := filepath.Join(pkgRoot, "prompts")
				e, _ := discoverDir(promptPath, KindPrompt, TierPackages, false, pkgRoot)
				entries = append(entries, e...)
			}

			themes := manifest.AllThemes()
			if !cfg.DisableThemes && len(themes) > 0 {
				for _, theme := range themes {
					themePath := filepath.Join(pkgRoot, theme.Path)
					e, err := discoverArtifactPath(themePath, KindTheme, TierPackages)
					if err != nil {
						warn(cfg.Log, "theme", themePath, err)
						continue
					}
					entries = append(entries, e...)
				}
			} else if !cfg.DisableThemes {
				themePath := filepath.Join(pkgRoot, "themes")
				e, _ := discoverDir(themePath, KindTheme, TierPackages, false, pkgRoot)
				entries = append(entries, e...)
			}
		}
	}

	return entries
}

// discoverArtifactPath discovers artifacts at a specific path.
func discoverArtifactPath(path string, kind ArtifactKind, tier SourceTier) ([]Entry, error) {
	path = expandPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		// For loose-skill, CLI, prompts, themes dirs: skillDirMode=false.
		// skillDirMode=true is only used for cross-harness .agents/skills/ dirs
		// via discoverCrossHarnessDir.
		return discoverDir(path, kind, tier, false, path)
	}
	return discoverFile(path, kind, tier)
}

// discoverArtifactPathWithSettingsTier discovers artifacts at settings paths.
func discoverArtifactPathWithSettingsTier(path, cwd, homeDir string, kind ArtifactKind) ([]Entry, SourceTier) {
	path = expandPath(path)
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, TierBuiltin
	}

	tier := TierSettings
	if cwd != "" && (strings.HasPrefix(path, cwd) || strings.HasPrefix(path+string(filepath.Separator), cwd+string(filepath.Separator))) {
		tier = TierProject
	} else if strings.HasPrefix(path, homeDir) {
		tier = TierUser
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, tier
	}

	if info.IsDir() {
		// For settings paths, skillDirMode=false so that:
		// - Prompts/themes: no recursion (handled by the else branch below).
		// - Skills: find both root-level .md files and subdirs with SKILL.md.
		// Determine rootDir based on tier for ignore file collection.
		var settingsRoot string
		if cwd != "" && (strings.HasPrefix(path, cwd) || strings.HasPrefix(path+string(filepath.Separator), cwd+string(filepath.Separator))) {
			settingsRoot = cwd
		} else if strings.HasPrefix(path, homeDir) {
			settingsRoot = homeDir
		} else {
			settingsRoot = path
		}
		e, _ := discoverDir(path, kind, tier, false, settingsRoot)
		return e, tier
	}

	e, err := discoverFile(path, kind, tier)
	if err != nil || len(e) == 0 {
		return nil, tier
	}
	return e, tier
}

// discoverDir discovers all artifacts in a directory.
// rootDir specifies the base directory for ignore file collection.
// If rootDir is empty, dir is used as root.
func discoverDir(dir string, kind ArtifactKind, tier SourceTier, skillDirMode bool, rootDir string) ([]Entry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	// Default rootDir to dir if not specified
	if rootDir == "" {
		rootDir = dir
	}

	entries := make([]Entry, 0)
	visited := make(map[string]bool)
	// Track real paths where loose skills have been discovered to avoid duplicates.
	// When a symlink points to a real path we've already processed, we skip
	// re-discovering loose .md files from the symlink.
	discoveredRealPaths := make(map[string]bool)

	// Create ignore matcher for this directory tree
	ignore := newIgnoreMatcher(dir, rootDir)

	// Resolve the root directory once for comparison with resolved symlink paths.
	rootRealPath, err := filepath.EvalSymlinks(dir)
	if err != nil {
		rootRealPath = dir
	}

	// Explicit symlink-aware walk instead of filepath.Walk.
	// filepath.Walk follows symlinks by default but has no cycle detection,
	// leading to potential infinite loops. Per the spec: "Symlinks: followed,
	// but cycle-detected."
	queue := []string{dir}

	for len(queue) > 0 {
		currentDir := queue[0]
		queue = queue[1:]

		// Resolve currentDir (which may be a symlink path) to its real path
		// for cycle detection. If it can't be resolved, use the path as-is.
		realPath, err := filepath.EvalSymlinks(currentDir)
		if err != nil {
			realPath = currentDir
		}

		// Cycle detection: skip if we've already processed this real path.
		if visited[realPath] {
			continue
		}
		visited[realPath] = true

		files, err := os.ReadDir(currentDir)
		if err != nil {
			// Skip directories we can't read.
			continue
		}

		// Whether this directory is itself a skill directory. Loose .md files
		// only load from the skills-dir root and from skill directories;
		// grouping dirs contribute SKILL.md dirs only.
		var hasSkillMD bool
		if kind == KindSkill && !skillDirMode {
			for _, f := range files {
				if f.Name() == "SKILL.md" && !f.IsDir() {
					hasSkillMD = true
					break
				}
			}
		}

		for _, entry := range files {
			path := filepath.Join(currentDir, entry.Name())

			// Handle symlinks: resolve and check for cycles.
			if entry.Type()&os.ModeSymlink != 0 {
				symTarget, err := filepath.EvalSymlinks(path)
				if err != nil {
					continue
				}
				if visited[symTarget] {
					// Already processed this real path (or a cycle): skip.
					continue
				}

				// NOTE: do NOT pre-mark symTarget as visited here. The
				// queue loop marks EvalSymlinks(currentDir) when the queued
				// symlink path is actually processed; pre-marking here made
				// that same check skip the first legitimate traversal, so
				// symlinked skill dirs (e.g. ~/.tau/agent/skills/<name> ->
				// <extension package>/skills/<name>) were never discovered.

				// Get info about the symlink target.
				symInfo, err := os.Stat(path)
				if err != nil {
					continue
				}

				if symInfo.IsDir() {
					// Symlink to directory: add to queue for processing.
					queue = append(queue, path)

					// Per the spec, loose-skill mode discovers root-level .md files from
					// symlink directories. Only discover loose skills from symlinks if
					// this is the root directory AND we haven't already discovered
					// from this real path.
					if kind == KindSkill && !skillDirMode && realPath == rootRealPath {
						if !discoveredRealPaths[symTarget] {
							// First time discovering from this real path via this symlink.
							// Discover the loose .md files.
							discoveredRealPaths[symTarget] = true
							symFiles, _ := os.ReadDir(path)
							for _, symFile := range symFiles {
								if symFile.Type().IsRegular() {
									symFilePath := filepath.Join(path, symFile.Name())
									symExt := strings.ToLower(filepath.Ext(symFilePath))
									if symExt == ".md" && filepath.Base(symFilePath) != "SKILL.md" {
										e, _ := discoverFile(symFilePath, kind, tier)
										entries = append(entries, e...)
									}
								}
							}
						}
					}
				}
				continue
			}

			if entry.IsDir() {
				// Check if directory should be ignored
				if isIgnored(ignore, path, true) {
					continue
				}

				// For skill kind with skillDirMode=false (loose-skill dirs): recurse
				// into every subdir so grouped layouts
				// (skills/<category>/<name>/SKILL.md) are discovered; loose .md
				// files are gated per-directory in the file loop below.
				// For skill kind with skillDirMode=true (agentskills.io dirs): only
				// SKILL.md subdirs, skip everything else.
				// For prompts/themes: no recursion into subdirs.
				if kind == KindSkill && skillDirMode {
					// agentskills.io mode: only subdirs with SKILL.md.
					skillMD := filepath.Join(path, "SKILL.md")
					if _, err := os.Stat(skillMD); err == nil {
						e, _ := discoverFile(skillMD, KindSkill, tier)
						entries = append(entries, e...)
					}
				} else if kind == KindSkill {
					// Loose-skill dirs: recurse into every subdir to find grouped
					// directory skills (skills/<category>/<name>/SKILL.md). VCS and
					// vendored dirs are never walked; loose .md gating happens in
					// the file loop below.
					if isSkippedDirName(entry.Name()) {
						continue
					}
					queue = append(queue, path)
				}
				continue
			}

			if entry.Type().IsRegular() {
				// Check if file should be ignored
				if isIgnored(ignore, path, false) {
					continue
				}

				ext := strings.ToLower(filepath.Ext(path))
				switch kind {
				case KindSkill:
					// Directory skills: SKILL.md anywhere in the walk. Loose
					// skills: .md files only in the skills-dir root or in
					// directories that are themselves skill dirs.
					if ext == ".md" {
						baseName := filepath.Base(path)
						if baseName == "SKILL.md" && !skillDirMode {
							e, _ := discoverFile(path, kind, tier)
							entries = append(entries, e...)
						} else if baseName != "SKILL.md" && (currentDir == dir || hasSkillMD) {
							e, _ := discoverFile(path, kind, tier)
							entries = append(entries, e...)
						}
					}
				case KindPrompt:
					if ext == ".md" {
						e, _ := discoverFile(path, kind, tier)
						entries = append(entries, e...)
					}
				case KindTheme:
					if ext == ".json" {
						e, _ := discoverFile(path, kind, tier)
						entries = append(entries, e...)
					}
				}
			}
		}
	}

	return entries, nil
}

// discoverFile discovers a single artifact file.
func discoverFile(path string, kind ArtifactKind, tier SourceTier) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Resolve real path for duplicate detection via symlinks.
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath = path
	}
	return []Entry{{
		Tier:       tier,
		Kind:       kind,
		SourcePath: path,
		RealPath:   realPath,
		Raw:        data,
	}}, nil
}

// discoverCrossHarness walks from cwd up to repo root looking for .agents/skills/.
// Per the spec: only directories within the git repository (at or below the repo root)
// are scanned; directories above the repo root are never visited.
func discoverCrossHarness(cwd string, tier SourceTier) []Entry {
	var entries []Entry

	repoRoot := findRepoRoot(cwd)

	if repoRoot == "" {
		// No git repo: check cwd itself only.
		agentsDir := filepath.Join(cwd, ".agents", "skills")
		e, _ := discoverCrossHarnessDir(agentsDir, tier)
		entries = append(entries, e...)
		return entries
	}

	// Walk from cwd up to (but not including) repo root.
	// The repo root itself is handled after the loop.
	for dir := cwd; dir != "" && dir != "." && dir != repoRoot; dir = filepath.Dir(dir) {
		agentsDir := filepath.Join(dir, ".agents", "skills")
		e, _ := discoverCrossHarnessDir(agentsDir, tier)
		entries = append(entries, e...)
	}

	// Also check the repo root itself.
	agentsDir := filepath.Join(repoRoot, ".agents", "skills")
	e, _ := discoverCrossHarnessDir(agentsDir, tier)
	entries = append(entries, e...)

	return entries
}

// discoverCrossHarnessDir discovers skills in a cross-harness .agents/skills/ dir.
func discoverCrossHarnessDir(dir string, tier SourceTier) ([]Entry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var entries []Entry
	entries4, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries4 {
		if entry.IsDir() {
			skillMD := filepath.Join(dir, entry.Name(), "SKILL.md")
			e, err := discoverFile(skillMD, KindSkill, tier)
			if err == nil {
				entries = append(entries, e...)
			}
		}
	}

	return entries, nil
}

// findRepoRoot finds the git repository root or returns empty string.
func findRepoRoot(cwd string) string {
	dir := cwd
	for {
		gitDir := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// discoverConventionFallback discovers artifacts from a package root using
// convention-based layout (skills/, prompts/, themes/ directories at root).
// Discovered entries are appended to entries. Returns true if any artifacts were discovered.
func discoverConventionFallback(pkgRoot string, cfg DiscoveryConfig, entries *[]Entry) bool {
	var found bool

	// Discover skills from skills/ directory.
	if !cfg.DisableSkills {
		skillsPath := filepath.Join(pkgRoot, "skills")
		if e, err := discoverDir(skillsPath, KindSkill, TierPackages, false, pkgRoot); err == nil && len(e) > 0 {
			*entries = append(*entries, e...)
			found = true
		}
	}

	// Discover prompts from prompts/ directory.
	if !cfg.DisablePrompts {
		promptsPath := filepath.Join(pkgRoot, "prompts")
		if e, err := discoverDir(promptsPath, KindPrompt, TierPackages, false, pkgRoot); err == nil && len(e) > 0 {
			*entries = append(*entries, e...)
			found = true
		}
	}

	// Discover themes from themes/ directory.
	if !cfg.DisableThemes {
		themesPath := filepath.Join(pkgRoot, "themes")
		if e, err := discoverDir(themesPath, KindTheme, TierPackages, false, pkgRoot); err == nil && len(e) > 0 {
			*entries = append(*entries, e...)
			found = true
		}
	}

	return found
}

// isSkippedDirName reports directories that never contain artifacts and
// may be expensive to walk (VCS internals, vendored dependency trees).
func isSkippedDirName(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".venv", "node_modules", "vendor", "__pycache__":
		return true
	default:
		return false
	}
}

// expandPath expands ~ and environment variables in a path.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	path = os.ExpandEnv(path)
	return path
}

// warn logs a warning.
func warn(logger *slog.Logger, kind, path string, err error) {
	if logger != nil {
		logger.Warn("artifacts: discovery warning", "kind", kind, "path", path, "error", err)
	}
}

// sortEntriesByTier orders entries by tier precedence (higher precedence
// first), with source-path alphabetical order as the tiebreaker. Within a
// tier, deterministic ordering is preserved by sorting entries by source path.
//
// TierCLI wins on collision; TierBuiltin loses. Registries that consume the
// result of this function (and register with first-wins semantics) therefore
// see higher-tier entries first and skip lower-tier duplicates.
func sortEntriesByTier(entries []Entry) []Entry {
	if len(entries) <= 1 {
		return entries
	}

	sorted := make([]Entry, len(entries))
	copy(sorted, entries)

	slices.SortStableFunc(sorted, func(a, b Entry) int {
		if c := cmp.Compare(b.Tier.Precedence(), a.Tier.Precedence()); c != 0 {
			return c
		}
		return cmp.Compare(a.SourcePath, b.SourcePath)
	})

	return sorted
}

// sortEntries sorts entries alphabetically by source path.
func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Compare(a.SourcePath, b.SourcePath)
	})
}

// deduplicateByRealPath removes entries that resolve to the same real path,
// keeping only the first (highest precedence) entry for each unique real path.
// This matches TS behavior where realpathSync is used to detect symlink duplicates.
func deduplicateByRealPath(entries []Entry) []Entry {
	if len(entries) == 0 {
		return entries
	}

	seen := make(map[string]bool)
	var result []Entry

	for _, e := range entries {
		// Use RealPath if available, otherwise fall back to SourcePath
		key := e.RealPath
		if key == "" {
			key = e.SourcePath
		}

		if !seen[key] {
			seen[key] = true
			result = append(result, e)
		}
		// Skip duplicate entries
	}

	return result
}

// parseFrontmatter extracts YAML frontmatter from markdown content.
func parseFrontmatter(data []byte) (yamlData, body []byte, err error) {
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil, data, nil
	}

	rest := data[4:]
	idx := bytes.Index(rest, []byte("\n---\n"))
	if idx == -1 {
		idx = bytes.Index(rest, []byte("\r\n---\r\n"))
		if idx == -1 {
			return nil, data, nil
		}
		yamlData = rest[:idx]
		body = rest[idx+7:]
	} else {
		yamlData = rest[:idx]
		body = rest[idx+5:]
	}

	body = bytes.TrimPrefix(body, []byte("\n"))
	body = bytes.TrimPrefix(body, []byte("\r\n"))

	return yamlData, body, nil
}

// slugify converts a string to a URL-safe slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return -1
	}, s)
	return strings.Trim(s, "-")
}

// parseSimpleYAML is a lenient fallback parser.
func parseSimpleYAML(data []byte) (map[string]any, error) {
	result := make(map[string]any)
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found || key == "" {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if val == ">" || val == "|" {
			var parts []string
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], " ") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			result[key] = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			continue
		}
		result[key] = val
	}
	return result, nil
}
