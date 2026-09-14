package cron

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestAppend_OverCapPaths_LogDistinguishably pins R20260602-CR-2: Append has
// three over-cap outcomes and log-based alerting must be able to tell them
// apart. Before the fix the preflight path and the post-marshal retry emitted
// the identical string, so an operator could not tell whether truncation
// happened early (no first marshal) or late (marshal succeeded, payload still
// too big).
//
// This replaced a source anchor that read runstore.go and asserted the two
// message literals appear in it, plus a third check comparing two Go constants
// to each other — which the compiler already settles, so it could never fail
// (#2547). Neither the anchor nor its constants established that the paths are
// reachable or that a given run takes the one you think it does; this does, by
// driving each path and reading what actually came out of slog.
//
// Not t.Parallel: slog.SetDefault is process-global.
func TestAppend_OverCapPaths_LogDistinguishably(t *testing.T) {
	cases := []struct {
		name string
		// setup returns a store and the run to append.
		setup func(t *testing.T) (*runStore, *CronRun)
		// wantSubstr identifies the path by the message it must emit.
		wantSubstr string
	}{
		{
			// Result alone overshoots cap-headroom, so Append skips the doomed
			// first marshal (#1111).
			name:       "preflight over-cap",
			wantSubstr: "preflight over-cap",
			setup: func(t *testing.T) (*runStore, *CronRun) {
				s := newTestStore(t, 200, 30*24*time.Hour)
				s.maxRunBytes = 2048
				run := makeRun(mustGenerateID(), time.Now())
				run.Result = strings.Repeat("Y", 8192)
				return s, run
			},
		},
		{
			// Under the preflight threshold (Result+Prompt+ErrorMsg fits in
			// cap-headroom) but the marshalled payload does not, so the
			// post-marshal gate fires instead.
			name:       "post-marshal retry",
			wantSubstr: "payload exceeds size cap",
			setup: func(t *testing.T) (*runStore, *CronRun) {
				s := newTestStore(t, 200, 30*24*time.Hour)
				s.maxRunBytes = 2048
				run := makeRun(mustGenerateID(), time.Now())
				// Result is under the preflight threshold (cap minus the 1024
				// headroom), so preflightOverCap is false. WorkDir is not in
				// the preflight sum but is in the payload, which is what pushes
				// the marshalled record over the cap — the "metadata alone"
				// gap the drop path's comment names. Truncating Result to 256
				// runes brings it back under, so this case lands rather than
				// dropping.
				run.Result = strings.Repeat("Z", 900)
				run.WorkDir = "/" + strings.Repeat("d", 1000)
				return s, run
			},
		},
		{
			// Cap below the fixed metadata, so even the truncated record does
			// not fit and the run is dropped.
			name:       "dropped, truncated still over cap",
			wantSubstr: "run record dropped",
			setup: func(t *testing.T) (*runStore, *CronRun) {
				s := newTestStore(t, 200, 30*24*time.Hour)
				s.maxRunBytes = 40
				run := makeRun(mustGenerateID(), time.Now())
				run.Result = strings.Repeat("W", 4096)
				return s, run
			},
		},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf strings.Builder
			origDefault := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(origDefault) })

			s, run := tc.setup(t)
			s.Append(run)

			out := logBuf.String()
			if !strings.Contains(out, tc.wantSubstr) {
				t.Fatalf("%s path did not log %q, so this case is not exercising it.\ngot:\n%s",
					tc.name, tc.wantSubstr, out)
			}
			// Record the whole message line so the cross-case comparison below
			// works on what an operator's log filter actually sees.
			for line := range strings.SplitSeq(out, "\n") {
				if strings.Contains(line, tc.wantSubstr) {
					seen[tc.name] = msgField(line)
					break
				}
			}
		})
	}

	if len(seen) != len(cases) {
		t.Fatalf("only %d of %d paths logged; cannot compare messages", len(seen), len(cases))
	}
	// Every path's message must be unique: a copy-paste that gives two paths
	// the same string makes them indistinguishable in a log query.
	byMsg := make(map[string]string, len(seen))
	for name, msg := range seen {
		if msg == "" {
			t.Errorf("%s: could not extract the msg= field from its log line", name)
			continue
		}
		if other, dup := byMsg[msg]; dup {
			t.Errorf("%s and %s both log msg=%s; the three over-cap outcomes must be "+
				"distinguishable by message alone", name, other, msg)
			continue
		}
		byMsg[msg] = name
	}
}

// msgField extracts the msg= value from one slog text line.
func msgField(line string) string {
	const key = "msg="
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			return rest[1 : 1+end]
		}
		return ""
	}
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		return rest[:end]
	}
	return rest
}
