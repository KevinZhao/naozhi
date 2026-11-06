// Package midway reads when this machine's Midway session expires: the
// expiry of the session cookie mwinit writes to ~/.midway/cookie (a Netscape
// cookie file). Only the domain, name and expiry columns are read; no
// cookie value leaves the package. A machine without the file has no
// expiry, and everything built on it stays quiet.
package midway

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxBytes caps the read; the file is a few kilobytes.
const maxBytes = 1 << 20

// domain and names are the Midway session cookie's.
const domain = "midway-auth.amazon.com"

var names = []string{"session", "__Host-session"}

// CookiePath is mwinit's cookie file; "" when the home directory is
// unknown.
func CookiePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".midway", "cookie")
}

// Expiry is when the Midway session in the cookie file at path expires:
// the latest expiry of its session cookies. Zero, with no error, when there
// is no file or no session cookie in it.
func Expiry(path string) (time.Time, error) {
	if path == "" {
		return time.Time{}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var latest int64
	sc := bufio.NewScanner(io.LimitReader(f, maxBytes))
	sc.Buffer(make([]byte, 0, 64<<10), maxBytes)
	for sc.Scan() {
		// mwinit, like curl, writes an HttpOnly cookie as a comment line.
		line := strings.TrimPrefix(sc.Text(), "#HttpOnly_")
		if strings.HasPrefix(line, "#") {
			continue
		}
		col := strings.Split(line, "\t")
		if len(col) < 7 || strings.TrimPrefix(col[0], ".") != domain || !slices.Contains(names, col[5]) {
			continue
		}
		if exp, err := strconv.ParseInt(col[4], 10, 64); err == nil && exp > latest {
			latest = exp
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, err
	}
	if latest == 0 {
		return time.Time{}, nil
	}
	return time.Unix(latest, 0), nil
}
