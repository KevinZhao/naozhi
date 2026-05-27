package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestTruncatePromptUTF8 pins the byte-cap + rune-boundary contract for
// the helper that powers GET /api/cron?compact=1 (R236-SEC-08 / #494):
//
//   - max <= 0 or len(s) <= max → return s unchanged, truncated=false.
//   - len(s) > max → clip at the most recent UTF-8 RuneStart byte so the
//     returned slice is still valid UTF-8, truncated=true.
//
// The rune-boundary clamp is the load-bearing detail: a naive `s[:max]`
// would split a multi-byte rune at the cap and JSON consumers would
// receive a U+FFFD replacement character. The clamp is bounded by the
// 4-byte UTF-8 max so it cannot blow up on adversarial input.
func TestTruncatePromptUTF8(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		max       int
		want      string
		wantTrunc bool
	}{
		{
			name:      "short_ascii_unchanged",
			input:     "hello world",
			max:       compactPromptPrefixBytes,
			want:      "hello world",
			wantTrunc: false,
		},
		{
			name:      "exact_length_unchanged",
			input:     strings.Repeat("a", compactPromptPrefixBytes),
			max:       compactPromptPrefixBytes,
			want:      strings.Repeat("a", compactPromptPrefixBytes),
			wantTrunc: false,
		},
		{
			name:      "ascii_clipped_at_max",
			input:     strings.Repeat("a", compactPromptPrefixBytes+10),
			max:       compactPromptPrefixBytes,
			want:      strings.Repeat("a", compactPromptPrefixBytes),
			wantTrunc: true,
		},
		{
			name:      "max_zero_returns_input",
			input:     "abc",
			max:       0,
			want:      "abc",
			wantTrunc: false,
		},
		{
			name:      "negative_max_returns_input",
			input:     "abc",
			max:       -5,
			want:      "abc",
			wantTrunc: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotTrunc := truncatePromptUTF8(tc.input, tc.max)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if gotTrunc != tc.wantTrunc {
				t.Fatalf("got truncated=%v, want %v", gotTrunc, tc.wantTrunc)
			}
			// Output must be valid UTF-8 unconditionally — the rune-
			// boundary clamp is the contract; any test failure here
			// means a future refactor reintroduced a half-rune split.
			if !utf8.ValidString(got) {
				t.Fatalf("returned string is not valid UTF-8: %q", got)
			}
		})
	}
}

// TestTruncatePromptUTF8_MultiByteRuneBoundary exercises the load-bearing
// case the simple ASCII tests above cannot cover: a Chinese rune that
// straddles the byte cap. UTF-8 encodes 中 as 3 bytes (E4 B8 AD). With a
// max placed mid-rune, the helper must walk back to the previous rune
// start so the returned bytes parse cleanly.
func TestTruncatePromptUTF8_MultiByteRuneBoundary(t *testing.T) {
	t.Parallel()

	// "AAA中BBB" — A is 1 byte each, 中 is 3 bytes.
	// Bytes: 41 41 41 E4 B8 AD 42 42 42  (total 9 bytes)
	// max=4 falls mid-rune (after E4 only). The helper must walk back
	// to byte 3 so the result is "AAA" — dropping the half-encoded 中.
	input := "AAA中BBB"
	got, trunc := truncatePromptUTF8(input, 4)
	if !trunc {
		t.Fatal("expected truncated=true at mid-rune cap")
	}
	if got != "AAA" {
		t.Fatalf("got %q, want %q (must clip at rune boundary, not split 中)", got, "AAA")
	}
	if !utf8.ValidString(got) {
		t.Fatal("clipped output must be valid UTF-8")
	}

	// max=6 lands exactly at the byte after 中 — full rune fits, BBB clipped.
	got2, trunc2 := truncatePromptUTF8(input, 6)
	if !trunc2 {
		t.Fatal("expected truncated=true at byte 6")
	}
	if got2 != "AAA中" {
		t.Fatalf("got %q, want %q (full rune should fit at exact boundary)", got2, "AAA中")
	}
}

// TestHandleList_CompactMode pins the wire-shape contract for the
// R236-SEC-08 / #494 opt-in compact mode:
//
//   - GET /api/cron (no param)        → full prompt, no prompt_truncated
//     field on the wire (omitempty zero value).
//   - GET /api/cron?compact=1         → prompt clipped to 256 UTF-8 bytes,
//     prompt_truncated:true present.
//   - GET /api/cron?compact=anything  → treated as "off" (only "1" opts in)
//     so legacy callers hitting /api/cron?compact=true keep full prompts.
//
// The third case is deliberate paranoia: a future caller that thinks any
// truthy value enables compact would silently get full prompts under our
// strict gate, which is the safe fallback (no data loss).
func TestHandleList_CompactMode(t *testing.T) {
	t.Parallel()

	sched := cron.NewScheduler(cron.SchedulerConfig{})
	// 8 KiB prompt — same scale as the issue's bandwidth example, padded
	// past the 256-byte compact cap so truncation is visible.
	bigPrompt := strings.Repeat("xy", 4096)
	if err := sched.AddJob(&cron.Job{
		ID:       "aa00000000000001",
		Schedule: "*/10 * * * *",
		Prompt:   bigPrompt,
		Platform: "feishu",
		ChatID:   "oc_test",
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	h := &CronHandlers{scheduler: sched}

	hit := func(query string) cronListResp {
		req := httptest.NewRequest(http.MethodGet, "/api/cron"+query, nil)
		w := httptest.NewRecorder()
		h.handleList(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status %d, body=%s", query, w.Code, w.Body.String())
		}
		var got cronListResp
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("query %q: decode: %v body=%s", query, err, w.Body.String())
		}
		return got
	}

	// Default — full prompt round-trips, prompt_truncated absent (zero
	// value omitted via omitempty so wire stays byte-equal to pre-#494
	// shape for non-dashboard consumers).
	full := hit("")
	if len(full.Jobs) != 1 {
		t.Fatalf("default mode: want 1 job, got %d", len(full.Jobs))
	}
	if full.Jobs[0].Prompt != bigPrompt {
		t.Fatalf("default mode: prompt was clipped (len %d, want %d)",
			len(full.Jobs[0].Prompt), len(bigPrompt))
	}
	if full.Jobs[0].PromptTruncated {
		t.Fatal("default mode: prompt_truncated must be false")
	}

	// compact=1 — prompt clipped, flag set.
	compact := hit("?compact=1")
	if len(compact.Jobs) != 1 {
		t.Fatalf("compact mode: want 1 job, got %d", len(compact.Jobs))
	}
	if got := len(compact.Jobs[0].Prompt); got > compactPromptPrefixBytes {
		t.Fatalf("compact mode: prompt %d bytes > %d cap", got, compactPromptPrefixBytes)
	}
	if !compact.Jobs[0].PromptTruncated {
		t.Fatal("compact mode: prompt_truncated must be true")
	}

	// compact=true (anything-but-"1") → off, full prompt restored.
	loose := hit("?compact=true")
	if loose.Jobs[0].Prompt != bigPrompt {
		t.Fatal("compact=true (not '1') must NOT enable compact — only '1' opts in")
	}
	if loose.Jobs[0].PromptTruncated {
		t.Fatal("compact=true: prompt_truncated must be false (gate is strict '1')")
	}
}

// TestHandleList_CompactBandwidthBound is the regression-prone numeric
// pin for R236-SEC-08 / #494: 50 jobs × 8 KiB prompt = 400 KiB per poll
// pre-fix. Compact mode caps each prompt at 256 bytes so the same shape
// fits in ~13 KiB. We assert the per-job prompt length to make the
// quantitative win visible — a future regression that drops the cap or
// truncates inside the wrong code path would balloon back to KB-scale.
func TestHandleList_CompactBandwidthBound(t *testing.T) {
	t.Parallel()

	sched := cron.NewScheduler(cron.SchedulerConfig{})
	const N = 5 // small N keeps the test fast; the wire-shape pin is per-job
	bigPrompt := strings.Repeat("z", 8192)
	for i := 0; i < N; i++ {
		// IDs must satisfy cron.IsValidID (16 lowercase hex). Construct
		// one per-iteration so the scheduler accepts all N inserts.
		id := "aa00000000000a0" + string(rune('1'+i))
		if err := sched.AddJob(&cron.Job{
			ID:       id,
			Schedule: "*/10 * * * *",
			Prompt:   bigPrompt,
			Platform: "feishu",
			ChatID:   "oc_test",
		}); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
	}

	h := &CronHandlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron?compact=1", nil)
	w := httptest.NewRecorder()
	h.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var got cronListResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Jobs) != N {
		t.Fatalf("want %d jobs, got %d", N, len(got.Jobs))
	}
	for i, job := range got.Jobs {
		if len(job.Prompt) > compactPromptPrefixBytes {
			t.Fatalf("job[%d] prompt %d bytes exceeds %d cap (R236-SEC-08 regression)",
				i, len(job.Prompt), compactPromptPrefixBytes)
		}
		if !job.PromptTruncated {
			t.Fatalf("job[%d] prompt_truncated must be true under compact=1", i)
		}
	}
}
