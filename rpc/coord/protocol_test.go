package coord

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFrameUsesSnakeCaseTags(t *testing.T) {
	f := Frame{
		Type:       TypeEvent,
		InstanceID: "i",
		SessionID:  "s",
		ClientID:   "c",
		Instance:   &Instance{ID: "i", NodeID: "n", PID: 7},
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"type":"event"`, `"instance_id":"i"`, `"session_id":"s"`, `"client_id":"c"`, `"node_id":"n"`, `"pid":7`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("encoded frame %s missing %s", data, want)
		}
	}
	if strings.Contains(string(data), `"token"`) || strings.Contains(string(data), `"version"`) {
		t.Errorf("encoded frame %s includes empty fields", data)
	}
	got, err := decodeFrame(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InstanceID != "i" || got.Instance == nil || got.Instance.NodeID != "n" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestDecodeFrameRejects(t *testing.T) {
	long := strings.Repeat("x", maxIDBytes+1)
	tests := map[string]string{
		"unknown field":     `{"type":"hello","admin":true}`,
		"unknown type":      `{"type":"launch_missiles"}`,
		"missing type":      `{"id":"1"}`,
		"trailing data":     `{"type":"hello"}{"type":"hello"}`,
		"not json":          `hello`,
		"long id":           `{"type":"command","id":"` + long + `"}`,
		"long token":        `{"type":"hello","token":"` + long + `"}`,
		"long cwd":          `{"type":"start_instance","cwd":"` + strings.Repeat("x", maxTextBytes+1) + `"}`,
		"long instance id":  `{"type":"hello","instance":{"instance_id":"` + long + `"}}`,
		"unknown inst key":  `{"type":"hello","instance":{"instance_id":"a","secret":1}}`,
		"long error":        `{"type":"response","error":{"code":"` + long + `"}}`,
		"wrong field types": `{"type":"hello","version":"1"}`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeFrame([]byte(line)); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("decodeFrame(%.60s) error = %v, want ErrInvalidFrame", line, err)
			}
		})
	}
}

func TestErrorMatchesWithErrorsAs(t *testing.T) {
	err := error(&Error{Code: CodeUnauthorized, Message: "nope"})
	var protoErr *Error
	if !errors.As(err, &protoErr) || protoErr.Code != CodeUnauthorized {
		t.Fatalf("errors.As = %v", protoErr)
	}
	if err.Error() != "unauthorized: nope" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
