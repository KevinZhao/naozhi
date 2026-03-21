package platform

import (
	"context"
	"net/http"
)

// MessageHandler is the callback invoked when a platform receives a message.
type MessageHandler func(ctx context.Context, msg IncomingMessage)

// Image represents an image attachment downloaded by a platform or to be sent.
type Image struct {
	Data     []byte
	MimeType string // e.g., "image/png", "image/jpeg"
}

// IncomingMessage is the platform-agnostic inbound message.
type IncomingMessage struct {
	Platform  string
	EventID   string
	UserID    string
	ChatID    string
	ChatType  string // "direct" | "group"
	Text      string
	MentionMe bool
	Images    []Image
}

// OutgoingMessage is the platform-agnostic outbound message.
type OutgoingMessage struct {
	ChatID   string
	Text     string
	ThreadID string
	Images   []Image
}

// Platform is the interface every IM platform must implement.
type Platform interface {
	Name() string
	RegisterRoutes(mux *http.ServeMux, handler MessageHandler)
	Reply(ctx context.Context, msg OutgoingMessage) (msgID string, err error)
	EditMessage(ctx context.Context, msgID string, text string) error
	MaxReplyLength() int
}

// RunnablePlatform extends Platform for platforms needing background goroutines.
type RunnablePlatform interface {
	Platform
	Start(handler MessageHandler) error
	Stop() error
}
