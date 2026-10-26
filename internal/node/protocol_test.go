package node

import (
	"encoding/json"
	"testing"
)

func TestReverseMsg_ProtocolVersionOmitEmpty(t *testing.T) {
	// Zero value must be omitted from JSON so older peers and older servers
	// receive a byte-identical payload to pre-field-added releases.
	msg := ReverseMsg{Type: "register", NodeID: "n1"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["protocol_version"]; ok {
		t.Errorf("protocol_version must be omitted when zero, got %s", data)
	}

	// Non-zero must round-trip intact.
	msg.ProtocolVersion = 1
	data, err = json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got ReverseMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion round-trip = %d, want 1", got.ProtocolVersion)
	}
}

func TestReverseMsg_CapabilitiesOmitEmpty(t *testing.T) {
	// nil slice: omitted.
	msg := ReverseMsg{Type: "register", NodeID: "n1"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["capabilities"]; ok {
		t.Errorf("capabilities must be omitted when nil, got %s", data)
	}

	// Empty (non-nil) slice: omitempty drops it too for slices with len==0.
	msg.Capabilities = []string{}
	data, err = json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["capabilities"]; ok {
		t.Errorf("capabilities must be omitted when empty, got %s", data)
	}

	// Populated: round-trips with stable ordering.
	msg.Capabilities = []string{"gemini", "acp", "askuser"}
	data, err = json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got ReverseMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 3 || got.Capabilities[0] != "gemini" {
		t.Errorf("Capabilities round-trip = %v, want [gemini acp askuser]", got.Capabilities)
	}
}

func TestServerMsgOmitsEmptyFields(t *testing.T) {
	msg := ServerMsg{Type: "auth_ok"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"key", "event", "events", "id", "status", "state", "reason", "error", "node"} {
		if _, ok := m[field]; ok {
			t.Errorf("field %q should be omitted when empty, but was present in %s", field, data)
		}
	}
}
