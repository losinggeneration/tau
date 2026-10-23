package ai

import (
	"slices"
	"strings"
)

// InferCopilotInitiator returns "user" when the last message is a user
// message, otherwise "agent".
func InferCopilotInitiator(messages []Message) string {
	if len(messages) == 0 {
		return "user"
	}
	last := messages[len(messages)-1]
	if last.Role != "user" {
		return "agent"
	}
	return "user"
}

// HasCopilotVisionInput reports whether the conversation includes image input
// that requires Copilot's vision request header.
func HasCopilotVisionInput(messages []Message) bool {
	for _, msg := range messages {
		switch normalizeMessageRole(msg.Role) {
		case "user", "toolResult":
			for _, block := range msg.ContentBlocks() {
				if block.Type == ContentBlockImage {
					return true
				}
			}
		}
	}
	return false
}

// CopilotDynamicHeaderParams mirrors Pi's buildCopilotDynamicHeaders parameter
// object.
type CopilotDynamicHeaderParams struct {
	Messages  []Message
	HasImages bool
}

// BuildCopilotDynamicHeaders builds the dynamic Copilot headers used by the
// OpenAI-compatible path. It preserves the older Go signature and infers the
// image flag from messages.
func BuildCopilotDynamicHeaders(messages []Message) map[string]string {
	return BuildCopilotDynamicHeadersWithParams(CopilotDynamicHeaderParams{
		Messages:  messages,
		HasImages: HasCopilotVisionInput(messages),
	})
}

// BuildCopilotDynamicHeadersWithParams builds dynamic Copilot headers from the
// same parameter shape Pi uses: { messages, hasImages }.
func BuildCopilotDynamicHeadersWithParams(params CopilotDynamicHeaderParams) map[string]string {
	headers := map[string]string{
		"X-Initiator":   InferCopilotInitiator(params.Messages),
		"Openai-Intent": "conversation-edits",
	}
	if params.HasImages {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}

// CopilotBaseURL returns the API endpoint a GitHub Copilot token names in its
// proxy-ep field, or "" when the token names none.
func CopilotBaseURL(token string) string {
	_, rest, ok := strings.Cut(token, "proxy-ep=")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, ";")
	if host == "" {
		return ""
	}
	if after, ok := strings.CutPrefix(host, "proxy."); ok {
		host = "api." + after
	}
	return "https://" + host
}

// requestBaseURL returns the endpoint a request's credential selects, or ""
// to keep the model's. GitHub Copilot tokens name their account's endpoint.
func requestBaseURL(model *Model, apiKey string) string {
	if model == nil || model.Provider != ProviderGitHubCopilot {
		return ""
	}
	return CopilotBaseURL(apiKey)
}

// withCopilotHeaders adds the dynamic GitHub Copilot headers for prompt ahead
// of the caller's headers, which still win.
func withCopilotHeaders(model *Model, prompt *Context, opts []Option) []Option {
	if model == nil || model.Provider != ProviderGitHubCopilot || prompt == nil {
		return opts
	}
	headers := BuildCopilotDynamicHeaders(prompt.Messages)
	copyConfiguredHeaders(headers, applyOptions(opts).headers)
	return append(slices.Clone(opts), WithHeaders(headers))
}
