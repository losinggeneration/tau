package ai // import "github.com/losinggeneration/tau/ai"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// KeybindingsFile is the in-memory shape of ~/.tau/agent/keybindings.json.
// The on-disk format is { "<canonical-id>": "<key>" | ["<key1>", "<key2>"] }.
// Entries that don't decode cleanly (non-object top-level, non-string or
// non-string-array bindings) are silently ignored — the spec is explicit
// that invalid entries must not break load.
type KeybindingsFile struct {
	// Bindings is keyed by canonical keybinding id (e.g. "app.model.select").
	Bindings map[string]Keybinding `json:"-"`
	// raw is the original on-disk shape so SaveSettings round-trips verbatim.
	raw map[string]json.RawMessage
}

// Keybinding is one or more key sequences bound to an action.
type Keybinding []string

// UnmarshalJSON accepts either a single string ("ctrl+l") or a list
// (["ctrl+x","t"]).
func (k *Keybinding) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*k = Keybinding{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*k = Keybinding(list)
	return nil
}

// MarshalJSON writes the binding back as a string when there's one entry,
// otherwise as an array.
func (k Keybinding) MarshalJSON() ([]byte, error) {
	if len(k) == 1 {
		return json.Marshal(k[0])
	}
	return json.Marshal([]string(k))
}

// LoadKeybindings reads keybindings.json from path. A missing file returns an
// empty KeybindingsFile (not an error). Invalid JSON, non-object top-level
// values, and per-entry non-string/non-array bindings are skipped per the
// spec's "ignore" policy.
func LoadKeybindings(path string) (*KeybindingsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &KeybindingsFile{Bindings: map[string]Keybinding{}, raw: map[string]json.RawMessage{}}, nil
		}
		return nil, fmt.Errorf("read keybindings: %w", err)
	}
	if len(data) == 0 {
		return &KeybindingsFile{Bindings: map[string]Keybinding{}, raw: map[string]json.RawMessage{}}, nil
	}

	// Reject non-object top-level values.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse keybindings: %w", err)
	}

	parsed := &KeybindingsFile{
		Bindings: make(map[string]Keybinding, len(probe)),
		raw:      make(map[string]json.RawMessage, len(probe)),
	}
	for k, v := range probe {
		parsed.raw[k] = v
		var kb Keybinding
		if err := json.Unmarshal(v, &kb); err != nil {
			// Skip invalid entries per spec; preserve raw bytes so the save
			// round-trip is unchanged.
			continue
		}
		parsed.Bindings[k] = kb
	}
	return parsed, nil
}

// SaveKeybindings writes the keybindings file at path with mode 0644.
func SaveKeybindings(path string, kb *KeybindingsFile) error {
	if kb == nil {
		kb = &KeybindingsFile{}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create keybindings dir: %w", err)
	}
	out := map[string]json.RawMessage{}
	for k, v := range kb.raw {
		out[k] = v
	}
	for k, v := range kb.Bindings {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal binding %s: %w", k, err)
		}
		out[k] = b
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keybindings: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write keybindings: %w", err)
	}
	return nil
}

// Lookup returns the key binding for an action id, or empty if none is set.
func (k *KeybindingsFile) Lookup(id string) Keybinding {
	if k == nil || k.Bindings == nil {
		return nil
	}
	return k.Bindings[id]
}

// Set replaces the binding for an action id. Empty value removes the entry.
func (k *KeybindingsFile) Set(id string, binding Keybinding) {
	if k.Bindings == nil {
		k.Bindings = make(map[string]Keybinding)
	}
	if k.raw == nil {
		k.raw = make(map[string]json.RawMessage)
	}
	if len(binding) == 0 {
		delete(k.Bindings, id)
		delete(k.raw, id)
		return
	}
	k.Bindings[id] = binding
	b, _ := json.Marshal(binding)
	k.raw[id] = b
}
