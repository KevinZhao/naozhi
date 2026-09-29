package cliinfo

import "strings"

// MaxAppendSystemPromptBytes caps cli.SpawnOptions.AppendSystemPrompt: the
// worst case (32 KiB agent prompt + 24 KiB scratch context or 8 KiB planner)
// fits with ~2x headroom, below the 128 KiB extra-args budget and ARG_MAX.
const MaxAppendSystemPromptBytes = 64 * 1024

// deniedExtraFlags lists Claude/ACP CLI flags that callers must not inject
// through opts.ExtraArgs, in both bare (`--name value`) and equals
// (`--name=value`) form; when the bare form fires the following element is
// dropped too so the orphaned value does not slide into argv. An allowlist
// would be safer in principle but brittle against legitimate operator flags
// (e.g. `--debug`); the denylist pins the known-dangerous surface. Callers
// needing one of these flags must wire it through a dedicated SpawnOptions
// field that cli.BuildArgs renders explicitly, not the catch-all ExtraArgs slice.
var deniedExtraFlags = map[string]struct{}{
	"--mcp-config":                   {}, // loads attacker-controlled MCP server defs
	"--add-dir":                      {}, // expands file-read sandbox
	"--dangerously-skip-permissions": {}, // BuildArgs already controls this
	"--append-system-prompt":         {}, // SpawnOptions.AppendSystemPrompt owns this site (#2493)
	"--system-prompt":                {}, // hard override of system prompt
	"--setting-sources":              {}, // BuildArgs pins "user" (load ~/.claude/settings.json)
	"--settings":                     {}, // BuildArgs owns SpawnOptions.SettingsFile
	"--resume":                       {}, // BuildArgs owns ResumeID validation
	"--allowed-tools":                {}, // permission allowlist override
	"--disallowed-tools":             {}, // permission allowlist override
	"--model":                        {}, // SpawnOptions.Model owns model selection
	"--effort":                       {}, // SpawnOptions.Effort owns the tier; config validates a closed set
	"--permission-mode":              {}, // SpawnOptions.PermissionMode owns this
	"--permission-prompt-tool":       {}, // permission gate plumbing
	"--output-format":                {}, // BuildArgs pins stream-json; operator override breaks the NDJSON parser
	"--input-format":                 {}, // same protocol-framing concern
	"--verbose":                      {}, // stream-json verbosity is BuildArgs-controlled
	"--replay-user-messages":         {}, // protocol replay flag owned by BuildArgs
}

// IsDeniedFlagName reports whether name (a bare `--name`, no value) is a
// denied flag.
func IsDeniedFlagName(name string) bool {
	_, bad := deniedExtraFlags[name]
	return bad
}

// IsDeniedExtraFlag reports whether a single argv token is a denied flag in
// bare (`--name`) or equals (`--name=value`) form — what cli.BuildArgs strips
// from SpawnOptions.ExtraArgs. The config loader uses it to tell an operator
// at load time, with the field path, instead of the flag vanishing at spawn
// behind a generic warning (#2493).
func IsDeniedExtraFlag(a string) bool {
	if !strings.HasPrefix(a, "--") {
		return false
	}
	if eq := strings.IndexByte(a, '='); eq > 0 {
		return IsDeniedFlagName(a[:eq])
	}
	return IsDeniedFlagName(a)
}
