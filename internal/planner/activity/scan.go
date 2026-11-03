package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Turn-end scan bounds: the scan finds the outputs a turn made without a
// Write tool (a Bash script rendering a chart), so it only has to look where
// such files land, and it must stay cheap when the working directory is a
// large repo or $HOME.
const (
	// ScanTimeout bounds one scan.
	ScanTimeout = 2 * time.Second
	// scanMaxDepth is how many directory levels below the root are read.
	scanMaxDepth = 4
	// scanMaxEntries caps the directory entries one scan looks at.
	scanMaxEntries = 5000
	// scanMaxFound caps the files one scan reports.
	scanMaxFound = 20
)

// scanSkipDirs are dependency and cache trees; hidden directories (.git,
// .venv, .next) are skipped too.
var scanSkipDirs = wordSet("node_modules", "vendor", "__pycache__", "venv", "target")

// Scan returns the output files (IsOutputFile) under dir whose mtime is at or
// after since, as absolute paths, shallowest first. It reads directories
// breadth first and stops at the bounds above or when ctx is done. Hidden
// files and directories, symlinks and the skipped trees are not followed.
func Scan(ctx context.Context, dir string, since time.Time) []string {
	type level struct {
		path  string
		depth int
	}
	queue := []level{{dir, 0}}
	seen := 0
	var out []string
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return out
		}
		cur := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}
		for _, de := range entries {
			if seen++; seen > scanMaxEntries {
				return out
			}
			name := de.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			switch {
			case de.IsDir():
				if cur.depth+1 < scanMaxDepth && !scanSkipDirs[name] {
					queue = append(queue, level{filepath.Join(cur.path, name), cur.depth + 1})
				}
			case de.Type().IsRegular() && IsOutputFile(name):
				info, err := de.Info()
				if err != nil || info.ModTime().Before(since) {
					continue
				}
				out = append(out, filepath.Join(cur.path, name))
				if len(out) == scanMaxFound {
					return out
				}
			}
		}
	}
	return out
}
