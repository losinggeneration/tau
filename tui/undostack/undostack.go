package undostack

// Stack stores snapshots for undo. Values are cloned on Push with the caller's
// clone function so popped snapshots are detached from subsequent mutations.
type Stack[S any] struct {
	clone func(S) S
	items []S
}

// New creates a stack that clones snapshots with clone. If clone is nil, values
// are stored as-is, which is suitable for immutable/value-only state.
func New[S any](clone func(S) S) *Stack[S] {
	return &Stack[S]{clone: clone}
}

// Push stores a cloned snapshot.
func (s *Stack[S]) Push(state S) {
	if s == nil {
		return
	}

	if s.clone != nil {
		state = s.clone(state)
	}
	s.items = append(s.items, state)
}

// Pop removes and returns the most recent snapshot.
func (s *Stack[S]) Pop() (S, bool) {
	var zero S
	if s == nil || len(s.items) == 0 {
		return zero, false
	}

	last := len(s.items) - 1
	state := s.items[last]
	s.items[last] = zero
	s.items = s.items[:last]

	return state, true
}

// Clear removes all snapshots.
func (s *Stack[S]) Clear() {
	if s == nil {
		return
	}

	var zero S
	for i := range s.items {
		s.items[i] = zero
	}
	s.items = nil
}

// Len returns the number of snapshots.
func (s *Stack[S]) Len() int {
	if s == nil {
		return 0
	}

	return len(s.items)
}
