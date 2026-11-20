// Package client provides a Go client for the coordinated tau protocol.
// Connect to a local coordinator socket with NewCoordinatedClient, or to the
// authenticated WebSocket bridge with NewCoordinatedWebSocketClient.
package client // import "github.com/losinggeneration/tau/rpc/client"

import "encoding/json"

// Command is a tau command routed to one instance.
type Command struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Args any    `json:"args,omitempty"`
}

// Response is an instance's reply to a Command.
type Response struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result,omitempty"`
	Error  *ErrorInfo     `json:"error,omitempty"`
}

// ErrorInfo contains error details for failed responses.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event is a decoded instance event, such as the raw event carried by a
// coordinated event frame.
type Event struct {
	Type      string         `json:"type"`
	SessionID string         `json:"session_id,omitempty"`
	Seq       int64          `json:"seq,omitempty"`
	TS        string         `json:"ts,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	// Typed payload fields for fidelity preservation
	Delta      string `json:"delta,omitempty"`        // text_delta, thinking_delta
	MessageID  string `json:"message_id,omitempty"`   // message metadata
	ToolCallID string `json:"tool_call_id,omitempty"` // tool_call, tool_result
	Tokens     int64  `json:"tokens,omitempty"`       // usage events
	Level      string `json:"level,omitempty"`        // thinking_level_change
}

// FromJSON parses an event from raw JSON bytes, extracting flattened
// payload fields into the typed struct while preserving the full payload map.
func (e *Event) FromJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if v, ok := raw["type"].(string); ok {
		e.Type = v
	}
	if v, ok := raw["session_id"].(string); ok {
		e.SessionID = v
	}
	if v, ok := raw["seq"].(float64); ok {
		e.Seq = int64(v)
	}
	if v, ok := raw["ts"].(string); ok {
		e.TS = v
	}

	if v, ok := raw["delta"].(string); ok {
		e.Delta = v
	}
	if v, ok := raw["message_id"].(string); ok {
		e.MessageID = v
	}
	if v, ok := raw["tool_call_id"].(string); ok {
		e.ToolCallID = v
	}
	if v, ok := raw["tokens"].(float64); ok {
		e.Tokens = int64(v)
	}
	if v, ok := raw["level"].(string); ok {
		e.Level = v
	}

	// Build payload map excluding top-level envelope fields
	payload := make(map[string]any)
	skipKeys := map[string]bool{
		"type": true, "session_id": true, "seq": true, "ts": true,
	}
	for k, v := range raw {
		if !skipKeys[k] {
			payload[k] = v
		}
	}
	if len(payload) > 0 {
		e.Payload = payload
	}

	return nil
}
