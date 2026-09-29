package backend

import "testing"

// The dashboard's type chip for a terminal session comes from the backend
// profile the CLI detected as; an unknown CLI gets the generic label.
func TestTerminalLabelFor(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()
		checkTerminalLabels(t)
	})
}

func checkTerminalLabels(t *testing.T) {
	for _, c := range []struct{ name, entrypoint, want string }{
		{"claude-code", "cli", "Claude CLI"},
		{"claude-code", "claude-vscode", "Claude VS Extension"},
		{"kiro", "", "Kiro CLI"},
		{"codex", "", "Codex CLI"},
		{"cli", "", "CLI"},
		{"", "claude-vscode", "CLI"},
	} {
		if got := TerminalLabelFor(c.name, c.entrypoint); got != c.want {
			t.Errorf("TerminalLabelFor(%q, %q) = %q, want %q", c.name, c.entrypoint, got, c.want)
		}
	}
}
