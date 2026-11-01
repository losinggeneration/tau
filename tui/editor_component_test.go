package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/losinggeneration/tau/tui/autocomplete"
	editormodel "github.com/losinggeneration/tau/tui/editor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditorComponentWrappedBoundaryRendersOneCursor(t *testing.T) {
	for _, tc := range []struct {
		text                        string
		cursor, width, padding, row int
		prefix                      string
	}{
		{"abcdef", 3, 4, 0, 2, ""},
		{"abcdef", 6, 4, 0, 2, "def"},
		{"界界界", len("界界"), 5, 0, 2, ""},
		{"abcdef", 3, 5, 1, 2, " "},
	} {
		t.Run(tc.text+tc.prefix, func(t *testing.T) {
			m := editormodel.New()
			m.SetText(tc.text)
			m.SetCursor(tc.cursor)
			c := NewEditorComponent(m)
			c.PaddingX = tc.padding
			rows := c.Render(tc.width)
			require.Greater(t, len(rows), tc.row)
			assert.Equal(t, 1, strings.Count(strings.Join(rows, "\n"), CursorMarker))
			assert.Equal(t, tc.prefix, strings.Split(rows[tc.row], CursorMarker)[0])
			for index, row := range rows {
				if index != tc.row {
					assert.NotContains(t, row, CursorMarker)
				}
			}
			assert.Equal(t, tc.text, m.Text())
			assert.Equal(t, tc.cursor, m.Cursor())
		})
	}
}

func TestEditorComponentCompletionSelectionTracksFullList(t *testing.T) {
	m := editormodel.New()
	m.SetAutocompleteMaxVisible(3)
	items := []autocomplete.Item{{Value: "zero"}, {Value: "one"}, {Value: "two"}, {Value: "three"}, {Value: "four"}, {Value: "five"}, {Value: "six"}}
	m.SetAutocomplete(&autocomplete.Suggestions{Items: items})
	for range 5 {
		m.AutocompleteNext()
	}
	c := NewEditorComponent(m)
	rows := c.Render(30)
	selected := []string{}
	for _, row := range rows {
		if strings.HasPrefix(row, "→ ") {
			selected = append(selected, strings.TrimSpace(strings.TrimPrefix(row, "→ ")))
		}
	}
	assert.Equal(t, []string{"five"}, selected)
	assert.Contains(t, strings.Join(rows, "\n"), "(6/7)")
	assert.NotContains(t, strings.Join(rows, "\n"), "zero")
	assert.Equal(t, 5, m.AutocompleteIndex())
	m.ApplyAutocomplete()
	assert.Equal(t, "five", m.Text())
}

func TestEditorComponentScrollBordersFitNarrowRows(t *testing.T) {
	for width := 1; width <= 24; width++ {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := editormodel.New()
			m.SetText("one\ntwo\nthree\nfour\nfive")
			m.SetCursor(len("one\ntwo\n"))
			c := NewEditorComponent(m)
			c.MaxVisibleRows = 1
			rows := c.Render(width)
			require.Len(t, rows, 3)
			for _, index := range []int{0, 2} {
				assert.Equal(t, width, TextWidth(rows[index]), "border %d: %q", index, rows[index])
			}
			assert.Equal(t, "one\ntwo\nthree\nfour\nfive", m.Text())
		})
	}
}

func TestScrollRuleUsesCenteredLabelsAndBoundedFallback(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  string
	}{{0, ""}, {1, "."}, {2, ".."}, {3, "..."}, {4, "─..."}, {10, "─── ↑ 1..."}, {11, "─── ↑ 1 ..."}, {12, "─ ↑ 1 more ─"}, {15, "── ↑ 1 more ───"}} {
		assert.Equal(t, tc.want, scrollRule(tc.width, "↑", 1), "width %d", tc.width)
	}
}

func TestEditorComponentRefillsViewportAfterResize(t *testing.T) {
	m := editormodel.New()
	m.SetText("abcdefghijkl")
	m.SetCursor(len(m.Text()))
	c := NewEditorComponent(m)
	c.MaxVisibleRows = 2
	assert.Len(t, c.Render(4), 4)
	c.MaxVisibleRows = 3
	rows := c.Render(4)
	require.Len(t, rows, 5)
	assert.Contains(t, rows[1], "def")
	assert.Contains(t, rows[3], "jkl")
	rows = c.Render(7)
	require.Len(t, rows, 4)
	assert.Contains(t, rows[1], "abcdef")
	assert.Contains(t, rows[2], "ghijkl")
	assert.Zero(t, m.ScrollOffset())
}
