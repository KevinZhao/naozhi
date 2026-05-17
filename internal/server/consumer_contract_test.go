package server

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that *session.Router satisfies HubRouter, the
// *Hub-only consumer subset declared in consumer.go. *Hub embeds the
// concrete *session.Router behind this interface today; the assertion
// catches signature drift at build time so a Router rename breaks the
// build instead of silently breaking structural typing. R222-CR-10.
var _ HubRouter = (*session.Router)(nil)
