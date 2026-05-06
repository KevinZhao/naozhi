// Live integration test against a real `claude` subprocess.
//
// This test mirrors the core passthrough logic from internal/cli/passthrough.go
// (slot matching, turn aggregation, head/follower fan-out) but writes NDJSON
// directly to claude's stdin / reads from its stdout without shim in between.
// Purpose: verify the CLI actually behaves the way our internal design
// assumes, before wiring the internal Process into the dispatch layer.
//
// Build tag `integration` so this doesn't run in CI-only go test ./... .
// Invoke with:
//   cd test/e2e/passthrough
//   CLAUDE_BIN=$(which claude) go test -tags=integration -v -timeout=5m -run=TestLive .
//
//go:build integration

package passthrough

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------ harness ------------------------------

type userContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type userMessageBody struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type userMessage struct {
	Type     string          `json:"type"`
	Message  userMessageBody `json:"message"`
	UUID     string          `json:"uuid,omitempty"`
	Priority string          `json:"priority,omitempty"`
}

type event struct {
	Type      string          `json:"type"`
	SubType   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Result    string          `json:"result,omitempty"`
	UUID      string          `json:"uuid,omitempty"`
	IsReplay  bool            `json:"isReplay,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
}

type slot struct {
	id        uint64
	uuid      string
	text      string
	priority  string
	resultCh  chan *slotResult
	errCh     chan error
	enqueueAt time.Time

	canceled bool
	replayed bool
}

type slotResult struct {
	text         string
	mergedCount  int
	mergedWithID uint64
	headText     string
}

// liveCLI hosts a real `claude` child process and the passthrough bookkeeping
// that mirrors internal/cli/passthrough.go behaviour.
type liveCLI struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu               sync.Mutex
	pending          []*slot
	currentTurnSlots []*slot
	nextID           uint64
	stopped          chan struct{}
}

func startLiveCLI(t *testing.T) *liveCLI {
	t.Helper()
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.Command(bin, "-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--replay-user-messages",
		"--setting-sources", "",
		"--dangerously-skip-permissions",
	)
	cmd.Env = append(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	cli := &liveCLI{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		stopped: make(chan struct{}),
	}
	go cli.readLoop()
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 8192), 1024*1024)
		for scanner.Scan() {
			t.Logf("[claude stderr] %s", scanner.Text())
		}
	}()
	return cli
}

func (c *liveCLI) close(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Log("claude subprocess did not exit in 8s; killing")
		_ = c.cmd.Process.Kill()
		<-done
	}
	close(c.stopped)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *liveCLI) send(text string, priority string) (*slot, error) {
	c.mu.Lock()
	if len(c.pending) >= 16 {
		c.mu.Unlock()
		return nil, errors.New("pending slots full")
	}
	c.nextID++
	s := &slot{
		id:        c.nextID,
		uuid:      newUUID(),
		text:      text,
		priority:  priority,
		resultCh:  make(chan *slotResult, 1),
		errCh:     make(chan error, 1),
		enqueueAt: time.Now(),
	}
	c.pending = append(c.pending, s)
	c.mu.Unlock()

	msg := userMessage{
		Type:     "user",
		Message:  userMessageBody{Role: "user", Content: text},
		UUID:     s.uuid,
		Priority: priority,
	}
	data, _ := json.Marshal(msg)
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("stdin write: %w", err)
	}
	return s, nil
}

func (c *liveCLI) wait(ctx context.Context, s *slot) (*slotResult, error) {
	select {
	case r := <-s.resultCh:
		return r, nil
	case err := <-s.errCh:
		return nil, err
	case <-ctx.Done():
		c.mu.Lock()
		s.canceled = true
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// readLoop: parse stdout, drive turn aggregation + fan-out.
func (c *liveCLI) readLoop() {
	reader := bufio.NewReader(c.stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			// CLI exited — fire ErrProcessExited on all pending.
			c.mu.Lock()
			victims := c.pending
			c.pending = nil
			c.currentTurnSlots = nil
			c.mu.Unlock()
			for _, s := range victims {
				if s.canceled {
					continue
				}
				select {
				case s.errCh <- errors.New("process exited"):
				default:
				}
			}
			return
		}
	}
}

func (c *liveCLI) handleLine(line []byte) {
	var ev event
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	if os.Getenv("LIVE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[live<-] type=%s sub=%s isReplay=%v uuid=%.8s result=%q\n",
			ev.Type, ev.SubType, ev.IsReplay, ev.UUID, truncate(ev.Result, 80))
		if ev.Type == "user" {
			txs := extractReplayTexts(ev.Message)
			for i, tx := range txs {
				fmt.Fprintf(os.Stderr, "  content[%d] (len=%d)=%q\n", i, len(tx), truncate(tx, 200))
			}
		}
	}

	// system/init starts a new turn clock but does NOT reset currentTurnSlots:
	// the CLI emits independent replays for queued messages *between* turns
	// (after prev result, before next init). Those claims must survive.
	if ev.Type == "system" && ev.SubType == "init" {
		return
	}

	// user replay: uuid match → independent claim; uuid miss → merged sweep
	if ev.Type == "user" && ev.IsReplay {
		c.mu.Lock()
		if s := c.findByUUIDLocked(ev.UUID); s != nil {
			if !s.replayed {
				s.replayed = true
				c.currentTurnSlots = append(c.currentTurnSlots, s)
			}
		} else {
			// Merged replay — sweep every unclaimed pending slot.
			for _, s := range c.pending {
				if s.replayed {
					continue
				}
				s.replayed = true
				c.currentTurnSlots = append(c.currentTurnSlots, s)
			}
		}
		c.mu.Unlock()
		return
	}

	if ev.Type == "result" {
		if ev.SubType == "error_during_execution" {
			// Pending slots that never got replayed AND are not the
			// priority:"now" triggers themselves were preempted.
			c.mu.Lock()
			var victims []*slot
			kept := c.pending[:0]
			for _, s := range c.pending {
				if !s.replayed && s.priority != "now" {
					victims = append(victims, s)
				} else {
					kept = append(kept, s)
				}
			}
			c.pending = kept
			c.mu.Unlock()
			for _, s := range victims {
				if s.canceled {
					continue
				}
				select {
				case s.errCh <- errors.New("aborted by priority:now preemption"):
				default:
				}
			}
		}
		c.mu.Lock()
		owners := c.currentTurnSlots
		c.currentTurnSlots = nil
		c.removeSlotsLocked(owners)
		c.mu.Unlock()
		fanout(owners, ev)
		return
	}
}

func (c *liveCLI) findByUUIDLocked(uuid string) *slot {
	for _, s := range c.pending {
		if s.uuid == uuid {
			return s
		}
	}
	return nil
}

func (c *liveCLI) matchMergedLocked(texts []string) []*slot {
	matched := make([]*slot, 0, len(texts))
	used := make(map[uint64]bool)
	for _, want := range texts {
		for _, s := range c.pending {
			if s.replayed || used[s.id] {
				continue
			}
			if s.text == want {
				s.replayed = true
				used[s.id] = true
				matched = append(matched, s)
				break
			}
		}
	}
	return matched
}

func (c *liveCLI) removeSlotsLocked(victims []*slot) {
	if len(victims) == 0 {
		return
	}
	victimSet := make(map[uint64]bool)
	for _, v := range victims {
		victimSet[v.id] = true
	}
	kept := c.pending[:0]
	for _, s := range c.pending {
		if !victimSet[s.id] {
			kept = append(kept, s)
		}
	}
	c.pending = kept
}

func extractReplayTexts(raw json.RawMessage) []string {
	// Two CLI shapes for the "content" field:
	//   1. []{type,text,...} — normal block array
	//   2. plain string      — replay of a text-only user message
	var body struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	if len(body.Content) == 0 {
		return nil
	}
	var asStr string
	if err := json.Unmarshal(body.Content, &asStr); err == nil {
		return []string{asStr}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body.Content, &blocks); err != nil {
		return nil
	}
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

func fanout(owners []*slot, ev event) {
	if len(owners) == 0 {
		return
	}
	head := owners[0]
	mergedCount := len(owners)
	deliver(head, &slotResult{
		text:        ev.Result,
		mergedCount: mergedCount,
	})
	for _, s := range owners[1:] {
		deliver(s, &slotResult{
			text:         "",
			mergedCount:  mergedCount,
			mergedWithID: head.id,
			headText:     ev.Result,
		})
	}
}

func deliver(s *slot, r *slotResult) {
	if s.canceled {
		return
	}
	select {
	case s.resultCh <- r:
	default:
	}
}

// ------------------------------ Tests ------------------------------

func TestLive_SingleMessage(t *testing.T) {
	if os.Getenv("CLAUDE_BIN") == "" && !hasClaude() {
		t.Skip("claude binary not on PATH and CLAUDE_BIN not set")
	}
	cli := startLiveCLI(t)
	defer cli.close(t)

	s, err := cli.send("Reply with just the word GREEN, nothing else.", "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := cli.wait(ctx, s)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(strings.ToUpper(r.text), "GREEN") {
		t.Errorf("result text = %q, want to contain GREEN", r.text)
	}
	if r.mergedCount != 1 {
		t.Errorf("merged = %d, want 1", r.mergedCount)
	}
	t.Logf("SingleMessage: got result %q merged=%d", r.text, r.mergedCount)
}

func TestLive_TwoSequentialMessages(t *testing.T) {
	if os.Getenv("CLAUDE_BIN") == "" && !hasClaude() {
		t.Skip("no claude")
	}
	cli := startLiveCLI(t)
	defer cli.close(t)

	// first
	s1, err := cli.send("Reply with just ONE, nothing else.", "")
	if err != nil {
		t.Fatalf("send1: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r1, err := cli.wait(ctx, s1)
	if err != nil {
		t.Fatalf("wait1: %v", err)
	}
	// second (after r1 returns)
	s2, err := cli.send("Reply with just TWO, nothing else.", "")
	if err != nil {
		t.Fatalf("send2: %v", err)
	}
	r2, err := cli.wait(ctx, s2)
	if err != nil {
		t.Fatalf("wait2: %v", err)
	}
	t.Logf("seq: r1=%q r2=%q", r1.text, r2.text)
	if !strings.Contains(strings.ToUpper(r1.text), "ONE") {
		t.Errorf("r1 missing ONE: %q", r1.text)
	}
	if !strings.Contains(strings.ToUpper(r2.text), "TWO") {
		t.Errorf("r2 missing TWO: %q", r2.text)
	}
}

// TestLive_BurstCoalesces — send 5 rapidly; expect CLI merges them into one
// turn. Exactly one slot becomes head (full text); the rest follow.
func TestLive_BurstCoalesces(t *testing.T) {
	if os.Getenv("CLAUDE_BIN") == "" && !hasClaude() {
		t.Skip("no claude")
	}
	cli := startLiveCLI(t)
	defer cli.close(t)

	words := []string{"APPLE", "BANANA", "CHERRY", "DATE", "ELDERBERRY"}
	var slots []*slot
	for _, w := range words {
		s, err := cli.send(fmt.Sprintf("Reply with just the word %s, nothing else.", w), "")
		if err != nil {
			t.Fatalf("send %s: %v", w, err)
		}
		slots = append(slots, s)
		time.Sleep(50 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var results []*slotResult
	for i, s := range slots {
		r, err := cli.wait(ctx, s)
		if err != nil {
			t.Fatalf("wait[%d]: %v", i, err)
		}
		results = append(results, r)
	}

	// Check head/follower semantics
	var heads, followers int
	for i, r := range results {
		if r.text != "" {
			heads++
			t.Logf("[%d] %s head merged=%d text=%q", i, words[i], r.mergedCount, truncate(r.text, 100))
		} else {
			followers++
			t.Logf("[%d] %s follower merged=%d head_id=%d head=%q", i, words[i], r.mergedCount, r.mergedWithID, truncate(r.headText, 100))
		}
	}
	if heads < 1 {
		t.Errorf("no head slots; expected at least 1")
	}
	if heads+followers != len(words) {
		t.Errorf("slot count mismatch: heads=%d followers=%d want total=%d", heads, followers, len(words))
	}
}

// TestLive_PriorityNowAborts — verify that priority:"now" aborts an in-flight
// long task. The preempted long slot should error with "aborted by
// priority:now preemption"; the urgent slot should succeed with PIVOT.
func TestLive_PriorityNowAborts(t *testing.T) {
	if os.Getenv("CLAUDE_BIN") == "" && !hasClaude() {
		t.Skip("no claude")
	}
	cli := startLiveCLI(t)
	defer cli.close(t)

	sLong, err := cli.send("Please run this bash: for i in $(seq 1 20); do echo tick=$i; sleep 2; done. Then say DONE.", "")
	if err != nil {
		t.Fatalf("send long: %v", err)
	}
	time.Sleep(3 * time.Second)
	sUrgent, err := cli.send("URGENT: stop everything and reply with just PIVOT.", "now")
	if err != nil {
		t.Fatalf("send urgent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startLong := time.Now()
	rLong, errLong := cli.wait(ctx, sLong)
	longDur := time.Since(startLong)
	rUrgent, errUrgent := cli.wait(ctx, sUrgent)

	t.Logf("long elapsed=%s err=%v", longDur, errLong)
	if errUrgent != nil {
		t.Fatalf("urgent err = %v", errUrgent)
	}
	t.Logf("urgent text=%q", truncate(rUrgent.text, 80))

	// Long slot should have errored with aborted-by-urgent. A successful
	// completion would mean priority:now failed to preempt.
	if errLong == nil {
		t.Errorf("long slot succeeded (text=%q); expected aborted-by-urgent", truncate(rLong.text, 80))
	} else if !strings.Contains(errLong.Error(), "aborted") {
		t.Errorf("long err = %v, want aborted-by-urgent", errLong)
	}
	if longDur > 25*time.Second {
		t.Errorf("long took %s; urgent did not preempt quickly", longDur)
	}
	if !strings.Contains(strings.ToUpper(rUrgent.text), "PIVOT") {
		t.Errorf("urgent reply missing PIVOT: %q", rUrgent.text)
	}
}

// TestLive_CtxCancelTombstone — cancel one slot mid-flight, verify the next
// slot still gets its own result (FIFO unbroken).
func TestLive_CtxCancelTombstone(t *testing.T) {
	if os.Getenv("CLAUDE_BIN") == "" && !hasClaude() {
		t.Skip("no claude")
	}
	cli := startLiveCLI(t)
	defer cli.close(t)

	ctxA, cancelA := context.WithCancel(context.Background())
	sA, err := cli.send("Take your time, then reply with just MORNING.", "")
	if err != nil {
		t.Fatalf("sendA: %v", err)
	}

	// Cancel A shortly; A's result returns ctx.Canceled
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelA()
	}()
	_, err = cli.wait(ctxA, sA)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait A err = %v, want canceled", err)
	}

	// Even though CLI's result for A may arrive (if it was already in flight),
	// we expect B to still receive its own result.
	time.Sleep(1 * time.Second)
	sB, err := cli.send("Reply with just EVENING.", "")
	if err != nil {
		t.Fatalf("sendB: %v", err)
	}
	ctxB, cancelB := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelB()
	rB, err := cli.wait(ctxB, sB)
	if err != nil {
		t.Fatalf("waitB: %v", err)
	}
	t.Logf("B result: %q", rB.text)
	if !strings.Contains(strings.ToUpper(rB.text), "EVENING") {
		t.Errorf("B result missing EVENING: %q", rB.text)
	}
}

// ------------------------------ helpers ------------------------------

func hasClaude() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
