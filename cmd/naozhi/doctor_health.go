// doctor 对 /health 的探测：一次 GET（有 token 则带鉴权）供存活检查、
// 服务端运行态检查与 config-drift 共用。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// healthBodyLimit caps the /health body doctor reads; the authenticated
// payload is a few KB, so a cut-off body means something else answered.
const healthBodyLimit = 64 << 10

// dispatchQuietWarnAfter is how long a process may run with no inbound IM
// message before doctor suspects the platform never connected.
const dispatchQuietWarnAfter = 10 * time.Minute

// healthReply is the one GET /health a doctor run makes.
type healthReply struct {
	err       error // request could not be built or sent
	status    int
	body      []byte
	payload   healthPayload
	decodeErr error
}

// authenticated reports whether the reply carries the authenticated section;
// with a missing or rejected token /health answers 200 with status and
// uptime only.
func (h *healthReply) authenticated() bool {
	return h.decodeErr == nil && h.payload.CLIAvailable != nil
}

// healthPayload is the subset of /health doctor reads. Field names follow
// internal/server/health.go; pointers distinguish an absent section.
type healthPayload struct {
	Status            string             `json:"status"`
	Uptime            string             `json:"uptime"`
	Version           string             `json:"version"`
	CLIAvailable      *bool              `json:"cli_available"`
	Platforms         map[string]string  `json:"platforms"`
	EventLog          *healthWriterStats `json:"eventlog"`
	AttachmentTracker *healthWriterStats `json:"attachment_tracker"`
	Dispatch          *struct {
		MessageCount        int64  `json:"message_count"`
		ReplyErrorCount     int64  `json:"reply_error_count"`
		SendFailCount       int64  `json:"send_fail_count"`
		LastReplySuccessAgo string `json:"last_reply_success_ago"`
	} `json:"dispatch"`
	ConfigSHA256   string `json:"config_sha256"`
	ConfigLoadedAt string `json:"config_loaded_at"`
}

// healthWriterStats covers the eventlog and attachment_tracker sections.
type healthWriterStats struct {
	WriterAlive  bool  `json:"writer_alive"`
	ChannelDepth int   `json:"channel_depth"`
	ChannelCap   int   `json:"channel_cap"`
	Dropped      int64 `json:"dropped_total"`
}

// fetchHealth GETs /health once per run, with the token when there is one.
func (d *doctor) fetchHealth() *healthReply {
	if d.health != nil {
		return d.health
	}
	h := &healthReply{}
	d.health = h
	url := d.addr + "/health"
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.err = fmt.Errorf("request build: %w", err)
		return h
	}
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		h.err = fmt.Errorf("%s unreachable: %w", url, err)
		return h
	}
	defer resp.Body.Close()
	h.status = resp.StatusCode
	h.body, _ = io.ReadAll(io.LimitReader(resp.Body, healthBodyLimit))
	h.decodeErr = json.Unmarshal(h.body, &h.payload)
	return h
}

// checkHealth is the liveness line: any 200 passes.
func (d *doctor) checkHealth() {
	h := d.fetchHealth()
	if h.err != nil {
		d.add("http /health", "fail", h.err.Error())
		return
	}
	// Response echoes to the terminal; a hijacked addr could emit escapes.
	bodyStr := osutil.SanitizeForLog(strings.TrimSpace(string(h.body)), 512)
	if h.status != http.StatusOK {
		d.add("http /health", "fail", fmt.Sprintf("status=%d body=%s", h.status, bodyStr))
		return
	}
	if h.authenticated() {
		p := h.payload
		d.addRemote("http /health", "pass", fmt.Sprintf("status=%s uptime=%s version=%s", p.Status, p.Uptime, p.Version))
		return
	}
	d.add("http /health", "pass", bodyStr)
}

// serverStateCategories are the findings checkServerState emits, in order.
var serverStateCategories = []string{"cli runtime", "platforms", "eventlog writer", "attachment tracker", "dispatch"}

// checkServerState reports the authenticated /health fields that predict
// whether the bot will answer. Every category emits exactly one finding so
// a JSON consumer can rely on its presence.
func (d *doctor) checkServerState() {
	skipAll := func(level, detail string) {
		for _, c := range serverStateCategories {
			d.addRemote(c, level, detail)
		}
	}
	if d.token == "" {
		skipAll("pass", "skipped (no token; auth-scoped)")
		return
	}
	h := d.fetchHealth()
	switch {
	case h.err != nil || h.status != http.StatusOK:
		skipAll("pass", "skipped (/health unavailable; see http /health)")
		return
	case h.decodeErr != nil:
		skipAll("warn", "cannot parse /health JSON: "+h.decodeErr.Error())
		return
	case !h.authenticated():
		skipAll("pass", "skipped (token not accepted by /health; see auth)")
		return
	}
	p := h.payload

	if *p.CLIAvailable {
		d.add("cli runtime", "pass", "the server finds its default CLI binary (cli_available=true)")
	} else {
		d.add("cli runtime", "fail", "the server cannot find its default CLI binary (cli_available=false) — new sessions cannot start; check cli.path / cli.backends")
	}

	names := make([]string, 0, len(p.Platforms))
	for name := range p.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		d.add("platforms", "warn", "no IM platform registered — dashboard-only mode")
	} else {
		d.addRemote("platforms", "pass", "registered: "+strings.Join(names, ", ")+" (registration only, not a connection state)")
	}

	d.writerFinding("eventlog writer", p.EventLog, "events are not reaching disk")
	d.writerFinding("attachment tracker", p.AttachmentTracker, "attachment metadata is not being recorded")
	d.dispatchFinding(p, len(names) > 0)
}

// writerFinding reports one writer_alive section; an absent section means the
// subsystem is disabled.
func (d *doctor) writerFinding(category string, w *healthWriterStats, consequence string) {
	if w == nil {
		d.add(category, "pass", "skipped (disabled on the server)")
		return
	}
	queue := fmt.Sprintf("queue %d/%d, dropped %d", w.ChannelDepth, w.ChannelCap, w.Dropped)
	if w.WriterAlive {
		d.add(category, "pass", "writer alive ("+queue+")")
		return
	}
	d.add(category, "fail", "writer stalled or closed ("+queue+") — "+consequence)
}

// dispatchFinding reports IM reply health. /health has no per-platform
// connection state, so a long quiet stretch is the only hint that a platform
// never connected.
func (d *doctor) dispatchFinding(p healthPayload, hasPlatforms bool) {
	ds := p.Dispatch
	if ds == nil {
		d.add("dispatch", "pass", "skipped (process reports no dispatch stats)")
		return
	}
	counts := fmt.Sprintf("messages=%d reply_errors=%d send_fails=%d", ds.MessageCount, ds.ReplyErrorCount, ds.SendFailCount)
	switch {
	case ds.LastReplySuccessAgo != "":
		d.addRemote("dispatch", "pass", "last successful reply "+ds.LastReplySuccessAgo+" ago · "+counts)
	case ds.ReplyErrorCount+ds.SendFailCount > 0:
		d.add("dispatch", "warn", "no successful reply since start, only failures · "+counts)
	case ds.MessageCount > 0 || !hasPlatforms:
		d.add("dispatch", "pass", "no reply sent yet · "+counts)
	default:
		// config is loaded once at startup, so its load time is the start time.
		loadedAt, err := time.Parse(time.RFC3339, p.ConfigLoadedAt)
		if err == nil && time.Since(loadedAt) > dispatchQuietWarnAfter {
			d.addRemote("dispatch", "warn", "no inbound IM messages (slash commands not counted) since start at "+p.ConfigLoadedAt+" — platform may not be connected")
			return
		}
		d.add("dispatch", "pass", "no inbound IM messages yet · "+counts)
	}
}

// addRemote adds a finding whose detail carries server-supplied text.
func (d *doctor) addRemote(category, level, detail string) {
	d.add(category, level, osutil.SanitizeForLog(detail, 512))
}
