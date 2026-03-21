package session

import (
	"context"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	Key        string
	SessionID  string
	LastActive time.Time

	process *cli.Process
	sendMu  sync.Mutex // serializes messages to the same session
}

// Send delivers a message to the claude process and returns the result.
// Messages to the same session are serialized via sendMu.
func (s *ManagedSession) Send(ctx context.Context, text string, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.LastActive = time.Now()
	result, err := s.process.Send(ctx, text, onEvent)
	if err != nil {
		return nil, err
	}

	// Capture session ID from first successful send
	if s.SessionID == "" && s.process.SessionID != "" {
		s.SessionID = s.process.SessionID
	}
	return result, nil
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return platform + ":" + chatType + ":" + id + ":" + agentID
}
