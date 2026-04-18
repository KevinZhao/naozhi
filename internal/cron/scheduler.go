package cron

import (
	"context"
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
	"github.com/naozhi/naozhi/internal/routing"
	"github.com/naozhi/naozhi/internal/session"
)

// SchedulerConfig holds configuration for the cron scheduler.
type SchedulerConfig struct {
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	StorePath     string
	MaxJobs       int
	ExecTimeout   time.Duration
}

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
	stopCtx       context.Context
	stopCancel    context.CancelFunc
	onExecute     OnExecuteFunc
}

// SetOnExecute registers a callback invoked after each cron job execution.
func (s *Scheduler) SetOnExecute(fn OnExecuteFunc) {
	s.mu.Lock()
	s.onExecute = fn
	s.mu.Unlock()
}

// NewScheduler creates a scheduler. Call Start() to begin.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 50
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 5 * time.Minute
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	cronLogger := robfigcron.PrintfLogger(log.New(slogWriter{}, "cron: ", 0))
	return &Scheduler{
		cron: robfigcron.New(robfigcron.WithChain(
			robfigcron.Recover(cronLogger),
			robfigcron.SkipIfStillRunning(cronLogger),
		)),
		jobs:          make(map[string]*Job),
		router:        cfg.Router,
		platforms:     cfg.Platforms,
		agents:        cfg.Agents,
		agentCommands: cfg.AgentCommands,
		storePath:     cfg.StorePath,
		maxJobs:       cfg.MaxJobs,
		execTimeout:   cfg.ExecTimeout,
		stopCtx:       stopCtx,
		stopCancel:    stopCancel,
	}
}

// Start loads persisted jobs and starts the cron scheduler.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	if restored := loadJobs(s.storePath); restored != nil {
		for _, j := range restored {
			if j.Paused {
				s.jobs[j.ID] = j
				continue
			}
			if err := s.registerJob(j); err != nil {
				slog.Warn("skip invalid cron job", "id", j.ID, "schedule", j.Schedule, "err", err)
				continue
			}
			s.jobs[j.ID] = j
		}
	}
	s.mu.Unlock()
	s.cron.Start()
	slog.Info("cron scheduler started", "jobs", len(s.jobs))
	return nil
}

// Stop halts the scheduler and saves state.
func (s *Scheduler) Stop() {
	s.stopCancel()
	ctx := s.cron.Stop()
	<-ctx.Done()
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

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("no job found with id %q", id)
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
		return nil, fmt.Errorf("no job found with id %q", id)
	}
	if j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("job %s already paused", j.ID)
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
		return nil, fmt.Errorf("no job found with id %q", id)
	}
	if !j.Paused {
		s.mu.Unlock()
		return nil, fmt.Errorf("job %s is not paused", j.ID)
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
		return fmt.Errorf("no job found with id %q", id)
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

// PreviewSchedule validates a schedule expression and returns the next run time.
func PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now()), nil
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
		return nil, fmt.Errorf("job %s already paused", j.ID)
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
		return nil, fmt.Errorf("job %s is not paused", j.ID)
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
		return fmt.Errorf("no job found with id %q", id)
	}
	if j.Paused {
		s.mu.Unlock()
		return fmt.Errorf("job %s is paused", id)
	}
	if j.Prompt == "" {
		s.mu.Unlock()
		return fmt.Errorf("job %s has no prompt", id)
	}
	entryID := j.entryID
	s.mu.Unlock()

	if entryID != 0 {
		// Run through the cron chain (SkipIfStillRunning + Recover) to prevent
		// double-execution when TriggerNow overlaps with a scheduled tick.
		// Guard: a concurrent DeleteJob may remove the entry between our Unlock
		// and this lookup, causing Entry() to return a zero-value with nil WrappedJob.
		entry := s.cron.Entry(entryID)
		if entry.WrappedJob != nil {
			go entry.WrappedJob.Run()
		} else {
			// Entry was concurrently deleted — skip execution.
			slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "id", id, "entry_id", entryID)
		}
	} else {
		go s.execute(j)
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
	s.mu.Unlock()

	log := slog.With("cron_id", jobID, "platform", platName, "chat", chatID)
	log.Info("cron job executing", "prompt_len", len(prompt))

	ctx, cancel := context.WithTimeout(s.stopCtx, s.execTimeout)
	defer cancel()

	agentID, cleanText := routing.ResolveAgent(prompt, s.agentCommands)
	opts := s.agents[agentID]
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	if workDir != "" {
		opts.Workspace = filepath.Clean(workDir)
	}
	key := "cron:" + jobID

	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		log.Error("cron session error", "err", err)
		s.recordResult(j, "", "session error: "+err.Error())
		s.notifyIM(j, fmt.Sprintf("[Cron %s] 执行跳过，请稍后重试。", jobID))
		return
	}

	// Direct Send without sendWithBroadcast — cron jobs notify via onExecute callback instead.
	result, err := sess.Send(ctx, cleanText, nil, nil)
	if err != nil {
		log.Error("cron send error", "err", err)
		s.recordResult(j, "", "send error: "+err.Error())
		s.notifyIM(j, fmt.Sprintf("[Cron %s] 执行失败，请稍后重试。", jobID))
		return
	}

	log.Info("cron job completed", "result_len", len(result.Text))
	s.recordResult(j, result.Text, "")

	// Send result to the job's own IM channel (for IM-created jobs)
	replyText := fmt.Sprintf("[Cron %s] %s", jobID, result.Text)
	s.notifyIM(j, replyText)

	// Send to optional notify target (for dashboard-created jobs that want IM push)
	if notifyPlat != "" && notifyChat != "" &&
		(notifyPlat != platName || notifyChat != chatID) {
		s.notifyTarget(notifyPlat, notifyChat, replyText)
	}
}

// recordResult persists the last execution result on the job and invokes the onExecute callback.
func (s *Scheduler) recordResult(j *Job, result, errMsg string) {
	const maxStoredRunes = 4 * 1024
	if utf8.RuneCountInString(result) > maxStoredRunes {
		result = string([]rune(result)[:maxStoredRunes]) + "…[truncated]"
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

// notifyIM sends a message to the job's own platform/chat. Skips if platform is nil (e.g. dashboard jobs).
func (s *Scheduler) notifyIM(j *Job, text string) {
	p := s.platforms[j.Platform]
	if p == nil {
		return
	}
	replyCtx, replyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer replyCancel()
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = 4000
	}
	chunks := platform.SplitText(text, maxLen)
	for _, chunk := range chunks {
		if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{
			ChatID: j.ChatID,
			Text:   chunk,
		}, 3); err != nil {
			slog.Warn("cron notify failed", "cron_id", j.ID, "chat", j.ChatID, "err", err)
		}
	}
}

// notifyTarget sends a message to an arbitrary platform/chat (notify target).
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	p := s.platforms[plat]
	if p == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
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
		return nil, fmt.Errorf("no job found with prefix %q", idPrefix)
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
