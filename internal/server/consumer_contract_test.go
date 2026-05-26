package server

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that *session.Router satisfies HubRouter, the
// *Hub-only consumer subset declared in consumer.go. *Hub embeds the
// concrete *session.Router behind this interface today; the assertion
// catches signature drift at build time so a Router rename breaks the
// build instead of silently breaking structural typing. R222-CR-10.
var _ HubRouter = (*session.Router)(nil)

// R215-ARCH-P1-4 (#566): mirror compile-time assertions for the
// scratch/send consumer subsets so a future *session.Router signature
// change that quietly drops one of these methods fails the build at
// the dependency boundary instead of producing a runtime panic deep
// inside the handler.
var (
	_ ScratchRouter = (*session.Router)(nil)
	_ SendRouter    = (*session.Router)(nil)
)
