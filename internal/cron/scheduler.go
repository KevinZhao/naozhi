package cron

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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

// Scheduler manages cron jobs and executes them on schedule.
type Scheduler struct {
	cron          *robfigcron.Cron
	mu            sync.Mutex
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
	return &Scheduler{
		cron:          robfigcron.New(),
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
	defer s.mu.Unlock()
	if err := saveJobs(s.storePath, s.jobs); err != nil {
		slog.Error("save cron store on shutdown", "err", err)
	}
}

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.maxJobs {
		return fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	j.ID = generateID()
	j.CreatedAt = time.Now()

	if err := s.registerJob(j); err != nil {
		return err
	}
	s.jobs[j.ID] = j
	s.save()
	return nil
}

// ListJobs returns jobs for a specific chat.
func (s *Scheduler) ListJobs(plat, chatID string) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []*Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID {
			result = append(result, j)
		}
	}
	return result
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		return nil, err
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
	}
	if s.router != nil {
		s.router.Reset("cron:" + j.ID)
	}
	delete(s.jobs, j.ID)
	s.save()
	return j, nil
}

// PauseJob pauses a job by ID prefix.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		return nil, err
	}
	if j.Paused {
		return nil, fmt.Errorf("job %s already paused", j.ID)
	}

	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
	j.Paused = true
	s.save()
	return j, nil
}

// ResumeJob resumes a paused job by ID prefix.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		return nil, err
	}
	if !j.Paused {
		return nil, fmt.Errorf("job %s is not paused", j.ID)
	}

	if err := s.registerJob(j); err != nil {
		return nil, err
	}
	j.Paused = false
	s.save()
	return j, nil
}

// NextRun returns the next scheduled run time for a job.
func (s *Scheduler) NextRun(j *Job) time.Time {
	s.mu.Lock()
	entryID := j.entryID
	s.mu.Unlock()
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
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
	log := slog.With("cron_id", j.ID, "platform", j.Platform, "chat", j.ChatID)
	log.Info("cron job executing", "prompt_len", len(j.Prompt))

	p := s.platforms[j.Platform]
	if p == nil {
		log.Error("platform not found for cron job")
		return
	}

	ctx, cancel := context.WithTimeout(s.stopCtx, s.execTimeout)
	defer cancel()

	agentID, cleanText := routing.ResolveAgent(j.Prompt, s.agentCommands)
	opts := s.agents[agentID]
	key := "cron:" + j.ID

	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		log.Error("cron session error", "err", err)
		if _, rerr := p.Reply(ctx, platform.OutgoingMessage{
			ChatID: j.ChatID,
			Text:   fmt.Sprintf("[Cron %s] skipped: %s", j.ID, err),
		}); rerr != nil {
			log.Error("reply failed", "err", rerr)
		}
		return
	}

	result, err := sess.Send(ctx, cleanText, nil, nil)
	if err != nil {
		log.Error("cron send error", "err", err)
		if _, rerr := p.Reply(ctx, platform.OutgoingMessage{
			ChatID: j.ChatID,
			Text:   fmt.Sprintf("[Cron %s] failed: %s", j.ID, err),
		}); rerr != nil {
			log.Error("reply failed", "err", rerr)
		}
		return
	}

	log.Info("cron job completed", "result_len", len(result.Text))
	if _, rerr := p.Reply(ctx, platform.OutgoingMessage{
		ChatID: j.ChatID,
		Text:   fmt.Sprintf("[Cron %s] %s", j.ID, result.Text),
	}); rerr != nil {
		log.Error("reply failed", "err", rerr)
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

func (s *Scheduler) save() {
	if err := saveJobs(s.storePath, s.jobs); err != nil {
		slog.Error("save cron store", "err", err)
	}
}
