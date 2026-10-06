package workflow

import (
	"errors"
	"io"
)

// MaxResultFileBytes caps the result file a board reads (RFC §10): CC's
// largest seen is ~0.6MB for 309 agents.
const MaxResultFileBytes = 16 << 20

// ErrResultFileTooLarge: the reader held more than MaxResultFileBytes.
var ErrResultFileTooLarge = errors.New("workflow: result file too large")

// ReadResultFile reads and parses a result file from r, an already opened
// file; it reads at most MaxResultFileBytes+1 bytes.
func ReadResultFile(r io.Reader) (*ResultFile, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxResultFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResultFileBytes {
		return nil, ErrResultFileTooLarge
	}
	return ParseResultFile(data)
}

// Terminal reports whether rf's status is one CC writes for an ended run;
// only such a file merges.
func (rf *ResultFile) Terminal() bool {
	_, ok := resultFileStatus(rf.Status)
	return ok
}
