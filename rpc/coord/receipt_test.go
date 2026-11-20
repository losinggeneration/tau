//go:build unix

package coord

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLaunchReceiptPruningCannotMakeAnOldIntentNew(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	store, err := openReceiptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	key := receiptKey("", launchIDAt(now.Add(time.Second)))
	life, err := store.create(key, launchReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if life != nil {
			if err := life.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	later := now.Add(365 * 24 * time.Hour)
	if _, err := store.prune(later); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(key); err != nil {
		t.Fatalf("pruned a live worker: %v", err)
	}
	if err := life.Close(); err != nil {
		t.Fatal(err)
	}
	life = nil
	if _, err := store.prune(later); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired receipt was not pruned: %v", err)
	}
	// A new coordinator at the original wall clock must still reject it.
	store, err = openReceiptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.create(key, launchReceipt{}); !errors.Is(err, errExpiredLaunchIntent) {
		t.Fatalf("pruned intent is eligible again: %v", err)
	}
	// A transient clock jump while pruning must not block unrelated fresh IDs.
	fresh := receiptKey("", launchIDAt(now.Add(2*time.Second)))
	if err := store.check(fresh, now); err != nil {
		t.Fatalf("fresh intent rejected after clock correction: %v", err)
	}
}

func TestNewReceiptStoreRejectsIntentsFromBeforeItExisted(t *testing.T) {
	key := receiptKey("", launchIDAt(time.Now().Add(-time.Minute)))
	dir := filepath.Join(t.TempDir(), "receipts")
	for range 2 {
		store, err := openReceiptStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.check(key, time.Now()); !errors.Is(err, errExpiredLaunchIntent) {
			t.Fatalf("intent from a lost store is eligible again: %v", err)
		}
	}
}

func TestLaunchReceiptClockRefusalsOnlyCoverUnrecordedIntents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	store, err := openReceiptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-3 * launchReceiptTTL).Truncate(time.Millisecond)
	if err := store.saveCutoff(old); err != nil {
		t.Fatal(err)
	}
	if store, err = openReceiptStore(dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		issued time.Time
		want   error
	}{
		{"before durable cutoff", old.Add(-time.Hour), errExpiredLaunchIntent},
		{"at durable cutoff", old, errExpiredLaunchIntent},
		{"stale after cutoff", now.Add(-2 * launchReceiptTTL), errStaleLaunchIntent},
		{"future", now.Add(launchClockSkew + time.Minute), errFutureLaunchIntent},
	} {
		if err := store.check(receiptKey("", launchIDAt(tc.issued)), now); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}

	// A recorded intent never reports a clock refusal, before or after pruning.
	key := receiptKey("", launchIDAt(now))
	life, err := store.create(key, launchReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * launchReceiptTTL)
	if err := store.check(key, later); !errors.Is(err, os.ErrExist) {
		t.Fatalf("recorded intent after the window: %v", err)
	}
	if err := store.check(key, now.Add(-time.Hour)); !errors.Is(err, os.ErrExist) {
		t.Fatalf("recorded intent seen from a slow clock: %v", err)
	}
	if err := life.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prune(later); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{later, now, now.Add(-time.Hour)} {
		if err := store.check(key, at); !errors.Is(err, errExpiredLaunchIntent) {
			t.Fatalf("pruned intent at %v: %v", at, err)
		}
	}
}

func launchIDAt(now time.Time) string {
	id := uuid.Must(uuid.NewV7())
	ms := now.UnixMilli()
	for i := 5; i >= 0; i-- {
		id[i] = byte(ms)
		ms >>= 8
	}
	return id.String()
}
