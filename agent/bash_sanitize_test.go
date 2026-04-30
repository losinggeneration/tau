package agent

import (
	"strings"
	"testing"
)

func TestSanitizeBashOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text passes through unchanged",
			input: "hello world\n",
			want:  "hello world\n",
		},
		{
			name:  "SGR color codes are stripped",
			input: "\x1b[31merror\x1b[0m: failed\n",
			want:  "error: failed\n",
		},
		{
			name:  "cursor movement sequences are stripped",
			input: "\x1b[2J\x1b[Hclear screen\n",
			want:  "clear screen\n",
		},
		{
			name:  "OSC sequences (terminated by BEL) are stripped",
			input: "\x1b]0;title\x07actual text\n",
			want:  "actual text\n",
		},
		{
			name:  "CRLF normalized to LF",
			input: "line1\r\nline2\r\n",
			want:  "line1\nline2\n",
		},
		{
			name:  "lone CR normalized to LF",
			input: "old\rmac",
			want:  "old\nmac",
		},
		{
			name:  "CR followed by LF in same string is one conversion",
			input: "a\r\nb\rc\n",
			want:  "a\nb\nc\n",
		},
		{
			name:  "NUL bytes are replaced with U+FFFD",
			input: "before\x00after",
			want:  "before\uFFFDafter",
		},
		{
			name:  "invalid UTF-8 bytes are replaced",
			input: string([]byte{0x68, 0x69, 0xff, 0xfe, 0x21}), // hi<bad>!
			want:  "hi\uFFFD\uFFFD!",
		},
		{
			name:  "tab and newline are preserved",
			input: "col1\tcol2\nnext",
			want:  "col1\tcol2\nnext",
		},
		{
			name:  "DEL (0x7F) is replaced",
			input: "bad\x7Fbyte",
			want:  "bad\uFFFDbyte",
		},
		{
			name:  "lone high surrogate bytes are replaced",
			input: "before\xed\xa0\x80after", // U+D800 (lone high surrogate)
			want:  "before\uFFFD\uFFFD\uFFFDafter",
		},
		{
			name:  "lone low surrogate bytes are replaced",
			input: "before\xed\xb0\x80after", // U+DC00 (lone low surrogate)
			want:  "before\uFFFD\uFFFD\uFFFDafter",
		},
		{
			name:  "valid surrogate pair is preserved",
			input: "before\xf0\x9f\x98\x80after", // U+1F600 (emoji)
			want:  "before\xf0\x9f\x98\x80after",
		},
		{
			name:  "ESC inside text is replaced (not just stripped)",
			input: "a\x1bb",
			want:  "a\uFFFDb",
		},
		{
			name:  "all passes combined",
			input: "\x1b[31mFAIL\x1b[0m\r\nbad\x00byte\r\x1b]0;t\x07ok\n",
			want:  "FAIL\nbad\uFFFDbyte\nok\n",
		},
		{
			name:  "empty input is empty output",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeBashOutput(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeBashOutput(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeBashOutputPreservesMeaningfulContent(t *testing.T) {
	// Integration check: a realistic bash error output that mixes ANSI,
	// CR, surrogates, and a stray NUL should clean up but keep the
	// semantically meaningful parts intact.
	input := "\x1b[1;31mError:\x1b[0m cannot find /tmp/missing\n\x1b[2J"
	want := "Error: cannot find /tmp/missing\n"
	got := SanitizeBashOutput(input)
	if got != want {
		t.Errorf("realistic output:\ngot:  %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("ANSI escape bytes leaked through: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("carriage return leaked through: %q", got)
	}
}
