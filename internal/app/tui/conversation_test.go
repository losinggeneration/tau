package tui

import "testing"

func TestShellProgressRetainsFragmentedANSI(t *testing.T) {
	conv := NewConversationModel()
	conv.AddToolCallWithID("call", "bash", `{"command":"pwd","exclude_from_context":true}`)
	for _, chunk := range []string{"\x1b[", "31mred", "\x1b", "[0m"} {
		conv.AddToolProgress("call", chunk)
	}
	tool := conv.Snapshot().Components[0].ToolExecution
	if tool.Output != "\x1b[31mred\x1b[0m" || !tool.ExcludeFromContext {
		t.Fatalf("fragmented progress corrupted: %+v", tool)
	}
	conv.addToolResult("call", "bash", "red", false, nil, new(1.0), true)
	tool = conv.Snapshot().Components[0].ToolExecution
	if tool.Output != "red" || !tool.ExcludeFromContext {
		t.Fatalf("final result retained stream fragments: %+v", tool)
	}
}
