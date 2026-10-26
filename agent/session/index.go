package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
)

// Index locates the entries of a session file by byte offset, so a long
// session's history can be read a page at a time instead of parsing the whole
// log. Each call indexes only what was appended since the previous one and
// starts over when the file was replaced (MarkDeleted and migrations rewrite
// it). It expects the current format; legacy files are migrated by ReadFile
// when a session is resumed.
//
// ponytail: keeps every entry ID in memory (about 100 bytes per entry); key by
// parsed UUID bytes if very long sessions make that matter.
type Index struct {
	file    os.FileInfo
	size    int64
	entries []indexEntry
	byID    map[string]int32
	// anchored lists the compactions whose kept history starts at an entry.
	// Their summaries display right before it, as in Reader.AllEntries.
	anchored map[int32][]int32
}

type indexEntry struct {
	offset int64
	length int32
	parent int32
	kind   indexKind
}

type indexKind uint8

const (
	indexHidden indexKind = iota
	indexDisplayed
	indexCompaction
)

// History returns up to limit displayable entries of the branch, oldest
// first. The page ends just before the entry before, or at head (inclusive)
// when before is empty. next is the entry to pass as before for the page
// preceding this one, empty once the start of the branch is reached.
// Compaction summaries appear before the first entry they kept, as in
// Reader.AllEntries.
func (x *Index) History(path, head, before string, limit int) (entries []Entry, next string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open session: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close session: %w", closeErr)
		}
	}()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, "", fmt.Errorf("lock session for read: %w", err)
	}
	defer func() {
		if unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); unlockErr != nil && err == nil {
			err = fmt.Errorf("unlock session read: %w", unlockErr)
		}
	}()
	if err := x.refresh(f); err != nil {
		return nil, "", err
	}

	start := int32(-1)
	if before != "" {
		if i, ok := x.byID[before]; ok {
			start = x.entries[i].parent
		}
	} else if i, ok := x.byID[head]; ok {
		start = i
	}

	// Walk newest-first, then reverse. A page only ends on a chain entry, so
	// the summaries anchored to it are never split from it.
	var picked []int32
	last := int32(-1)
	for i := start; i >= 0 && len(picked) < limit; i = x.entries[i].parent {
		last = i
		if x.entries[i].kind == indexDisplayed {
			picked = append(picked, i)
		}
		anchors := x.anchored[i]
		for j := len(anchors) - 1; j >= 0; j-- {
			picked = append(picked, anchors[j])
		}
	}
	slices.Reverse(picked)
	entries = make([]Entry, 0, len(picked))
	for _, i := range picked {
		e, err := x.read(f, i)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, e)
	}
	if last >= 0 && x.entries[last].parent >= 0 {
		e, err := x.read(f, last)
		if err != nil {
			return nil, "", err
		}
		next = e.ID
	}
	return entries, next, nil
}

func (x *Index) read(f *os.File, i int32) (Entry, error) {
	ie := x.entries[i]
	buf := make([]byte, ie.length)
	if _, err := f.ReadAt(buf, ie.offset); err != nil {
		return Entry{}, fmt.Errorf("read session entry: %w", err)
	}
	return ParseEntry(buf)
}

// refresh indexes the lines appended since the last call. The caller holds a
// shared lock, so every line up to EOF is complete.
func (x *Index) refresh(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat session: %w", err)
	}
	if x.file == nil || !os.SameFile(x.file, info) || info.Size() < x.size {
		*x = Index{byID: map[string]int32{}, anchored: map[int32][]int32{}}
	}
	x.file = info
	if _, err := f.Seek(x.size, io.SeekStart); err != nil {
		return fmt.Errorf("seek session: %w", err)
	}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan session: %w", err)
		}
		x.add(line)
		x.size += int64(len(line))
	}
}

func (x *Index) add(line []byte) {
	var head struct {
		Type        EntryType `json:"type"`
		ID          string    `json:"id"`
		ParentID    *string   `json:"parent_id"`
		Status      string    `json:"status"`
		FirstKeptID *string   `json:"first_kept_entry_id"`
	}
	if json.Unmarshal(line, &head) != nil || head.ID == "" || head.Type == EntryTypeSession || head.Status == EntryStatusDeleted {
		return
	}
	ie := indexEntry{offset: x.size, length: int32(len(line)), parent: -1}
	if head.ParentID != nil {
		if p, ok := x.byID[*head.ParentID]; ok {
			ie.parent = p
		}
	}
	i := int32(len(x.entries))
	switch head.Type {
	case EntryTypeMessage, EntryTypeCustomMessage, EntryTypeBranchSummary:
		ie.kind = indexDisplayed
	case EntryTypeCompaction:
		ie.kind = indexCompaction
		if head.FirstKeptID != nil {
			if kept, ok := x.byID[*head.FirstKeptID]; ok {
				x.anchored[kept] = append(x.anchored[kept], i)
			}
		}
	}
	x.entries = append(x.entries, ie)
	x.byID[head.ID] = i
}
