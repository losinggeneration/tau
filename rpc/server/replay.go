package server

import (
	"encoding/json"
	"fmt"

	"github.com/losinggeneration/tau/rpc/coord"
)

const (
	replayFrames = 2048
	replayBytes  = 8 << 20
	// Leave room for the RPC and coordinator envelopes, including routing IDs.
	replayPageBytes = 896 << 10
	replayPageCount = 64
)

type replayEntry struct {
	frame coord.Frame
	bytes int
}

// eventJournal is guarded by workerWriter.mu. Its sequence belongs to the
// worker, not its session or its connection to the coordinator.
type eventJournal struct {
	position coord.Position
	entries  []replayEntry
	start    int
	count    int
	bytes    int
}

func (j *eventJournal) publish(frame coord.Frame, size int) {
	if j.entries == nil {
		j.entries = make([]replayEntry, replayFrames)
	}
	for j.count == len(j.entries) || j.bytes+size > replayBytes {
		j.bytes -= j.entries[j.start].bytes
		j.entries[j.start] = replayEntry{}
		j.start = (j.start + 1) % len(j.entries)
		j.count--
	}
	idx := (j.start + j.count) % len(j.entries)
	j.entries[idx] = replayEntry{frame: frame, bytes: size}
	j.count++
	j.bytes += size
}

type replayPage struct {
	Events []coord.Frame `json:"events"`
	More   bool          `json:"more"`
}

func (j *eventJournal) replay(after *coord.Position) (replayPage, bool) {
	p := j.position
	if after == nil || after.InstanceID != p.InstanceID || after.Epoch != p.Epoch ||
		after.Offset > p.Offset || p.Offset-after.Offset > uint64(j.count) {
		return replayPage{}, false
	}
	page := replayPage{Events: []coord.Frame{}}
	var size int
	for i := j.count - int(p.Offset-after.Offset); i < j.count; i++ {
		e := j.entries[(j.start+i)%len(j.entries)]
		if size+e.bytes > replayPageBytes || len(page.Events) == replayPageCount {
			page.More = true
			break
		}
		page.Events = append(page.Events, e.frame)
		size += e.bytes
	}
	return page, true
}

func (w *workerWriter) replayEvents(cmd *Command) error {
	var args struct {
		After *coord.Position `json:"after"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return w.server.sendErrorResult(cmd.ID, fmt.Errorf("decode replay position: %w", err))
	}
	w.mu.Lock()
	page, ok := w.journal.replay(args.After)
	w.mu.Unlock()
	if !ok {
		return w.server.sendError(cmd.ID, coord.CodeResyncRequired, "event position missing, expired, or not for this worker")
	}
	return w.server.sendResult(cmd.ID, page)
}
