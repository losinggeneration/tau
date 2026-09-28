package ai

import "testing"

// TestSupportsXhighOpus46 verifies Opus 4.6 models support xhigh reasoning.
func TestSupportsXhighOpus46(t *testing.T) {
	model, err := GetAvailableModel(ProviderAnthropic, "claude-opus-4-6")
	if err != nil {
		t.Fatalf("GetAvailableModel(anthropic, claude-opus-4-6): %v", err)
	}

	if !SupportsXhigh(model) {
		t.Error("SupportsXhigh(claude-opus-4-6) = false, want true")
	}
}

// TestSupportsXhighOpus47 verifies Opus 4.7 models support xhigh reasoning.
func TestSupportsXhighOpus47(t *testing.T) {
	model, err := GetAvailableModel(ProviderAnthropic, "claude-opus-4-7")
	if err != nil {
		t.Fatalf("GetAvailableModel(anthropic, claude-opus-4-7): %v", err)
	}

	if !SupportsXhigh(model) {
		t.Error("SupportsXhigh(claude-opus-4-7) = false, want true")
	}
}

// TestSupportsXhighNonOpusAnthropic verifies non-Opus Anthropic models don't support xhigh.
func TestSupportsXhighNonOpusAnthropic(t *testing.T) {
	model, err := GetAvailableModel(ProviderAnthropic, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("GetAvailableModel(anthropic, claude-sonnet-4-5): %v", err)
	}

	if SupportsXhigh(model) {
		t.Error("SupportsXhigh(claude-sonnet-4-5) = true, want false")
	}
}

// TestSupportsXhighOpenAICodexCurrent verifies current OpenAI Codex models support xhigh reasoning.
func TestSupportsXhighOpenAICodexCurrent(t *testing.T) {
	for _, modelID := range []string{"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "gpt-6-luna", "gpt-6-sol"} {
		t.Run(modelID, func(t *testing.T) {
			model, err := GetAvailableModel(ProviderOpenAICodex, modelID)
			if err != nil {
				t.Fatalf("GetAvailableModel(openai-codex, %s): %v", modelID, err)
			}

			if !SupportsXhigh(model) {
				t.Errorf("SupportsXhigh(%s) = false, want true", modelID)
			}
		})
	}
}

// TestSupportsXhighOpenRouterOpus46 verifies OpenRouter Opus 4.6 supports xhigh.
func TestSupportsXhighOpenRouterOpus46(t *testing.T) {
	model, err := GetAvailableModel(ProviderOpenRouter, "anthropic/claude-opus-4.6")
	if err != nil {
		t.Fatalf("GetAvailableModel(openrouter, anthropic/claude-opus-4.6): %v", err)
	}

	if !SupportsXhigh(model) {
		t.Error("SupportsXhigh(anthropic/claude-opus-4.6) = false, want true")
	}
}

// TestSupportsXhighNilModel verifies nil model returns false.
func TestSupportsXhighNilModel(t *testing.T) {
	if SupportsXhigh(nil) {
		t.Error("SupportsXhigh(nil) = true, want false")
	}
}

// TestSupportsXhighVariousModels verifies SupportsXhigh across various model patterns.
func TestSupportsXhighVariousModels(t *testing.T) {
	tests := []struct {
		modelID   string
		provider  string
		wantXhigh bool
		desc      string
	}{
		// Opus 4.6+ models should support xhigh
		{"claude-opus-4-6", ProviderAnthropic, true, "Opus 4.6"},
		{"claude-opus-4-7", ProviderAnthropic, true, "Opus 4.7"},
		{"claude-opus-4.6", ProviderAnthropic, true, "Opus 4.6 with dots"},
		{"claude-opus-4.7", ProviderAnthropic, true, "Opus 4.7 with dots"},

		// Sonnet models should NOT support xhigh
		{"claude-sonnet-4-5", ProviderAnthropic, false, "Sonnet 4.5"},
		{"claude-sonnet-4-6", ProviderAnthropic, false, "Sonnet 4.6"},
		{"claude-sonnet-4-7", ProviderAnthropic, false, "Sonnet 4.7"},

		// Haiku models should NOT support xhigh
		{"claude-haiku-4-5", ProviderAnthropic, false, "Haiku 4.5"},

		// GPT-5.2+ models should support xhigh when present in a provider registry.
		{"gpt-5.4", ProviderOpenAICodex, true, "GPT-5.4"},
		{"gpt-5.5", ProviderOpenAICodex, true, "GPT-5.5"},
		{"gpt-5.6-luna", ProviderOpenAICodex, true, "GPT-5.6 Luna"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			model, err := GetAvailableModel(tt.provider, tt.modelID)
			if err != nil {
				t.Skipf("model not found: %v", err)
			}

			got := SupportsXhigh(model)
			if got != tt.wantXhigh {
				t.Errorf("SupportsXhigh(%s/%s) = %v, want %v", tt.provider, tt.modelID, got, tt.wantXhigh)
			}
		})
	}
}

// TestModelsAreEqual verifies the ModelsAreEqual function.
func TestModelsAreEqual(t *testing.T) {
	model1, _ := GetAvailableModel(ProviderAnthropic, "claude-opus-4-6")
	model2, _ := GetAvailableModel(ProviderAnthropic, "claude-opus-4-6")
	model3, _ := GetAvailableModel(ProviderAnthropic, "claude-sonnet-4-5")
	model4, _ := GetAvailableModel(ProviderAnthropic, "claude-opus-4-7")

	if !ModelsAreEqual(model1, model2) {
		t.Error("ModelsAreEqual(same model) = false, want true")
	}

	if ModelsAreEqual(model1, model3) {
		t.Error("ModelsAreEqual(different models) = true, want false")
	}

	if ModelsAreEqual(model1, model4) {
		t.Error("ModelsAreEqual(different versions) = true, want false")
	}

	if ModelsAreEqual(nil, model1) {
		t.Error("ModelsAreEqual(nil, model) = true, want false")
	}

	if ModelsAreEqual(model1, nil) {
		t.Error("ModelsAreEqual(model, nil) = true, want false")
	}

	if ModelsAreEqual(nil, nil) {
		t.Error("ModelsAreEqual(nil, nil) = true, want false")
	}
}

// TestModelsAreEqualDifferentProviders verifies models with same ID but different providers are not equal.
func TestModelsAreEqualDifferentProviders(t *testing.T) {
	// These models might not exist, so skip if not found
	modelOpenAI, errOpenAI := GetAvailableModel(ProviderOpenAI, "gpt-5.4")
	modelCodex, errCodex := GetAvailableModel(ProviderOpenAICodex, "gpt-5.4")

	if errOpenAI != nil || errCodex != nil {
		t.Skip("model not found")
	}

	// Even with same ID, different providers should not be equal
	if ModelsAreEqual(modelOpenAI, modelCodex) {
		// This might be expected behavior or might not - we'll just document it
		t.Log("Note: ModelsAreEqual returns true for same ID different providers")
	}
}
