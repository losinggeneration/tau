package oauth

import "context"

var (
	anthropicProviderFactory     = NewAnthropicProvider
	githubCopilotProviderFactory = NewGitHubCopilotProvider
	geminiCLIProviderFactory     = NewGeminiCLIProvider
	antigravityProviderFactory   = NewAntigravityProvider
)

var (
	AnthropicOAuthProvider     = anthropicProviderFactory()
	GitHubCopilotOAuthProvider = githubCopilotProviderFactory()
	GeminiCLIOAuthProvider     = geminiCLIProviderFactory()
	AntigravityOAuthProvider   = antigravityProviderFactory()
	OpenAICodexOAuthProvider   = NewOpenAICodexProvider("pi")
)

// LoginAnthropic runs the Anthropic OAuth login flow.
func LoginAnthropic(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return anthropicProviderFactory().Login(ctx, callbacks)
}

// RefreshAnthropicToken refreshes Anthropic OAuth credentials from a refresh token.
func RefreshAnthropicToken(ctx context.Context, refreshToken string) (*Credentials, error) {
	return anthropicProviderFactory().RefreshToken(ctx, &Credentials{Refresh: refreshToken})
}

// LoginGitHubCopilot runs the GitHub Copilot OAuth login flow.
func LoginGitHubCopilot(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return githubCopilotProviderFactory().Login(ctx, callbacks)
}

// RefreshGitHubCopilotToken refreshes GitHub Copilot OAuth credentials from a refresh token.
func RefreshGitHubCopilotToken(ctx context.Context, refreshToken string) (*Credentials, error) {
	return githubCopilotProviderFactory().RefreshToken(ctx, &Credentials{Refresh: refreshToken})
}

// LoginGeminiCLI runs the Gemini CLI OAuth login flow.
func LoginGeminiCLI(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return geminiCLIProviderFactory().Login(ctx, callbacks)
}

// RefreshGoogleCloudToken refreshes Google OAuth credentials from a refresh token.
func RefreshGoogleCloudToken(ctx context.Context, refreshToken string, projectID string) (*Credentials, error) {
	return geminiCLIProviderFactory().RefreshToken(ctx, &Credentials{Refresh: refreshToken, ProjectID: projectID})
}

// LoginAntigravity runs the Google Antigravity OAuth login flow.
func LoginAntigravity(ctx context.Context, callbacks LoginCallbacks) (*Credentials, error) {
	return antigravityProviderFactory().Login(ctx, callbacks)
}

// RefreshAntigravityToken refreshes Google Antigravity OAuth credentials from a refresh token.
func RefreshAntigravityToken(ctx context.Context, refreshToken string) (*Credentials, error) {
	return RefreshAntigravityTokenWithProjectID(ctx, refreshToken, "")
}

// RefreshAntigravityTokenWithProjectID refreshes Google Antigravity OAuth
// credentials and preserves projectID on the returned credentials, matching
// Pi's refreshAntigravityToken(refreshToken, projectId) shape.
func RefreshAntigravityTokenWithProjectID(ctx context.Context, refreshToken string, projectID string) (*Credentials, error) {
	return antigravityProviderFactory().RefreshToken(ctx, &Credentials{Refresh: refreshToken, ProjectID: projectID})
}

// LoginOpenAICodex runs the OpenAI Codex OAuth login flow.
// The originator parameter is passed to the OAuth flow for tracking purposes.
// If empty, defaults to "pi".
func LoginOpenAICodex(ctx context.Context, callbacks LoginCallbacks, originator string) (*Credentials, error) {
	if originator == "" {
		originator = "pi"
	}
	provider := NewOpenAICodexProvider(originator)
	return provider.Login(ctx, callbacks)
}

// RefreshOpenAICodexToken refreshes OpenAI Codex OAuth credentials from a refresh token.
func RefreshOpenAICodexToken(ctx context.Context, refreshToken string) (*Credentials, error) {
	provider := NewOpenAICodexProvider("pi")
	return provider.RefreshToken(ctx, &Credentials{Refresh: refreshToken})
}
