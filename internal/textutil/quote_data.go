package textutil

import "strings"

// QuoteData is s, a session's text the gateway puts in front of the
// Planner, in 「」 with its angle and corner brackets swapped for
// look-alikes: it can neither close the <naozhi-receipt> or
// <naozhi-snapshot> it rides in nor end the quote early, and the Planner's
// prompt reads a quote as data.
func QuoteData(s string) string { return "「" + dataQuoter.Replace(s) + "」" }

var dataQuoter = strings.NewReplacer("<", "‹", ">", "›", "「", "『", "」", "』")
