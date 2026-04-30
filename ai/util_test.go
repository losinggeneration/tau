package ai

import (
	"net/http"
	"testing"
)

func TestShortHashParity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: "k4n83c7h0j2b"},
		{input: "abc", want: "y0biex7f9bbh"},
		{input: "tool-call-id", want: "faooch1ovtnme"},
		{input: "🙈", want: "kphsz0153ms3q"},
	}

	for _, tt := range tests {
		if got := ShortHash(tt.input); got != tt.want {
			t.Fatalf("ShortHash(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseJSONStreaming(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKeys  []string
		wantEmpty bool // true if result should be "{}"
	}{
		{
			name:      "complete JSON",
			input:     `{"name": "test", "value": 42}`,
			wantKeys:  []string{"name", "value"},
			wantEmpty: false,
		},
		{
			name:      "empty string",
			input:     "",
			wantKeys:  []string{},
			wantEmpty: true, // pi-mono parseStreamingJson returns {} for empty
		},
		{
			name:      "incomplete JSON object",
			input:     `{"name": "test", "value":`,
			wantKeys:  []string{},
			wantEmpty: true, // partial parsing returns {}
		},
		{
			name:      "incomplete JSON with partial key",
			input:     `{"na`,
			wantKeys:  []string{},
			wantEmpty: true, // partial parsing returns {}
		},
		{
			name:      "nested incomplete",
			input:     `{"user": {"name": "test", "age":`,
			wantKeys:  []string{},
			wantEmpty: true, // partial parsing returns {}
		},
		{
			name:      "array as root is not a map",
			input:     `[1, 2, 3]`,
			wantKeys:  []string{},
			wantEmpty: true, // partial parsing returns {} for non-object root
		},
		{
			name:      "valid nested",
			input:     `{"user": {"name": "test"}}`,
			wantKeys:  []string{"user"},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseStreamingJSON(tt.input)
			if tt.wantEmpty {
				if len(result) != 0 {
					t.Errorf("ParseStreamingJSON(%q) = %#v, want empty object", tt.input, result)
				}
				return
			}
			if result == nil {
				t.Errorf("ParseStreamingJSON(%q) = nil, want non-nil", tt.input)
				return
			}
			if len(result) == 0 {
				t.Errorf("ParseStreamingJSON(%q) = empty object, want non-empty JSON", tt.input)
				return
			}
			if len(tt.wantKeys) > 0 {
				for _, key := range tt.wantKeys {
					if _, ok := result[key]; !ok {
						t.Errorf("ParseStreamingJSON(%q) missing key %q", tt.input, key)
					}
				}
			}
		})
	}
}

func TestHeadersToMap(t *testing.T) {
	headers := http.Header{
		"Content-Type": []string{"application/json"},
		"X-Empty":      nil,
		"X-Multi":      []string{"first", "second"},
	}

	got := HeadersToMap(headers)
	if got["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got["Content-Type"])
	}
	if got["X-Empty"] != "" {
		t.Fatalf("X-Empty = %q, want empty string", got["X-Empty"])
	}
	if got["X-Multi"] != "first" {
		t.Fatalf("X-Multi = %q, want first", got["X-Multi"])
	}
}
