package ai

func init() {
	registerClaudeAgentModels()
}

func registerClaudeAgentModels() {
	// Alias entries map to the Claude Code CLI model aliases.
	for _, model := range []*Model{
		{
			ID:            "haiku",
			Name:          "Claude Haiku through Agent SDK",
			API:           APIClaudeAgent,
			Provider:      ProviderClaudeAgent,
			Reasoning:     true,
			Input:         []InputType{InputText},
			Output:        []OutputType{OutputText},
			ContextWindow: 200000,
			MaxTokens:     8192,
		},
		{
			ID:            "opus",
			Name:          "Claude Opus through Agent SDK",
			API:           APIClaudeAgent,
			Provider:      ProviderClaudeAgent,
			Reasoning:     true,
			Input:         []InputType{InputText},
			Output:        []OutputType{OutputText},
			ContextWindow: 200000,
			MaxTokens:     8192,
		},
		{
			ID:            "sonnet",
			Name:          "Claude Sonnet through Agent SDK",
			API:           APIClaudeAgent,
			Provider:      ProviderClaudeAgent,
			Reasoning:     true,
			Input:         []InputType{InputText},
			Output:        []OutputType{OutputText},
			ContextWindow: 200000,
			MaxTokens:     8192,
		},
	} {
		RegisterModel(model)
	}

	// Mirror the Anthropic registry so exact model IDs (claude-opus-4-6, ...)
	// resolve to the Agent SDK path. Claude Code accepts the same model IDs
	// as the Messages API. Cost is zeroed because subscription usage does not
	// bill per token.
	for _, src := range models {
		if src == nil || src.Provider != ProviderAnthropic {
			continue
		}
		m := *src
		m.Provider = ProviderClaudeAgent
		m.API = APIClaudeAgent
		m.BaseURL = ""
		m.Headers = nil
		m.Compat = nil
		m.Cost = CostProfile{}
		RegisterModel(&m)
	}
}
