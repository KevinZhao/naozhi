package envpolicy

import "testing"

func TestFilterShimEnv_AllowsExpectedPrefixes(t *testing.T) {
	tests := []struct {
		env     string
		allowed bool
	}{
		// Allowed
		{"HOME=/home/user", true},
		{"USER=alice", true},
		{"LOGNAME=alice", true},
		{"PATH=/usr/bin:/bin", true},
		{"SHELL=/bin/bash", true},
		{"TERM=xterm-256color", true},
		{"TMPDIR=/tmp", true},
		{"TMP=/tmp", true},
		{"TEMP=/tmp", true},
		{"LANG=en_US.UTF-8", true},
		{"LC_ALL=C", true},
		{"LC_MESSAGES=en_US.UTF-8", true},
		{"TZ=UTC", true},
		{"XDG_RUNTIME_DIR=/run/user/1000", true},
		{"XDG_CONFIG_HOME=/home/user/.config", true},
		{"ANTHROPIC_API_KEY=sk-abc", true},
		{"ANTHROPIC_AUTH_TOKEN=tok", true},
		{"ANTHROPIC_MODEL=claude-opus", true},
		{"ANTHROPIC_BASE_URL=https://api.example.com", true},
		{"ANTHROPIC_BEDROCK_BASE_URL=https://bedrock.example.com", true},
		{"CLAUDE_CODE_USE_BEDROCK=1", true},
		{"CLAUDE_CODE_SKIP_BEDROCK_AUTH=1", true},
		{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-xxxx", true},
		{"CLAUDE_BIN=/usr/bin/claude", true},
		{"CLAUDE_MODEL=claude-opus", true},
		{"AWS_REGION=us-east-1", true},
		{"AWS_DEFAULT_REGION=us-east-1", true},
		{"AWS_ACCESS_KEY_ID=AKIA...", true},
		{"AWS_SECRET_ACCESS_KEY=...", true},
		{"AWS_SESSION_TOKEN=...", true},
		{"AWS_PROFILE=default", true},
		{"SSH_AUTH_SOCK=/tmp/ssh.sock", true},
		{"GIT_AUTHOR_NAME=Alice", true},
		{"GOPATH=/home/user/go", true},
		{"GOROOT=/usr/local/go", true},
		{"GOBIN=/home/user/go/bin", true},
		{"CARGO_HOME=/home/user/.cargo", true},
		{"RUSTUP_HOME=/home/user/.rustup", true},
		{"NODE_ENV=production", true},
		{"NPM_TOKEN=abc", true},
		{"NPM_CONFIG_REGISTRY=https://registry.npmjs.org", true},
		// Blocked — R112714-ARCH-2: NPM_CONFIG_* can redirect global-root /
		// prefix / cache (RCE-class module-hijack). Only REGISTRY and TOKEN
		// are allowed; everything else must be blocked.
		{"NPM_CONFIG_PREFIX=/tmp/evil", false},
		{"NPM_CONFIG_GLOBALCONFIG=/tmp/evil/.npmrc", false},
		{"NPM_CONFIG_CACHE=/tmp/evil-cache", false},
		{"NPM_LIFECYCLE_EVENT=install", false}, // hook execution context leak
		{"PYTHONDONTWRITEBYTECODE=1", true},
		{"PYTHONUNBUFFERED=1", true},
		{"CONDA_PREFIX=/opt/conda", true},
		{"CONDA_DEFAULT_ENV=base", true},
		{"CONDA_SHLVL=1", true},
		{"JAVA_HOME=/usr/lib/jvm/java-17", true},

		// Blocked — R222-SEC-2: trim Python/conda/nvm code-loading vectors
		// previously allowed by bare "CONDA_" / VIRTUAL_ENV / NVM_DIR /
		// PYTHONPATH / PYTHONHOME entries. See the Table shim column (table.go)
		// for the per-key rationale.
		{"NVM_DIR=/home/user/.nvm", false},
		{"PYTHONPATH=/usr/lib/python3", false},
		{"PYTHONHOME=/usr", false},
		{"VIRTUAL_ENV=/home/user/venv", false},
		{"CONDA_PYTHON_EXE=/opt/conda/bin/python", false},
		{"CONDA_EXE=/opt/conda/bin/conda", false},

		// Blocked — ANTHROPIC_* outside the explicit allowlist (R219-SEC-3).
		// The wildcard "ANTHROPIC_" prefix used to forward any ANTHROPIC_*
		// variable; collapse to the documented CLI/SDK surface so unrelated
		// envs cannot leak into the shim subprocess.
		{"ANTHROPIC_LOG_LEVEL=debug", false},
		{"ANTHROPIC_INTERNAL_DEBUG=1", false},

		// Blocked — generic secrets / unrelated
		{"DATABASE_URL=postgres://...", false},
		{"REDIS_PASSWORD=secret", false},
		{"SECRET_KEY=abc123", false},
		{"SLACK_TOKEN=xoxb-...", false},
		{"GITHUB_TOKEN=ghp_...", false},
		{"OPENAI_API_KEY=sk-...", false},
		{"STRIPE_SECRET=sk_live_...", false},
		{"MYSQL_PASSWORD=root", false},
		{"POSTGRES_PASSWORD=password", false},
		{"FOO=bar", false},
		{"DISPLAY=:0", false},

		// Blocked — Node.js / Python runtime loaders (code injection vectors)
		{"NODE_OPTIONS=--require /tmp/evil.js", false},
		{"NODE_PATH=/usr/lib/node_modules", false}, // module resolution hijack vector
		{"NODE_EXTRA_CA_CERTS=/tmp/fake.pem", false},
		{"NODE_TLS_REJECT_UNAUTHORIZED=0", false},
		{"PYTHONSTARTUP=/tmp/evil.py", false},
		{"PYTHONINSPECT=1", false},
		{"PYTHON=/usr/bin/python3", false}, // bare "PYTHON" no longer allowed
		// Blocked — AWS_* variables outside the explicit Bedrock allow list.
		// R218-SEC-10: wildcard "AWS_" used to forward any AWS_-prefixed
		// variable into the CLI; collapse to the documented Bedrock surface.
		{"AWS_MFA_TOKEN=arbitrary", false},
		{"AWS_VAULT=session-name", false},

		// Blocked — ANTHROPIC_* / CLAUDE_* outside the explicit allow list.
		// R214-SEC-3 / R219-SEC-3: wildcard "ANTHROPIC_" / "CLAUDE_" used to
		// forward any prefix-matching variable into the CLI; collapse to the
		// documented surface so future Anthropic-issued variables don't
		// silently leak into the Bash tool's reach.
		{"ANTHROPIC_LOG=debug", false},
		{"ANTHROPIC_TELEMETRY_TOKEN=tok", false},
		{"CLAUDE_CONFIG_DIR=/tmp/c", false},
		{"CLAUDE_TELEMETRY=1", false},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			result := FilterShimEnv([]string{tc.env})
			got := len(result) == 1
			if got != tc.allowed {
				t.Errorf("FilterShimEnv(%q) included=%v, want %v", tc.env, got, tc.allowed)
			}
		})
	}
}

func TestFilterShimEnv_PreservesOrder(t *testing.T) {
	input := []string{
		"HOME=/home/user",
		"SECRET=blocked",
		"PATH=/bin",
		"REDIS_URL=blocked",
		"ANTHROPIC_API_KEY=allowed",
	}
	got := FilterShimEnv(input)
	want := []string{"HOME=/home/user", "PATH=/bin", "ANTHROPIC_API_KEY=allowed"}

	if len(got) != len(want) {
		t.Fatalf("FilterShimEnv len = %d, want %d; got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FilterShimEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterShimEnv_EmptyInput(t *testing.T) {
	if len(FilterShimEnv(nil)) != 0 {
		t.Error("FilterShimEnv(nil) should return empty")
	}
	if len(FilterShimEnv([]string{})) != 0 {
		t.Error("FilterShimEnv([]) should return empty")
	}
}

func TestFilterShimEnv_AllBlocked(t *testing.T) {
	input := []string{"SECRET=foo", "DATABASE=bar", "MYSQL_PWD=baz"}
	if len(FilterShimEnv(input)) != 0 {
		t.Error("expected all blocked")
	}
}
