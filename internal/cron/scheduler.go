package cron

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"

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

// SchedulerConfig holds configuration for the cron scheduler.
type SchedulerConfig struct {
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	StorePath     string
	MaxJobs       int
	ExecTimeout   time.Duration
	// Location is the timezone in which schedule expressions are evaluated.
	// nil defaults to time.Local so cron expressions match wall-clock time
	// on the host (respects $TZ / /etc/localtime).
	Location *time.Location
	// NotifyDefault provides a fallback IM target for jobs that opt into
	// notifications (Job.Notify == true) but have no per-job target set.
	// Empty Platform or ChatID disables the default.
	NotifyDefault NotifyTarget
	// ParentCtx, if set, is used as the parent for the scheduler's internal stop context.
	// When it is cancelled (e.g. during application shutdown) all running cron jobs are
	// interrupted promptly.
	ParentCtx context.Context
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
	router        *session.Router
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
		stopCtx:       stopCtx,
		stopCancel:    stopCancel,
	}
}

// NotifyDefault returns the configured fallback IM target so the dashboard can
// show users where a "notify on completion" toggle will deliver messages.
func (s *Scheduler) NotifyDefault() NotifyTarget { return s.notifyDefault }

// Start loads persisted jobs and starts the cron scheduler.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	var restoredJobs []*Job
	if restored := loadJobs(s.storePath); restored != nil {
		for _, j := range restored {
			if j.Paused {
				s.jobs[j.ID] = j
				restoredJobs = append(restoredJobs, j)
				continue
			}
			if err := s.registerJob(j); err != nil {
				slog.Warn("skip invalid cron job", "id", j.ID, "schedule", j.Schedule, "err", err)
				continue
			}
			s.jobs[j.ID] = j
			restoredJobs = append(restoredJobs, j)
		}
	}
	s.mu.Unlock()
	// Register dashboard stub sessions after releasing the lock; the router's
	// notifyChange callback must not re-enter scheduler state.
	for _, j := range restoredJobs {
		s.registerStub(j)
	}
	s.cron.Start()
	slog.Info("cron scheduler started", "jobs", len(s.jobs))
	return nil
}

// registerStub creates (or refreshes) a router session entry for the job so it
// appears in the dashboard workspace list. Safe to call without a router (tests).
func (s *Scheduler) registerStub(j *Job) {
	if s.router == nil {
		return
	}
	s.router.RegisterCronStub("cron:"+j.ID, j.WorkDir, j.Prompt)
}

// Stop halts the scheduler and saves state. It waits for both scheduled jobs
// (drained by s.cron.Stop) and any TriggerNow-spawned goroutines before
// returning, so callers can safely tear down the router afterwards.
func (s *Scheduler) Stop() {
	s.stopCancel()
	ctx := s.cron.Stop()
	<-ctx.Done()
	s.triggerWG.Wait()
	s.mu.Lock()
	snap := s.snapshotJobs()
	s.mu.Unlock()
	if err := saveJobs(s.storePath, snap); err != nil {
		slog.Error("save cron store on shutdown", "err", err)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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

// ListAllJobsWithNextRun returns every job plus its next scheduled run, computed
// inside a single RLock acquisition. This avoids the O(N) lock churn of calling
// NextRunByID in a loop from the dashboard handler.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]JobWithNextRun, 0, len(s.jobs))
	for _, j := range s.jobs {
		var next time.Time
		if j.entryID != 0 {
			next = s.cron.Entry(j.entryID).Next
		}
		result = append(result, JobWithNextRun{Job: *j, NextRun: next})
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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

	snap := s.snapshotJobs()
	// Value-copy while still under lock so the caller sees a stable result
	// even if another goroutine mutates the job right after we unlock.
	result := *j
	s.mu.Unlock()

	s.saveSnapshot(snap)
	// Pass the snapshotted value (via result) to registerStub so a concurrent
	// SetJobPrompt cannot tear the Prompt/WorkDir pointers we read.
	s.registerStub(&result)
	slog.Info("cron job updated", "id", id,
		"schedule_changed", upd.Schedule != nil,
		"prompt_changed", upd.Prompt != nil,
		"workdir_changed", upd.WorkDir != nil)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
	snap := s.snapshotJobs()
	s.mu.Unlock()

	s.saveSnapshot(snap)
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
		go func() {
			defer s.triggerWG.Done()
			s.execute(j)
		}()
	}
	return nil
}

// registerJob registers a job with the robfig/cron scheduler.
func (s *Scheduler) registerJob(j *Job) error {
	entryID, err := s.cron.AddFunc(j.Schedule, func() {
		s.execute(j)
	})
	if err != nil {
		return fmt.Errorf("register cron: %w", err)
	}
	j.entryID = entryID
	return nil
}

// execute runs a cron job: send prompt to session, post result to chat.
func (s *Scheduler) execute(j *Job) {
	// Snapshot mutable fields under lock to avoid data race with SetJobPrompt.
	s.mu.Lock()
	prompt := j.Prompt
	workDir := j.WorkDir
	jobID := j.ID
	platName := j.Platform
	chatID := j.ChatID
	notifyPlat := j.NotifyPlatform
	notifyChat := j.NotifyChatID
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

	log := slog.With("cron_id", jobID, "platform", platName, "chat", chatID)
	log.Info("cron job executing", "prompt_len", len(prompt))
	execStart := time.Now()

	ctx, cancel := context.WithTimeout(s.stopCtx, s.execTimeout)
	defer cancel()

	agentID, cleanText := session.ResolveAgent(prompt, s.agentCommands)
	opts := s.agents[agentID]
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	if workDir != "" {
		opts.Workspace = filepath.Clean(workDir)
	}
	key := "cron:" + jobID

	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled mid-flight (graceful shutdown or job
			// deletion overlapping execute). The job will either be re-run
			// on the next tick or is intentionally gone; either way an IM
			// notification would be spam and the stored LastError would
			// falsely blame the job itself.
			log.Info("cron session cancelled", "err", err)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			log.Info("cron session deadline exceeded", "err", err)
		} else {
			log.Error("cron session error", "err", err)
		}
		s.recordResult(j, "", "session error: "+err.Error())
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行跳过，请稍后重试。", jobID))
		return
	}

	// Direct Send without sendWithBroadcast — cron jobs notify via onExecute callback instead.
	result, err := sess.Send(ctx, cleanText, nil, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures.
			log.Info("cron send cancelled", "err", err)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			log.Info("cron send deadline exceeded", "err", err)
		} else {
			log.Error("cron send error", "err", err)
		}
		s.recordResult(j, "", "send error: "+err.Error())
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行失败，请稍后重试。", jobID))
		return
	}

	log.Info("cron job completed",
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
	s.mu.Lock()
	j.LastRunAt = time.Now()
	j.LastResult = result
	j.LastError = errMsg
	snap := s.snapshotJobs()
	fn := s.onExecute
	s.mu.Unlock()

	s.saveSnapshot(snap)
	if fn != nil {
		fn(j.ID, result, errMsg)
	}
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

// snapshotJobs returns a deep copy of the jobs map, safe to use outside the lock.
// Each Job is value-copied so the caller can read fields without holding mu.
func (s *Scheduler) snapshotJobs() map[string]*Job {
	snap := make(map[string]*Job, len(s.jobs))
	for k, v := range s.jobs {
		jCopy := *v
		jCopy.entryID = 0 // runtime-only, not persisted
		snap[k] = &jCopy
	}
	return snap
}

// saveSnapshot persists a jobs snapshot to disk. No lock required.
func (s *Scheduler) saveSnapshot(snapshot map[string]*Job) {
	if err := saveJobs(s.storePath, snapshot); err != nil {
		slog.Error("save cron store", "err", err)
	}
}

// slogWriter adapts slog to io.Writer so robfig/cron's PrintfLogger can route
// through the project's structured logger instead of standard log.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	slog.Warn(msg)
	return len(p), nil
}
