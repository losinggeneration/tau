package ai

import (
	"strings"
	"testing"
)

// TestGitHubCopilotModelMetadata verifies Copilot models have correct metadata.
func TestGitHubCopilotModelMetadata(t *testing.T) {
	model, err := GetAvailableModel("github-copilot", "claude-sonnet-4")
	if err != nil {
		t.Fatalf("GetAvailableModel(github-copilot, claude-sonnet-4): %v", err)
	}

	if model.Provider != "github-copilot" {
		t.Errorf("Provider = %q, want github-copilot", model.Provider)
	}

	if model.API != APIAnthropicMessages {
		t.Errorf("API = %q, want %q", model.API, APIAnthropicMessages)
	}

	// Copilot models should have static headers
	if model.Headers == nil {
		t.Error("Headers is nil, want Copilot-specific headers")
	}

	if ua := model.Headers["User-Agent"]; ua == "" || !strings.Contains(ua, "GitHubCopilot") {
		t.Errorf("User-Agent = %q, want GitHubCopilot", ua)
	}

	if cid := model.Headers["Copilot-Integration-Id"]; cid != "vscode-chat" {
		t.Errorf("Copilot-Integration-Id = %q, want vscode-chat", cid)
	}

	// Copilot models use a specific endpoint
	expectedBaseURL := "https://api.individual.githubcopilot.com"
	if model.BaseURL != expectedBaseURL {
		t.Errorf("BaseURL = %q, want %q", model.BaseURL, expectedBaseURL)
	}
}

// TestGitHubCopilotModelLookup verifies we can look up Copilot models.
func TestGitHubCopilotModelLookup(t *testing.T) {
	tests := []struct {
		provider string
		id       string
		wantErr  bool
	}{
		{"github-copilot", "claude-sonnet-4", false},
		{"github-copilot", "claude-opus-4.5", false},
		{"github-copilot", "claude-haiku-4.5", false},
		{"github-copilot", "gpt-5-mini", false},
		{"github-copilot", "nonexistent", true},
	}

	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.id, func(t *testing.T) {
			_, err := GetAvailableModel(tt.provider, tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetAvailableModel(%q, %q) error = %v, wantErr %v", tt.provider, tt.id, err, tt.wantErr)
			}
		})
	}
}

// TestGitHubCopilotModelSupportsXhigh verifies which Copilot models support xhigh reasoning.
func TestGitHubCopilotModelSupportsXhigh(t *testing.T) {
	// Opus 4.6+ models should support xhigh
	model, err := GetAvailableModel("github-copilot", "claude-opus-4-6")
	if err == nil && !SupportsXhigh(model) {
		t.Errorf("claude-opus-4-6 should support xhigh")
	}

	model, err = GetAvailableModel("github-copilot", "claude-opus-4-7")
	if err == nil && !SupportsXhigh(model) {
		t.Errorf("claude-opus-4-7 should support xhigh")
	}

	// Sonnet models should not support xhigh
	model, err = GetAvailableModel("github-copilot", "claude-sonnet-4-5")
	if err == nil && SupportsXhigh(model) {
		t.Errorf("claude-sonnet-4-5 should NOT support xhigh")
	}

	// GPT-5 models should support xhigh
	model, err = GetAvailableModel("github-copilot", "gpt-5.4")
	if err == nil && !SupportsXhigh(model) {
		t.Errorf("gpt-5.4 should support xhigh")
	}
}

// TestGitHubCopilotProviderSupport verifies GitHub Copilot has proper support status.
func TestGitHubCopilotProviderSupport(t *testing.T) {
	status, ok := ProviderSupport("github-copilot")
	if !ok {
		t.Fatal("ProviderSupport(github-copilot) returned false")
	}

	if !status.HasProviderConstant {
		t.Error("HasProviderConstant should be true")
	}

	if !status.HasImplementation {
		t.Error("HasImplementation should be true")
	}

	if !status.IsRegistered {
		t.Error("IsRegistered should be true")
	}

	if !status.ReachableViaGetClient {
		t.Error("ReachableViaGetClient should be true")
	}

	if !status.Tested {
		t.Error("Tested should be true")
	}

	if !status.OfficiallySupported {
		t.Error("OfficiallySupported should be true")
	}
}

// TestGitHubCopilotCanGetClient verifies we can construct a client for Copilot models.
func TestGitHubCopilotCanGetClient(t *testing.T) {
	model, err := GetAvailableModel("github-copilot", "claude-sonnet-4")
	if err != nil {
		t.Fatalf("GetAvailableModel: %v", err)
	}

	client, err := GetClient(model)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}

	if client == nil {
		t.Error("client is nil")
	}
}

// TestGitHubCopilotOpenAIResponses verifies Copilot also supports OpenAI Responses API.
func TestGitHubCopilotOpenAIResponses(t *testing.T) {
	model, err := GetAvailableModel("github-copilot", "gpt-5-mini")
	if err != nil {
		t.Fatalf("GetAvailableModel: %v", err)
	}

	if model.API != APIOpenAIResponses {
		t.Errorf("API = %q, want %q", model.API, APIOpenAIResponses)
	}

	client, err := GetClient(model)
	if err != nil {
		t.Fatalf("GetClient for OpenAI Responses: %v", err)
	}

	if client == nil {
		t.Error("client is nil for OpenAI Responses")
	}
}

// inferCopilotInitiator returns "user" if the last message is from a user, otherwise "agent".
func inferCopilotInitiator(messages []Message) string {
	if len(messages) == 0 {
		return "user"
	}
	last := messages[len(messages)-1]
	if last.Role == "user" {
		return "user"
	}
	return "agent"
}

// hasCopilotVisionInput checks if any message contains image content.
func hasCopilotVisionInput(messages []Message) bool {
	for _, msg := range messages {
		if msg.Role == "user" || msg.Role == "toolResult" {
			if blocks, ok := msg.Content.([]ContentBlock); ok {
				for _, block := range blocks {
					if block.Type == "image" {
						return true
					}
				}
			}
		}
	}
	return false
}

// buildCopilotDynamicHeaders builds the dynamic headers for Copilot.
func buildCopilotDynamicHeaders(messages []Message) map[string]string {
	headers := map[string]string{
		"X-Initiator":   inferCopilotInitiator(messages),
		"Openai-Intent": "conversation-edits",
	}

	if hasCopilotVisionInput(messages) {
		headers["Copilot-Vision-Request"] = "true"
	}

	return headers
}

// TestBuildCopilotDynamicHeaders verifies Copilot dynamic header generation.
func TestBuildCopilotDynamicHeaders(t *testing.T) {
	tests := []struct {
		name       string
		messages   []Message
		wantXInit  string
		wantIntent string
		wantVision bool
	}{
		{
			name: "user last",
			messages: []Message{
				{Role: "assistant", Content: "Hello"},
				{Role: "user", Content: "Hi"},
			},
			wantXInit:  "user",
			wantIntent: "conversation-edits",
			wantVision: false,
		},
		{
			name: "assistant last",
			messages: []Message{
				{Role: "user", Content: "Hi"},
				{Role: "assistant", Content: "Hello"},
			},
			wantXInit:  "agent",
			wantIntent: "conversation-edits",
			wantVision: false,
		},
		{
			name: "with images",
			messages: []Message{
				{Role: "user", Content: []ContentBlock{{Type: "image", Data: "base64"}}},
			},
			wantXInit:  "user",
			wantIntent: "conversation-edits",
			wantVision: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := buildCopilotDynamicHeaders(tt.messages)

			if headers["X-Initiator"] != tt.wantXInit {
				t.Errorf("X-Initiator = %q, want %q", headers["X-Initiator"], tt.wantXInit)
			}

			if headers["Openai-Intent"] != tt.wantIntent {
				t.Errorf("Openai-Intent = %q, want %q", headers["Openai-Intent"], tt.wantIntent)
			}

			_, hasVision := headers["Copilot-Vision-Request"]
			if hasVision != tt.wantVision {
				t.Errorf("Copilot-Vision-Request present = %v, want %v", hasVision, tt.wantVision)
			}
		})
	}
}

// TestInferCopilotInitiator verifies the initiator inference logic.
func TestInferCopilotInitiator(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		want     string
	}{
		{
			name:     "user last",
			messages: []Message{{Role: "user"}},
			want:     "user",
		},
		{
			name:     "assistant last",
			messages: []Message{{Role: "user"}, {Role: "assistant"}},
			want:     "agent",
		},
		{
			name:     "tool result last",
			messages: []Message{{Role: "user"}, {Role: "assistant"}, {Role: "toolResult"}},
			want:     "agent",
		},
		{
			name:     "empty messages",
			messages: []Message{},
			want:     "user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferCopilotInitiator(tt.messages)
			if got != tt.want {
				t.Errorf("inferCopilotInitiator() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHasCopilotVisionInput verifies image detection.
func TestHasCopilotVisionInput(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		want     bool
	}{
		{
			name:     "no images",
			messages: []Message{{Role: "user", Content: "hello"}},
			want:     false,
		},
		{
			name:     "with text only",
			messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}}},
			want:     false,
		},
		{
			name:     "with image",
			messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "image", Data: "base64data"}}}},
			want:     true,
		},
		{
			name:     "tool result with image",
			messages: []Message{{Role: "toolResult", Content: []ContentBlock{{Type: "image", Data: "base64data"}}}},
			want:     true,
		},
		{
			name:     "mixed content",
			messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}, {Type: "image", Data: "base64"}}}},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasCopilotVisionInput(tt.messages)
			if got != tt.want {
				t.Errorf("hasCopilotVisionInput() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGitHubCopilotModelsList verifies we can list all Copilot models.
func TestGitHubCopilotModelsList(t *testing.T) {
	models := ListAvailableModels("github-copilot")
	if len(models) == 0 {
		t.Error("expected at least one Copilot model")
	}

	// Should have both Anthropic and OpenAI Responses models
	hasAnthropic := false
	hasOpenAIResponses := false
	for _, m := range models {
		if m.API == APIAnthropicMessages {
			hasAnthropic = true
		}
		if m.API == APIOpenAIResponses {
			hasOpenAIResponses = true
		}
	}

	if !hasAnthropic {
		t.Error("expected at least one Anthropic model")
	}
	if !hasOpenAIResponses {
		t.Error("expected at least one OpenAI Responses model")
	}
}
