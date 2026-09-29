package envpolicy

// SysessionRunnerAllowlist is the Runner allowlist a sysession daemon's
// `claude -p` gets on top of the Table's sysession column: the Bedrock,
// Anthropic and proxy plumbing a session spawn also receives. An entry ending
// in "_" is a prefix. Each call returns a fresh slice.
//
// It is the only copy: sysession.NewRunner applies it itself, so no caller
// restates it (#2897 C5). A key it admits still faces the Table's shim guard
// (sysession filterEnv), and raw credentials pass only for the detected
// backend (EnvCredsForBackend).
func SysessionRunnerAllowlist() []string {
	return []string{
		"ANTHROPIC_",
		"CLAUDE_",
		"AWS_",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	}
}
