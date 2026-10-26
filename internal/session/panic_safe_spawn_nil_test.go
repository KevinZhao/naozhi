package session

// A nil *cli.Wrapper reaching panicSafeSpawn must fail soft (no nil
// dereference) and classify like the upstream nil-wrapper guard.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestPanicSafeSpawn_NilWrapperFailsSoft(t *testing.T) {
	t.Parallel()

	proc, err := panicSafeSpawn(context.Background(), (*cli.Wrapper)(nil), cli.SpawnOptions{}, "k1", "claude")
	if proc != nil {
		t.Errorf("proc = %v, want nil", proc)
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil for nil wrapper")
	}
	if !strings.Contains(err.Error(), "no CLI wrapper") {
		t.Errorf("err = %q, want substring %q", err.Error(), "no CLI wrapper")
	}
	if !errors.Is(err, ErrNoCLIWrapper) {
		t.Errorf("errors.Is(err, ErrNoCLIWrapper) = false, want true")
	}
}
