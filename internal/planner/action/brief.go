package action

import "strings"

// Brief is the Planner's own text of a dispatched session's first message:
// what follows the relay envelope a with_user_words dispatch leads it with
// (relayEnvelope), else the whole message. The digest takes the task's goal
// from it (§15.12 ②), not from the user's words the envelope quotes. Only
// the gateway writes the envelope's closing tag (noEnvelope), so the first
// one ends it.
func Brief(message string) string {
	s := strings.TrimSpace(message)
	if !strings.HasPrefix(s, "<"+envelopeTag) {
		return s
	}
	_, after, ok := strings.Cut(s, "</"+envelopeTag+">")
	if !ok {
		return s
	}
	after = strings.TrimSpace(after)
	return strings.TrimSpace(strings.TrimPrefix(after, "Planner 的说明："))
}
