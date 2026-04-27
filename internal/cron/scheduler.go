package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ErrJobNotFound is returned by lookup/mutation APIs when no cron job matches.
// Callers should use errors.Is(err, cron.ErrJobNotFound) instead of string matching.
var ErrJobNotFound = errors.New("cron: job not found")

// ErrJobAlreadyPaused is returned by PauseJob when the target job is already
// paused. Callers (especially HTTP handlers) should map this to 409 Conflict
// rather than 400, since the request was well-formed but the target state is
// incompatible.
var ErrJobAlreadyPaused = errors.New("cron: job already paused")

// ErrJobNotPaused is returned by ResumeJob when the target job is not paused.
var ErrJobNotPaused = errors.New("cron: job not paused")

// ErrJobPaused is returned by TriggerNow when the target job is paused, so a
// manual trigger from the dashboard is rejected instead of silently running
// against the operator's pause intent.
var ErrJobPaused = errors.New("cron: job is paused")

// ErrPersistFailed is returned by mutation APIs (AddJob/DeleteJob/Update/
// Pause/Resume/SetJobPrompt) when the post-mutation JSON serialisation fails.
// The in-memory state has already been changed and cannot be rolled back —
// marshal failure is observationally unrecoverable (OOM / type-system bug),
// so the caller MUST surface this as a 500-class error so the operator can
// intervene (restart the process or rebuild the job). R51-QUAL-001: prior
// to this sentinel, persistJobsLocked returned a silent no-op func on
// marshal error, causing DeleteJob to "succeed" via the API while the
// deletion never reached disk — a restart replayed the deleted job.
var ErrPersistFailed = errors.New("cron: persist jobs failed")

// SessionRouter is the subset of session.Router that the cron Scheduler
// actually consumes. Declaring it here (consumer-side interface, Go idiom)
// inverts the historical cron → session dependency: a future Router refactor
// only has to preserve these three method shapes to stay scheduler-
// compatible, and tests can inject a fake without pulling the whole router
// graph. Any new s.router.X() call requires extending this interface, which
// makes accidental surface-area growth a compile error instead of a silent
// regression.
type SessionRouter interface {
	// RegisterCronStub creates (or refreshes) a suspended exempt session
	// entry so the cron job shows up in the dashboard sidebar before its
	// first run. Key is always "cron:<jobID>".
	RegisterCronStub(key, workspace, lastPrompt string)
	// Reset discards the session for the given key (used by fresh-mode
	// cron jobs and by Delete/Rename flows).
	Reset(key string)
	// GetOrCreate returns an existing session or spawns a new one at
	// execute time. The SessionStatus and *ManagedSession escape the
	// cron package because the scheduler needs to call Send on them.
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
}

// SchedulerConfig holds configuration for the cron scheduler.
type SchedulerConfig struct {
	// Router is the session router the scheduler talks to. Accepts the
	// SessionRouter interface so tests can pass a minimal fake; production
	// passes a *session.Router which satisfies it transparently.
	Router        SessionRouter
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	StorePath     string
	MaxJobs       int
	ExecTimeout   time.Duration
	// Location is the timezone in which schedule expressions are evaluated.
	// nil defaults to time.Local so cron expressions match wall-clock time
	// on the host (respects $TZ / /etc/localtime).
	//
	// DST caveats (inherited from robfig/cron v3):
	//   - Spring-forward (hour skipped): a schedule whose expression lands in
	//     the missing hour fires zero times that day.
	//   - Fall-back (hour repeated): a schedule whose expression lands in the
	//     repeated hour may fire twice within the same wall-clock hour. Fast
	//     jobs that complete before the second trigger are not protected by
	//     SkipIfStillRunning.
	// For time-critical periodic work (billing, audit snapshots) prefer a UTC
	// Location so the schedule is immune to DST transitions.
	Location *time.Location
	// NotifyDefault provides a fallback IM target for jobs that opt into
	// notifications (Job.Notify == true) but have no per-job target set.
	// Empty Platform or ChatID disables the default.
	NotifyDefault NotifyTarget
	// ParentCtx, if set, is used as the parent for the scheduler's internal stop context.
	// When it is cancelled (e.g. during application shutdown) all running cron jobs are
	// interrupted promptly.
	ParentCtx context.Context
	// AllowedRoot mirrors Server.allowedRoot: the only directory tree under
	// which cron jobs may execute. Persisted jobs whose WorkDir falls outside
	// this root are refused at Start() load time — otherwise an attacker who
	// tampers with cron_jobs.json on disk (or a job persisted before the
	// operator configured AllowedRoot) could escape the sandbox at replay.
	// Empty disables the check (back-compat for tests and legacy deployments).
	AllowedRoot string
}

// NotifyTarget identifies an IM channel for cron completion notifications.
type NotifyTarget struct {
	Platform string
	ChatID   string
}

// IsSet reports whether both fields are populated.
func (n NotifyTarget) IsSet() bool { return n.Platform != "" && n.ChatID != "" }

// OnExecuteFunc is called after a cron job finishes execution.
// It receives the job ID, result text (or empty), and error message (or empty).
type OnExecuteFunc func(jobID, result, errMsg string)

// Scheduler manages cron jobs and executes them on schedule.
type Scheduler struct {
	cron          *robfigcron.Cron
	mu            sync.RWMutex
	jobs          map[string]*Job
	router        SessionRouter
	platforms     map[string]platform.Platform
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	storePath     string
	maxJobs       int
	execTimeout   time.Duration
	// location is the timezone used to interpret schedule expressions and to
	// compute preview/next-run times exposed via the dashboard.
	location *time.Location
	// notifyDefault is the fallback IM target used when a job has Notify=true
	// but no per-job target; zero value means no default (then notifications
	// only flow when per-job NotifyPlatform/NotifyChatID are set).
	notifyDefault NotifyTarget
	// allowedRoot restricts job WorkDir to a filesystem subtree. Applied at
	// Start() load time to catch tampered/legacy store entries, and at
	// execute() time to catch symlink races that retarget post-creation.
	// Empty disables enforcement (tests/legacy).
	allowedRoot string
	// stopCtx is the scheduler's lifecycle context. Storing context in a
	// struct is usually an anti-pattern, but here execute() is invoked via
	// a callback from robfig/cron whose signature has no ctx parameter, so
	// the scheduler itself owns the root context so Stop() can cancel in-
	// flight executions. Callers outside execute() take ctx as an argument.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	onExecute  OnExecuteFunc

	// triggerWG tracks goroutines spawned by TriggerNow so Stop() can wait
	// for them to finish. The scheduled entries are already drained by
	// s.cron.Stop(), but manual TriggerNow fires a goroutine outside the
	// cron scheduler's purview.
	triggerWG sync.WaitGroup

	// runningJobs serializes execute(j) calls per job ID so a manual
	// TriggerNow cannot overlap a scheduled tick for the same job (the cron
	// chain's SkipIfStillRunning only protects the scheduled path). Entries
	// are intentionally NOT cleared on job delete — a concurrent execute()
	// may still hold the atomic.Bool and be about to CAS it back to false;
	// if a fresh AddJob reused the same ID (low but non-zero given the hex8
	// generator), creating a new guard entry would split the CAS gate between
	// two goroutines and permit double execution. The leak is bounded by
	// maxJobsHardCap so the trade is cheap vs. a correctness gap.
	runningJobs sync.Map // map[jobID]*atomic.Bool

	// storeMu serialises saveSnapshot writes so the last-writer-wins order
	// matches the order snapshots were marshaled under s.mu. WriteFileAtomic
	// now uses os.CreateTemp so the underlying .tmp file is unique per call
	// and cannot be corrupted by parallel writers; storeMu remains only as
	// a logical barrier against reordering (an older snapshot rename-winning
	// over a newer one). Held only around the saveJobs call — snapshot
	// construction stays on s.mu to avoid cross-lock latency.
	storeMu sync.Mutex
}

// SetOnExecute registers a callback invoked after each cron job execution.
func (s *Scheduler) SetOnExecute(fn OnExecuteFunc) {
	s.mu.Lock()
	s.onExecute = fn
	s.mu.Unlock()
}

// maxJobsHardCap caps user-configurable MaxJobs to prevent accidental
// overload. 500 jobs ≈ 500 tick timers; well within robfig/cron's tested
// scale, but higher values tend to indicate a config mistake.
const maxJobsHardCap = 500

// workDirUnderRoot reports whether workDir resolves (after symlink evaluation)
// to a path at or under allowedRoot. EvalSymlinks is done per-call so the
// check reflects current filesystem state, which closes the TOCTOU window
// between creation-time validateWorkspace and execute-time workspace binding.
// Both arguments must be absolute; relative workDir is rejected.
func workDirUnderRoot(workDir, allowedRoot string) bool {
	if workDir == "" || allowedRoot == "" {
		return true // empty WorkDir uses router default; empty root = disabled
	}
	if !filepath.IsAbs(workDir) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		// Missing directory / permission denied — refuse to execute rather
		// than silently re-create the sandbox escape.
		return false
	}
	rootResolved, err := filepath.EvalSymlinks(allowedRoot)
	if err != nil {
		rootResolved = allowedRoot
	}
	if resolved == rootResolved {
		return true
	}
	return strings.HasPrefix(resolved, rootResolved+string(filepath.Separator))
}

// NewScheduler creates a scheduler. Call Start() to begin.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 50
	}
	if cfg.MaxJobs > maxJobsHardCap {
		slog.Warn("cron max_jobs exceeds hard cap, clamping", "requested", cfg.MaxJobs, "cap", maxJobsHardCap)
		cfg.MaxJobs = maxJobsHardCap
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 5 * time.Minute
	}
	parent := cfg.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	stopCtx, stopCancel := context.WithCancel(parent)
	cronLogger := robfigcron.PrintfLogger(log.New(slogWriter{}, "cron: ", 0))
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return &Scheduler{
		cron: robfigcron.New(
			robfigcron.WithLocation(loc),
			robfigcron.WithChain(
				robfigcron.Recover(cronLogger),
				robfigcron.SkipIfStillRunning(cronLogger),
			),
		),
		jobs:          make(map[string]*Job),
		router:        cfg.Router,
		platforms:     cfg.Platforms,
		agents:        cfg.Agents,
		agentCommands: cfg.AgentCommands,
		storePath:     cfg.StorePath,
		maxJobs:       cfg.MaxJobs,
		execTimeout:   cfg.ExecTimeout,
		location:      loc,
		notifyDefault: cfg.NotifyDefault,
		allowedRoot:   cfg.AllowedRoot,
		stopCtx:       stopCtx,
		stopCancel:    stopCancel,
	}
}

// NotifyDefault returns the configured fallback IM target so the dashboard can
// show users where a "notify on completion" toggle will deliver messages.
func (s *Scheduler) NotifyDefault() NotifyTarget { return s.notifyDefault }

// Start loads persisted jobs and starts the cron scheduler.
func (s *Scheduler) Start() error {
	// loadJobs distinguishes three outcomes: (map, nil) normal, (nil, nil)
	// corrupt-but-rescued, (nil, error) original file still on disk. In the
	// error case we must refuse to start — otherwise the first subsequent
	// persist would overwrite the operator's real jobs with `[]`, silently
	// losing data that is still recoverable from the preserved file.
	restored, err := loadJobs(s.storePath)
	if err != nil {
		return fmt.Errorf("load cron store: %w", err)
	}

	s.mu.Lock()
	// Snapshot the fields we pass to registerStub under lock so we don't
	// dereference *Job after releasing s.mu — once cron.Start() fires, any
	// future UpdateJob could race with a stub read via the map pointer.
	type stubRow struct{ id, workDir, prompt string }
	var stubs []stubRow
	if restored != nil {
		for _, j := range restored {
			// Reject persisted jobs whose WorkDir escapes the configured
			// sandbox. Replaying an on-disk tampered entry must not grant
			// filesystem access that validateWorkspace would reject at
			// creation. When allowedRoot is empty (tests), this is a no-op.
			if s.allowedRoot != "" && j.WorkDir != "" && !workDirUnderRoot(j.WorkDir, s.allowedRoot) {
				slog.Warn("cron job work_dir outside allowed_root; skipping",
					"id", j.ID, "work_dir", j.WorkDir)
				continue
			}
			if j.Paused {
				s.jobs[j.ID] = j
				stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt})
				continue
			}
			if err := s.registerJob(j); err != nil {
				slog.Warn("skip invalid cron job", "id", j.ID, "schedule", j.Schedule, "err", err)
				continue
			}
			s.jobs[j.ID] = j
			stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt})
		}
	}
	jobCount := len(s.jobs)
	s.mu.Unlock()
	// Register dashboard stub sessions after releasing the lock; the router's
	// notifyChange callback must not re-enter scheduler state. Use snapshotted
	// values (not the *Job pointer) so a concurrent UpdateJob mutating the map
	// entry cannot race with our reads.
	for _, st := range stubs {
		s.registerStubByValue(st.id, st.workDir, st.prompt)
	}
	s.cron.Start()
	slog.Info("cron scheduler started", "jobs", jobCount)
	return nil
}

// registerStub creates (or refreshes) a router session entry for the job so it
// appears in the dashboard workspace list. Safe to call without a router (tests).
// Callers must not be holding s.mu — RegisterCronStub re-enters router state.
func (s *Scheduler) registerStub(j *Job) {
	if s.router == nil {
		return
	}
	s.router.RegisterCronStub("cron:"+j.ID, j.WorkDir, j.Prompt)
}

// registerStubByValue is the pointer-free variant used from Start() where the
// caller has already snapshotted mutable fields under s.mu.
func (s *Scheduler) registerStubByValue(id, workDir, prompt string) {
	if s.router == nil {
		return
	}
	s.router.RegisterCronStub("cron:"+id, workDir, prompt)
}

// EnsureStub lazily (re-)registers a dashboard stub session for the given
// key (format "cron:<jobID>"). Returns true when the matching job still
// exists and a stub is now registered (either newly created or already
// present); returns false when the key is malformed, not a cron key, or
// the job is gone.
//
// Rationale: the sidebar "×" button routes through router.Remove and
// deletes the stub. Cron stubs are meant to be re-bornable — the next
// scheduled tick rebuilds them via executeJob's GetOrCreate — but between
// the dismissal and that tick, clicking the task card in the Cron panel
// would otherwise hit "session not found" because the WS subscribe path
// has nothing to attach to. This method is the idempotent recovery hook
// wired into handleSubscribe and /api/sessions/events.
func (s *Scheduler) EnsureStub(key string) bool {
	const prefix = "cron:"
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	id := key[len(prefix):]
	if id == "" {
		return false
	}
	// Snapshot workDir/prompt under RLock, release before reaching into
	// router: RegisterCronStub calls notifyChange which fans out to hub
	// broadcasters, and holding s.mu across that path risks lock-order
	// inversion with the cron dispatcher (see ListAllJobsWithNextRun).
	s.mu.RLock()
	j, ok := s.jobs[id]
	var workDir, prompt string
	if ok {
		workDir = j.WorkDir
		prompt = j.Prompt
	}
	s.mu.RUnlock()
	if !ok {
		return false
	}
	s.registerStubByValue(id, workDir, prompt)
	return true
}

// stopBudget is the overall deadline Scheduler.Stop() will spend waiting on
// cron.Stop + triggerWG before proceeding to save. Shared between both waits
// (not doubled per wait) so a production deployment with execTimeout=3600s
// cannot pin restart for ≈2 h — the prior two-budget design had a worst case
// of 2×(execTimeout+5s). Aligned with session.ShutdownTimeout (30s) so both
// subsystems agree on the upper bound systemd sees.
//
// Package-level var (not const) so tests can shorten it to milliseconds
// without race-racing a Stop call with real wall-clock timeouts.
// R49-REL-CRON-STOP-BUDGET.
var stopBudget = 30 * time.Second

// Stop halts the scheduler and saves state. It waits for both scheduled jobs
// (drained by s.cron.Stop) and any TriggerNow-spawned goroutines before
// returning, so callers can safely tear down the router afterwards.
//
// Shutdown is bounded by stopBudget (30s by default). A stuck cron job
// (execute() hanging past ctx cancel, e.g. a broken shim ignoring context)
// or a stuck triggerWG (deliverNotice → platform Reply webhook that refuses
// to honour its own timeout) cannot hold us past this budget. The final
// saveJobs runs regardless so a stuck drain does not cost the state file.
func (s *Scheduler) Stop() {
	s.stopCancel()
	cronDoneCtx := s.cron.Stop()

	// Single overall deadline shared across both waits. If cron.Stop drains
	// fast we have the full budget for triggerWG; if it eats the budget we
	// skip triggerWG.Wait entirely (the leaked goroutines die when the
	// process exits moments later). Either way saveJobs runs — losing it
	// would undo mutations that had already returned 2xx to dashboard/IM.
	deadline := time.NewTimer(stopBudget)
	defer deadline.Stop()

	deadlineHit := false
	select {
	case <-cronDoneCtx.Done():
	case <-deadline.C:
		deadlineHit = true
		slog.Warn("cron scheduler: stop deadline exceeded before cron.Stop drained, proceeding",
			"budget", stopBudget)
	}

	// Bound triggerWG.Wait with the *remaining* share of the same budget:
	// while manual TriggerNow respects stopCtx via execute(), the notify
	// delivery path (deliverNotice → platform Reply) uses a Background-
	// derived ctx so stopCtx cancellation does not interrupt an in-flight
	// webhook POST. Without this deadline, a stuck platform HTTP call
	// could pin Stop() past systemd TimeoutStopSec.
	if !deadlineHit {
		triggerDone := make(chan struct{})
		go func() {
			s.triggerWG.Wait()
			close(triggerDone)
		}()
		select {
		case <-triggerDone:
		case <-deadline.C:
			slog.Warn("cron scheduler: stop deadline exceeded during triggerWG wait, proceeding",
				"budget", stopBudget)
		}
	}

	s.mu.Lock()
	data, err := s.marshalJobsLocked()
	s.mu.Unlock()
	if err != nil {
		slog.Error("marshal cron store on shutdown", "err", err)
		return
	}
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		slog.Error("save cron store on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
}

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}

	s.mu.Lock()

	if len(s.jobs) >= s.maxJobs {
		s.mu.Unlock()
		return fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	// Per-chat limit to prevent one chat from exhausting global quota
	const maxJobsPerChat = 10
	chatCount := 0
	for _, existing := range s.jobs {
		if existing.Platform == j.Platform && existing.ChatID == j.ChatID {
			chatCount++
		}
	}
	if chatCount >= maxJobsPerChat {
		s.mu.Unlock()
		return fmt.Errorf("per-chat cron limit reached (%d)", maxJobsPerChat)
	}

	j.ID = generateID()
	// Retry on unlikely ID collision
	for _, exists := s.jobs[j.ID]; exists; _, exists = s.jobs[j.ID] {
		j.ID = generateID()
	}
	j.CreatedAt = time.Now()

	if !j.Paused {
		if err := s.registerJob(j); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.jobs[j.ID] = j
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		// In-memory insertion + cron scheduling already happened; we cannot
		// roll those back safely (another goroutine may have observed the
		// registered entry). Surface the error so the HTTP layer returns a
		// 500 and the operator sees the persistence gap.
		return perr
	}
	save()
	s.registerStub(j)
	return nil
}

// ListJobs returns jobs for a specific chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID {
			result = append(result, *j)
		}
	}
	return result
}

// ListAllJobs returns all jobs regardless of platform/chat scope.
// Returns value copies safe to read outside the lock.
func (s *Scheduler) ListAllJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, *j)
	}
	return result
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (*Job, entryID) under s.mu.RLock, release s.mu, then
// call s.cron.Entry() without holding s.mu. Calling cron.Entry inside s.mu
// would invert the lock order taken by the cron dispatcher path
// (cron-internal → execute → recordResult → s.mu.Lock), risking a deadlock.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	s.mu.RLock()
	type pair struct {
		job     Job
		entryID robfigcron.EntryID
	}
	pairs := make([]pair, 0, len(s.jobs))
	for _, j := range s.jobs {
		pairs = append(pairs, pair{job: *j, entryID: j.entryID})
	}
	s.mu.RUnlock()

	result := make([]JobWithNextRun, 0, len(pairs))
	for _, p := range pairs {
		var next time.Time
		if p.entryID != 0 {
			next = s.cron.Entry(p.entryID).Next
		}
		result = append(result, JobWithNextRun{Job: p.job, NextRun: next})
	}
	return result
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
	}
	if s.router != nil {
		s.router.Reset("cron:" + j.ID)
	}
	delete(s.jobs, j.ID)
	// Intentionally do NOT delete from s.runningJobs here: a concurrent
	// execute() for this job may still hold the atomic.Bool and be about to
	// CAS it back to false; if a fresh AddJob somehow reused the same ID
	// (low but non-zero given the hex8 generator), creating a new guard
	// entry here could split the CAS gate between two goroutines and permit
	// double execution. Retaining the entry is bounded by maxJobsHardCap
	// (one *atomic.Bool per historical job) — cheap vs a correctness gap.
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
	j.Paused = true
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if !j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}

	if err := s.registerJob(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	j.Paused = false
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// JobUpdate captures fields a dashboard user may edit on an existing cron
// job. Only non-nil pointers are applied, so callers can update a single
// field without resending the rest.
type JobUpdate struct {
	Schedule *string
	Prompt   *string
	WorkDir  *string
	// Notify sets Job.Notify when non-nil. nil leaves the field unchanged;
	// pointer-to-true/false writes the explicit tri-state. There's no API
	// to reset back to legacy-default (nil) once a value is set — callers
	// typically toggle between true and false instead.
	Notify *bool
	// NotifyPlatform / NotifyChatID behave like Prompt / WorkDir: nil keeps
	// the existing value, a pointer to "" clears it.
	NotifyPlatform *string
	NotifyChatID   *string
	// FreshContext toggles whether each run resets the session before
	// executing. nil leaves existing behavior unchanged.
	FreshContext *bool
}

// UpdateJob applies a partial edit to an existing cron job. Schedule changes
// are validated and re-registered atomically (the old robfig entry is
// removed before the new one is installed) so a failed reschedule leaves
// the previous behavior intact. Prompt/WorkDir changes flow through to the
// router stub so the dashboard sidebar reflects the edit immediately.
func (s *Scheduler) UpdateJob(id string, upd JobUpdate) (*Job, error) {
	// Validate schedule first (no lock needed) so we fail fast on bad input.
	if upd.Schedule != nil {
		if *upd.Schedule == "" {
			return nil, fmt.Errorf("schedule must not be empty")
		}
		if err := validateSchedule(*upd.Schedule); err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", *upd.Schedule, err)
		}
	}
	// Validate WorkDir against allowedRoot here (lock-free) so dashboard
	// edits fail fast with a clear error instead of silently persisting a
	// path that execute() will later refuse at runtime. AddJob's creation
	// path applies the same check; UpdateJob previously skipped it.
	if upd.WorkDir != nil && *upd.WorkDir != "" && s.allowedRoot != "" {
		if !workDirUnderRoot(*upd.WorkDir, s.allowedRoot) {
			return nil, fmt.Errorf("work_dir outside allowed root")
		}
	}

	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}

	if upd.Prompt != nil {
		j.Prompt = *upd.Prompt
	}
	if upd.WorkDir != nil {
		j.WorkDir = *upd.WorkDir
	}
	if upd.Notify != nil {
		v := *upd.Notify
		j.Notify = &v
	}
	if upd.NotifyPlatform != nil {
		j.NotifyPlatform = *upd.NotifyPlatform
	}
	if upd.NotifyChatID != nil {
		j.NotifyChatID = *upd.NotifyChatID
	}
	if upd.FreshContext != nil {
		j.FreshContext = *upd.FreshContext
	}

	if upd.Schedule != nil && *upd.Schedule != j.Schedule {
		j.Schedule = *upd.Schedule
		// Re-register with the new schedule unless paused (paused jobs have
		// no live entry; ResumeJob will register with the new schedule).
		if !j.Paused {
			if j.entryID != 0 {
				s.cron.Remove(j.entryID)
				j.entryID = 0
			}
			if err := s.registerJob(j); err != nil {
				s.mu.Unlock()
				return nil, fmt.Errorf("re-register cron: %w", err)
			}
		}
	}

	save, perr := s.persistJobsLocked()
	// Value-copy while still under lock so the caller sees a stable result
	// even if another goroutine mutates the job right after we unlock.
	result := *j
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	// Pass the snapshotted value (via result) to registerStub so a concurrent
	// SetJobPrompt cannot tear the Prompt/WorkDir pointers we read.
	s.registerStub(&result)
	slog.Info("cron job updated", "id", id,
		"schedule_changed", upd.Schedule != nil,
		"prompt_changed", upd.Prompt != nil,
		"workdir_changed", upd.WorkDir != nil,
		"fresh_context_changed", upd.FreshContext != nil)
	return &result, nil
}

// SetJobPrompt updates a job's prompt. If the job was paused with an empty
// prompt (created from dashboard), it also unpauses and registers the schedule.
func (s *Scheduler) SetJobPrompt(id, prompt string) error {
	if prompt == "" {
		return fmt.Errorf("prompt must not be empty")
	}

	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Prompt != "" {
		s.mu.Unlock()
		return nil // already has a prompt, no-op
	}

	j.Prompt = prompt
	if j.Paused {
		if err := s.registerJob(j); err != nil {
			s.mu.Unlock()
			return err
		}
		j.Paused = false
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return perr
	}
	save()
	slog.Info("cron job prompt set", "id", id, "prompt_len", len(prompt))
	return nil
}

// PreviewSchedule validates a schedule expression and returns the next run
// time in UTC. Used by tests and the dashboard bootstrap path where no live
// Scheduler is wired; the live path should call Scheduler.PreviewSchedule so
// the configured timezone is honoured.
func PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	// Pass UTC explicitly so the returned time.Time has Location=UTC and the
	// godoc contract matches the implementation. `time.Now()` would inherit
	// the host TZ, making the return value's location non-deterministic
	// across machines.
	return sched.Next(time.Now().UTC()), nil
}

// PreviewSchedule computes the next run time using the scheduler's configured
// timezone, which matches how the live scheduler evaluates cron expressions.
func (s *Scheduler) PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	loc := s.location
	if loc == nil {
		loc = time.Local
	}
	return sched.Next(time.Now().In(loc)), nil
}

// PreviewScheduleN returns the next n run times for a schedule expression, in
// the scheduler's configured timezone. Used by the dashboard to preview what
// "接下来会在这些时间运行" looks like before a user commits to a frequency.
// Callers get a validation error on the first Parse failure; n is clamped to
// a sane range by the caller.
func (s *Scheduler) PreviewScheduleN(schedule string, n int) ([]time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 1
	}
	loc := s.location
	if loc == nil {
		loc = time.Local
	}
	out := make([]time.Time, 0, n)
	t := time.Now().In(loc)
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		out = append(out, t)
	}
	return out, nil
}

// Location returns the timezone the scheduler uses to evaluate cron
// expressions, so the dashboard can surface it alongside preview/next-run
// timestamps.
func (s *Scheduler) Location() *time.Location {
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
	}
	if s.router != nil {
		s.router.Reset("cron:" + j.ID)
	}
	delete(s.jobs, j.ID)
	// Retain the runningJobs entry for the same reason as DeleteJobByID —
	// a concurrent execute() may still be mid-CAS on this guard.
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// PauseJob pauses a job by ID prefix.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
	j.Paused = true
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// ResumeJob resumes a paused job by ID prefix.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}

	if err := s.registerJob(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	j.Paused = false
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// NextRun returns the next scheduled run time for a job.
func (s *Scheduler) NextRun(j *Job) time.Time {
	s.mu.RLock()
	entryID := j.entryID
	s.mu.RUnlock()
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// NextRunByID returns the next scheduled run time for a job by ID.
func (s *Scheduler) NextRunByID(id string) time.Time {
	s.mu.RLock()
	j, ok := s.jobs[id]
	if !ok || j.entryID == 0 {
		s.mu.RUnlock()
		return time.Time{}
	}
	entryID := j.entryID
	s.mu.RUnlock()
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// TriggerNow manually executes a job by ID in a new goroutine (for debugging/dashboard).
// Returns an error if the job is not found, paused, or has no prompt.
func (s *Scheduler) TriggerNow(id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	}
	if j.Prompt == "" {
		s.mu.Unlock()
		return fmt.Errorf("job %s has no prompt", id)
	}
	entryID := j.entryID
	// Register the trigger goroutine with triggerWG before releasing s.mu.
	// This prevents a Stop() on another goroutine from observing triggerWG as
	// empty and returning before our goroutine starts. We pair Add(1) here
	// with a Done() in each goroutine body below; if we bail out before
	// spawning (concurrent delete), we Done() the counter inline.
	s.triggerWG.Add(1)
	s.mu.Unlock()

	if entryID != 0 {
		// Run through the cron chain (SkipIfStillRunning + Recover) to prevent
		// double-execution when TriggerNow overlaps with a scheduled tick.
		// Guard: a concurrent DeleteJob may remove the entry between our Unlock
		// and this lookup, causing Entry() to return a zero-value with nil WrappedJob.
		// robfig/cron EntryIDs are a monotonically incrementing int and never
		// reused within a process; this rules out the "stale entryID returns a
		// different job's entry" hazard.
		entry := s.cron.Entry(entryID)
		if entry.WrappedJob != nil {
			go func() {
				defer s.triggerWG.Done()
				entry.WrappedJob.Run()
			}()
		} else {
			// Entry was concurrently deleted — skip execution and release
			// the WaitGroup slot we reserved above.
			s.triggerWG.Done()
			slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "id", id, "entry_id", entryID)
		}
	} else {
		// Resolve the job by ID inside the goroutine so the freshest pointer
		// is used (matches the cron-tick path in registerJob). If the job was
		// concurrently deleted, skip execution — recordResult would then write
		// to an orphan pointer whose updates are not visible in the snapshot.
		jobID := j.ID
		go func() {
			defer s.triggerWG.Done()
			s.mu.RLock()
			cur, ok := s.jobs[jobID]
			paused := ok && cur.Paused
			s.mu.RUnlock()
			if !ok {
				slog.Debug("TriggerNow: job deleted before execute, skipping", "id", jobID)
				return
			}
			// Honor a Pause that landed between the TriggerNow snapshot and the
			// goroutine starting: the operator's "stop now" intent outranks the
			// in-flight trigger click.
			if paused {
				slog.Debug("TriggerNow: job paused concurrently, skipping", "id", jobID)
				return
			}
			s.execute(cur)
		}()
	}
	return nil
}

// registerJob registers a job with the robfig/cron scheduler.
//
// The closure captures the job's ID rather than the *Job pointer: if the
// job is removed and re-added (UpdateJob path) while the scheduler goroutine
// holds an old entry, we want the next tick to resolve the currently-registered
// job rather than fire against a stale pointer whose fields may have diverged
// from the user's intent.
func (s *Scheduler) registerJob(j *Job) error {
	jobID := j.ID
	entryID, err := s.cron.AddFunc(j.Schedule, func() {
		s.mu.RLock()
		cur, ok := s.jobs[jobID]
		paused := ok && cur.Paused
		s.mu.RUnlock()
		if !ok {
			slog.Debug("cron: scheduled job no longer registered, skipping", "id", jobID)
			return
		}
		// A Pause that lands between cron-tick dispatch and our re-lock should
		// be honored; otherwise the user sees a paused job still firing once.
		// PauseJobByID removes the entry via cron.Remove(), so normally this
		// tick wouldn't fire — but robfig/cron may already be mid-dispatch when
		// Remove runs, yielding exactly this race.
		if paused {
			slog.Debug("cron: tick fired for job paused concurrently, skipping", "id", jobID)
			return
		}
		s.execute(cur)
	})
	if err != nil {
		return fmt.Errorf("register cron: %w", err)
	}
	j.entryID = entryID
	return nil
}

// jobRunningGuard returns a lazily created *atomic.Bool per job ID used by
// execute() to prevent concurrent runs of the same job (see execute).
// Entries are cleared on DeleteJob; occasional leaks for never-deleted jobs
// are bounded by the scheduler's maxJobs cap.
func (s *Scheduler) jobRunningGuard(id string) *atomic.Bool {
	if v, ok := s.runningJobs.Load(id); ok {
		return v.(*atomic.Bool)
	}
	guard := &atomic.Bool{}
	actual, _ := s.runningJobs.LoadOrStore(id, guard)
	return actual.(*atomic.Bool)
}

// execute runs a cron job: send prompt to session, post result to chat.
func (s *Scheduler) execute(j *Job) {
	// Guard against concurrent execution of the same job. The cron chain's
	// SkipIfStillRunning protects the scheduled-tick path, but TriggerNow
	// that arrives while a tick is in flight bypasses the chain entirely
	// (it calls execute directly when entryID == 0 or Run() on the entry
	// which is separately serialized). Using a per-job *atomic.Bool kept
	// outside Job (to preserve value-copy semantics of Job) provides a
	// uniform CAS gate.
	guard := s.jobRunningGuard(j.ID)
	if !guard.CompareAndSwap(false, true) {
		slog.Info("cron: job already running, skipping overlap", "cron_id", j.ID)
		return
	}
	defer guard.Store(false)

	// Snapshot mutable fields under lock to avoid data race with SetJobPrompt.
	s.mu.Lock()
	prompt := j.Prompt
	workDir := j.WorkDir
	jobID := j.ID
	platName := j.Platform
	chatID := j.ChatID
	notifyPlat := j.NotifyPlatform
	notifyChat := j.NotifyChatID
	fresh := j.FreshContext
	var notifyOpt *bool
	if j.Notify != nil {
		v := *j.Notify
		notifyOpt = &v
	}
	s.mu.Unlock()

	// Resolve the effective notification target for this run. Returns empty
	// struct when no delivery should happen, so both success and failure
	// paths below can call notify*() unconditionally-guarded by IsSet().
	notifyTo := s.resolveNotifyTarget(platName, chatID, notifyPlat, notifyChat, notifyOpt)

	// Named `lg` instead of `log` to avoid shadowing the standard `log`
	// package imported at the top of the file. While no current code inside
	// execute() calls into the `log` package, the shadow would silently
	// redirect any future `log.Printf` edit to this local *slog.Logger and
	// fail to compile (or worse, compile against a shadowed name). R60-GO-M2.
	lg := slog.With("cron_id", jobID, "platform", platName, "chat", chatID)
	lg.Info("cron job executing", "prompt_len", len(prompt))
	execStart := time.Now()

	ctx, cancel := context.WithTimeout(s.stopCtx, s.execTimeout)
	defer cancel()

	agentID, cleanText := session.ResolveAgent(prompt, s.agentCommands)
	opts := s.agents[agentID]
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	if workDir != "" {
		// Re-check allowedRoot at execute time to close the symlink-swap race:
		// validateWorkspace at creation resolved symlinks once, but the target
		// could have been retargeted since. workDirUnderRoot re-evaluates
		// symlinks against the current filesystem state before binding.
		if s.allowedRoot != "" && !workDirUnderRoot(workDir, s.allowedRoot) {
			lg.Warn("cron job work_dir outside allowed_root; aborting run",
				"work_dir", workDir)
			s.recordResult(j, "", "work_dir outside allowed_root")
			return
		}
		opts.Workspace = filepath.Clean(workDir)
	}
	key := "cron:" + jobID

	// Fresh mode: drop any existing session (and its process + history) so
	// GetOrCreate spawns a brand-new CLI. Reset is a no-op when no session
	// exists yet, so first-run behavior is identical to persistent mode.
	// On error paths (GetOrCreate/Send failure) the sidebar stub is rebuilt
	// so the cron row does not vanish from the dashboard. On the success
	// path we skip the stub refresh because the live session carries its
	// own sidebar entry; re-registering would trigger a spurious broadcast
	// and briefly revert the row state before the next executeSucceeded.
	stubRefresh := func() {}
	if fresh {
		// CRON3 guard: Scheduler.Stop() cancels stopCtx as its first act; if a
		// scheduled tick fired just before Stop() took that edge it is still
		// inside execute() here. Running Reset + GetOrCreate past this point
		// would race with Router.Shutdown's session-drain and can leak a brand-
		// new CLI process tied to an orphan "cron:<id>" key that outlives
		// naozhi (shim cleanup only covers sessions that were in the map at
		// Shutdown). Early-exit on ctx cancellation instead — the job will
		// run again on the next start.
		if err := s.stopCtx.Err(); err != nil {
			lg.Info("cron fresh spawn suppressed during shutdown", "err", err)
			return
		}
		s.router.Reset(key)
		lg.Info("cron fresh context: session reset before run")
		stubRefresh = func() {
			s.mu.RLock()
			jobCopy, ok := s.jobs[jobID]
			var j2 Job
			if ok {
				j2 = *jobCopy
			}
			s.mu.RUnlock()
			if ok {
				s.registerStub(&j2)
			}
		}
		// Re-check job existence after Reset: a concurrent DeleteJobByID could
		// have run between the pre-execute snapshot and Reset, in which case
		// GetOrCreate below would leak a brand-new CLI process tied to an
		// orphan "cron:<id>" key until TTL cleanup.
		s.mu.RLock()
		_, stillExists := s.jobs[jobID]
		s.mu.RUnlock()
		if !stillExists {
			lg.Info("cron job deleted mid-execute, skipping GetOrCreate")
			return
		}
	}

	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled mid-flight (graceful shutdown or job
			// deletion overlapping execute). The job will either be re-run
			// on the next tick or is intentionally gone; either way an IM
			// notification would be spam and the stored LastError would
			// falsely blame the job itself.
			lg.Info("cron session cancelled", "err", err)
			stubRefresh()
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			lg.Info("cron session deadline exceeded", "err", err)
		} else {
			lg.Error("cron session error", "err", err)
		}
		s.recordResult(j, "", "session error: "+err.Error())
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行跳过，请稍后重试。", jobID))
		stubRefresh()
		return
	}

	// Direct Send without sendWithBroadcast — cron jobs notify via onExecute callback instead.
	result, err := sess.Send(ctx, cleanText, nil, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures.
			lg.Info("cron send cancelled", "err", err)
			stubRefresh()
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			lg.Info("cron send deadline exceeded", "err", err)
		} else {
			lg.Error("cron send error", "err", err)
		}
		s.recordResult(j, "", "send error: "+err.Error())
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行失败，请稍后重试。", jobID))
		stubRefresh()
		return
	}

	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", time.Since(execStart).Milliseconds())
	s.recordResult(j, result.Text, "")

	replyText := fmt.Sprintf("[Cron %s] %s", jobID, result.Text)
	s.deliverNotice(notifyTo, replyText)
}

// resolveNotifyTarget picks the IM destination for this execution's
// completion notice. Priority:
//  1. Per-job NotifyPlatform/NotifyChatID (always honored when both set).
//  2. notify==true + scheduler default target.
//  3. notify==false disables delivery even for IM-created jobs.
//  4. notify==nil (unset) preserves legacy behavior: IM-created jobs reply
//     to their own source chat; dashboard-created jobs stay silent.
func (s *Scheduler) resolveNotifyTarget(platName, chatID, notifyPlat, notifyChat string, notify *bool) NotifyTarget {
	// Explicit disable wins over everything.
	if notify != nil && !*notify {
		return NotifyTarget{}
	}

	// Per-job override always wins when fully specified.
	if notifyPlat != "" && notifyChat != "" {
		return NotifyTarget{Platform: notifyPlat, ChatID: notifyChat}
	}

	// Explicit enable: fall back to scheduler default.
	if notify != nil && *notify {
		if s.notifyDefault.IsSet() {
			return s.notifyDefault
		}
		// Enabled but no target anywhere — log once per run so users notice
		// misconfiguration instead of silently dropping notifications.
		slog.Warn("cron notify enabled but no target configured",
			"hint", "set cron.notify_default.platform + chat_id, or provide per-job notify_platform + notify_chat_id")
		return NotifyTarget{}
	}

	// Legacy default (notify==nil): IM-created jobs reply to their source chat.
	// Platform "dashboard" has no registered platform object so this naturally
	// no-ops for dashboard jobs that predate the toggle.
	if platName != "" && chatID != "" {
		return NotifyTarget{Platform: platName, ChatID: chatID}
	}
	return NotifyTarget{}
}

// deliverNotice sends a result/error message to the resolved target.
// No-op when target is unset or the platform is not registered.
func (s *Scheduler) deliverNotice(target NotifyTarget, text string) {
	if !target.IsSet() {
		return
	}
	s.notifyTarget(target.Platform, target.ChatID, text)
}

// runeByteOffset returns the byte offset that contains maxRunes runes.
// truncated is true iff s has more than maxRunes runes.
// Zero allocations, unlike `[]rune(s)[:n]`.
func runeByteOffset(s string, maxRunes int) (int, bool) {
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			return i, true
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return i, false
}

// recordResult persists the last execution result on the job and invokes the onExecute callback.
func (s *Scheduler) recordResult(j *Job, result, errMsg string) {
	const maxStoredRunes = 4 * 1024
	// Byte-level rune decode: avoids the two O(n) rune-slice allocations that
	// `string([]rune(result)[:maxStoredRunes])` performs on a 4KB-result path.
	if byteOffset, truncated := runeByteOffset(result, maxStoredRunes); truncated {
		result = result[:byteOffset] + "…[truncated]"
	}
	// Redact absolute filesystem paths from errMsg before persisting to
	// cron_jobs.json and broadcasting to all authenticated dashboard
	// clients. session.GetOrCreate / session.Send surface workspace-bearing
	// errors that would otherwise enumerate operator filesystem layout on
	// disk (at 0600) and in every connected browser's event stream. Keep
	// the structural prefix ("session error: …" / "send error: …") so the
	// error class remains obvious. R61-SEC-8.
	errMsg = redactPathsInCronError(errMsg)
	s.mu.Lock()
	// If the job was deleted between execute()'s snapshot and recordResult's
	// write-back, skip both the persist and the onExecute callback: broadcasting
	// a "completed" result for an already-deleted job can flash a stale row in
	// the dashboard (Round 49 HIGH-4) and persists nothing useful since the job
	// is already gone from s.jobs.
	if _, ok := s.jobs[j.ID]; !ok {
		s.mu.Unlock()
		return
	}
	j.LastRunAt = time.Now()
	j.LastResult = result
	j.LastError = errMsg
	save, perr := s.persistJobsLocked()
	fn := s.onExecute
	s.mu.Unlock()

	// recordResult has no error return (it runs from the internal execute
	// goroutine), so marshal failure here can only be logged. persistJobsLocked
	// already emitted the slog.Error; proceed with onExecute so the dashboard
	// still sees the LastResult update via live broadcast even when the on-disk
	// snapshot misses this run.
	if perr == nil {
		save()
	}
	if fn != nil {
		fn(j.ID, result, errMsg)
	}
}

// redactPathsInCronError strips absolute filesystem paths from a cron
// execution error message before persistence. session.GetOrCreate and
// session.Send produce errors like "session error: workspace …/repo/x:
// permission denied" that would otherwise enumerate the operator's
// filesystem layout to every authenticated dashboard viewer and any
// cron_jobs.json backup reader. We replace both POSIX and Windows-style
// absolute paths with a literal "<path>" placeholder; error classification
// (permission denied, no such file) stays intact because the surrounding
// tokens aren't paths. R61-SEC-8.
//
// The implementation is a token-wise scan rather than a regex to avoid
// pulling a regex compile onto every cron run: recordResult is invoked on
// every execution and the regex cost would dominate the redaction budget.
func redactPathsInCronError(s string) string {
	if s == "" {
		return s
	}
	const maxErrLen = 2048
	if len(s) > maxErrLen {
		s = s[:maxErrLen] + "…"
	}
	// Fast path: if the string contains no POSIX slash and no Windows
	// backslash, there is nothing path-shaped to redact — skip the Builder
	// allocation and return the input unchanged. recordResult runs on every
	// cron execution, and common error classes ("dispatcher queue full",
	// "session error: context deadline exceeded") have no embedded paths.
	// R62-PERF-3.
	if strings.IndexByte(s, '/') < 0 && strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// POSIX absolute path: leading '/' followed by a non-space/non-quote
		// byte. Drive letter path C:\… also counts.
		isPosix := c == '/' && i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' && s[i+1] != '\n'
		isWin := i+2 < len(s) &&
			((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) &&
			s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/')
		if !isPosix && !isWin {
			b.WriteByte(c)
			i++
			continue
		}
		// Consume the path until a delimiter that cannot appear in a
		// typical error-embedded path. Stopping at whitespace is the key
		// rule: error messages from the Go standard library spell paths
		// as tokens separated by whitespace ("open /tmp/x: reason"), and
		// the rare legitimate "path with space" in an error string is
		// vanishingly unlikely to survive redaction cleanly anyway. A
		// conservative scan errs on the side of over-redacting.
		j := i
		for j < len(s) {
			cc := s[j]
			if cc == '\n' || cc == ' ' || cc == '\t' || cc == ',' || cc == ';' ||
				cc == '\'' || cc == '"' || cc == '`' {
				break
			}
			if cc == ':' && j+1 < len(s) && (s[j+1] == ' ' || s[j+1] == '\n') {
				// `path: reason` — stop before the ':' so the reason tail
				// survives redaction.
				break
			}
			j++
		}
		b.WriteString("<path>")
		i = j
	}
	return b.String()
}

// notifyTarget sends a message to an arbitrary platform/chat (notify target).
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	p := s.platforms[plat]
	if p == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	// Use Background parent: during shutdown stopCtx is cancelled first, then
	// cron.Stop() waits for in-flight jobs — those must still be able to deliver
	// their IM replies within the 30s bound rather than fail instantly.
	replyCtx, replyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer replyCancel()
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = 4000
	}
	chunks := platform.SplitText(text, maxLen)
	for _, chunk := range chunks {
		if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Text:   chunk,
		}, 3); err != nil {
			slog.Warn("cron notify target failed", "platform", plat, "chat", chatID, "err", err)
		}
	}
}

// findByPrefix finds a job by ID prefix scoped to a specific chat.
func (s *Scheduler) findByPrefix(idPrefix, plat, chatID string) (*Job, error) {
	var matches []*Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID && strings.HasPrefix(j.ID, idPrefix) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("ambiguous prefix %q, matches: %s", idPrefix, strings.Join(ids, ", "))
	}
}

// marshalJobs is the JSON serializer used by marshalJobsLocked. It is a
// package-level var (not a direct json.Marshal call) so tests can inject a
// failure path to exercise ErrPersistFailed without needing to construct a
// cyclic graph in Job. Production always uses json.Marshal.
var marshalJobs = json.Marshal

// marshalJobsLocked serialises the current jobs map to JSON while the caller
// still holds s.mu. Round 47: replaces the map clone on every mutation. Safe
// because json.Marshal only reads Job fields (no mutation) and the output []byte
// is independent of s.jobs lifetime, so the caller can drop s.mu immediately.
// The (*Job).entryID field is unexported and therefore invisible to Marshal,
// so the runtime-only value never leaks into cron_jobs.json.
func (s *Scheduler) marshalJobsLocked() ([]byte, error) {
	entries := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		entries = append(entries, j)
	}
	return marshalJobs(entries)
}

// persistJobsLocked marshals under the caller's s.mu and writes asynchronously.
// Callers hold s.mu (write or read), invoke this to produce the byte payload
// and the save func, unlock, then call the save func. This keeps marshal
// latency in the critical section (needed for snapshot consistency) but moves
// disk I/O + storeMu contention outside.
//
// Return contract:
//   - On success, returns a non-nil save func and nil err. Caller must unlock
//     s.mu before invoking save() so disk I/O does not block the mutex.
//   - On marshal failure, returns (nil, ErrPersistFailed). Caller MUST plumb
//     the error back to the HTTP layer (e.g. map to 500) because the in-memory
//     mutation has already happened and is now unpersisted — a restart would
//     replay the prior on-disk state. marshal failure is only observable under
//     OOM or a broken Job schema; either way an alert-worthy event.
//
// R51-QUAL-001: previously this returned a no-op func on marshal failure,
// so every mutation appeared to succeed even when nothing reached disk.
func (s *Scheduler) persistJobsLocked() (func(), error) {
	data, err := s.marshalJobsLocked()
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	return func() { s.saveMarshaled(data) }, nil
}

// saveMarshaled persists an already-marshaled jobs payload to disk. Serialised
// by storeMu so an older snapshot cannot rename-win over a newer one — the
// .tmp file itself is now unique per call (os.CreateTemp in WriteFileAtomic),
// so storeMu is a logical ordering barrier rather than file-level protection.
func (s *Scheduler) saveMarshaled(data []byte) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		slog.Error("save cron store", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
}

// slogWriter adapts slog to io.Writer so robfig/cron's PrintfLogger can route
// through the project's structured logger instead of standard log.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	// Trim on the byte slice first so the single string conversion only
	// happens on the already-shortened payload — avoids one alloc per cron
	// log line.
	msg := string(bytes.TrimRight(p, "\n"))
	slog.Warn(msg)
	return len(p), nil
}
