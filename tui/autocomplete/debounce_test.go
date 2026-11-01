package autocomplete

import (
	"context"
	"errors"
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

func TestDebouncerJoinsSupersededProviderBeforeStartingNext(t *testing.T) {
	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	release := make(chan struct{})
	secondStarted := make(chan struct{})
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	provider, err := NewCombinedProvider(WithCommands([]SlashCommand{{Name: "cmd", CompleteArgs: func(ctx context.Context, prefix string) ([]Item, error) {
		if prefix == "first" {
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			<-release
		} else {
			close(secondStarted)
		}
		return []Item{{Value: prefix}}, nil
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDebouncer(provider, 0)
	go func() {
		defer close(firstDone)
		s, err := d.Request(t.Context(), []string{"/cmd first"}, 0, len("/cmd first"), false)
		if err != nil || s != nil {
			t.Errorf("superseded request=%+v,%v", s, err)
		}
	}()
	<-firstStarted
	go func() {
		defer close(secondDone)
		s, err := d.Request(t.Context(), []string{"/cmd second"}, 0, len("/cmd second"), false)
		if err != nil || s == nil {
			t.Errorf("current request=%+v,%v", s, err)
		}
	}()
	<-firstCancelled
	select {
	case <-secondStarted:
		t.Error("new provider started while superseded provider is still running")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	<-firstDone
	<-secondDone
	if d.StaleResponses() != 1 {
		t.Fatalf("stale=%d", d.StaleResponses())
	}
}

func TestDebouncerRejectsSuccessAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	provider, err := NewCombinedProvider(WithCommands([]SlashCommand{{Name: "cmd", CompleteArgs: func(context.Context, string) ([]Item, error) { cancel(); return []Item{{Value: "stale"}}, nil }}}))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDebouncer(provider, 0)
	s, err := d.Request(ctx, []string{"/cmd x"}, 0, len("/cmd x"), false)
	if s != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request=%+v,%v", s, err)
	}
}

func TestDebouncerCancelRejectsLateSuccessfulResult(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider, err := NewCombinedProvider(WithCommands([]SlashCommand{{Name: "cmd", CompleteArgs: func(context.Context, string) ([]Item, error) {
		close(started)
		<-release
		return []Item{{Value: "stale"}}, nil
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDebouncer(provider, 0)
	go func() {
		defer close(done)
		s, err := d.Request(t.Context(), []string{"/cmd x"}, 0, len("/cmd x"), false)
		if err != nil || s != nil {
			t.Errorf("cancelled request=%+v,%v", s, err)
		}
	}()
	<-started
	d.Cancel()
	close(release)
	<-done
	if d.StaleResponses() != 1 {
		t.Fatalf("stale=%d", d.StaleResponses())
	}
}

func TestDebouncerCallerCancellationDuringDelay(t *testing.T) {
	provider, err := NewCombinedProvider(WithCommands([]SlashCommand{{Name: "help"}}))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDebouncer(provider, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s, err := d.Request(ctx, []string{"/h"}, 0, 2, false)
	if s != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delay=%+v,%v", s, err)
	}
}
