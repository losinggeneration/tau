package ai

import (
	"encoding/json"
	"testing"
)

func TestOpenAIBuildRequestIncludesMetadata(t *testing.T) {
	client := &openAIClient{model: &Model{ID: "gpt-4o"}}

	body, err := client.buildRequest(&Model{ID: "gpt-4o"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		metadata: map[string]any{"trace_id": "abc123"},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req openAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata.trace_id = %v, want abc123", req.Metadata["trace_id"])
	}
}

func TestBuildOpenAICompatRequestBodyIncludesMetadata(t *testing.T) {
	body := buildOpenAICompatRequestBody(&Model{ID: "grok-4", Provider: ProviderxAI}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		metadata: map[string]any{"trace_id": "abc123"},
	})

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata type = %T", body["metadata"])
	}
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata.trace_id = %v, want abc123", metadata["trace_id"])
	}
}

func TestBuildCodexRequestBodyIncludesPromptCacheKeyAndMetadata(t *testing.T) {
	body := buildCodexRequestBody(&Model{ID: "gpt-5.3-codex", Provider: ProviderOpenAICodex, API: APIOpenAICodexResponses}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID: "sess_123",
		metadata:  map[string]any{"trace_id": "abc123"},
	})

	if body["prompt_cache_key"] != "sess_123" {
		t.Fatalf("prompt_cache_key = %v, want sess_123", body["prompt_cache_key"])
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata type = %T", body["metadata"])
	}
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata.trace_id = %v, want abc123", metadata["trace_id"])
	}
}

func TestSessionCacheRetentionNoneSuppressesPromptCacheState(t *testing.T) {
	openAIClient := &openAIClient{model: &Model{ID: "gpt-4o"}}
	openAIBody, err := openAIClient.buildRequest(&Model{ID: "gpt-4o"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID:      "sess_123",
		cacheRetention: string(CacheRetentionNone),
	})
	if err != nil {
		t.Fatalf("openai buildRequest: %v", err)
	}

	var openAIReq openAIRequest
	if err := json.Unmarshal([]byte(openAIBody), &openAIReq); err != nil {
		t.Fatalf("unmarshal openai request: %v", err)
	}
	if openAIReq.Store != nil && *openAIReq.Store {
		t.Fatal("openai request store = true, want false when cacheRetention=none")
	}

	openAIResponsesBody, err := buildOpenAIResponsesBody(&Model{ID: "gpt-4o", Provider: ProviderOpenAI, API: APIOpenAIResponses}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID:      "sess_123",
		cacheRetention: string(CacheRetentionNone),
	})
	if err != nil {
		t.Fatalf("openai responses buildOpenAIResponsesBody: %v", err)
	}
	if _, ok := openAIResponsesBody["prompt_cache_key"]; ok {
		t.Fatalf("openai responses prompt_cache_key = %v, want omitted", openAIResponsesBody["prompt_cache_key"])
	}
	if _, ok := openAIResponsesBody["prompt_cache_retention"]; ok {
		t.Fatalf("openai responses prompt_cache_retention = %v, want omitted", openAIResponsesBody["prompt_cache_retention"])
	}

	azureBody := buildAzureRequestBody(&Model{ID: "gpt-4.1"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID:      "sess_123",
		cacheRetention: string(CacheRetentionNone),
	})
	if _, ok := azureBody["prompt_cache_key"]; ok {
		t.Fatalf("azure prompt_cache_key = %v, want omitted", azureBody["prompt_cache_key"])
	}

	codexBody := buildCodexRequestBody(&Model{ID: "gpt-5.3-codex", Provider: ProviderOpenAICodex, API: APIOpenAICodexResponses}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID:      "sess_123",
		cacheRetention: string(CacheRetentionNone),
	})
	if _, ok := codexBody["prompt_cache_key"]; ok {
		t.Fatalf("codex prompt_cache_key = %v, want omitted", codexBody["prompt_cache_key"])
	}
}

func TestBuildAzureRequestBodyIncludesPromptCacheKeyAndMetadata(t *testing.T) {
	body := buildAzureRequestBody(&Model{ID: "gpt-4.1"}, &Context{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, &options{
		sessionID: "sess_123",
		metadata:  map[string]any{"trace_id": "abc123"},
	})

	if body["prompt_cache_key"] != "sess_123" {
		t.Fatalf("prompt_cache_key = %v, want sess_123", body["prompt_cache_key"])
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata type = %T", body["metadata"])
	}
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata.trace_id = %v, want abc123", metadata["trace_id"])
	}
}

func TestConvertAzureResponsesMessageProducesFunctionCallOutput(t *testing.T) {
	converted := convertAzureResponsesMessage(&Model{Input: []InputType{InputText}}, Message{
		Role:       "toolResult",
		ToolCallID: "call_1",
		ToolName:   "read",
		Content: []ContentBlock{
			{Type: ContentBlockText, Text: "output"},
		},
	})

	if len(converted) != 1 {
		t.Fatalf("len(converted) = %d, want 1", len(converted))
	}
	if converted[0]["type"] != "function_call_output" {
		t.Fatalf("type = %v, want function_call_output", converted[0]["type"])
	}
	if converted[0]["call_id"] != "call_1" {
		t.Fatalf("call_id = %v, want call_1", converted[0]["call_id"])
	}
}

func TestOpenAIBuildRequestNormalizesLegacyToolRole(t *testing.T) {
	client := &openAIClient{model: &Model{ID: "gpt-4o"}}

	body, err := client.buildRequest(&Model{ID: "gpt-4o"}, &Context{
		Messages: []Message{{
			Role:       "tool",
			ToolCallID: "call_1",
			ToolName:   "read",
			Content: []ContentBlock{
				{Type: ContentBlockText, Text: "output"},
			},
		}},
	}, &options{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var req openAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if len(req.Messages) != 1 {
		t.Fatalf("len(req.Messages) = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "tool" {
		t.Fatalf("req.Messages[0].Role = %q, want tool", req.Messages[0].Role)
	}
	if req.Messages[0].ToolCallID != "call_1" {
		t.Fatalf("req.Messages[0].ToolCallID = %q, want call_1", req.Messages[0].ToolCallID)
	}
	if req.Messages[0].Content != "output" {
		t.Fatalf("req.Messages[0].Content = %q, want output", req.Messages[0].Content)
	}
}

func TestAnthropicConvertMessagesNormalizesLegacyToolRole(t *testing.T) {
	client := &anthropicClient{}

	msgs, err := client.convertMessages(&Context{
		Messages: []Message{{
			Role:       "tool",
			ToolCallID: "call_1",
			ToolName:   "read",
			Content:    []ContentBlock{{Type: ContentBlockText, Text: "output"}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("msgs[0].Role = %q, want user", msgs[0].Role)
	}
	blocks, ok := msgs[0].Content.([]anthropicToolResultBlock)
	if !ok {
		t.Fatalf("msgs[0].Content type = %T", msgs[0].Content)
	}
	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(blocks))
	}
	if blocks[0].ToolUseID != "call_1" {
		t.Fatalf("blocks[0].ToolUseID = %q, want call_1", blocks[0].ToolUseID)
	}
}

func TestGoogleConvertMessagesNormalizesLegacyToolRole(t *testing.T) {
	client := &googleClient{}

	contents := client.convertMessages(&Context{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hi"}}},
			{Role: "tool", ToolCallID: "t1", ToolName: "read", Content: []ContentBlock{{Type: ContentBlockText, Text: "a"}}},
		},
	})
	if len(contents) != 2 {
		t.Fatalf("len(contents) = %d, want 2", len(contents))
	}
	if contents[1].Role != "user" {
		t.Fatalf("contents[1].Role = %q, want user", contents[1].Role)
	}
	if len(contents[1].Parts) != 1 || contents[1].Parts[0].FunctionResponse == nil {
		t.Fatalf("contents[1].Parts = %#v, want function response", contents[1].Parts)
	}
}

func TestBuildVertexRequestBodyNormalizesLegacyToolRole(t *testing.T) {
	body, _ := buildVertexRequestBody(&Model{ID: "gemini-2.0-flash"}, &Context{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: ContentBlockText, Text: "hi"}}},
			{Role: "tool", ToolCallID: "t1", ToolName: "read", Content: []ContentBlock{{Type: ContentBlockText, Text: "a"}}},
		},
	}, &options{})

	contents, ok := body["contents"].([]map[string]any)
	if !ok {
		t.Fatalf("contents type = %T", body["contents"])
	}
	if len(contents) != 2 {
		t.Fatalf("len(contents) = %d, want 2", len(contents))
	}
	if contents[1]["role"] != "user" {
		t.Fatalf("contents[1].role = %v, want user", contents[1]["role"])
	}
	parts, ok := contents[1]["parts"].([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts = %#v", contents[1]["parts"])
	}
	if _, ok := parts[0]["functionResponse"]; !ok {
		t.Fatalf("parts[0] = %#v, want functionResponse", parts[0])
	}
}

func TestConvertAzureResponsesMessageNormalizesLegacyToolRole(t *testing.T) {
	converted := convertAzureResponsesMessage(&Model{Input: []InputType{InputText}}, Message{
		Role:       "tool",
		ToolCallID: "call_1",
		ToolName:   "read",
		Content:    []ContentBlock{{Type: ContentBlockText, Text: "output"}},
	})

	if len(converted) != 1 {
		t.Fatalf("len(converted) = %d, want 1", len(converted))
	}
	if converted[0]["type"] != "function_call_output" {
		t.Fatalf("type = %v, want function_call_output", converted[0]["type"])
	}
}

func TestConvertMessageForOpenAICompatNormalizesLegacyToolRole(t *testing.T) {
	converted := convertMessageForOpenAICompat(Message{
		Role:       "tool",
		ToolCallID: "call_1",
		ToolName:   "read",
		Content:    []ContentBlock{{Type: ContentBlockText, Text: "output"}},
	})

	if converted["role"] != "tool" {
		t.Fatalf("role = %v, want tool", converted["role"])
	}
	if converted["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", converted["tool_call_id"])
	}
}

func TestConvertMessageForCodexNormalizesLegacyToolRole(t *testing.T) {
	converted := convertMessageForCodex(Message{
		Role:       "tool",
		ToolCallID: "call_1",
		ToolName:   "read",
		Content:    []ContentBlock{{Type: ContentBlockText, Text: "output"}},
	})

	if converted["role"] != "tool" {
		t.Fatalf("role = %v, want tool", converted["role"])
	}
	content, ok := converted["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v", converted["content"])
	}
	if content[0]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", content[0]["tool_call_id"])
	}
}

func TestMistralConvertMessagesNormalizesLegacyToolRole(t *testing.T) {
	client := &mistralClient{}

	messages := client.convertMessages(&Context{
		Messages: []Message{{
			Role:       "tool",
			ToolCallID: "call_1",
			ToolName:   "read",
			Content:    []ContentBlock{{Type: ContentBlockText, Text: "contents"}},
		}},
	})

	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if messages[0]["role"] != "tool" {
		t.Fatalf("role = %v, want tool", messages[0]["role"])
	}
	if messages[0]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v, want call_1", messages[0]["tool_call_id"])
	}
}
