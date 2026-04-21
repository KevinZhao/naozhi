package project

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// DetectGitHubRemote inspects the project's .git/config and returns the first
// remote URL it finds along with a flag indicating whether the URL points to
// github.com. "origin" takes precedence; otherwise the first remote wins.
//
// Returns ("", false) if the directory is not a git repo, config is missing,
// or no remote is configured.
func DetectGitHubRemote(projectPath string) (url string, isGitHub bool) {
	cfgPath := filepath.Join(projectPath, ".git", "config")
	f, err := os.Open(cfgPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	var currentRemote string
	remotes := make(map[string]string)
	var firstName string

	scanner := bufio.NewScanner(f)
	// Cap line length to guard against pathological config files.
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := line[1 : len(line)-1]
			currentRemote = ""
			// Parse: remote "origin"
			if strings.HasPrefix(section, "remote ") {
				rest := strings.TrimSpace(section[len("remote"):])
				rest = strings.Trim(rest, "\"")
				currentRemote = rest
			}
			continue
		}
		if currentRemote == "" {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			k := strings.TrimSpace(line[:eq])
			v := strings.TrimSpace(line[eq+1:])
			if k == "url" {
				if _, ok := remotes[currentRemote]; !ok {
					if firstName == "" {
						firstName = currentRemote
					}
					remotes[currentRemote] = v
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false
	}

	chosen := remotes["origin"]
	if chosen == "" {
		chosen = remotes[firstName]
	}
	if chosen == "" {
		return "", false
	}
	return chosen, isGitHubURL(chosen)
}

// isGitHubURL returns true if the remote URL's host is github.com.
// Handles the common forms:
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
//   - ssh://git@github.com/owner/repo.git
func isGitHubURL(raw string) bool {
	u := strings.TrimSpace(raw)
	if u == "" {
		return false
	}
	// SCP-like form: user@host:path
	if !strings.Contains(u, "://") {
		if at := strings.IndexByte(u, '@'); at >= 0 {
			rest := u[at+1:]
			if colon := strings.IndexByte(rest, ':'); colon > 0 {
				host := strings.ToLower(rest[:colon])
				return host == "github.com"
			}
		}
		return false
	}
	// URL form: scheme://[user@]host[:port]/path
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			rest = rest[at+1:]
		}
		host := rest
		if slash := strings.IndexByte(host, '/'); slash >= 0 {
			host = host[:slash]
		}
		if colon := strings.IndexByte(host, ':'); colon >= 0 {
			host = host[:colon]
		}
		return strings.ToLower(host) == "github.com"
	}
	return false
}
