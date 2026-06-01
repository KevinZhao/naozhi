package server

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that *session.Router satisfies HubRouter, the
// *Hub-only consumer subset declared in consumer.go. *Hub embeds the
// concrete *session.Router behind this interface today; the assertion
// catches signature drift at build time so a Router rename breaks the
// build instead of silently breaking structural typing. R222-CR-10.
var _ HubRouter = (*session.Router)(nil)

// Compile-time assertion that *Hub satisfies HubBroadcaster, the
// Broadcaster facet declared in consumer.go (R237-ARCH-10). This pins the
// broadcast/fan-out surface as a named seam so the eventual ConnPool /
// Broadcaster / SendPath / AgentLinker struct split can carve these
// methods onto a dedicated type without silently dropping or renaming
// one — a signature drift breaks the build here instead of leaving the
// facet contract stale.
var _ HubBroadcaster = (*Hub)(nil)
