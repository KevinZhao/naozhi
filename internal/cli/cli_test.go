package cli

import "testing"

func TestProcessStateString(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  string
	}{
		{StateSpawning, "spawning"},
		{StateReady, "ready"},
		{StateRunning, "running"},
		{StateDead, "dead"},
		{ProcessState(99), "unknown"},
	}

	for _, tt := range tests {
		got := tt.state.String()
		if got != tt.want {
			t.Errorf("ProcessState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestNewUserMessage(t *testing.T) {
	msg := NewUserMessage("hello world")
	if msg.Type != "user" {
		t.Errorf("Type = %q, want %q", msg.Type, "user")
	}
	if msg.Message.Role != "user" {
		t.Errorf("Role = %q, want %q", msg.Message.Role, "user")
	}
	if msg.Message.Content != "hello world" {
		t.Errorf("Content = %q, want %q", msg.Message.Content, "hello world")
	}
}
