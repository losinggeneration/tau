package server

import (
	"testing"

	"github.com/losinggeneration/tau/rpc/coord"
)

func TestEventJournalRejectsExpiredPositions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		size  int
	}{
		{"frame limit", replayFrames + 1, 256},
		{"byte limit", replayBytes/(replayPageBytes/2) + 1, replayPageBytes / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := eventJournal{position: coord.Position{InstanceID: "instance", Epoch: "epoch"}}
			origin := j.position
			for range tc.count {
				j.position.Offset++
				p := j.position
				j.publish(coord.Frame{Type: coord.TypeEvent, Position: &p}, tc.size)
			}
			before := origin
			before.Offset = 1
			page, ok := j.replay(&before)
			if !ok || !page.More || len(page.Events) == 0 || page.Events[0].Position.Offset != 2 {
				t.Fatalf("oldest retained page: %+v, valid=%v", page, ok)
			}
			wrongEpoch, wrongInstance, future := j.position, j.position, j.position
			wrongEpoch.Epoch = "previous epoch"
			wrongInstance.InstanceID = "other instance"
			future.Offset++
			for name, invalid := range map[string]*coord.Position{
				"expired": &origin, "missing": nil, "epoch": &wrongEpoch, "instance": &wrongInstance, "future": &future,
			} {
				if _, ok := j.replay(invalid); ok {
					t.Errorf("accepted %s cursor: %+v", name, invalid)
				}
			}
		})
	}
}
