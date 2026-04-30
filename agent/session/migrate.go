// Package session implements the append-only JSONL session format with a
// tree-of-entries model, flock-based concurrency, and index management.
package session

import (
	"encoding/json"
	"fmt"
)

// CurrentSessionVersion is the current session file format version.
// See migrate.go for migration functions.
const CurrentSessionVersion = 3

// Migrate applies all necessary migrations to bring entries to current version.
// It mutates entries in place. Returns true if any migration was applied.
func Migrate(entries []json.RawMessage) (bool, error) {
	if len(entries) == 0 {
		return false, nil
	}

	// Parse header to determine version
	var header Header
	if err := json.Unmarshal(entries[0], &header); err != nil {
		return false, fmt.Errorf("parse header: %w", err)
	}

	if header.Type != EntryTypeSession {
		return false, fmt.Errorf("first entry is not a session header")
	}

	version := header.Version
	if version == 0 {
		version = 1 // v1 sessions don't have version field
	}

	if version >= CurrentSessionVersion {
		return false, nil
	}

	migrated := false

	// Apply migrations in order
	if version < 2 {
		if err := migrateV1ToV2(entries); err != nil {
			return false, fmt.Errorf("v1->v2 migration: %w", err)
		}
		migrated = true
		version = 2
	}

	if version < 3 {
		if err := migrateV2ToV3(entries); err != nil {
			return false, fmt.Errorf("v2->v3 migration: %w", err)
		}
		migrated = true
	}

	return migrated, nil
}

// migrateV1ToV2 migrates from v1 to v2:
// - Add id/parentId tree structure
// - Generate IDs for entries that don't have them
// - Derive parentId from ordering (prevId chain)
// - Convert firstKeptEntryIndex to firstKeptEntryId for compaction
func migrateV1ToV2(entries []json.RawMessage) error {
	if len(entries) == 0 {
		return nil
	}

	// Parse all entries
	type entryInfo struct {
		idx   int
		raw   json.RawMessage
		entry *Entry
	}

	parsed := make([]entryInfo, 0, len(entries))
	idSet := make(map[string]bool)

	// Update header version
	var header Header
	if err := json.Unmarshal(entries[0], &header); err != nil {
		return fmt.Errorf("parse header: %w", err)
	}
	header.Version = 2
	updatedHeader, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal updated header: %w", err)
	}
	parsed = append(parsed, entryInfo{idx: 0, raw: updatedHeader})

	// Parse remaining entries
	for i := 1; i < len(entries); i++ {
		var e Entry
		if err := json.Unmarshal(entries[i], &e); err != nil {
			// Keep raw for migration even if parsing fails
			parsed = append(parsed, entryInfo{idx: i, raw: entries[i]})
			continue
		}
		parsed = append(parsed, entryInfo{idx: i, raw: entries[i], entry: &e})
	}

	// Track previous ID for parent chain
	var prevID string

	for i := range parsed {
		if i == 0 {
			continue // header already updated
		}

		info := &parsed[i]

		// Skip entries that couldn't be parsed
		if info.entry == nil {
			continue
		}

		// Generate ID if missing
		if info.entry.ID == "" {
			info.entry.ID = generateUniqueID(idSet)
		}
		idSet[info.entry.ID] = true

		// Set parentId from chain
		info.entry.ParentID = nil
		if prevID != "" {
			info.entry.ParentID = &prevID
		}

		// Convert firstKeptEntryIndex to firstKeptEntryId for compaction
		if info.entry.Type == EntryTypeCompaction && info.entry.FirstKeptID == nil {
			// Try to find firstKeptEntryIndex in raw JSON
			var rawMap map[string]json.RawMessage
			if err := json.Unmarshal(info.raw, &rawMap); err == nil {
				if idxRaw, ok := rawMap["firstKeptEntryIndex"]; ok {
					var idx int
					if err := json.Unmarshal(idxRaw, &idx); err == nil {
						// firstKeptEntryIndex is the direct index into entries array
						targetIdx := idx
						if targetIdx >= 0 && targetIdx < len(entries) {
							var target Entry
							if err := json.Unmarshal(entries[targetIdx], &target); err == nil {
								if target.ID != "" {
									info.entry.FirstKeptID = &target.ID
								} else {
									// Target entry doesn't have ID yet, assign one now
									newID := generateUniqueID(idSet)
									idSet[newID] = true
									info.entry.FirstKeptID = &newID

									// Also update the target entry with this ID
									target.ID = newID
									target.ParentID = nil
									updatedTarget, err := json.Marshal(target)
									if err == nil {
										entries[targetIdx] = updatedTarget
									}
								}
							}
						}
					}
				}
			}
		}

		// Update the raw JSON
		updated, err := json.Marshal(info.entry)
		if err != nil {
			return fmt.Errorf("marshal entry %d: %w", info.idx, err)
		}
		info.raw = updated

		// Update in entries slice
		entries[info.idx] = info.raw

		// Move to next in chain
		prevID = info.entry.ID
	}

	return nil
}

// migrateV2ToV3 migrates from v2 to v3:
// - Rename hookMessage role to custom
func migrateV2ToV3(entries []json.RawMessage) error {
	for i, raw := range entries {
		// Try to parse as entry
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}

		if e.Type == EntryTypeMessage {
			// Check message content for hookMessage role
			if e.Message != nil {
				var msgMap map[string]json.RawMessage
				if err := json.Unmarshal(e.Message, &msgMap); err == nil {
					if roleRaw, ok := msgMap["role"]; ok {
						var role string
						if err := json.Unmarshal(roleRaw, &role); err == nil {
							if role == "hookMessage" {
								// Update role to "custom"
								msgMap["role"] = json.RawMessage(`"custom"`)
								newMsg, err := json.Marshal(msgMap)
								if err != nil {
									continue
								}
								e.Message = newMsg
								updated, err := json.Marshal(e)
								if err != nil {
									continue
								}
								entries[i] = updated
							}
						}
					}
				}
			}
		} else if e.Type == EntryTypeSession {
			// Update header version
			var h Header
			if err := json.Unmarshal(raw, &h); err == nil {
				h.Version = 3
				updated, err := json.Marshal(h)
				if err == nil {
					entries[i] = updated
				}
			}
		}
	}

	return nil
}

// generateUniqueID generates a unique entry ID.
func generateUniqueID(seen map[string]bool) string {
	id := NewID()
	for seen[id] {
		id = NewID()
	}
	return id
}
