package killring

import "testing"

func TestRingPushPeekAndLen(t *testing.T) {
	var ring Ring
	if got := ring.Len(); got != 0 {
		t.Fatalf("initial Len = %d, want 0", got)
	}
	if _, ok := ring.Peek(); ok {
		t.Fatalf("initial Peek ok = true, want false")
	}

	ring.Push("first", PushOptions{})
	ring.Push("second", PushOptions{})

	if got := ring.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	if got, ok := ring.Peek(); !ok || got != "second" {
		t.Fatalf("Peek = %q, %v, want second true", got, ok)
	}
}

func TestRingIgnoresEmptyPush(t *testing.T) {
	var ring Ring
	ring.Push("", PushOptions{})
	if got := ring.Len(); got != 0 {
		t.Fatalf("Len after empty push = %d, want 0", got)
	}
}

func TestRingAccumulatesByAppending(t *testing.T) {
	var ring Ring
	ring.Push("hello", PushOptions{})
	ring.Push(" world", PushOptions{Accumulate: true})
	if got, ok := ring.Peek(); !ok || got != "hello world" {
		t.Fatalf("Peek = %q, %v, want accumulated append", got, ok)
	}
}

func TestRingAccumulatesByPrepending(t *testing.T) {
	var ring Ring
	ring.Push("world", PushOptions{})
	ring.Push("hello ", PushOptions{Accumulate: true, Prepend: true})
	if got, ok := ring.Peek(); !ok || got != "hello world" {
		t.Fatalf("Peek = %q, %v, want accumulated prepend", got, ok)
	}
}

func TestRingRotateCyclesYankPopOrder(t *testing.T) {
	var ring Ring
	ring.Push("first", PushOptions{})
	ring.Push("second", PushOptions{})
	ring.Push("third", PushOptions{})

	if got, _ := ring.Peek(); got != "third" {
		t.Fatalf("initial peek = %q, want third", got)
	}

	ring.Rotate()
	if got, _ := ring.Peek(); got != "second" {
		t.Fatalf("peek after rotate = %q, want second", got)
	}

	ring.Rotate()
	if got, _ := ring.Peek(); got != "first" {
		t.Fatalf("peek after second rotate = %q, want first", got)
	}

	ring.Rotate()
	if got, _ := ring.Peek(); got != "third" {
		t.Fatalf("peek after third rotate = %q, want third", got)
	}
}

func TestRingRotateNoopForSmallRings(t *testing.T) {
	var empty Ring
	empty.Rotate()
	if got := empty.Len(); got != 0 {
		t.Fatalf("empty Len = %d, want 0", got)
	}

	var one Ring
	one.Push("only", PushOptions{})
	one.Rotate()
	if got, _ := one.Peek(); got != "only" {
		t.Fatalf("single Peek after rotate = %q, want only", got)
	}
}

func TestRingEntriesReturnsCopy(t *testing.T) {
	var ring Ring
	ring.Push("first", PushOptions{})
	ring.Push("second", PushOptions{})

	entries := ring.Entries()
	entries[0] = "changed"

	got := ring.Entries()
	if got[0] != "first" || got[1] != "second" {
		t.Fatalf("Entries aliased ring: %v", got)
	}
}

func TestRingClear(t *testing.T) {
	var ring Ring
	ring.Push("first", PushOptions{})
	ring.Clear()
	if got := ring.Len(); got != 0 {
		t.Fatalf("Len after Clear = %d, want 0", got)
	}
}
