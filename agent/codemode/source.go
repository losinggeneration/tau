package codemode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	optionsPrefix     = "// @options:"
	supportedFields   = "`max_output_tokens` and `timeout_ms`"
	maxSourceTimeout  = 2147483647
	maxSafeInteger    = 1<<53 - 1
	objectOptionsHint = "@options must be a JSON object with supported fields " + supportedFields
)

// SourceOptions are the settings of a `// @options:` first line. Nil fields
// were not given. The caller applies them.
type SourceOptions struct {
	MaxOutputTokens *int64
	TimeoutMs       *int64
}

// SourceError reports invalid script source or options.
type SourceError struct{ Message string }

func (e *SourceError) Error() string { return e.Message }

// ParseSource splits an optional `// @options:` first line from the script.
// The directive line becomes empty so stack line numbers still match.
func ParseSource(input string) (string, SourceOptions, error) {
	if strings.TrimSpace(input) == "" {
		return "", SourceOptions{}, &SourceError{"Expected JavaScript source text (non-empty). Provide JS only, optionally with a first line `// @options: {\"max_output_tokens\": 1000}`."}
	}
	lines := strings.Split(input, "\n")
	for i, line := range lines[1:] {
		if strings.HasPrefix(strings.TrimLeft(line, " \t\r\n\v\f"), optionsPrefix) {
			return "", SourceOptions{}, &SourceError{fmt.Sprintf("The %s line must be the first line of the input; found one on line %d", optionsPrefix, i+2)}
		}
	}
	first := strings.TrimLeft(strings.TrimSuffix(lines[0], "\r"), " \t\v\f")
	if !strings.HasPrefix(first, optionsPrefix) {
		return input, SourceOptions{}, nil
	}
	var code string
	if newline := strings.IndexByte(input, '\n'); newline >= 0 {
		code = input[newline:]
	}
	if strings.TrimSpace(code) == "" {
		return "", SourceOptions{}, &SourceError{"The @options line must be followed by JavaScript source on subsequent lines"}
	}
	options, err := parseOptions(strings.TrimSpace(strings.TrimPrefix(first, optionsPrefix)))
	return code, options, err
}

func parseOptions(directive string) (SourceOptions, error) {
	var options SourceOptions
	if directive == "" {
		return options, &SourceError{objectOptionsHint}
	}
	var raw any
	decoder := json.NewDecoder(strings.NewReader(directive))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil || decoder.More() {
		if err == nil {
			err = errors.New("unexpected data after the JSON value")
		}
		return options, &SourceError{fmt.Sprintf("@options must be valid JSON with supported fields %s: %v", supportedFields, err)}
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return options, &SourceError{objectOptionsHint}
	}
	// Report unknown fields in source order, like upstream's Object.keys.
	for _, key := range objectKeys(directive) {
		if key != "max_output_tokens" && key != "timeout_ms" {
			return options, &SourceError{fmt.Sprintf("@options only supports %s; got `%s`", supportedFields, key)}
		}
	}
	if v, ok := fields["max_output_tokens"]; ok {
		n, ok := safeInteger(v)
		if !ok {
			return options, &SourceError{"@options field `max_output_tokens` must be a non-negative safe integer"}
		}
		options.MaxOutputTokens = &n
	}
	if v, ok := fields["timeout_ms"]; ok {
		n, ok := safeInteger(v)
		if !ok || n == 0 || n > maxSourceTimeout {
			return options, &SourceError{fmt.Sprintf("@options field `timeout_ms` must be a positive integer up to %d", maxSourceTimeout)}
		}
		options.TimeoutMs = &n
	}
	return options, nil
}

func safeInteger(v any) (int64, bool) {
	number, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := number.Float64()
	if err != nil || f < 0 || f > maxSafeInteger || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

func objectKeys(object string) []string {
	decoder := json.NewDecoder(bytes.NewReader([]byte(object)))
	var keys []string
	if _, err := decoder.Token(); err != nil {
		return nil
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return keys
		}
		keys = append(keys, token.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}
