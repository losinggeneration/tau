package autocomplete

import (
	"context"
	"testing"
	"time"
)

func TestDebouncerCancellationAndStaleResponse(t *testing.T) {
	provider, err := NewCombinedProvider(WithCommands([]SlashCommand{{Name: "help"}}))
	if err != nil {
		t.Fatalf("NewCombinedProvider: %v", err)
	}
	d := NewDebouncer(provider, 50*time.Millisecond)

	firstDone := make(chan *Suggestions, 1)
	go func() {
		s, reqErr := d.Request(context.Background(), []string{"/h"}, 0, 2, false)
		if reqErr != nil {
			t.Errorf("first Request error: %v", reqErr)
		}
		firstDone <- s
	}()

	time.Sleep(5 * time.Millisecond)
	second, err := d.Request(context.Background(), []string{"/he"}, 0, 3, false)
	if err != nil {
		t.Fatalf("second Request: %v", err)
	}
	if second == nil || len(second.Items) != 1 || second.Items[0].Value != "help" {
		t.Fatalf("second suggestions = %+v, want help", second)
	}

	select {
	case got := <-firstDone:
		if got != nil {
			t.Fatalf("first stale/cancelled request returned %+v, want nil", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("first request did not complete")
	}
}
