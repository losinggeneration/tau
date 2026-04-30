package killring

// Ring stores deleted text for Emacs-style kill/yank operations.
//
// The most recent entry is at the logical end of the ring. Consecutive kills
// can accumulate into a single entry, either prepending backward deletions or
// appending forward deletions. Rotate supports yank-pop by moving the most
// recent entry to the oldest position, exposing the previous entry as the next
// Peek value.
type Ring struct {
	entries []string
}

// PushOptions configures how text is added to the ring.
type PushOptions struct {
	// Prepend controls accumulation order. When Accumulate is true, Prepend
	// inserts text before the current most-recent entry; otherwise text is
	// appended after it.
	Prepend bool

	// Accumulate merges text into the current most-recent entry instead of
	// creating a new entry.
	Accumulate bool
}

// Push adds text to the ring. Empty text is ignored.
func (r *Ring) Push(text string, opts PushOptions) {
	if text == "" {
		return
	}

	if opts.Accumulate && len(r.entries) > 0 {
		last := r.entries[len(r.entries)-1]
		if opts.Prepend {
			r.entries[len(r.entries)-1] = text + last
		} else {
			r.entries[len(r.entries)-1] = last + text
		}

		return
	}

	r.entries = append(r.entries, text)
}

// Peek returns the most recent entry without modifying the ring.
func (r *Ring) Peek() (string, bool) {
	if r == nil || len(r.entries) == 0 {
		return "", false
	}

	return r.entries[len(r.entries)-1], true
}

// Rotate moves the most recent entry to the oldest position for yank-pop
// cycling. It is a no-op unless the ring contains at least two entries.
func (r *Ring) Rotate() {
	if r == nil || len(r.entries) <= 1 {
		return
	}

	last := r.entries[len(r.entries)-1]
	copy(r.entries[1:], r.entries[:len(r.entries)-1])
	r.entries[0] = last
}

// Len returns the number of entries in the ring.
func (r *Ring) Len() int {
	if r == nil {
		return 0
	}

	return len(r.entries)
}

// Entries returns a copy of the ring entries from oldest to newest.
func (r *Ring) Entries() []string {
	if r == nil {
		return nil
	}

	out := make([]string, len(r.entries))
	copy(out, r.entries)

	return out
}

// Clear removes all entries.
func (r *Ring) Clear() {
	if r == nil {
		return
	}

	r.entries = nil
}
