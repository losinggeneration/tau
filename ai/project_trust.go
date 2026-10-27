package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ProjectTrustStore persists canonical directory decisions in the global agent directory.
// A nil decision removes a local override while retaining ancestor inheritance.
type ProjectTrustStore struct{ Path string }

func canonicalTrustPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve trust path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize trust path: %w", err)
	}
	return resolved, nil
}

func (s ProjectTrustStore) read() (map[string]*bool, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]*bool), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read project trust: %w", err)
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("invalid project trust: expected object")
	}
	var entries map[string]*bool
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, errors.New("invalid project trust: expected boolean or null decisions")
	}
	return entries, nil
}

// Get returns the nearest saved directory or ancestor decision, or nil if unknown.
func (s ProjectTrustStore) Get(ctx context.Context, cwd string) (*bool, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	canonical, err := canonicalTrustPath(cwd)
	if err != nil {
		return nil, err
	}
	entries, err := s.read()
	if err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	for {
		if decision := entries[canonical]; decision != nil {
			return new(*decision), nil
		}
		parent := filepath.Dir(canonical)
		if parent == canonical {
			return nil, nil
		}
		canonical = parent
	}
}

// Set saves a decision without overwriting unrelated concurrent decisions.
func (s ProjectTrustStore) Set(ctx context.Context, cwd string, decision *bool) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return s.SetMany(ctx, map[string]*bool{cwd: decision})
}

// SetMany atomically saves related overrides, including clearing a child when trusting its parent.
func (s ProjectTrustStore) SetMany(ctx context.Context, updates map[string]*bool) error {
	owned := make(map[string]*bool, len(updates))
	for path, decision := range updates {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		canonical, err := canonicalTrustPath(path)
		if err != nil {
			return err
		}
		if decision != nil {
			owned[canonical] = new(*decision)
		} else {
			owned[canonical] = nil
		}
	}
	return withSettingsLock(ctx, s.Path, func(ctx context.Context) error {
		entries, err := s.read()
		if err != nil {
			return err
		}
		for path, decision := range owned {
			entries[path] = decision
		}
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return fmt.Errorf("encode project trust: %w", err)
		}
		return replaceSettings(ctx, s.Path, append(data, '\n'))
	})
}

// HasTrustRequiringProjectResources checks protected configuration and ancestor skills.
// Global home skills remain user resources even when the working directory is home.
func HasTrustRequiringProjectResources(ctx context.Context, cwd, home string) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	canonical, err := canonicalTrustPath(cwd)
	if err != nil {
		return false, err
	}
	home, err = canonicalTrustPath(home)
	if err != nil {
		return false, err
	}
	exists := func(path string) (bool, error) {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect protected project resource: %w", err)
		}
		return true, nil
	}
	for _, entry := range []string{"settings.json", "mcp.json", "extensions", "skills", "prompts", "themes", "packages", "SYSTEM.md", "APPEND_SYSTEM.md"} {
		found, err := exists(filepath.Join(canonical, ".tau", entry))
		if err != nil || found {
			return found, err
		}
	}
	for {
		if canonical != home {
			found, err := exists(filepath.Join(canonical, ".agents", "skills"))
			if err != nil || found {
				return found, err
			}
		}
		parent := filepath.Dir(canonical)
		if parent == canonical {
			return false, nil
		}
		canonical = parent
	}
}

// ProjectTrustOptions supplies session overrides and caller-owned decisions.
// Decide may only consult trusted extensions. Prompt runs after saved/default policy.
type ProjectTrustOptions struct {
	Override *bool
	Decide   func(context.Context, string) (*bool, error)
	Prompt   func(context.Context, string) (*bool, error)
}

// ResolveProjectTrust applies explicit, extension/UI, saved, and global default policy.
func ResolveProjectTrust(ctx context.Context, store ProjectTrustStore, cwd, home, defaultPolicy string, opts ProjectTrustOptions) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	if opts.Override != nil {
		return *opts.Override, nil
	}
	protected, err := HasTrustRequiringProjectResources(ctx, cwd, home)
	if err != nil || !protected {
		return !protected, err
	}
	if opts.Decide != nil {
		decision, err := opts.Decide(ctx, cwd)
		if err != nil {
			return false, err
		}
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		if decision != nil {
			return *decision, nil
		}
	}
	decision, err := store.Get(ctx, cwd)
	if err != nil {
		return false, err
	}
	if decision != nil {
		return *decision, nil
	}
	switch defaultPolicy {
	case "always":
		return true, nil
	case "never":
		return false, nil
	}
	if opts.Prompt != nil {
		decision, err := opts.Prompt(ctx, cwd)
		if err != nil {
			return false, err
		}
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		if decision != nil {
			return *decision, nil
		}
	}
	return false, nil
}

// ProjectResourcesTrusted reports the admitted directory policy. Programmatic
// settings without a trust decision retain their caller-controlled behavior.
func (s *Settings) ProjectResourcesTrusted() bool {
	return s == nil || s.projectTrusted == nil || *s.projectTrusted
}

// LoadTrustedSettings resolves trust using only global settings before opening
// project configuration. The returned policy must also gate resource discovery.
func LoadTrustedSettings(ctx context.Context, globalDir, cwd, home string, opts ProjectTrustOptions) (*Settings, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	user, err := LoadSettings(filepath.Join(globalDir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("load user settings: %w", err)
	}
	trusted, err := ResolveProjectTrust(ctx, ProjectTrustStore{Path: filepath.Join(globalDir, "trust.json")}, cwd, home, user.DefaultProjectTrust, opts)
	if err != nil {
		return nil, fmt.Errorf("resolve project trust: %w", err)
	}
	var project *Settings
	if trusted {
		project, err = LoadSettings(filepath.Join(cwd, ".tau", "settings.json"))
		if err != nil {
			return nil, fmt.Errorf("load project settings: %w", err)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	result := MergeSettings(user, project)
	result.projectTrusted = new(trusted)
	result.projectTrustPath, err = canonicalTrustPath(cwd)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ProjectResourcesTrustedAt prevents a session directory change from carrying
// another directory's trust decision into protected prompt discovery.
func (s *Settings) ProjectResourcesTrustedAt(cwd string) bool {
	if !s.ProjectResourcesTrusted() {
		return false
	}
	if s == nil || s.projectTrustPath == "" {
		return true
	}
	canonical, err := canonicalTrustPath(cwd)
	return err == nil && canonical == s.projectTrustPath
}

// PromptProjectTrust presents persistent and session-only directory choices.
// Dismissal denies without saving. The selector is supplied by the active UI.
func PromptProjectTrust(ctx context.Context, store ProjectTrustStore, cwd string, selectOption func(context.Context, string, []string) (int, error)) (*bool, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	canonical, err := canonicalTrustPath(cwd)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(canonical)
	choices := []string{"Trust", "Trust (this session only)", "Do not trust", "Do not trust (this session only)"}
	if parent != canonical {
		choices = append(choices, "Trust parent folder ("+parent+")")
	}
	selected, err := selectOption(ctx, "Trust project folder?\n"+canonical+"\nAllow project settings, resources, packages and extensions?", choices)
	if err != nil {
		return nil, fmt.Errorf("select project trust: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	switch selected {
	case 0:
		if err := store.Set(ctx, canonical, new(true)); err != nil {
			return nil, err
		}
		return new(true), nil
	case 1:
		return new(true), nil
	case 2:
		if err := store.Set(ctx, canonical, new(false)); err != nil {
			return nil, err
		}
		return new(false), nil
	case 4:
		if parent != canonical {
			if err := store.SetMany(ctx, map[string]*bool{parent: new(true), canonical: nil}); err != nil {
				return nil, err
			}
			return new(true), nil
		}
	}
	return new(false), nil
}
