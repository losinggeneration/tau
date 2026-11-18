package session

import (
	"os"
	"testing"
)

func TestAccessorPreservesExplicitOwnerClaim(t *testing.T) {
	for _, operation := range []string{"append", "label"} {
		t.Run(operation, func(t *testing.T) {
			storage, err := NewStorageAt(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			header := NewHeader(t.TempDir(), nil, nil)
			writer, err := storage.Create(header)
			if err != nil {
				t.Fatal(err)
			}
			entry := NewCustomEntry("fixture", []byte(`{}`))
			if err := writer.Append(entry); err != nil {
				t.Fatal(err)
			}
			path := writer.Path()
			if err := ClaimOwner(path); err != nil {
				t.Fatal(err)
			}
			accessor := NewSessionStoreAccessor(storage)
			if operation == "append" {
				_, err = accessor.AppendEntry(t.Context(), header.ID, "custom", []byte(`{"custom_type":"test"}`), nil)
			} else {
				_, err = accessor.SetLabel(t.Context(), header.ID, entry.ID, "label")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path + ".owner"); err != nil {
				t.Errorf("accessor consumed another caller's explicit claim: %v", err)
			}
			if err := ReleaseOwner(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
