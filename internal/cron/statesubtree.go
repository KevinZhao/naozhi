package cron

// stateSubtree resolves a sibling subtree of the cron store file
// (<store-dir>/<parts...>). Returns "" when persistence is disabled so every
// caller folds its storePath=="" early-return into this helper. The path and
// the symlink-refusing mkdir below are sandboxstore's, so every state writer —
// in cron or in the store — goes through one guard.
func (s *Scheduler) stateSubtree(parts ...string) string {
	return s.sandboxState().Subtree(parts...)
}

// mkdirStateSubtree creates a state subtree (0700) below the cron store
// directory, refusing any symlinked or non-directory component
// (sandboxstore.Store.MkdirSubtree, #2166).
func (s *Scheduler) mkdirStateSubtree(dir string) error {
	return s.sandboxState().MkdirSubtree(dir)
}
