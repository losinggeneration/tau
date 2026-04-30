package ai

import "fmt"

// ConvertMessagesForOpenAI exposes the OpenAI-completions message conversion
// path for parity with the TS deep-import helper.
func ConvertMessagesForOpenAI(model *Model, prompt *Context) ([]map[string]any, error) {
	client, err := newOpenAIClient(model)
	if err != nil {
		return nil, err
	}

	transformedCtx, err := TransformContext(prompt, model, nil)
	if err != nil {
		return nil, fmt.Errorf("transform messages: %w", err)
	}

	compat := model.GetCompat()
	msgs, err := client.convertMessages(transformedCtx, model, compat)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, len(msgs))
	for _, msg := range msgs {
		entry := map[string]any{
			"role": msg.Role,
		}
		if msg.Content != "" {
			entry["content"] = msg.Content
		}
		if len(msg.ToolCalls) > 0 {
			entry["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			entry["tool_call_id"] = msg.ToolCallID
		}
		result = append(result, entry)
	}

	return result, nil
}
