package cli

import "testing"

func TestProtocolCaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		proto Protocol
		want  Caps
	}{
		// EffortTier: the Claude CLI takes --effort as of 2.1.226; BuildArgs
		// forwards SpawnOptions.Effort there.
		{&ClaudeProtocol{}, Caps{Replay: true, EffortTier: true}},
		// EffortTier: kiro-cli acp takes --effort; see kiro-effort-control.md.
		{&ACPProtocol{}, Caps{EffortTier: true}},
		// A protocol without Capabilities() derives Replay from SupportsReplay.
		{writeMessageFailingProtocol{}, Caps{}},
	} {
		if got := ProtocolCaps(tc.proto); got != tc.want {
			t.Errorf("%s caps: got %+v want %+v", tc.proto.Name(), got, tc.want)
		}
	}
}
