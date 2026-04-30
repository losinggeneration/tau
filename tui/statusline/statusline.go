package statusline

import (
	"strconv"
	"strings"
	"time"

	"github.com/losinggeneration/rovel/text"
)

type Cost struct {
	InputTokens, OutputTokens int
	Dollars                   float64
}
type State struct {
	Model, Provider, Session, Branch string
	Thinking                         bool
	ToolsEnabled                     int
	Pending                          int
	Running                          bool
	Abortable                        bool
	Started                          time.Time
	Cost                             Cost
}

func RenderHeader(s State, width int) string {
	parts := []string{}
	if s.Session != "" {
		parts = append(parts, s.Session)
	}
	if s.Branch != "" {
		parts = append(parts, " "+s.Branch)
	}
	model := s.Model
	if s.Provider != "" {
		model = s.Provider + "/" + model
	}
	if model != "" {
		parts = append(parts, model)
	}
	if s.Thinking {
		parts = append(parts, "thinking")
	}
	return fit(strings.Join(parts, " · "), width)
}

func RenderFooter(s State, width int) string {
	parts := []string{}
	if s.Running {
		if s.Abortable {
			parts = append(parts, "running (esc abort)")
		} else {
			parts = append(parts, "running")
		}
	} else {
		parts = append(parts, "idle")
	}
	if s.Pending > 0 {
		parts = append(parts, "queued "+strconv.Itoa(s.Pending))
	}
	if s.ToolsEnabled > 0 {
		parts = append(parts, "tools "+strconv.Itoa(s.ToolsEnabled))
	}
	if s.Cost.InputTokens+s.Cost.OutputTokens > 0 {
		parts = append(parts, "tok "+strconv.Itoa(s.Cost.InputTokens)+"/"+strconv.Itoa(s.Cost.OutputTokens))
	}
	if s.Cost.Dollars > 0 {
		parts = append(parts, "$"+formatCents(s.Cost.Dollars))
	}
	if !s.Started.IsZero() {
		parts = append(parts, time.Since(s.Started).Round(time.Second).String())
	}
	return fit(strings.Join(parts, " · "), width)
}

func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	width := text.Width(s)
	if width <= w {
		return s + strings.Repeat(" ", w-width)
	}
	return text.Truncate(s, w, true)
}

func formatCents(f float64) string {
	cents := int(f*100 + 0.5)
	return strconv.Itoa(cents/100) + "." + digit(cents/10%10) + digit(cents%10)
}
func digit(n int) string { return string(rune('0' + n)) }
