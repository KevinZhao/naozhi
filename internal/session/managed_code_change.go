package session

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// codeChangeNotifier is the optional hook a process offers for the PRs its
// CLI reports; see bookCodeChanges.
type codeChangeNotifier interface {
	SetOnCodeChange(fn func(clievent.CodeChange))
}

// CodeChanges returns the PRs this session published or touched, newest last.
// The slice is shared and READ-ONLY.
func (s *ManagedSession) CodeChanges() []clievent.CodeChange {
	if p := s.codeChanges.Load(); p != nil {
		return *p
	}
	return nil
}

// setCodeChanges installs list (restore, respawn and rename carry). The caller
// hands over ownership.
func (s *ManagedSession) setCodeChanges(list []clievent.CodeChange) {
	if len(list) == 0 {
		s.codeChanges.Store(nil)
		return
	}
	s.codeChanges.Store(&list)
}

// recordCodeChange merges c into the list and reports whether it changed.
func (s *ManagedSession) recordCodeChange(c clievent.CodeChange) bool {
	for {
		cur := s.codeChanges.Load()
		var list []clievent.CodeChange
		if cur != nil {
			list = *cur
		}
		next, changed := clievent.MergeCodeChange(list, c)
		if !changed {
			return false
		}
		if s.codeChanges.CompareAndSwap(cur, &next) {
			return true
		}
	}
}

// bookCodeChanges records the PRs proc's CLI reports on s and calls changed
// after each one that altered the list. Bound wherever bookUnownedResults is;
// the router's changed persists the list and pushes it to the dashboard.
func bookCodeChanges(s *ManagedSession, proc processIface, changed func()) {
	n, ok := proc.(codeChangeNotifier)
	if !ok {
		return
	}
	n.SetOnCodeChange(func(c clievent.CodeChange) {
		if s.recordCodeChange(c) {
			changed()
		}
	})
}

// markChanged is the transaction a recorded PR runs so the next save writes it.
func markChanged(tx sessTx) { tx.MarkChanged() }

// restoredCodeChanges filters a sessions.json list the way the CLI frame is
// filtered: the file is hand-editable and the URLs become dashboard links.
func restoredCodeChanges(key string, list []clievent.CodeChange) []clievent.CodeChange {
	var out []clievent.CodeChange
	for _, c := range list {
		if !c.Valid() {
			slog.Warn("dropping invalid persisted code change", "key", key)
			continue
		}
		out = append(out, c)
	}
	if len(out) > clievent.MaxCodeChanges {
		out = out[len(out)-clievent.MaxCodeChanges:]
	}
	return out
}
