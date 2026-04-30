package ai

import (
	"encoding/json"
	"net/http"
	"unicode/utf16"
	"unicode/utf8"
)

// SanitizeSurrogates removes lone surrogate characters from a string.
// Lone surrogates (U+D800 to U+DFFF) are invalid Unicode and can cause
// problems when encoding to JSON or passing to APIs.
//
// This matches pi-mono's sanitize-unicode.ts behavior.
func SanitizeSurrogates(s string) string {
	// More precise approach: walk the string and remove invalid surrogates
	var buf []byte
	runes := []rune(s)
	for i, r := range runes {
		if r >= 0xD800 && r <= 0xDFFF {
			// This is a surrogate
			if r >= 0xD800 && r <= 0xDBFF {
				// Lead surrogate - check if followed by trail surrogate
				if i+1 < len(runes) && runes[i+1] >= 0xDC00 && runes[i+1] <= 0xDFFF {
					// Valid surrogate pair - keep it
					buf = append(buf, string(runes[i])...)
					continue
				}
			}
			// Lone surrogate - skip it
			continue
		}
		buf = append(buf, string(runes[i])...)
	}

	return string(buf)
}

// ShortHash matches pi-mono's utils/hash.ts behavior used by Mistral and
// other provider-specific ID normalizers. The TypeScript implementation hashes
// UTF-16 code units, so we mirror that here instead of hashing UTF-8 bytes.
func ShortHash(s string) string {
	var h1 uint32 = 0xdeadbeef
	var h2 uint32 = 0x41c6ce57

	for _, ch := range utf16.Encode([]rune(s)) {
		c := uint32(ch)
		h1 = imul32(h1^c, 2654435761)
		h2 = imul32(h2^c, 1597334677)
	}

	h1 = imul32(h1^(h1>>16), 2246822507) ^ imul32(h2^(h2>>13), 3266489909)
	h2 = imul32(h2^(h2>>16), 2246822507) ^ imul32(h1^(h1>>13), 3266489909)

	return base36Encode(h2) + base36Encode(h1)
}

// base36Encode encodes a uint32 as a base-36 string.
func base36Encode(n uint32) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	const base = uint32(len(chars))

	if n == 0 {
		return "0"
	}

	var buf [8]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = chars[n%base]
		n /= base
	}

	return string(buf[i:])
}

func imul32(a, b uint32) uint32 {
	return a * b
}

// PartialJSONParser provides partial-JSON tolerant parsing for streaming tool call arguments.
// It can recover from malformed JSON by waiting for complete tokens.
type PartialJSONParser struct {
	buffer []byte
	depth  int
}

// NewPartialJSONParser creates a new partial JSON parser.
func NewPartialJSONParser() *PartialJSONParser {
	return &PartialJSONParser{}
}

// Feed adds more data to the parser buffer.
func (p *PartialJSONParser) Feed(data string) {
	p.buffer = append(p.buffer, data...)
}

// CanParse reports whether the current buffer contains a valid JSON object or array.
func (p *PartialJSONParser) CanParse() bool {
	// Check if we have a complete object or array
	depth := 0
	inString := false
	escape := false

	for _, r := range string(p.buffer) {
		if escape {
			escape = false
			continue
		}
		if r == '\\' && inString {
			escape = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		switch r {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}

	return depth == 0 && !inString
}

// Parse attempts to parse the buffered data as JSON.
func (p *PartialJSONParser) Parse() (map[string]any, bool) {
	if !p.CanParse() {
		return nil, false
	}

	var result map[string]any
	if err := json.Unmarshal(p.buffer, &result); err != nil {
		return nil, false
	}

	return result, true
}

// Reset clears the parser buffer.
func (p *PartialJSONParser) Reset() {
	p.buffer = nil
	p.depth = 0
}

// ExtractValidJSON extracts a valid JSON object from potentially partial JSON data.
// Returns the parsed map and whether extraction was successful.
func ExtractValidJSON(data string) (map[string]any, bool) {
	parser := NewPartialJSONParser()
	parser.Feed(data)

	if parser.CanParse() {
		result, ok := parser.Parse()
		return result, ok
	}

	// Try to find a complete object in the data
	return extractPartialObject(data)
}

// extractPartialObject attempts to extract a complete JSON object from partial data.
func extractPartialObject(data string) (map[string]any, bool) {
	// Find the first complete object
	start := -1
	depth := 0
	inString := false
	escape := false

	for i, r := range data {
		if escape {
			escape = false
			continue
		}
		if r == '\\' && inString {
			escape = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		switch r {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				// Found a complete object
				objStr := data[start : i+1]
				var result map[string]any
				if err := json.Unmarshal([]byte(objStr), &result); err == nil {
					return result, true
				}
			}
		}
	}

	return nil, false
}

// IsValidUTF8 checks if a string contains valid UTF-8.
func IsValidUTF8(s string) bool {
	return utf8.ValidString(s)
}

// HeadersToMap converts HTTP headers to a single-value string map, matching the
// exported TS helper surface used for provider response metadata.
func HeadersToMap(headers http.Header) map[string]string {
	result := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			result[key] = ""
			continue
		}
		result[key] = values[0]
	}

	return result
}
