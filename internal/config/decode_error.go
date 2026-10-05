package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxDecodeErrorEntries caps how many TypeError entries Load's error lists; a
// garbage file can produce hundreds. The overflow is counted, not dropped.
const maxDecodeErrorEntries = 5

// yaml.v3 TypeError entry shapes recognised by describeDecodeError. Both embed
// operator text (a value prefix, a key) that is dropped from the description.
var (
	decodeLineRe     = regexp.MustCompile(`^line (\d+):`)
	decodeMismatchRe = regexp.MustCompile(`^line (\d+): cannot unmarshal (\S+)`)
	decodeDupKeyRe   = regexp.MustCompile(`^line (\d+): mapping key .* already defined at line (\d+)$`)
	// goTypeRe admits the reflect type strings Config's fields produce
	// ("int", "[]string", "map[string]interface {}", "config.CronConfig").
	goTypeRe = regexp.MustCompile(`^[A-Za-z0-9_.*\[\]{} ]+$`)
)

// standardYAMLTags are the only tags echoed back. Any other tag is operator
// text, possibly from a ${VAR} expansion, and is dropped like a value.
var standardYAMLTags = map[string]bool{
	"!!str": true, "!!int": true, "!!bool": true, "!!float": true, "!!null": true,
	"!!map": true, "!!seq": true, "!!timestamp": true, "!!binary": true,
}

// describeDecodeError summarises a Node.Decode failure without echoing any
// config text. yaml.v3 quotes up to the whole value (or key) in its messages,
// and after ${VAR} expansion that may be a secret, so only line numbers, the
// standard yaml tag and the target Go type survive. Unrecognised entry shapes
// keep only their line number; a non-TypeError keeps nothing.
func describeDecodeError(err error) string {
	var te *yaml.TypeError
	if !errors.As(err, &te) || len(te.Errors) == 0 {
		return "yaml decode error"
	}
	entries := make([]string, 0, min(len(te.Errors), maxDecodeErrorEntries))
	for _, msg := range te.Errors[:min(len(te.Errors), maxDecodeErrorEntries)] {
		entries = append(entries, describeTypeErrorEntry(msg))
	}
	out := "yaml type error: " + strings.Join(entries, "; ")
	if extra := len(te.Errors) - maxDecodeErrorEntries; extra > 0 {
		out += fmt.Sprintf(" (+%d more)", extra)
	}
	return out
}

func describeTypeErrorEntry(msg string) string {
	if m := decodeDupKeyRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("line %s: duplicate key (first defined at line %s)", m[1], m[2])
	}
	if m := decodeMismatchRe.FindStringSubmatch(msg); m != nil {
		// The target type follows the last " into ": the value sits before it
		// in backticks, so a value containing " into " cannot shift the split.
		i := strings.LastIndex(msg, " into ")
		if i >= 0 && goTypeRe.MatchString(msg[i+len(" into "):]) {
			goType := msg[i+len(" into "):]
			if standardYAMLTags[m[2]] {
				return fmt.Sprintf("line %s: cannot unmarshal %s into %s", m[1], m[2], goType)
			}
			return fmt.Sprintf("line %s: cannot unmarshal into %s", m[1], goType)
		}
	}
	if m := decodeLineRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("line %s: invalid value", m[1])
	}
	return "invalid value"
}
