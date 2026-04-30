package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/losinggeneration/tau/sdk"
)

func sessionInfoDisplayPrompt(info sdk.SessionInfo) string {
	if prompt := strings.TrimSpace(info.FirstPrompt); prompt != "" {
		return strings.Join(strings.Fields(prompt), " ")
	}
	if name := strings.TrimSpace(info.Name); name != "" {
		return name
	}
	return "(unnamed)"
}

func sessionInfoDisplayMeta(info sdk.SessionInfo) string {
	status := info.Status
	if status == "" {
		status = "waiting"
	}
	model := strings.TrimSpace(info.Model)
	parts := []string{status, fmt.Sprintf("%d", info.MessageCount), relativeSessionTime(sessionInfoTime(info))}
	if model != "" {
		parts = append(parts, model)
	}
	if cwd := strings.TrimSpace(info.CWD); cwd != "" {
		parts = append(parts, cwd)
	}
	return strings.Join(parts, " ")
}

func sessionInfoTime(info sdk.SessionInfo) string {
	if info.LastModified != "" {
		return info.LastModified
	}
	return info.Created
}

func relativeSessionTime(created string) string {
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		if len(created) >= 10 {
			return created[:10]
		}
		return created
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	minute := time.Minute
	hour := time.Hour
	day := 24 * hour
	week := 7 * day
	month := 30 * day
	year := 365 * day
	switch {
	case d < minute:
		return "now"
	case d < hour:
		return fmt.Sprintf("%dm", int(d/minute))
	case d < day:
		return fmt.Sprintf("%dh", int(d/hour))
	case d < week:
		return fmt.Sprintf("%dd", int(d/day))
	case d < month:
		return fmt.Sprintf("%dw", int(d/week))
	case d < year:
		return fmt.Sprintf("%dmo", int(d/month))
	default:
		return fmt.Sprintf("%dy", int(d/year))
	}
}
