package server

import "github.com/losinggeneration/tau/sdk"

type activeSessionInfo struct {
	id         string
	streaming  bool
	steering   int
	followUp   int
	needsInput bool
}

func (s *Server) activeSessionInfo() activeSessionInfo {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.session == nil {
		return activeSessionInfo{}
	}
	state := s.session.State()
	steering, followUp := queueSizes(s.session)
	return activeSessionInfo{
		id:         s.session.ID(),
		streaming:  state.IsStreaming(),
		steering:   steering,
		followUp:   followUp,
		needsInput: s.ui.PendingRequests() > 0,
	}
}

func workspaceStatus(info sdk.SessionInfo, active activeSessionInfo) string {
	if info.ID == active.id {
		// Blocked on a UI response outranks the streaming indicator: the run
		// cannot continue until the user answers.
		if active.needsInput {
			return "needs_input"
		}
		if active.streaming {
			return "running"
		}
		if active.steering+active.followUp > 0 {
			return "queued"
		}
	}
	if info.Status != "" {
		return info.Status
	}
	return "waiting"
}
