package ai

import (
	"encoding/json"
	"testing"
)

func TestTranscriptReplaysPromptSectionsAndTools(t *testing.T) {
	context := &Context{SystemPrompt: "base", Tools: []Tool{{Name: "old"}}, Messages: []Message{
		{Role: "system", Content: "extra", Sections: map[string]*string{"a": new("first"), "b": new("second")}, ToolsRemoved: []ToolReference{{Name: "old"}}, ToolsAdded: []Tool{{Name: "new"}}},
		{Role: "system", Sections: map[string]*string{"a": nil}},
		{Role: "user", Content: "hello"},
	}}
	normalized := NormalizeContext(context)
	if normalized.SystemPrompt != "" || len(normalized.Tools) != 0 || len(normalized.Messages) != 4 {
		t.Fatal("shorthand not folded into transcript")
	}
	request := providerContext(normalized)
	if request.SystemPrompt != "base\n\nextra\n\nsecond" || len(request.Tools) != 1 || request.Tools[0].Name != "new" {
		t.Fatalf("request=%+v", request)
	}
	if len(request.Messages) != 1 || request.Messages[0].Role != "user" {
		t.Fatal("provider messages contain system patches")
	}
	if context.Messages[0].Sections["a"] == nil || context.SystemPrompt != "base" {
		t.Fatal("input mutated")
	}
}

func TestValidateContextLimitsCountsHistoricalImages(t *testing.T) {
	model := &Model{InputLimits: &ModelInputLimits{Images: &ModelImageInputLimits{MaxPerMessage: 1, MaxPerRequest: 1}}}
	prompt := &Context{Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentBlockImage}}}, {Role: "toolResult", Content: []ContentBlock{{Type: ContentBlockImage}}}}}
	if err := ValidateContextLimits(model, prompt); err == nil {
		t.Fatal("historical images exceeded request limit")
	}
	prompt.Messages = prompt.Messages[:1]
	if err := ValidateContextLimits(model, prompt); err != nil {
		t.Fatal(err)
	}
	model.InputLimits.MaxRequestBytes = 1
	if err := ValidateContextLimits(model, prompt); err == nil {
		t.Fatal("request byte limit ignored")
	}
}

func TestValidateContextLimitsAfterJSONRoundTrip(t *testing.T) {
	prompt := Context{Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: ContentBlockImage}, {Type: ContentBlockImage}}}}}
	data, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	var restored Context
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	model := &Model{InputLimits: &ModelInputLimits{Images: &ModelImageInputLimits{MaxPerRequest: 1}}}
	if err := ValidateContextLimits(model, &restored); err == nil {
		t.Fatal("JSON content bypassed image limit")
	}
}

func TestCurrentSystemMessageRetainsReplayedBaseline(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "base", Sections: map[string]*string{"policy": new("old")}, ToolsAdded: []Tool{{Name: "old"}}},
		{Role: "user", Content: "hello"},
		{Role: "system", Content: "extra", Sections: map[string]*string{"policy": new("new"), "removed": nil}, ToolsRemoved: []ToolReference{{Name: "old"}}, ToolsAdded: []Tool{{Name: "new"}}},
	}
	baseline := CurrentSystemMessage(messages)
	if baseline == nil || baseline.Content != "base\n\nextra" || len(baseline.Sections) != 1 || *baseline.Sections["policy"] != "new" || len(baseline.ToolsAdded) != 1 || baseline.ToolsAdded[0].Name != "new" {
		t.Fatalf("baseline=%+v", baseline)
	}
	*baseline.Sections["policy"] = "mutated"
	if *messages[2].Sections["policy"] != "new" {
		t.Fatal("baseline aliases source section")
	}
	if CurrentSystemMessage([]Message{{Role: "user", Content: "hello"}}) != nil {
		t.Fatal("ordinary transcript gained a baseline")
	}
}

func TestCurrentSystemMessageRetainsInitialTimestamp(t *testing.T) {
	baseline := CurrentSystemMessage([]Message{{Role: "user", Timestamp: 5}, {Role: "system", Content: "base", Timestamp: 123}, {Role: "system", Content: "later", Timestamp: 456}})
	if baseline == nil || baseline.Timestamp != 123 {
		t.Fatalf("baseline=%+v", baseline)
	}
}

func TestCurrentSystemPromptOmitsEmptySectionText(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "base", Sections: map[string]*string{"empty": new(""), "policy": new("policy")}},
		{Role: "system", Sections: map[string]*string{"policy": new("")}},
	}
	if got := CurrentSystemPrompt(messages); got != "base" {
		t.Fatalf("empty section added prompt separators: %q", got)
	}
	baseline := CurrentSystemMessage(messages)
	if baseline == nil || len(baseline.Sections) != 2 {
		t.Fatalf("empty patches lost from state: %+v", baseline)
	}
}
