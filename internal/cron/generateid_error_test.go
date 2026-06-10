package cron

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
)

// generateid_error_test.go covers the crypto/rand failure path that
// generateHexID propagates as error (#706 / R242-CR-14). The historical
// implementation panicked from arbitrary caller stacks (AddJob,
// executeOpt's tick goroutine); these tests pin the new contract that
// callers receive a wrapped error and the process stays up.
//
// All tests in this file MUST be serial — they swap the package-level
// crypto/rand.Reader, which is shared with every other goroutine in the
// test binary. t.Parallel would race against any concurrent test that
// also uses crypto/rand.

// errReader returns the configured error on every Read; used to simulate
// kernel getrandom(2) failure in tests without touching the real OS
// entropy pool.
type errReader struct{ err error }

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

// withFailingRandReader swaps crypto/rand.Reader for the test duration.
// The cleanup restores the original Reader even on t.Fatal.
func withFailingRandReader(t *testing.T, err error) {
	t.Helper()
	orig := rand.Reader
	rand.Reader = errReader{err: err}
	t.Cleanup(func() { rand.Reader = orig })
}

func TestGenerateHexID_RandFailure(t *testing.T) {
	sentinel := errors.New("getrandom: synthetic test failure")
	withFailingRandReader(t, sentinel)

	id, err := generateHexID()
	if err == nil {
		t.Fatalf("generateHexID: expected error, got id=%q nil err", id)
	}
	if id != "" {
		t.Errorf("generateHexID: expected empty id on error, got %q", id)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("generateHexID: expected wrapped sentinel via errors.Is, got %v", err)
	}
	if !strings.Contains(err.Error(), "crypto/rand unavailable") {
		t.Errorf("generateHexID: expected error to mention crypto/rand, got %q", err.Error())
	}
}

func TestGenerateID_RandFailure(t *testing.T) {
	withFailingRandReader(t, io.ErrUnexpectedEOF)

	id, err := generateID()
	if err == nil {
		t.Fatalf("generateID: expected error, got id=%q nil err", id)
	}
	if id != "" {
		t.Errorf("generateID: expected empty id on error, got %q", id)
	}
}

func TestGenerateRunID_RandFailure(t *testing.T) {
	withFailingRandReader(t, io.ErrUnexpectedEOF)

	id, err := generateRunID()
	if err == nil {
		t.Fatalf("generateRunID: expected error, got id=%q nil err", id)
	}
	if id != "" {
		t.Errorf("generateRunID: expected empty id on error, got %q", id)
	}
}

// TestAddJob_RandFailurePropagates pins the AddJob contract: a crypto/rand
// failure during job-ID generation surfaces as a normal error return,
// not a panic. This is the original symptom in issue #706 — historically
// AddJob's caller (dashboard handler / IM message router) saw the panic
// unwind through their goroutine.
func TestAddJob_RandFailurePropagates(t *testing.T) {
	sentinel := errors.New("getrandom: synthetic test failure")
	withFailingRandReader(t, sentinel)

	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	j := &Job{
		Schedule: "@every 5m",
		Prompt:   "test",
		Title:    "rand-failure-canary",
	}
	err := s.AddJob(j)
	if err == nil {
		t.Fatalf("AddJob: expected error, got nil (and j.ID=%q)", j.ID)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("AddJob: expected wrapped sentinel via errors.Is, got %v", err)
	}
	if !strings.Contains(err.Error(), "generate job id") {
		t.Errorf("AddJob: expected error to mention 'generate job id', got %q", err.Error())
	}
	// AddJob must not have inserted anything into the in-memory map: the
	// caller's view (failure -> nothing happened) must hold.
	s.mu.Lock()
	jobCount := len(s.jobs)
	s.mu.Unlock()
	if jobCount != 0 {
		t.Errorf("AddJob: expected 0 jobs in map after rand failure, got %d", jobCount)
	}
}
