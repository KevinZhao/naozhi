package runstore

// Test seams for cron's scheduler-level tests, which cannot reach the store's
// unexported fields across the package boundary. Never call these from
// production code.

// SetTrimGCForTest turns Append's retention trim on or off so a test can count
// records without the trim racing it.
func (s *Store) SetTrimGCForTest(enabled bool) { s.enableTrimGC = enabled }

// EnsureJobDirForTest creates jobID's record directory the way the first
// Append does, so a test can then make it unwritable.
func (s *Store) EnsureJobDirForTest(jobID string) (string, error) {
	return s.layout.EnsureOwnerDir(jobID)
}

// CacheStateForTest reports jobID's cache entry without warming it: ok is
// false when no entry exists.
func (s *Store) CacheStateForTest(jobID string) (count int, warm, ok bool) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		return 0, false, false
	}
	entry := v.(*recentCacheEntry)
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.count, entry.warm, true
}
