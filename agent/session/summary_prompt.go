package session

import (
	"encoding/json"
	"strings"

	"github.com/losinggeneration/tau/ai"
)

func firstPromptFromSummaryMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var envelope struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Role != string(MessageRoleUser) || len(envelope.Content) == 0 {
		return ""
	}

	var textContent string
	if err := json.Unmarshal(envelope.Content, &textContent); err == nil {
		return strings.TrimSpace(textContent)
	}

	var blocks []ai.ContentBlock
	if err := json.Unmarshal(envelope.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type != ai.ContentBlockText || block.Text == "" {
			continue
		}
		b.WriteString(block.Text)
	}
	return strings.TrimSpace(b.String())
}
