package ai

import "testing"

func TestIsContextOverflow_Anthropic(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "Anthropic token overflow",
			stopReason:    "error",
			errorMessage:  "prompt is too long: 213462 tokens > 200000 maximum",
			usage:         OverflowUsage{Input: 213462},
			contextWindow: 0,
			want:          true,
		},
		{
			name:          "Anthropic request too large",
			stopReason:    "error",
			errorMessage:  `413 {"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`,
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_OpenAI(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "OpenAI context window exceeded",
			stopReason:    "error",
			errorMessage:  "Your input exceeds the context window of this model",
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_Google(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "Google token count exceeded",
			stopReason:    "error",
			errorMessage:  "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575)",
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_Groq(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "Groq reduce message length",
			stopReason:    "error",
			errorMessage:  "Please reduce the length of the messages or completion",
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_NonOverflow(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "Rate limit error should not be overflow",
			stopReason:    "error",
			errorMessage:  "rate limit exceeded, please try again later",
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          false,
		},
		{
			name:          "Throttling error should not be overflow",
			stopReason:    "error",
			errorMessage:  "Throttling error: Too many tokens, please wait before trying again",
			usage:         OverflowUsage{},
			contextWindow: 0,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflow_SilentOverflow(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		errorMessage  string
		usage         OverflowUsage
		contextWindow int
		want          bool
	}{
		{
			name:          "Silent overflow (z.ai style)",
			stopReason:    "stop",
			errorMessage:  "",
			usage:         OverflowUsage{Input: 200000, CacheHit: 0},
			contextWindow: 128000,
			want:          true,
		},
		{
			name:          "Silent overflow with cache hits",
			stopReason:    "stop",
			errorMessage:  "",
			usage:         OverflowUsage{Input: 100000, CacheHit: 50000},
			contextWindow: 128000,
			want:          true,
		},
		{
			name:          "No overflow within context",
			stopReason:    "stop",
			errorMessage:  "",
			usage:         OverflowUsage{Input: 50000},
			contextWindow: 128000,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsContextOverflow(tt.stopReason, tt.errorMessage, tt.usage, tt.contextWindow)
			if got != tt.want {
				t.Errorf("IsContextOverflow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetOverflowPatterns(t *testing.T) {
	patterns := getOverflowPatterns()
	if len(patterns) == 0 {
		t.Error("getOverflowPatterns() returned empty slice")
	}

	for i, p := range patterns {
		if p == nil {
			t.Errorf("getOverflowPatterns()[%d] is nil", i)
		}
	}
}

func TestLengthStopOverflowAndRecovery(t *testing.T) {
	if !IsContextOverflow("length", "", OverflowUsage{Input: 1000, CacheHit: 197_000}, 200_000) {
		t.Fatal("a zero-output length stop that fills the window is an overflow")
	}
	if IsContextOverflow("length", "", OverflowUsage{Input: 100}, 200_000) {
		t.Fatal("a zero-output length stop far below the window is not an overflow")
	}
	if IsContextOverflow("length", "", OverflowUsage{Input: 199_000, Output: 4096}, 200_000) {
		t.Fatal("a length stop with output is not an overflow")
	}
	if !IsRecoverableLength("length", 16, 128_000) || !IsRecoverableLength("length", 0, 128_000) {
		t.Fatal("a length stop below the desired output limit is recoverable")
	}
	if IsRecoverableLength("length", 1024, 1024) || IsRecoverableLength("stop", 0, 1024) || IsRecoverableLength("length", 0, 0) {
		t.Fatal("only length stops below a known limit are recoverable")
	}
}
