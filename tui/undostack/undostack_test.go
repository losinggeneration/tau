package undostack

import "testing"

type state struct {
	Text  string
	Items []string
}

func TestStackPushClonesAndPopReturnsLatest(t *testing.T) {
	stack := New(func(s state) state {
		s.Items = append([]string(nil), s.Items...)
		return s
	})

	first := state{Text: "first", Items: []string{"a"}}
	stack.Push(first)
	first.Text = "mutated"
	first.Items[0] = "changed"

	stack.Push(state{Text: "second", Items: []string{"b"}})

	if got := stack.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}

	latest, ok := stack.Pop()
	if !ok || latest.Text != "second" || latest.Items[0] != "b" {
		t.Fatalf("latest Pop = %+v, %v, want second", latest, ok)
	}

	snapshot, ok := stack.Pop()
	if !ok || snapshot.Text != "first" || snapshot.Items[0] != "a" {
		t.Fatalf("snapshot Pop = %+v, %v, want cloned first", snapshot, ok)
	}

	if _, ok := stack.Pop(); ok {
		t.Fatalf("empty Pop ok = true, want false")
	}
}

func TestStackNilCloneStoresValue(t *testing.T) {
	stack := New[int](nil)
	stack.Push(1)
	stack.Push(2)
	got, ok := stack.Pop()
	if !ok || got != 2 {
		t.Fatalf("Pop = %d, %v, want 2 true", got, ok)
	}
}

func TestStackClear(t *testing.T) {
	stack := New[int](nil)
	stack.Push(1)
	stack.Push(2)
	stack.Clear()
	if got := stack.Len(); got != 0 {
		t.Fatalf("Len after Clear = %d, want 0", got)
	}
	if _, ok := stack.Pop(); ok {
		t.Fatalf("Pop after Clear ok = true, want false")
	}
}

func TestNilStackMethodsAreSafe(t *testing.T) {
	var stack *Stack[int]
	stack.Push(1)
	stack.Clear()
	if got := stack.Len(); got != 0 {
		t.Fatalf("nil Len = %d, want 0", got)
	}
	if _, ok := stack.Pop(); ok {
		t.Fatalf("nil Pop ok = true, want false")
	}
}
