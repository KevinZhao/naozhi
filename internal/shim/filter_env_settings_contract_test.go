package shim

import "testing"

// TestFilterShimEnv_SettingsOwnedKeysNotForwarded locks in the contract written
// on shimEnvAllowedPrefixes: functional Claude knobs that live in
// ~/.claude/settings.json must NOT be forwarded through the shim env allowlist.
//
// The spawned claude reads settings.json itself via `--setting-sources user`,
// and a settings.json `env` value wins over the inherited process env, so these
// keys already reach the CLI identically to the TUI claude. Forwarding them here
// would be redundant and would re-widen the secret-leak surface the allowlist
// bounds (the CLI's Bash tool can `env | grep` whatever we pass through).
//
// If a future edit adds one of these to shimEnvAllowedPrefixes to "make a
// setting take effect", this test fails and points the author back to
// settings.json as the single source of truth.
func TestFilterShimEnv_SettingsOwnedKeysNotForwarded(t *testing.T) {
	// Keys that belong to ~/.claude/settings.json's `env` block and must be
	// resolved by the CLI's own --setting-sources read, not the shim allowlist.
	settingsOwned := []string{
		"API_TIMEOUT_MS=1200000",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=128000",
		"ENABLE_PROMPT_CACHING_1H=1",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"CLAUDE_STREAM_IDLE_TIMEOUT_MS=900000",
		"CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS=900000",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=global.anthropic.claude-opus-4-8",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=global.anthropic.claude-sonnet-4-6",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=global.anthropic.claude-haiku-4-5",
		"ECC_HOOK_PROFILE=minimal",
	}

	for _, kv := range settingsOwned {
		if got := filterShimEnv([]string{kv}); len(got) != 0 {
			t.Errorf("filterShimEnv(%q) forwarded %v; settings.json-owned keys must NOT be in the shim allowlist (see shimEnvAllowedPrefixes contract — settings.json is the single source of truth)", kv, got)
		}
	}
}

// TestFilterShimEnv_PlumbingStillForwarded is the positive counterpart: the
// system/toolchain plumbing and raw Bedrock credentials that settings.json does
// NOT carry must keep flowing through, otherwise the spawned CLI loses its
// runtime essentials and AWS auth.
func TestFilterShimEnv_PlumbingStillForwarded(t *testing.T) {
	mustForward := []string{
		"PATH=/usr/bin",
		"HOME=/home/ec2-user",
		"AWS_REGION=us-west-2",
		"AWS_ACCESS_KEY_ID=AKIAEXAMPLE",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"ANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:8889",
		// Subscription (Pro/Max) OAuth token from `claude setup-token`. Like
		// ANTHROPIC_AUTH_TOKEN / AWS creds it is a credential the CLI cannot
		// obtain headlessly on its own — NOT a settings.json-owned knob — so a
		// secret must never be written to the plaintext settings.json env block.
		// It is injected via an access profile's *_FILE reference and MUST flow
		// through the shim allowlist to reach the CLI.
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-xxxx",
	}

	for _, kv := range mustForward {
		if got := filterShimEnv([]string{kv}); len(got) != 1 || got[0] != kv {
			t.Errorf("filterShimEnv(%q) = %v; required plumbing/credential var must be forwarded", kv, got)
		}
	}
}
