// title.go — the titles a session transcript records, read off its tail.
package claudefs

import (
	"bytes"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/osutil"
)

// titleMarker pre-filters lines before any JSON decode: it matches both
// "custom-title" and "ai-title" records.
var titleMarker = []byte(`-title"`)

// TranscriptTitles returns the last custom-title (set by --name or /rename)
// and ai-title (the CLI's own summary) of the transcript at path; "" for one
// not found within the widest tail window. The CLI re-appends its title lines
// as the session goes on, so they sit near the end; the same windows as
// TranscriptTurnEnded bound the read.
func TranscriptTitles(path string) (custom, ai string, err error) {
	f, fi, err := osutil.OpenRegular(path, 0)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	for _, window := range []int64{turnTailWindow, turnTailMaxWindow} {
		buf, err := readTail(f, fi.Size(), window)
		if err != nil {
			return "", "", err
		}
		custom, ai = lastTitles(buf)
		if custom != "" || window >= fi.Size() {
			break
		}
	}
	return custom, ai, nil
}

// lastTitles scans buf backwards for its last custom-title and ai-title,
// stopping at the custom-title.
func lastTitles(buf []byte) (custom, ai string) {
	for len(buf) > 0 && custom == "" {
		i := bytes.LastIndexByte(buf, '\n')
		line := buf[i+1:]
		buf = buf[:max(i, 0)]
		if !bytes.Contains(line, titleMarker) {
			continue
		}
		var rec struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch {
		case rec.Type == "custom-title":
			custom = rec.CustomTitle
		case rec.Type == "ai-title" && ai == "":
			ai = rec.AITitle
		}
	}
	return custom, ai
}
