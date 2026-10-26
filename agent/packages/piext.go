package packages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const notPiPackageError = "not a pi package — install without --pi"

// PiPackage is the install metadata tau needs from package.json's pi field.
type PiPackage struct {
	Name        string
	Version     string
	Description string
	Extensions  []string
	Skills      []string
	Prompts     []string
	Themes      []string
	PeerDeps    []string
	HasDeps     bool
}

// ParsePiPackage reads package.json and returns pi install metadata.
func ParsePiPackage(dir string) (*PiPackage, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}

	var raw struct {
		Name             string            `json:"name"`
		Version          string            `json:"version"`
		Description      string            `json:"description"`
		Dependencies     map[string]string `json:"dependencies"`
		PeerDependencies map[string]string `json:"peerDependencies"`
		Pi               *PiManifest       `json:"pi"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &raw); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	if raw.Pi == nil {
		return nil, errors.New(notPiPackageError)
	}
	if raw.Name == "" {
		return nil, fmt.Errorf("package.json name is required")
	}
	if err := validatePackageName(raw.Name); err != nil {
		return nil, err
	}
	if raw.Version == "" {
		return nil, fmt.Errorf("package.json version is required")
	}

	extensions := raw.Pi.Extensions
	if slices.ContainsFunc(extensions, func(entry string) bool {
		return strings.ContainsAny(entry, "*?") || strings.HasPrefix(entry, "!") || strings.HasPrefix(entry, "+") || strings.HasPrefix(entry, "-")
	}) {
		resolved, err := PiResourcePaths(dir, extensions, ResourceKindExtension)
		if err != nil {
			return nil, err
		}
		extensions = make([]string, 0, len(resolved))
		for _, path := range resolved {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil, fmt.Errorf("relative Pi extension: %w", err)
			}
			extensions = append(extensions, filepath.ToSlash(rel))
		}
	}
	peerDeps := packageSpecs(raw.PeerDependencies)

	return &PiPackage{
		Name:        raw.Name,
		Version:     raw.Version,
		Description: raw.Description,
		Extensions:  resolvePiExtensionEntries(dir, extensions),
		Skills:      slices.Clone(raw.Pi.Skills),
		Prompts:     slices.Clone(raw.Pi.Prompts),
		Themes:      slices.Clone(raw.Pi.Themes),
		PeerDeps:    peerDeps,
		HasDeps:     len(raw.Dependencies) > 0 || len(peerDeps) > 0,
	}, nil
}

// GenerateManifest renders the tau manifest for a pi package through pi-ext-shim.
func GenerateManifest(pkg *PiPackage, shimDir, nodeBin string) ([]byte, error) {
	if pkg == nil {
		return nil, fmt.Errorf("missing pi package")
	}
	if len(pkg.Extensions) > 0 && !filepath.IsAbs(nodeBin) {
		return nil, fmt.Errorf("node path must be absolute")
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "[package]\n")
	writeTomlString(&b, "name", pkg.Name)
	writeTomlString(&b, "version", pkg.Version)
	desc := strings.TrimSpace(pkg.Description)
	if desc != "" {
		desc += " "
	}
	if len(pkg.Extensions) > 0 {
		desc += "(pi extension via pi-ext-shim)"
	}
	writeTomlString(&b, "description", desc)
	if len(pkg.Extensions) > 0 {
		fmt.Fprintf(&b, "\n[plugin]\napi_version = \"1\"\n\n[run]\n")
		args := []string{nodeBin, filepath.Join(shimDir, "index.js")}
		args = append(args, cleanPiEntries(pkg.Extensions)...)
		for i, arg := range args {
			args[i] = quotePiCommandArg(arg)
		}
		writeTomlString(&b, "command", strings.Join(args, " "))
	}
	for _, family := range []struct {
		name    string
		entries []string
	}{{"skills", pkg.Skills}, {"prompts", pkg.Prompts}, {"themes", pkg.Themes}} {
		for _, entry := range cleanPiEntries(family.entries) {
			fmt.Fprintf(&b, "\n[[%s]]\n", family.name)
			writeTomlString(&b, "path", entry)
		}
	}
	return b.Bytes(), nil
}

func quotePiCommandArg(arg string) string {
	if arg != "" && strings.Trim(arg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

func writeTomlString(b *bytes.Buffer, key, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal TOML string: %v", err))
	}
	fmt.Fprintf(b, "%s = %s\n", key, encoded)
}

func cleanPiEntries(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = cleanPiEntry(entry)
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

func cleanPiEntry(entry string) string {
	entry = strings.TrimSpace(filepath.ToSlash(entry))
	if entry == "./" || entry == "." {
		return "."
	}
	return strings.TrimPrefix(entry, "./")
}

func resolvePiExtensionEntries(dir string, entries []string) []string {
	if entries == nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		clean := cleanPiEntry(entry)
		if clean == "" {
			continue
		}
		if clean == "." {
			if rootEntry := firstExistingPath(dir, "index.ts", "index.js", "index.mjs", "index.cjs"); rootEntry != "" {
				out = append(out, rootEntry)
				continue
			}
		}
		if info, err := os.Stat(filepath.Join(dir, clean)); err == nil && info.IsDir() {
			if rootEntry := firstExistingPath(filepath.Join(dir, clean), "index.ts", "index.js", "index.mjs", "index.cjs"); rootEntry != "" {
				out = append(out, filepath.ToSlash(filepath.Join(clean, rootEntry)))
				continue
			}
		}
		out = append(out, clean)
	}
	return out
}

func firstExistingPath(dir string, names ...string) string {
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !info.IsDir() {
			return name
		}
	}
	return ""
}

func packageSpecs(deps map[string]string) []string {
	if len(deps) == 0 {
		return nil
	}
	specs := make([]string, 0, len(deps))
	for name, version := range deps {
		version = strings.TrimSpace(version)
		if version == "" || version == "*" {
			specs = append(specs, name)
			continue
		}
		specs = append(specs, name+"@"+version)
	}
	sort.Strings(specs)
	return specs
}
