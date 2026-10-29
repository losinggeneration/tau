package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/losinggeneration/tau/agent/codemode"
	sessionpkg "github.com/losinggeneration/tau/agent/session"
)

// loadCodemodeStore and saveCodemodeStore keep store() values as
// codemode-store entries on the session branch, or in memory while the
// session has no file.
func (s *agentSession) loadCodemodeStore(context.Context) (map[string]json.RawMessage, error) {
	entries, persisted, err := s.branchCustomEntries(codemode.StoreEntryType)
	if err != nil {
		return nil, fmt.Errorf("read codemode store: %w", err)
	}
	values := map[string]json.RawMessage{}
	if !persisted {
		s.mu.Lock()
		maps.Copy(values, s.codemodeValues)
		s.mu.Unlock()
		return values, nil
	}
	for _, entry := range entries {
		if err := codemode.ApplyStoreWrites(values, entry.Data); err != nil {
			return nil, fmt.Errorf("entry %s: %w", entry.ID, err)
		}
	}
	return values, nil
}

func (s *agentSession) saveCodemodeStore(_ context.Context, writes codemode.StoreWrites) error {
	data, err := json.Marshal(writes)
	if err != nil {
		return fmt.Errorf("encode codemode store: %w", err)
	}
	s.mu.Lock()
	if s.codemodeValues == nil {
		s.codemodeValues = map[string]json.RawMessage{}
	}
	err = codemode.ApplyStoreWrites(s.codemodeValues, data)
	writer, parent := s.sessionWriter, s.parentMsgID
	s.mu.Unlock()
	if err != nil || writer == nil || parent == nil {
		return err
	}
	entry := sessionpkg.NewCustomEntry(codemode.StoreEntryType, data)
	entry.ParentID = parent
	if err := writer.Append(entry); err != nil {
		return fmt.Errorf("append codemode store: %w", err)
	}
	return nil
}
