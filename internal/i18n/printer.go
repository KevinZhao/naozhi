package i18n

import (
	"fmt"
	"strings"
)

// stringify renders v as a string; strings pass through unchanged.
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// compiledTemplate is a pre-scanned message template split into alternating
// literal and placeholder segments so render never re-parses.
type compiledTemplate struct {
	segments []segment
}

type segment struct {
	text  string // literal text, or placeholder name when isArg
	isArg bool
}

// compile scans {name} tokens; an unmatched "{" or empty name stays literal.
func compile(tmpl string) *compiledTemplate {
	ct := &compiledTemplate{}
	i := 0
	for i < len(tmpl) {
		open := strings.IndexByte(tmpl[i:], '{')
		if open < 0 {
			ct.segments = append(ct.segments, segment{text: tmpl[i:]})
			break
		}
		open += i
		close := strings.IndexByte(tmpl[open:], '}')
		if close < 0 {
			ct.segments = append(ct.segments, segment{text: tmpl[i:]})
			break
		}
		close += open
		name := tmpl[open+1 : close]
		if name == "" || strings.ContainsAny(name, "{") {
			// Malformed placeholder: emit "{" as literal and rescan after it.
			ct.segments = append(ct.segments, segment{text: tmpl[i : open+1]})
			i = open + 1
			continue
		}
		if open > i {
			ct.segments = append(ct.segments, segment{text: tmpl[i:open]})
		}
		ct.segments = append(ct.segments, segment{text: name, isArg: true})
		i = close + 1
	}
	return ct
}

// render fills placeholders from args; a missing arg leaves "{name}" literal.
func (ct *compiledTemplate) render(args map[string]any) string {
	var sb strings.Builder
	for _, seg := range ct.segments {
		if !seg.isArg {
			sb.WriteString(seg.text)
			continue
		}
		if v, ok := args[seg.text]; ok {
			sb.WriteString(stringify(v))
		} else {
			sb.WriteByte('{')
			sb.WriteString(seg.text)
			sb.WriteByte('}')
		}
	}
	return sb.String()
}

// Printer is a locale-bound view of a Bundle.
type Printer struct {
	locale string
	bundle *Bundle
}

// T renders key with named args: unknown key or locale → "[key]"; a missing
// arg leaves "{name}" literal; extra args ignored. Only the first map is used.
func (p *Printer) T(key string, args ...map[string]any) string {
	localeMsgs, ok := p.bundle.msgs[p.locale]
	if !ok {
		return "[" + key + "]"
	}
	tmpl, ok := localeMsgs[key]
	if !ok {
		return "[" + key + "]"
	}
	var m map[string]any
	if len(args) > 0 {
		m = args[0]
	}
	return tmpl.render(m)
}
