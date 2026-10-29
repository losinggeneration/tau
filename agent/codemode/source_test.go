package codemode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSourceOptions(t *testing.T) {
	ptr := func(n int64) *int64 { return &n }
	cases := []struct {
		input   string
		code    string
		options SourceOptions
	}{
		{"text('hi')", "text('hi')", SourceOptions{}},
		{"// just a comment\nreturn 1", "// just a comment\nreturn 1", SourceOptions{}},
		{"// @options: {\"timeout_ms\": 10}\nconst a = 1;\ntext(a)", "\nconst a = 1;\ntext(a)", SourceOptions{TimeoutMs: ptr(10)}},
		{"  // @options:{\"max_output_tokens\":0,\"timeout_ms\":1500}\r\ntext(1)", "\ntext(1)", SourceOptions{MaxOutputTokens: ptr(0), TimeoutMs: ptr(1500)}},
		{"// @options: {}\ntext(1)", "\ntext(1)", SourceOptions{}},
		{"// @optionsx {}\ntext(1)", "// @optionsx {}\ntext(1)", SourceOptions{}},
		{`text("// @options: {}")`, `text("// @options: {}")`, SourceOptions{}},
	}
	for _, c := range cases {
		code, options, err := ParseSource(c.input)
		require.NoError(t, err, c.input)
		assert.Equal(t, c.code, code, c.input)
		assert.Equal(t, c.options, options, c.input)
	}
}

func TestParseSourceRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"// count things\n// @options: {\"timeout_ms\": 1000}\nwhile (true) {}":       "The // @options: line must be the first line of the input; found one on line 2",
		"\n  // @options: {\"timeout_ms\": 1000}\nwhile (true) {}":                    "The // @options: line must be the first line of the input; found one on line 2",
		"text(1)\r\n// @options: {\"timeout_ms\": 1}":                                 "The // @options: line must be the first line of the input; found one on line 2",
		"// @options: {\"timeout_ms\": 1}\ntext(1)\n// @options: {\"timeout_ms\": 2}": "The // @options: line must be the first line of the input; found one on line 3",
		"  \n":                                               "Expected JavaScript source text (non-empty)",
		"// @options:\ntext(1)":                              "@options must be a JSON object with supported fields",
		"// @options: {timeout_ms: 1}\ntext(1)":              "@options must be valid JSON with supported fields",
		"// @options: [1]\ntext(1)":                          "@options must be a JSON object with supported fields",
		"// @options: {\"yield\": 1}\ntext(1)":               "@options only supports `max_output_tokens` and `timeout_ms`; got `yield`",
		"// @options: {\"max_output_tokens\": 1.5}\ntext(1)": "@options field `max_output_tokens` must be a non-negative safe integer",
		"// @options: {\"timeout_ms\": 0}\ntext(1)":          "@options field `timeout_ms` must be a positive integer up to 2147483647",
		"// @options: {\"timeout_ms\": 1}":                   "The @options line must be followed by JavaScript source on subsequent lines",
		"// @options: {\"timeout_ms\": 1}\n  \n":             "The @options line must be followed by JavaScript source on subsequent lines",
	}
	for input, message := range cases {
		_, _, err := ParseSource(input)
		var sourceErr *SourceError
		require.ErrorAs(t, err, &sourceErr, input)
		assert.Contains(t, err.Error(), message, input)
	}
}
