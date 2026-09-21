package runlog

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Record is one run record found on disk by WalkRecords.
type Record struct {
	// OwnerDir is the directory name the record was found under — the mapped
	// name, not the owner ID, because the mapping is one-way for stores that
	// hash (runhistory).
	OwnerDir string
	// ID is the record's file name without the .json suffix.
	ID string
	// Path is the full path, for error messages.
	Path string
	// Raw is the file's bytes.
	Raw []byte
}

// WalkRecords calls fn for every run record two levels below root
// (<root>/<ownerDir>/<id>.json). A missing root is not an error: a store that
// never persisted anything has nothing to walk.
//
// Read-only on purpose, and NOT a Layout method: the one caller that needs it
// (cost backfill) is handed a root from config and must not create or chmod
// anything — `naozhi cost backfill --dry-run` creating directories would be a
// surprising side effect of a read command.
//
// onError is called with the path and error for an entry that cannot be read,
// and the walk continues: a backfill that aborts on one unreadable file
// imports nothing. Pass nil to ignore them.
//
// This is the third hand-rolled version of this walk (#2709): cron's
// scanSortedRunDir and runhistory's warmLocked each have their own, and they
// have already drifted once on what counts as a record file (#871). Those two
// are staying where they are for now — they carry sort order and retention
// semantics this does not — but no fourth copy needs writing.
func WalkRecords(root string, onError func(path string, err error), fn func(Record)) {
	if root == "" {
		return
	}
	owners, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) && onError != nil {
			onError(root, err)
		}
		return
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		ownerDir := filepath.Join(root, owner.Name())
		files, err := os.ReadDir(ownerDir)
		if err != nil {
			if onError != nil {
				onError(ownerDir, err)
			}
			continue
		}
		for _, f := range files {
			if !isRecordFile(f) {
				continue
			}
			path := filepath.Join(ownerDir, f.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				if onError != nil {
					onError(path, err)
				}
				continue
			}
			fn(Record{
				OwnerDir: owner.Name(),
				ID:       f.Name()[:len(f.Name())-len(".json")],
				Path:     path,
				Raw:      raw,
			})
		}
	}
}

// isRecordFile is the one definition of "this file is a run record": a plain
// .json file, never a directory and never the .corrupt.<ts> sibling
// jsonfile.Load leaves behind (that one is evidence, not a record).
func isRecordFile(e fs.DirEntry) bool {
	if e.IsDir() {
		return false
	}
	return filepath.Ext(e.Name()) == ".json"
}
