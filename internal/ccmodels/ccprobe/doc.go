// Package ccprobe decides whether each model alias the toolbox recommends is
// actually usable, by invoking it. It holds every impure step ccmodels keeps
// out of its own pure reconciliation logic: minting credentials, calling
// Bedrock, and running cc.
//
// The probe runs in two stages because the two ways a recommended alias can be
// unusable fail at different layers:
//
//   - Stage 1 (Converse) tests the PROFILE. A hand-signed bedrock-runtime
//     Converse call with a one-token budget says whether this caller may invoke
//     it at all. Profiles are deduped, so it costs one call per distinct
//     profile and screens out denied models before any cc turn is spent.
//   - Stage 2 (RunCC) tests the ALIAS REACHING the profile, which raw Bedrock
//     cannot: cc resolves some aliases before consulting modelOverrides, and
//     cc alone knows what "[1m]" means. It runs one real `claude -p` turn per
//     alias against a settings file holding only that alias and NO
//     fallbackModel, so a misresolved alias surfaces as a hard error instead of
//     silently answering from a substitute model.
//
// Stage 2 also reports the context window the turn really ran with, which is
// how a "[1m]" alias that quietly delivers 200k gets noticed.
//
// Every stage fails SOFT: throttling, timeouts, and unavailable credentials all
// return ccmodels.StatusUnknown, which ccmodels treats as keepable. A probe
// outage must never be the reason an operator's model list shrinks.
package ccprobe
