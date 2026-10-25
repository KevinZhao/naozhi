package cliinfo

import (
	"slices"
	"testing"
)

func TestAllSessionStates(t *testing.T) {
	t.Parallel()
	if got, want := AllSessionStates(), []string{"dead", "ready", "running"}; !slices.Equal(got, want) {
		t.Errorf("AllSessionStates() = %v, want %v", got, want)
	}
}
