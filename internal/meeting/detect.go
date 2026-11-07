package meeting

import (
	"strings"
	"time"
)

// App is a meeting program: a process of one of its bundle-ID prefixes taking microphone input
// starts a recording, and the tap captures every process under the same prefixes (meeting apps
// play the far end from helper processes, e.g. Teams' ModuleHost / WebView helpers).
type App struct {
	Name    string
	Bundles []string
}

// Apps is the meeting programs that start a recording (§3.2: a constant list, not configuration).
// Browsers count while they use the microphone, for meetings joined on the web.
var Apps = []App{
	{Name: "Teams", Bundles: []string{"com.microsoft.teams"}},
	{Name: "Zoom", Bundles: []string{"us.zoom."}},
	{Name: "Chime", Bundles: []string{"com.amazon.Amazon-Chime", "com.amazon.chime"}},
	{Name: "Webex", Bundles: []string{"com.cisco.webex", "Cisco-Systems.Spark"}},
	{Name: "Slack", Bundles: []string{"com.tinyspeck.slackmacgap"}},
	{Name: "飞书", Bundles: []string{"com.larksuite.", "com.bytedance.lark", "com.electron.lark"}},
	{Name: "腾讯会议", Bundles: []string{"com.tencent.meeting", "com.tencent.wemeet"}},
	{Name: "Chrome", Bundles: []string{"com.google.Chrome"}},
	{Name: "Safari", Bundles: []string{"com.apple.Safari", "com.apple.WebKit"}},
	{Name: "Edge", Bundles: []string{"com.microsoft.edgemac"}},
	{Name: "Firefox", Bundles: []string{"org.mozilla.firefox"}},
	{Name: "Arc", Bundles: []string{"company.thebrowser."}},
}

// HelperBundle is naozhi-audio's own bundle ID; its microphone use never counts as a meeting.
const HelperBundle = "com.naozhi.audio"

// Grace is how long the meeting program may leave the microphone before the recording ends:
// muting, switching devices and reconnecting release it briefly (MR1).
const Grace = 2 * time.Minute

// Proc is one Core Audio process as naozhi-audio watch reports it.
type Proc struct {
	PID    int    `json:"pid"`
	PPID   int    `json:"ppid"`
	Bundle string `json:"bundle"`
	Path   string `json:"path"`
	Input  bool   `json:"input"`
	Output bool   `json:"output"`
}

// AppFor is the meeting program a bundle ID belongs to.
func AppFor(bundle string) (App, bool) {
	if bundle == "" || strings.HasPrefix(bundle, HelperBundle) {
		return App{}, false
	}
	for _, a := range Apps {
		for _, p := range a.Bundles {
			if strings.HasPrefix(bundle, p) {
				return a, true
			}
		}
	}
	return App{}, false
}

// Action is what the detector asks for after a snapshot.
type Action int

const (
	ActNone Action = iota
	// ActStart starts recording Detector.App().
	ActStart
	// ActStop ends the recording: the program left the microphone for Grace.
	ActStop
)

// Detector turns process snapshots into start / stop decisions. It is not safe for concurrent
// use; the manager owns it.
type Detector struct {
	app       *App
	lastInput time.Time
	// declined is set when the user stopped this meeting's recording: it stays stopped until the
	// program has left the microphone for Grace, then the next use is a new meeting.
	declined bool
}

// App is the meeting program being followed, nil when none.
func (d *Detector) App() *App { return d.app }

// Observe takes one snapshot at now.
func (d *Detector) Observe(now time.Time, procs []Proc) Action {
	if d.app != nil {
		if usesMic(*d.app, procs) {
			d.lastInput = now
			return ActNone
		}
		if now.Sub(d.lastInput) < Grace {
			return ActNone
		}
		declined := d.declined
		d.app, d.declined = nil, false
		if declined {
			return ActNone
		}
		return ActStop
	}
	for _, p := range procs {
		if !p.Input {
			continue
		}
		if a, ok := AppFor(p.Bundle); ok {
			d.app, d.lastInput, d.declined = &a, now, false
			return ActStart
		}
	}
	return ActNone
}

// Decline marks the followed meeting as stopped by the user.
func (d *Detector) Decline() {
	if d.app != nil {
		d.declined = true
	}
}

// Declined reports whether the user stopped the followed meeting.
func (d *Detector) Declined() bool { return d.app != nil && d.declined }

func usesMic(a App, procs []Proc) bool {
	for _, p := range procs {
		if !p.Input {
			continue
		}
		if b, ok := AppFor(p.Bundle); ok && b.Name == a.Name {
			return true
		}
	}
	return false
}
