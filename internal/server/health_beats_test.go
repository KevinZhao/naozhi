package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/beat"
)

// healthFakeClock never sleeps: Mono follows the wall clock.
type healthFakeClock struct{ wall time.Time }

var healthFakeEpoch = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func (c *healthFakeClock) Now() time.Time      { return c.wall }
func (c *healthFakeClock) Mono() time.Duration { return c.wall.Sub(healthFakeEpoch) }

// status degrades on a stale heartbeat while the HTTP code stays 200; the
// loop names are auth-only.
func TestHandleHealth_StaleBeatDegrades(t *testing.T) {
	tests := []struct {
		name       string
		elapsed    time.Duration // after registration, with no beat
		auth       bool
		wantStatus string
		wantStale  []string
	}{
		{"inside startup grace", beat.StartupGrace - time.Second, true, "ok", nil},
		{"stale", beat.StartupGrace, true, "degraded", []string{"cron_tick"}},
		{"stale, unauthenticated", beat.StartupGrace, false, "degraded", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
			c := &healthFakeClock{wall: healthFakeEpoch}
			hs.healthH.beats = beat.New(c)
			hs.healthH.beats.Register("cron_tick", time.Minute)
			c.wall = c.wall.Add(tt.elapsed)

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			if tt.auth {
				req.Header.Set("Authorization", "Bearer secret")
			}
			w := httptest.NewRecorder()
			hs.healthH.handleHealth(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d, want 200", w.Code)
			}
			var body struct {
				Status     string   `json:"status"`
				StaleLoops []string `json:"stale_loops"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Status != tt.wantStatus || !slices.Equal(body.StaleLoops, tt.wantStale) {
				t.Errorf("status=%q stale_loops=%v, want %q %v", body.Status, body.StaleLoops, tt.wantStatus, tt.wantStale)
			}
		})
	}
}
