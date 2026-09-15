package config

import (
	"bytes"
	"os"
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/envpolicy"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// allowEnvExpansion reports whether key is safe to expand in config.yaml; on
// false the caller leaves the placeholder intact so the secret never reaches
// the in-memory Config. Policy lives in the envpolicy Table
// (SourceExpansion): upstream-credential and secret-suffixed names are
// refused, naozhi-owned namespaces are the legitimate config inputs,
// everything else expands.
func allowEnvExpansion(key string) bool {
	_, ok := envpolicy.Allowed(key, envpolicy.SourceExpansion)
	return ok
}

// expandEnvVars resolves ${VAR} placeholders in the YAML payload. Denied names
// (allowEnvExpansion) and values containing control bytes (which could inject
// sibling YAML keys, #637) are left as literal placeholders so the bad config
// fails containsEnvPlaceholder validation loudly instead of leaking or forging.
func expandEnvVars(data []byte) []byte {
	if !bytes.Contains(data, []byte("${")) {
		return data
	}
	return envVarRe.ReplaceAllFunc(data, func(match []byte) []byte {
		key := string(bytes.TrimSuffix(bytes.TrimPrefix(match, []byte("${")), []byte("}")))
		if !allowEnvExpansion(key) {
			return match
		}
		if val, ok := os.LookupEnv(key); ok {
			if containsYAMLBreakingByte(val) {
				return match
			}
			return []byte(val)
		}
		return match
	})
}

// containsYAMLBreakingByte reports whether s has a byte that could break a
// YAML token's scope when substituted raw: newlines are the vector, other
// control bytes and tab (illegal in YAML indentation) are refused defensively.
func containsYAMLBreakingByte(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\n' || b == '\r' || b == '\t' {
			return true
		}
		if b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

func containsEnvPlaceholder(s string) bool {
	return strings.Contains(s, "${")
}
