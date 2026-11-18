package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// unreferenced.go — retention for an attachment root whose payloads are only
// reachable through the event-log records that name them (the state root's
// planner/attachments: tool screenshots and Planner send images). Once the
// session's log is dropped or rotated past the record, nothing can show the
// file again, and the opt-in attachment-gc daemon is the only other cleaner.

// refScanChunk is the read size of CollectRefs; refScanOverlap is carried
// into the next chunk so a path straddling the boundary is still seen whole
// (a reference is the prefix plus "yyyy-mm-dd/" plus a ~40-byte name).
const (
	refScanChunk   = 1 << 20
	refScanOverlap = 256
)

// prunableExt is the payload set PersistIn's image callers write; anything
// else under the root is left alone.
var prunableExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// CollectRefs returns the "<yyyy-mm-dd>/<name>" of every "<prefix><yyyy-mm-dd>/<name>"
// written in the regular files of dir whose name ends in ext. Any read error,
// a missing dir included, is returned: the caller must then prune nothing,
// since an incomplete set would make referenced files look unreferenced.
func CollectRefs(dir, ext, prefix string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]struct{})
	needle := []byte(prefix)
	buf := make([]byte, refScanOverlap+refScanChunk)
	for _, e := range entries {
		if !e.Type().IsRegular() || filepath.Ext(e.Name()) != ext {
			continue
		}
		if err := scanRefs(filepath.Join(dir, e.Name()), needle, buf, refs); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func scanRefs(path string, needle, buf []byte, refs map[string]struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed since ReadDir: it names nothing any more
		}
		return err
	}
	defer f.Close()
	carry := 0
	for {
		n, rerr := io.ReadFull(f, buf[carry:])
		data := buf[:carry+n]
		for rest := data; ; {
			i := bytes.Index(rest, needle)
			if i < 0 {
				break
			}
			rest = rest[i+len(needle):]
			if ref := refAt(rest); ref != "" {
				refs[ref] = struct{}{}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("scan %s: %w", path, rerr)
		}
		carry = copy(buf, data[len(data)-refScanOverlap:])
	}
}

// refAt reads "<yyyy-mm-dd>/<name>" at the head of b, "" when b does not
// start with one. The name ends at the first byte PersistIn never writes in
// one (a JSON quote, a backslash, whitespace...).
func refAt(b []byte) string {
	const day = len("2006-01-02")
	if len(b) <= day+1 || b[day] != '/' {
		return ""
	}
	end := day + 1
	for end < len(b) && isNameByte(b[end]) {
		end++
	}
	if end == day+1 {
		return ""
	}
	return string(b[:end])
}

func isNameByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '_'
}

// PruneResult reports what one PruneUnreferenced removed.
type PruneResult struct {
	Removed int
	Bytes   int64
}

// PruneUnreferenced removes from root (laid out as PersistIn writes it,
// <root>/<yyyy-mm-dd>/<id>.<ext> + <id>.meta) every image payload, with its
// .meta, whose mtime is older than maxAge and whose "<yyyy-mm-dd>/<name>" is
// not in refs; then the day directories left empty that are older than
// maxAge. A referenced file is never removed, whatever its age.
func PruneUnreferenced(root string, maxAge time.Duration, refs map[string]struct{}, now time.Time) (PruneResult, error) {
	var res PruneResult
	if maxAge <= 0 || refs == nil {
		return res, nil
	}
	days, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	cutoff := now.Add(-maxAge)
	for _, d := range days {
		// Type() is Lstat-based: a symlinked day directory is not followed.
		if !d.Type().IsDir() {
			continue
		}
		dayTime, err := time.Parse("2006-01-02", d.Name())
		if err != nil {
			continue // not a PersistIn day directory
		}
		dayPath := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dayPath)
		if err != nil {
			continue
		}
		for _, fe := range files {
			name := fe.Name()
			if !fe.Type().IsRegular() || !prunableExt[filepath.Ext(name)] {
				continue
			}
			if _, ok := refs[d.Name()+"/"+name]; ok {
				continue
			}
			info, err := fe.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			abs := filepath.Join(dayPath, name)
			if err := os.Remove(abs); err != nil {
				continue
			}
			_ = os.Remove(MetaPathFor(abs))
			res.Removed++
			res.Bytes += info.Size()
		}
		// os.Remove refuses a directory that still holds anything, so only a
		// day emptied here (or before) goes; never today's PersistIn target.
		if dayTime.Add(24 * time.Hour).Before(cutoff) {
			_ = os.Remove(dayPath)
		}
	}
	return res, nil
}
