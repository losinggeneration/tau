package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/losinggeneration/tau/ai"
)

// ansiEscapeRegex matches ANSI CSI and OSC escape sequences. CSI is
// ESC [ followed by parameter bytes (digits, semicolons, question marks)
// and a final byte in the 0x40-0x7E range. OSC is ESC ] followed by any
// non-terminator bytes and terminated by BEL (0x07) or the string
// terminator ESC \. The non-greedy character classes avoid catastrophic
// backtracking on long inputs.
var ansiEscapeRegex = regexp.MustCompile(`\x1b(?:\[[\d;?]*[a-zA-Z]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

// SanitizeBashOutput prepares captured bash output for the model by
// stripping terminal styling, normalizing carriage returns, removing lone
// surrogates, and replacing binary garbage with U+FFFD. The function is
// pure and safe to call on partial streaming chunks as well as final
// output.
//
// Pass order is intentional:
//
//  1. Strip ANSI escapes FIRST so the ESC byte (0x1b) inside sequences is
//     removed before binary-garbage replacement could corrupt sequences
//     whose valid form happens to contain control bytes.
//  2. Normalize CR (\r\n → \n, then any remaining \r → \n) so downstream
//     passes see clean newlines.
//  3. Strip surrogates (lone high/low surrogates that aren't valid UTF-8).
//  4. Replace remaining binary garbage (invalid UTF-8 bytes, C0 control
//     chars except \t and \n, and DEL) with U+FFFD so position context is
//     preserved without leaking terminal-control characters to the model.
//
// Truncation is intentionally NOT part of this function. Callers apply
// truncation after sanitization so the cap is measured against the
// cleaned text.
func SanitizeBashOutput(output string) string {
	output = stripANSI(output)
	output = normalizeCR(output)
	output = ai.SanitizeSurrogates(output)
	output = replaceBinaryGarbage(output)
	return output
}

// stripANSI removes CSI and OSC escape sequences from s. Other ANSI
// sequence types (DCS, SOS, PM, APC) are exceedingly rare in shell
// output and are not handled here.
func stripANSI(s string) string {
	return ansiEscapeRegex.ReplaceAllString(s, "")
}

// normalizeCR converts Windows-style line endings and lone carriage
// returns to LF. The order matters: \r\n must be replaced first so the
// standalone \r pass doesn't leave stray newlines.
func normalizeCR(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// replaceBinaryGarbage walks s as UTF-8, replacing invalid byte sequences
// and C0/DEL control characters (except \t and \n) with U+FFFD. Tab and
// newline are preserved because they are valid whitespace in shell
// output. Replacement (rather than dropping) preserves position context
// so the model can see "this byte was bad here" without losing
// surrounding meaningful content.
func replaceBinaryGarbage(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// Invalid UTF-8 byte: replace with U+FFFD and skip 1 byte.
			b.WriteRune('\uFFFD')
			i++
		case r == '\t' || r == '\n':
			// Whitespace: preserve.
			b.WriteRune(r)
			i += size
		case r < 0x20 || r == 0x7F:
			// C0 control (except \t, \n) and DEL: replace.
			b.WriteRune('\uFFFD')
			i += size
		default:
			b.WriteRune(r)
			i += size
		}
	}
	return b.String()
}
