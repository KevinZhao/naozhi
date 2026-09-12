package platform

// capabilities.go — one place that answers "which platform can do what". J10 of
// #2548.
//
// The core Platform interface is five methods; everything beyond it is an
// optional interface a platform may or may not satisfy. That is a good shape for
// the code and a bad one for an operator: the only way to learn that
// AskUserQuestion renders as a native card on Feishu and a plain-text option list
// everywhere else was to read four adapters and grep for type assertions. A
// feature working on one platform and silently degrading on another is not
// visible from the outside at all.
//
// Two kinds of "capable" are deliberately kept apart here, because conflating
// them overstates what a platform does:
//
//   - Interfaces with a bool method (InterimMessageCapable,
//     SingleUseReplyTokenCapable) are asked at runtime. weixin implements
//     InterimMessageCapable and returns FALSE, so a type assertion alone would
//     report interim messages as supported when they are not.
//   - Interfaces that are pure method sets (Reactor, QuestionCardSender,
//     RunnablePlatform) are a type assertion, because implementing them IS the
//     capability.

// Capabilities is the answer for one platform. Field order matches the
// declaration order of the optional interfaces in platform.go.
type Capabilities struct {
	// InterimMessages: can deliver a "thinking…" message before the final reply.
	// Runtime answer (weixin implements the interface and declines).
	InterimMessages bool `json:"interim_messages"`
	// SingleUseReplyToken: only ONE reply per inbound message arrives, so a long
	// reply must be collapsed into one truncated message rather than N chunks
	// (#2136). Runtime answer.
	SingleUseReplyToken bool `json:"single_use_reply_token"`
	// Reactions: can add/remove reactions on an inbound message (the queued
	// marker). Type assertion.
	Reactions bool `json:"reactions"`
	// QuestionCards: renders AskUserQuestion as a native card instead of a
	// plain-text option list. Type assertion.
	QuestionCards bool `json:"question_cards"`
	// Runnable: needs background goroutines started (Start/Stop). Type assertion.
	Runnable bool `json:"runnable"`
	// MaxReplyLength is the platform's per-message rune cap. Reported because it
	// is what decides whether a reply is chunked at all; 0 means unset, which for
	// a configured adapter means "no cap applied".
	MaxReplyLength int `json:"max_reply_length"`
}

// CapabilitiesOf reports what p can do. A nil Platform yields the zero value —
// every capability absent — which is the safe reading for a platform that failed
// to build.
func CapabilitiesOf(p Platform) Capabilities {
	if p == nil {
		return Capabilities{}
	}
	_, reactions := AsCapability[Reactor](p)
	_, cards := AsCapability[QuestionCardSender](p)
	_, runnable := AsCapability[RunnablePlatform](p)
	return Capabilities{
		InterimMessages:     SupportsInterimMessages(p),
		SingleUseReplyToken: UsesSingleUseReplyToken(p),
		Reactions:           reactions,
		QuestionCards:       cards,
		Runnable:            runnable,
		MaxReplyLength:      p.MaxReplyLength(),
	}
}

// CapabilityMatrix reports capabilities for every named platform, keyed by the
// name the registry knows it by (not p.Name(), which an adapter may spell
// differently). Nil entries are kept with an all-false row rather than dropped:
// "this platform is configured and can do nothing" is the signal an operator
// needs.
func CapabilityMatrix(platforms map[string]Platform) map[string]Capabilities {
	if len(platforms) == 0 {
		return nil
	}
	out := make(map[string]Capabilities, len(platforms))
	for name, p := range platforms {
		out[name] = CapabilitiesOf(p)
	}
	return out
}
