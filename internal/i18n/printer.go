package i18n

import (
	"fmt"
	"strings"
)

// stringify converts an arg value to its display string. Strings pass through
// unchanged; everything else uses fmt's default formatting.
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// compiledTemplate is a pre-scanned message template. It splits a string into
// alternating literal and placeholder segments so rendering is allocation-light
// and never re-parses at T() time.
type compiledTemplate struct {
	// segments alternate literal/placeholder per the isArg flags.
	segments []segment
}

type segment struct {
	text  string // literal text, or placeholder name when isArg
	isArg bool
}

// compile scans {name} tokens. A "{" with no matching "}" (or an empty name
// "{}") is treated as literal text, preserving the raw characters.
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
			// No closing brace: rest is literal.
			ct.segments = append(ct.segments, segment{text: tmpl[i:]})
			break
		}
		close += open
		name := tmpl[open+1 : close]
		if name == "" || strings.ContainsAny(name, "{") {
			// Empty or malformed placeholder: emit "{" as literal and continue
			// scanning after it so nested/odd braces don't swallow content.
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

// render fills placeholders from args. A missing arg leaves the literal
// "{name}" in place (passthrough). Extra args are ignored.
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

// Printer is locale-bound. It holds a *Bundle pointer (not map refs) so a
// future atomic.Pointer Reload can redirect lookups (NNH1).
type Printer struct {
	locale string
	bundle *Bundle
}

// T renders a key with named args. Behavior:
//   - unknown key (or unknown locale) → "[" + key + "]"
//   - missing arg in map → placeholder "{name}" left unchanged
//   - extra args ignored
//
// At most one args map is consulted (the first). Passing no map renders the
// template with no substitutions, leaving any placeholders literal.
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
