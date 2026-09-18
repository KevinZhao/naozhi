package cron

// entry_registration.go — serialises the writers that swap a job's robfig entry.
//
// A schedule change cannot be done in one critical section. It has to clear the
// old entry id, hand the old id to robfig's Remove and the new schedule to its
// Schedule — both of which rendezvous with the run loop — and only then write
// the new id back. Holding s.mu across those calls would park every registry
// reader (the dashboard's 1 Hz list, each tick's own jobs[id] lookup) behind the
// run loop, so UpdateJob deliberately releases it in the middle.
//
// That release is correct for readers and wrong for writers. Two concurrent
// UpdateJob schedule changes interleave like this:
//
//	U1: lock; entryID = 0, remember E0; unlock
//	U2: lock; entryID = 0, remember *zero* (U1 already cleared it); unlock
//	U1: Remove(E0); lock; register → E1; unlock
//	U2: Remove(0) — a no-op; lock; register → E2; unlock
//
// E1 and E2 are both live and j.entryID names only E2, so the job fires on the
// union of two schedules — more often than anyone configured — and DeleteJob
// later removes one of them, leaving the other ticking for a job that is gone.
//
// entryMu is what closes that: one writer at a time owns "this job's entry", for
// the whole clear → Remove → register → assign span. Readers never touch it, so
// the latency the release bought is kept.
//
// LOCK ORDER: entryMu → s.mu. Never the reverse. A path that holds s.mu and then
// wants entryMu has to release s.mu first — and if that feels awkward it is a
// sign the transaction wants to start here instead.
//
// One mutex for all jobs rather than a shard array: schedule changes come from
// dashboard edits and are sparse, so there is nothing to contend for. It is
// deliberately NOT the per-job jobGates array, which guards run EXECUTION —
// binding the two would make "the table and the gate are never held together"
// (docs/rfc/cron-jobtable-extraction.md §3.3) false by construction.
