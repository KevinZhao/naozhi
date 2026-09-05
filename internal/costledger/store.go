package costledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dayLayout    = "2006-01-02"
	fileSuffix   = ".jsonl"
	filePerm     = 0o600
	dirPerm      = 0o700
	queueDepth   = 4096
	batchMax     = 64
	flushEvery   = time.Second
	dropLogEvery = time.Minute

	DefaultRetentionDays = 400
	DefaultRollupDays    = 35
	MaxRetentionDays     = 3650
)

// Options tunes a Store; zero values take the package defaults.
type Options struct {
	RetentionDays int
	RollupDays    int
	Now           func() time.Time
}

// Store is the on-disk ledger: one JSONL file per UTC day under dir, written
// by a single worker fed from a bounded queue, plus an in-memory rollup of
// the most recent RollupDays for cheap low-cardinality summaries. A nil or
// disabled Store accepts every call as a no-op so producers never nil-check.
type Store struct {
	dir       string
	disabled  bool
	retention time.Duration
	rollupWin time.Duration
	now       func() time.Time

	ch        chan Entry
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
	dropped   atomic.Int64
	lastDrop  atomic.Int64 // unix nanos of the last drop warn

	rollup *rollup

	// cur is the open day file; owned by the worker goroutine.
	cur    *os.File
	curDay string
}

// NewStore opens (or creates) the ledger directory. Empty dir disables the
// store. Recent day files are folded into the rollup synchronously so the
// first summary after startup is complete.
func NewStore(dir string, opts Options) *Store {
	if dir == "" {
		return &Store{disabled: true}
	}
	rd, ru := clampDays(opts.RetentionDays, opts.RollupDays)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{
		dir:       dir,
		retention: time.Duration(rd) * 24 * time.Hour,
		rollupWin: time.Duration(ru) * 24 * time.Hour,
		now:       now,
		ch:        make(chan Entry, queueDepth),
		rollup:    newRollup(),
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		slog.Error("costledger: mkdir failed; ledger disabled", "dir", dir, "err", err)
		return &Store{disabled: true}
	}
	s.warmRollup()
	s.sweep()
	s.wg.Add(1)
	go s.worker()
	return s
}

// clampDays applies the documented ranges: retention [1, MaxRetentionDays],
// rollup [1, retention]; out-of-range values warn and clamp, never fail.
func clampDays(retention, rollup int) (int, int) {
	if retention <= 0 {
		retention = DefaultRetentionDays
	}
	if retention > MaxRetentionDays {
		slog.Warn("costledger: retention_days above cap, clamping", "requested", retention, "cap", MaxRetentionDays)
		retention = MaxRetentionDays
	}
	if rollup <= 0 {
		rollup = DefaultRollupDays
	}
	if rollup > retention {
		slog.Warn("costledger: rollup_days above retention_days, clamping", "requested", rollup, "retention", retention)
		rollup = retention
	}
	return retention, rollup
}

// Enabled reports whether entries are persisted.
func (s *Store) Enabled() bool { return s != nil && !s.disabled }

// Dropped returns how many entries were discarded because the queue was full.
func (s *Store) Dropped() int64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Append validates and enqueues e without blocking. It returns false when the
// entry was rejected (invalid enums or nothing to record) or dropped (queue
// full / store closed); only drops count toward Dropped.
func (s *Store) Append(e Entry) bool {
	if s == nil || s.disabled || s.closed.Load() {
		return false
	}
	if !e.normalize() {
		return false
	}
	defer func() { _ = recover() }() // Close racing the send
	select {
	case s.ch <- e:
		return true
	default:
		n := s.dropped.Add(1)
		nowNs := s.now().UnixNano()
		if last := s.lastDrop.Load(); nowNs-last > int64(dropLogEvery) && s.lastDrop.CompareAndSwap(last, nowNs) {
			slog.Warn("costledger: queue full, dropping entries; totals will under-report", "dropped_total", n)
		}
		return false
	}
}

// Close flushes queued entries and stops the worker. Idempotent.
func (s *Store) Close() {
	if s == nil || s.disabled {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.ch)
	})
	s.wg.Wait()
}

// worker drains the queue in batches: up to batchMax entries or flushEvery,
// whichever first, then one fsync. Retention sweeps run once per UTC day.
func (s *Store) worker() {
	defer s.wg.Done()
	defer s.closeCur()
	timer := time.NewTimer(flushEvery)
	defer timer.Stop()
	batch := make([]Entry, 0, batchMax)
	sweptDay := s.now().UTC().Format(dayLayout)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.writeBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= batchMax {
				flush()
			}
		case <-timer.C:
			flush()
			if d := s.now().UTC().Format(dayLayout); d != sweptDay {
				sweptDay = d
				s.sweep()
			}
			timer.Reset(flushEvery)
		}
	}
}

// writeBatch appends the batch to its day file(s), fsyncs once per file
// touched and folds every entry into the rollup only after the bytes are
// durable, so summaries never claim what disk does not hold.
func (s *Store) writeBatch(batch []Entry) {
	var buf []byte
	written := batch[:0:0]
	for _, e := range batch {
		day := e.TS.Format(dayLayout)
		if day != s.curDay {
			// Pending bytes belong to the previous day: land them in the
			// old file before switching.
			if len(buf) > 0 {
				s.flushBuf(&buf, &written)
			}
			s.syncCur()
			if err := s.openDay(day); err != nil {
				slog.Error("costledger: open day file failed; entry lost", "day", day, "err", err)
				s.dropped.Add(1)
				continue
			}
		}
		line, err := json.Marshal(e)
		if err != nil {
			slog.Error("costledger: marshal failed; entry lost", "err", err)
			s.dropped.Add(1)
			continue
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
		written = append(written, e)
	}
	s.flushBuf(&buf, &written)
	s.syncCur()
}

// flushBuf writes buf to the current file and, on success, folds the pending
// entries into the rollup. Callers hand over the pending list so a failed
// write drops exactly those entries.
func (s *Store) flushBuf(buf *[]byte, pending *[]Entry) {
	if len(*buf) == 0 || s.cur == nil {
		*buf, *pending = (*buf)[:0], (*pending)[:0]
		return
	}
	if _, err := s.cur.Write(*buf); err != nil {
		slog.Error("costledger: write failed; entries lost", "n", len(*pending), "err", err)
		s.dropped.Add(int64(len(*pending)))
	} else {
		for _, e := range *pending {
			s.rollup.add(e)
		}
	}
	*buf, *pending = (*buf)[:0], (*pending)[:0]
}

func (s *Store) syncCur() {
	if s.cur != nil {
		_ = s.cur.Sync()
	}
}

func (s *Store) closeCur() {
	if s.cur != nil {
		_ = s.cur.Sync()
		_ = s.cur.Close()
		s.cur, s.curDay = nil, ""
	}
}

// openDay switches the worker's output file. O_NOFOLLOW plus an Lstat check
// refuse to write through a symlink planted in the ledger directory.
func (s *Store) openDay(day string) error {
	s.closeCur()
	path := filepath.Join(s.dir, day+fileSuffix)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to write through symlink")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|openNoFollow, filePerm)
	if err != nil {
		return err
	}
	s.cur, s.curDay = f, day
	return nil
}

// dayFiles lists ledger day files sorted ascending by day.
func (s *Store) dayFiles() []string {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, fileSuffix) && len(n) == len(dayLayout)+len(fileSuffix) {
			if _, err := time.Parse(dayLayout, strings.TrimSuffix(n, fileSuffix)); err == nil {
				days = append(days, strings.TrimSuffix(n, fileSuffix))
			}
		}
	}
	sort.Strings(days)
	return days
}

// sweep deletes day files older than the retention window.
func (s *Store) sweep() {
	cutoff := s.now().UTC().Add(-s.retention).Format(dayLayout)
	for _, d := range s.dayFiles() {
		if d < cutoff {
			if err := os.Remove(filepath.Join(s.dir, d+fileSuffix)); err != nil {
				slog.Warn("costledger: retention sweep failed", "day", d, "err", err)
			}
		}
	}
}

// warmRollup folds the last RollupDays of day files into memory.
func (s *Store) warmRollup() {
	since := s.now().UTC().Add(-s.rollupWin).Format(dayLayout)
	for _, d := range s.dayFiles() {
		if d < since {
			continue
		}
		s.scanDay(d, func(e Entry) bool { s.rollup.add(e); return true })
	}
	// Set even when no file was loaded: every day >= since is now covered
	// (possibly empty), so summaries over that range may use the rollup.
	s.rollup.warmedFrom = since
}

// scanDay streams one day file through fn (stop when fn returns false). A
// trailing half-written line (crash mid-append) is skipped with a warning.
func (s *Store) scanDay(day string, fn func(Entry) bool) {
	f, err := os.Open(filepath.Join(s.dir, day+fileSuffix))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			slog.Warn("costledger: skipping unparsable line", "day", day, "err", err)
			continue
		}
		if !fn(e) {
			return
		}
	}
}
