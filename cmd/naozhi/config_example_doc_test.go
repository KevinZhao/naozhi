package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// naozhiCmdSpan matches a backticked `naozhi <word> [<word>]`, letting the
// span wrap onto the next comment line.
var naozhiCmdSpan = regexp.MustCompile("`naozhi\\s+(?:#\\s*)?([a-z][a-z-]*)(?:\\s+(?:#\\s*)?([a-z][a-z-]*))?")

func readConfigExample(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	return string(data)
}

// TestConfigExample_CommentsNameRealCommands: every `naozhi <cmd>` the
// template tells an operator to run must be a registered subcommand (and a
// real `config` verb), so a renamed or invented command fails here.
func TestConfigExample_CommentsNameRealCommands(t *testing.T) {
	matches := naozhiCmdSpan.FindAllStringSubmatch(readConfigExample(t), -1)
	if len(matches) < 3 {
		t.Fatalf("found %d `naozhi <cmd>` spans; the pattern no longer matches the template", len(matches))
	}
	for _, m := range matches {
		if findSubcmd(m[1]) == nil {
			t.Errorf("config.example.yaml names %q: no such subcommand", m[0])
			continue
		}
		if m[1] == "config" && !strings.Contains(configUsage, "naozhi config "+m[2]+" ") {
			t.Errorf("config.example.yaml names %q: `config` has no verb %q", m[0], m[2])
		}
	}
}

// TestConfigExample_NoFakeFlagsOrAnchors: doctor and install are subcommands,
// there is no log-level flag, and review-finding IDs do not belong in a file
// operators copy.
func TestConfigExample_NoFakeFlagsOrAnchors(t *testing.T) {
	src := readConfigExample(t)
	for _, flag := range []string{"--doctor", "--install", "--log-level"} {
		if strings.Contains(src, flag) {
			t.Errorf("config.example.yaml mentions %s, which naozhi does not accept", flag)
		}
	}
	for _, a := range regexp.MustCompile(`\bR(NEW|\d+)-[A-Z]+-\d+\b`).FindAllString(src, -1) {
		t.Errorf("config.example.yaml carries review anchor %s", a)
	}
}
