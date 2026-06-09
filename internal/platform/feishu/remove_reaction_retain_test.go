package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestRemoveReaction_RetainsCacheOnFailure pins #1984: RemoveReaction must NOT
// evict the cached reaction_id when the DELETE never confirms success. The old
// code used LoadAndDelete (evict-then-send), so any transient failure —
// token-refresh cooldown, HTTP transport error, or a non-zero business code —
// lost the reaction_id forever and left the ⏳ HOURGLASS reaction stranded on
// the Feishu message with no mechanism to ever remove it. Eviction must be
// strictly later than success confirmation; on the success path the entry IS
// dropped so a later sweep doesn't re-issue a stale DELETE.
func TestRemoveReaction_RetainsCacheOnFailure(t *testing.T) {
	const (
		msgID = "om_msg1"
		rid   = "r-123"
	)

	cases := []struct {
		name string
		// setup mutates f and returns the test HTTP server (nil = no server
		// reached because the failure happens before the DELETE is issued).
		setup      func(t *testing.T, f *Feishu) *httptest.Server
		wantErr    bool
		wantCached bool // entry must survive in reactionIDs after the call
	}{
		{
			name: "token failure retains cache (DELETE never sent)",
			setup: func(t *testing.T, f *Feishu) *httptest.Server {
				// Trip the token circuit breaker so getAccessToken returns the
				// cached error without any network call.
				f.tokenLastFailed = errors.New("app_secret revoked")
				f.tokenLastFailAt = time.Now()
				return nil
			},
			wantErr:    true,
			wantCached: true,
		},
		{
			name: "HTTP transport failure retains cache",
			setup: func(t *testing.T, f *Feishu) *httptest.Server {
				// Server that immediately closes the connection to force a
				// transport-level Do() error.
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Fatal("ResponseWriter is not a Hijacker")
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Fatalf("hijack: %v", err)
					}
					_ = conn.Close()
				}))
				f.baseURL = srv.URL
				f.accessToken = "tok"
				f.tokenExpiry = time.Now().Add(time.Hour)
				return srv
			},
			wantErr:    true,
			wantCached: true,
		},
		{
			name: "non-zero business code retains cache",
			setup: func(t *testing.T, f *Feishu) *httptest.Server {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":99991663,"msg":"rate limited"}`))
				}))
				f.baseURL = srv.URL
				f.accessToken = "tok"
				f.tokenExpiry = time.Now().Add(time.Hour)
				return srv
			},
			wantErr:    true,
			wantCached: true,
		},
		{
			name: "success evicts cache",
			setup: func(t *testing.T, f *Feishu) *httptest.Server {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
				}))
				f.baseURL = srv.URL
				f.accessToken = "tok"
				f.tokenExpiry = time.Now().Add(time.Hour)
				return srv
			},
			wantErr:    false,
			wantCached: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Feishu{}
			srv := tc.setup(t, f)
			if srv != nil {
				defer srv.Close()
			}

			key := reactionCacheKey(msgID, "HOURGLASS")
			f.reactionIDs.Store(key, reactionCacheEntry{
				id:     rid,
				expiry: time.Now().Add(time.Hour).UnixNano(),
			})

			err := f.RemoveReaction(context.Background(), msgID, platform.ReactionQueued)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveReaction err = %v, wantErr = %v", err, tc.wantErr)
			}

			v, ok := f.reactionIDs.Load(key)
			if ok != tc.wantCached {
				t.Fatalf("cache present = %v, want %v (reaction_id must survive failure for retry)", ok, tc.wantCached)
			}
			if tc.wantCached {
				if entry, _ := v.(reactionCacheEntry); entry.id != rid {
					t.Errorf("retained entry id = %q, want %q", entry.id, rid)
				}
			}
		})
	}
}

// TestRemoveReaction_DropsMalformedEntry confirms that a malformed cache value
// (wrong type) is still evicted on the RemoveReaction probe — it can never
// produce a valid DELETE, so retaining it would be pointless. Distinct from the
// failure-retention contract above, which only protects valid reaction_ids.
func TestRemoveReaction_DropsMalformedEntry(t *testing.T) {
	f := &Feishu{}
	key := reactionCacheKey("om_bad", "HOURGLASS")
	f.reactionIDs.Store(key, "legacy-raw-string")

	if err := f.RemoveReaction(context.Background(), "om_bad", platform.ReactionQueued); err != nil {
		t.Fatalf("RemoveReaction on malformed entry should be a no-op, got %v", err)
	}
	if _, ok := f.reactionIDs.Load(key); ok {
		t.Error("malformed reactionIDs entry must be dropped (can never DELETE successfully)")
	}
}
