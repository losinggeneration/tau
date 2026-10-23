package ai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotBaseURL(t *testing.T) {
	assert.Equal(t, "https://api.business.githubcopilot.com", CopilotBaseURL("tid=1;proxy-ep=proxy.business.githubcopilot.com;exp=2"))
	assert.Equal(t, "https://copilot.example.com", CopilotBaseURL("proxy-ep=copilot.example.com"))
	assert.Empty(t, CopilotBaseURL("tid=1;exp=2"))
	assert.Empty(t, CopilotBaseURL("proxy-ep=;exp=2"))
}

func TestCopilotRequestsUseTokenEndpointAndDynamicHeaders(t *testing.T) {
	const token = "tid=1;proxy-ep=proxy.business.githubcopilot.com;exp=2"
	for _, api := range []string{APIOpenAICompletions, APIOpenAIResponses, APIAnthropicMessages} {
		t.Run(api, func(t *testing.T) {
			var request *http.Request
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				request = req
				return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"stop"}`))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })

			model := &Model{ID: "m", API: api, Provider: ProviderGitHubCopilot, BaseURL: "https://api.individual.githubcopilot.com", ContextWindow: 100000, MaxTokens: 1000}
			client, err := GetClient(model)
			require.NoError(t, err)
			prompt := &Context{Messages: []Message{
				{Role: "user", Content: []ContentBlock{{Type: ContentBlockImage, Data: "aGk=", MimeType: "image/png"}}},
				{Role: "assistant", Content: "ok"},
				{Role: "toolResult", Content: "done"},
			}}
			err = client.Stream(t.Context(), model, prompt, func(context.Context, StreamEvent) {}, WithAPIKey(token), WithHeaders(map[string]string{"Openai-Intent": "caller"}))
			require.Error(t, err)
			require.NotNil(t, request, "no request sent")
			assert.Equal(t, "api.business.githubcopilot.com", request.URL.Host, "the token's proxy endpoint was ignored")
			assert.Equal(t, "agent", request.Header.Get("X-Initiator"))
			assert.Equal(t, "true", request.Header.Get("Copilot-Vision-Request"))
			assert.Equal(t, "caller", request.Header.Get("Openai-Intent"), "caller headers must win")
		})
	}
}
