package server

import (
	"testing"

	"github.com/losinggeneration/tau/sdk"
)

func TestWorkspaceStatusNeedsInputOutranksRunningAndQueued(t *testing.T) {
	info := sdk.SessionInfo{ID: "s1", Status: "waiting"}

	if got := workspaceStatus(info, activeSessionInfo{id: "s1", needsInput: true, streaming: true, steering: 1}); got != "needs_input" {
		t.Errorf("needs_input+streaming+queued = %q, want needs_input", got)
	}
}

func TestWorkspaceStatusActiveOverlaysPersistedStatus(t *testing.T) {
	info := sdk.SessionInfo{ID: "s1", Status: "complete"}

	if got := workspaceStatus(info, activeSessionInfo{id: "s1", streaming: true}); got != "running" {
		t.Errorf("streaming active = %q, want running", got)
	}
	if got := workspaceStatus(info, activeSessionInfo{id: "s1", steering: 1}); got != "queued" {
		t.Errorf("queued active = %q, want queued", got)
	}
	if got := workspaceStatus(info, activeSessionInfo{id: "s1"}); got != "complete" {
		t.Errorf("idle active = %q, want persisted complete", got)
	}
	// A different active session must not overlay this row.
	if got := workspaceStatus(info, activeSessionInfo{id: "other", streaming: true, needsInput: true}); got != "complete" {
		t.Errorf("other active = %q, want persisted complete", got)
	}
	if got := workspaceStatus(sdk.SessionInfo{ID: "s2"}, activeSessionInfo{}); got != "waiting" {
		t.Errorf("empty status = %q, want waiting", got)
	}
}

func TestActiveSessionInfoReportsPendingUIRequests(t *testing.T) {
	s := &Server{}
	if s.ui.PendingRequests() != 0 {
		t.Fatalf("PendingRequests with no UI = %d, want 0", s.ui.PendingRequests())
	}
	if got := s.activeSessionInfo(); got.needsInput {
		t.Fatal("activeSessionInfo without session reported needsInput")
	}
}
