package node

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInjectNodeField(t *testing.T) {
	nodeID := "node-1"
	rawNode, _ := json.Marshal(nodeID)
	nodeField := []byte(`"node":` + string(rawNode) + `,`)

	tests := []struct {
		name      string
		input     string
		wantKey   bool   // true if we expect "node" field in result
		wantExact string // if non-empty, exact output assertion
	}{
		{
			name:    "normal object gets node injected",
			input:   `{"type":"event","key":"k1"}`,
			wantKey: true,
		},
		{
			name:      "empty object gets node without trailing comma",
			input:     `{}`,
			wantKey:   true,
			wantExact: `{"node":"node-1"}`,
		},
		{
			name:    "object already has node key is returned as-is",
			input:   `{"node":"other","type":"event"}`,
			wantKey: true,
		},
		{
			name:      "non-object (array) returned as-is",
			input:     `[1,2,3]`,
			wantKey:   false,
			wantExact: `[1,2,3]`,
		},
		{
			name:      "empty bytes returned as-is",
			input:     ``,
			wantKey:   false,
			wantExact: ``,
		},
		{
			name:      "string literal returned as-is",
			input:     `"hello"`,
			wantKey:   false,
			wantExact: `"hello"`,
		},
		{
			name:    "longer object gets node injected at front",
			input:   `{"type":"history","key":"k","events":[]}`,
			wantKey: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := injectNodeField([]byte(tc.input), nodeField)

			if tc.wantExact != "" {
				if string(result) != tc.wantExact {
					t.Fatalf("want %q, got %q", tc.wantExact, result)
				}
				return
			}

			// Verify result is valid JSON.
			var m map[string]json.RawMessage
			if tc.wantKey {
				if err := json.Unmarshal(result, &m); err != nil {
					t.Fatalf("result is not valid JSON: %v — got %q", err, result)
				}
				if _, ok := m["node"]; !ok {
					t.Fatalf("expected 'node' key in result, got %q", result)
				}
			}
		})
	}
}

func TestInjectNodeField_alreadyHasNodeKey(t *testing.T) {
	// Remote message already carries "node":"remote". Must not be overwritten.
	nodeField := []byte(`"node":"injected",`)
	input := []byte(`{"node":"remote","type":"event"}`)
	result := injectNodeField(input, nodeField)
	if !bytes.Equal(result, input) {
		t.Fatalf("should return as-is when 'node' key present, got %q", result)
	}
}

func TestInjectNodeField_nodeKeyInValue_notSkipped(t *testing.T) {
	// "node" only appears as a value, not a key — injection should proceed.
	nodeField := []byte(`"node":"node-1",`)
	input := []byte(`{"type":"event","target":"node"}`)
	result := injectNodeField(input, nodeField)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result invalid JSON: %v — got %q", err, result)
	}
	// We expect "node" key to be injected (input had "node" only as a value,
	// not as `"node":` pattern).
	if _, ok := m["node"]; !ok {
		t.Errorf("expected node key injected, got %q", result)
	}
}

func TestInjectNodeField_windowTruncationGuard(t *testing.T) {
	// Build an object where "node": pattern is beyond 256 bytes.
	// The window guard should NOT see it and injection should still proceed
	// (it will produce duplicate node key — but the guard is conservative:
	// if it appears in first 256 bytes skip, otherwise inject).
	prefix := make([]byte, 300)
	for i := range prefix {
		prefix[i] = 'x'
	}
	input := []byte(`{"padding":"` + string(prefix) + `","type":"event"}`)
	nodeField := []byte(`"node":"n1",`)
	result := injectNodeField(input, nodeField)

	// Result must still be injected since pattern not in first 256 bytes.
	if result[0] != '{' {
		t.Fatalf("result should start with '{', got %q", result[:10])
	}
	if !bytes.Contains(result, []byte(`"node":`)) {
		t.Errorf("expected node field in result, got partial: %q", result[:50])
	}
}
