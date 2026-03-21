package session

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	Key       string
	SessionID string

	// lastActive stores time.UnixNano atomically to avoid data races
	// between Send() (under sendMu) and Cleanup/evictOldest (under r.mu).
	lastActive atomic.Int64

	process *cli.Process
	sendMu  sync.Mutex // serializes messages to the same session
}

// GetLastActive returns the last active time.
func (s *ManagedSession) GetLastActive() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

// touchLastActive updates the last active timestamp.
func (s *ManagedSession) touchLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

// Send delivers a message to the claude process and returns the result.
// Messages to the same session are serialized via sendMu.
func (s *ManagedSession) Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.touchLastActive()
	result, err := s.process.Send(ctx, text, images, onEvent)
	if err != nil {
		return nil, err
	}

	// Capture session ID from first successful send
	if s.SessionID == "" && s.process.SessionID != "" {
		s.SessionID = s.process.SessionID
	}
	return result, nil
}

// getSessionID returns the session ID safely under sendMu.
func (s *ManagedSession) getSessionID() string {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.SessionID
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return platform + ":" + chatType + ":" + id + ":" + agentID
}
