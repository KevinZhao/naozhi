package patrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/planner/front"
)

// Limits on a deep check's output (§15.23 ②). What is only too long is cut;
// a finding that is not one (a bad id, an unknown kind, no evidence the
// gateway can open) is dropped alone; a reply that is no contract JSON at
// all fails the run.
const (
	// MaxFindings is how many findings one deep check reports.
	MaxFindings   = 10
	titleCap      = 80
	impactRunes   = 300
	fixRunes      = 500
	evidenceRunes = 300
	maxEvidence   = 5
	// maxOutputBytes bounds the reply decoded at all.
	maxOutputBytes = 256 << 10
	// maxEvidenceFile bounds a file whose lines are counted for a finding's
	// evidence.
	maxEvidenceFile = 5 << 20
	urlBytes        = 2048
)

// Kinds, sizes and confidences of a finding.
const (
	KindBug   = "bug"
	KindRisk  = "risk"
	KindDebt  = "debt"
	KindDoc   = "doc"
	KindStale = "stale"

	SizeS = "S"
	SizeM = "M"
	SizeL = "L"

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

var (
	kinds       = []string{KindBug, KindRisk, KindDebt, KindDoc, KindStale}
	sizes       = []string{SizeS, SizeM, SizeL}
	confidences = []string{ConfidenceHigh, ConfidenceMedium, ConfidenceLow}
)

// Finding is one thing a deep check found, as the session reports it.
type Finding struct {
	// ID is the session's stable short name of the thing: the same thing
	// found again a week later carries the same one, which is all the
	// gateway tells it apart by.
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// Evidence is where it shows: "path:line" or "path:from-to" relative to
	// the workspace's directory, or an http(s) link.
	Evidence   []string `json:"evidence"`
	Impact     string   `json:"impact"`
	Fix        string   `json:"fix"`
	Size       string   `json:"size"`
	Reversible bool     `json:"reversible"`
	Confidence string   `json:"confidence"`
}

// idRE is a finding's ID; refRE the name it goes by everywhere outside its
// workspace's file, "<workspace ID>/<id>".
var (
	idRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,59}$`)
	refRE = regexp.MustCompile(`^(ws_[a-z0-9]{8})/([a-z0-9][a-z0-9-]{2,59})$`)
)

// Ref names finding id of workspace ws.
func Ref(ws, id string) string { return ws + "/" + id }

// SplitRef is the workspace ID and the finding ID ref names.
func SplitRef(ref string) (ws, id string, ok bool) {
	m := refRE.FindStringSubmatch(ref)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// ItemSource is the source of the open item a finding is filed as. It never
// changes: the item is told by it (D143), and a pull request's link in its
// place would hand the item to the stalled-PR check, which closes what it
// no longer hits.
func ItemSource(ref string) string { return front.PatrolSource + ref }

// ParseFindings reads a deep check's reply strictly: one JSON object with
// exactly the contract's fields, no text before or after it and no code
// fence, at most 256KB. Of its findings the first MaxFindings are looked
// at; one with a malformed id, a kind, size or confidence outside the
// lists, an empty title, impact or fix, or no evidence left after the check
// below is dropped, as is a second one of the same id. dropped counts them.
//
// The gateway checks the evidence again, whatever the session says it did:
// a link must be http(s); a file must lie inside dir with its symlinks
// resolved, be a regular file and have the line named. Evidence that fails
// is dropped, and a finding with none left goes with it: what cannot be
// opened is not shown to the user as evidence.
func ParseFindings(reply, dir string) (kept []Finding, dropped int, err error) {
	text := strings.TrimSpace(reply)
	if !strings.HasPrefix(text, "{") {
		return nil, 0, errors.New("回复不是一个 JSON 对象")
	}
	if len(text) > maxOutputBytes {
		return nil, 0, fmt.Errorf("回复 %d 字节，超过 %d", len(text), maxOutputBytes)
	}
	var in struct {
		Findings *[]Finding `json:"findings"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, 0, fmt.Errorf("JSON 不合格式：%w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, 0, errors.New("JSON 对象后面还有内容")
	}
	if in.Findings == nil {
		return nil, 0, errors.New("缺少 findings")
	}
	all := *in.Findings
	if len(all) > MaxFindings {
		dropped = len(all) - MaxFindings
		all = all[:MaxFindings]
	}
	root, rootErr := filepath.EvalSymlinks(dir)
	for _, f := range all {
		f.ID = strings.TrimSpace(f.ID)
		f.Title, f.Impact, f.Fix = fold(f.Title), strings.TrimSpace(f.Impact), strings.TrimSpace(f.Fix)
		var evidence []string
		for _, e := range f.Evidence {
			if e = fold(e); len(evidence) < maxEvidence && !slices.Contains(evidence, e) && rootErr == nil && evidenceOK(root, e) {
				evidence = append(evidence, e)
			}
		}
		f.Evidence = evidence
		if !idRE.MatchString(f.ID) || !slices.Contains(kinds, f.Kind) || !slices.Contains(sizes, f.Size) || !slices.Contains(confidences, f.Confidence) ||
			f.Title == "" || f.Impact == "" || f.Fix == "" || len(f.Evidence) == 0 ||
			slices.ContainsFunc(kept, func(o Finding) bool { return o.ID == f.ID }) {
			dropped++
			continue
		}
		f.Title, f.Impact, f.Fix = clip(f.Title, titleCap), clip(f.Impact, impactRunes), clip(f.Fix, fixRunes)
		kept = append(kept, f)
	}
	return kept, dropped, nil
}

// evidenceOK reports whether e is evidence the user can open: an http(s)
// link, or a line of a regular file inside root (a directory with its
// symlinks resolved).
func evidenceOK(root, e string) bool {
	if e == "" || utf8.RuneCountInString(e) > evidenceRunes {
		return false
	}
	if strings.HasPrefix(e, "http://") || strings.HasPrefix(e, "https://") {
		u, err := url.Parse(e)
		return err == nil && u.Hostname() != "" && len(e) <= urlBytes
	}
	rel, _, to, ok := fileLine(e)
	if !ok {
		return false
	}
	// A symlink in the repository may point anywhere: the file read is the
	// one it resolves to, which must still be the repository's.
	path, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil || !within(path, root) {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxEvidenceFile {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return to <= lines
}

// fileLine splits evidence "path:line" or "path:from-to" into a path
// relative to the workspace, clean and not leaving it, and its lines.
func fileLine(e string) (rel string, from, to int, ok bool) {
	i := strings.LastIndexByte(e, ':')
	if i <= 0 {
		return "", 0, 0, false
	}
	first, last, ranged := strings.Cut(e[i+1:], "-")
	from, err := strconv.Atoi(first)
	if err != nil || from < 1 {
		return "", 0, 0, false
	}
	to = from
	if ranged {
		if to, err = strconv.Atoi(last); err != nil || to < from {
			return "", 0, 0, false
		}
	}
	rel = filepath.Clean(filepath.FromSlash(e[:i]))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", 0, 0, false
	}
	return rel, from, to, true
}

// blobLinks is a GitHub link for each file line of evidence, at commit sha
// of repo ("owner/name"), prefix being the workspace's directory relative
// to the repository's top ("" at the top): what the user clicks to see the
// evidence. Links in the evidence itself are not repeated; nil for a
// repository not on GitHub or with no commit.
func blobLinks(repo, sha, prefix string, evidence []string) []string {
	if repo == "" || sha == "" {
		return nil
	}
	var out []string
	for _, e := range evidence {
		rel, from, to, ok := fileLine(e)
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(filepath.Join(prefix, rel)), "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		link := "https://github.com/" + repo + "/blob/" + sha + "/" + strings.Join(parts, "/") + "#L" + strconv.Itoa(from)
		if to > from {
			link += "-L" + strconv.Itoa(to)
		}
		out = append(out, link)
	}
	return out
}
