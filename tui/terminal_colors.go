package tui

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RGBColor is an 8-bit terminal color.
type RGBColor struct{ R, G, B uint8 }

// TerminalColors contains reported defaults and a palette only when all 16
// entries arrived. Missing values are nil, not black.
type TerminalColors struct {
	Foreground *RGBColor
	Background *RGBColor
	Palette    *[16]RGBColor
}

var (
	terminalColorResponsePattern = regexp.MustCompile(`^\x1b\](10|11|4;[0-9]{1,3});([^\x07\x1b]*)(?:\x07|\x1b\\)$`)
	terminalHexPattern           = regexp.MustCompile(`^[0-9a-fA-F]{1,16}$`)
)

// Targets 16 and 17 represent foreground and background, respectively.
func parseTerminalColorReply(sequence string) (int, *RGBColor, bool) {
	match := terminalColorResponsePattern.FindStringSubmatch(sequence)
	if match == nil {
		return 0, nil, false
	}
	target := 16
	switch match[1] {
	case "10":
	case "11":
		target = 17
	default:
		index, err := strconv.Atoi(strings.TrimPrefix(match[1], "4;"))
		if err != nil {
			return 0, nil, false
		}
		target = index
		if target >= 16 {
			return 18, nil, true
		}
	}
	return target, parseTerminalRGB(match[2]), true
}

func parseTerminalRGB(value string) *RGBColor {
	value = strings.TrimSpace(value)
	var channels []string
	if strings.HasPrefix(value, "#") {
		value = value[1:]
		if len(value) != 6 && len(value) != 12 {
			return nil
		}
		size := len(value) / 3
		channels = []string{value[:size], value[size : 2*size], value[2*size:]}
	} else {
		value = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(value), "rgba:"), "rgb:")
		channels = strings.Split(value, "/")
		if len(channels) < 3 {
			return nil
		}
	}
	var rgb [3]uint8
	for i := range 3 {
		channel := channels[i]
		if !terminalHexPattern.MatchString(channel) {
			return nil
		}
		n, err := strconv.ParseUint(channel, 16, 64)
		if err != nil {
			return nil
		}
		maximum := ^uint64(0) >> (64 - 4*len(channel))
		rgb[i] = uint8(float64(n)/float64(maximum)*255 + 0.5)
	}
	return &RGBColor{rgb[0], rgb[1], rgb[2]}
}

func (colors TerminalColors) clone() TerminalColors {
	if colors.Foreground != nil {
		colors.Foreground = new(*colors.Foreground)
	}
	if colors.Background != nil {
		colors.Background = new(*colors.Background)
	}
	if colors.Palette != nil {
		colors.Palette = new(*colors.Palette)
	}
	return colors
}

type terminalColorQuery struct {
	colors       TerminalColors
	palette      [16]RGBColor
	replied      [18]bool
	count        int
	deliver      func(TerminalColors)
	timer        *time.Timer
	active       bool
	complete     bool
	expired      bool
	paletteValid bool
}

type terminalColorState struct {
	started       bool
	notifications bool
	queries       []*terminalColorQuery
	// A nil entry belongs to program-status negotiation. DA replies have no IDs,
	// so every query must consume its own sentinel in write order.
	sentinels []*terminalColorQuery
}

// QueryTerminalColors requests defaults and ANSI colors followed by a DA sentinel.
// Call on the app loop after Start. The callback runs on that loop at completion
// or timeout, and once more if late replies arrive before the sentinel.
func (a *App) QueryTerminalColors(timeout time.Duration, apply func(TerminalColors)) error {
	if !a.terminalColors.started {
		return fmt.Errorf("tui: terminal colors require Start")
	}
	if apply == nil {
		return fmt.Errorf("tui: terminal colors require a callback")
	}
	q := &terminalColorQuery{deliver: apply, active: true, paletteValid: true}
	a.terminalColors.queries = append(a.terminalColors.queries, q)
	a.terminalColors.sentinels = append(a.terminalColors.sentinels, q)
	var query strings.Builder
	query.WriteString("\x1b]10;?\a\x1b]11;?\a")
	for i := range 16 {
		fmt.Fprintf(&query, "\x1b]4;%d;?\a", i)
	}
	query.WriteString("\x1b[c")
	if err := a.writeTerminalControl(query.String()); err != nil {
		a.terminalColors.queries = a.terminalColors.queries[:len(a.terminalColors.queries)-1]
		a.terminalColors.sentinels = a.terminalColors.sentinels[:len(a.terminalColors.sentinels)-1]
		q.active = false
		return fmt.Errorf("query terminal colors: %w", err)
	}
	q.timer = time.AfterFunc(max(timeout, 0), func() {
		if err := a.Post(func(*UpdateCtx) { a.expireTerminalColorQuery(q) }); err != nil && !errors.Is(err, ErrClosed) {
			fmt.Fprintf(os.Stderr, "tui: terminal color timeout: %v\n", err)
		}
	})
	return nil
}

func (a *App) expireTerminalColorQuery(q *terminalColorQuery) {
	if !q.active || q.complete || q.expired {
		return
	}
	q.expired = true
	q.timer.Stop()
	q.deliver(q.colors.clone())
}

func (a *App) completeTerminalColorQuery(q *terminalColorQuery) {
	if q.complete {
		return
	}
	q.complete = true
	if q.timer != nil {
		q.timer.Stop()
	}
	q.deliver(q.colors.clone())
}

func (a *App) consumeTerminalColorReply(sequence string) bool {
	if strings.HasPrefix(sequence, "\x1b[?") && strings.HasSuffix(sequence, "c") {
		if len(a.terminalColors.sentinels) == 0 {
			return false
		}
		q := a.terminalColors.sentinels[0]
		a.terminalColors.sentinels = a.terminalColors.sentinels[1:]
		if q == nil {
			a.programStatus.pending = false
			return true
		}
		a.terminalColors.queries = slices.Delete(a.terminalColors.queries, 0, 1)
		q.active = false
		a.completeTerminalColorQuery(q)
		return true
	}
	target, color, ok := parseTerminalColorReply(sequence)
	if !ok {
		return false
	}
	if len(a.terminalColors.queries) == 0 {
		return false
	}
	if target >= 18 {
		return true
	}
	q := a.terminalColors.queries[0]
	if q.complete || q.replied[target] {
		return true
	}
	q.replied[target] = true
	q.count++
	switch target {
	case 16:
		q.colors.Foreground = color
	case 17:
		q.colors.Background = color
	default:
		if color != nil {
			q.palette[target] = *color
		}
	}
	// A malformed entry counts as a reply, but cannot complete the palette.
	if target < 16 && color == nil {
		q.paletteValid = false
	}
	paletteComplete := q.paletteValid
	for i := range 16 {
		paletteComplete = paletteComplete && q.replied[i]
	}
	if paletteComplete {
		q.colors.Palette = new(q.palette)
	}
	if q.count == 18 {
		a.completeTerminalColorQuery(q)
	}
	return true
}

// SetTerminalColorSchemeNotifications enables CSI 997 appearance reports.
// Call before Start or on the app loop. Stop disables them until the next Start.
func (a *App) SetTerminalColorSchemeNotifications(enabled bool) error {
	if a.terminalColors.notifications == enabled {
		return nil
	}
	if a.terminalColors.started {
		if err := a.writeTerminalColorNotifications(enabled); err != nil {
			return err
		}
	}
	a.terminalColors.notifications = enabled
	return nil
}

func (a *App) writeTerminalColorNotifications(enabled bool) error {
	sequence := "\x1b[?2031l"
	if enabled {
		sequence = "\x1b[?2031h"
	}
	return a.writeTerminalControl(sequence)
}

func (a *App) startTerminalColors() error {
	a.terminalColors.started = true
	if a.terminalColors.notifications {
		return a.writeTerminalColorNotifications(true)
	}
	return nil
}

func (a *App) stopTerminalColors() error {
	for _, q := range a.terminalColors.queries {
		q.active = false
		if q.timer != nil {
			q.timer.Stop()
		}
	}
	a.terminalColors.queries = nil
	a.terminalColors.sentinels = nil
	started := a.terminalColors.started
	a.terminalColors.started = false
	if started && a.terminalColors.notifications {
		return a.writeTerminalColorNotifications(false)
	}
	return nil
}
