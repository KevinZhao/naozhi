package session

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// Dashboard introspection
	GetState() cli.ProcessState
	TotalCost() float64
	EventEntries() []cli.EventEntry
	ProtocolName() string
}

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	Key string

	// sessionID stores the CLI session ID atomically.
	// Written once during first successful Send, read by Snapshot lock-free.
	sessionID atomic.Value // stores string

	// lastActive stores time.UnixNano atomically to avoid data races
	// between Send() (under sendMu) and Cleanup/evictOldest (under r.mu).
	lastActive atomic.Int64

	process processIface
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
	if s.getSessionID() == "" && result.SessionID != "" {
		s.setSessionID(result.SessionID)
	}
	return result, nil
}

// getSessionID returns the session ID lock-free via atomic.Value.
func (s *ManagedSession) getSessionID() string {
	v := s.sessionID.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// setSessionID stores the session ID atomically.
func (s *ManagedSession) setSessionID(id string) {
	s.sessionID.Store(id)
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return platform + ":" + chatType + ":" + id + ":" + agentID
}

// SessionSnapshot is a point-in-time view of a session for the dashboard API.
type SessionSnapshot struct {
	Key        string  `json:"key"`
	Platform   string  `json:"platform"`
	Agent      string  `json:"agent"`
	SessionID  string  `json:"session_id"`
	State      string  `json:"state"`
	Protocol   string  `json:"protocol"`
	LastActive int64   `json:"last_active"` // unix ms
	TotalCost  float64 `json:"total_cost"`
}

// Snapshot returns a point-in-time view of this session.
func (s *ManagedSession) Snapshot() SessionSnapshot {
	snap := SessionSnapshot{
		Key:        s.Key,
		SessionID:  s.getSessionID(),
		LastActive: s.GetLastActive().UnixMilli(),
	}

	// Parse key: platform:chatType:userId:agentId
	parts := strings.SplitN(s.Key, ":", 4)
	if len(parts) >= 1 {
		snap.Platform = parts[0]
	}
	if len(parts) >= 4 {
		snap.Agent = parts[3]
	}

	if s.process == nil {
		if snap.SessionID != "" {
			snap.State = "suspended"
		} else {
			snap.State = "dead"
		}
	} else {
		snap.State = s.process.GetState().String()
		snap.Protocol = s.process.ProtocolName()
		snap.TotalCost = s.process.TotalCost()
	}
	return snap
}

// EventEntries returns the event log entries for this session.
func (s *ManagedSession) EventEntries() []cli.EventEntry {
	if s.process == nil {
		return nil
	}
	return s.process.EventEntries()
}
