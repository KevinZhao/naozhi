package workflow

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// TestReadResultFile: the probe's result file reads into the slim form; a
// reader longer than the cap fails without reading past it.
func TestReadResultFile(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/run/wf_2997921d-435.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rf, err := ReadResultFile(f)
	if err != nil {
		t.Fatalf("ReadResultFile: %v", err)
	}
	if rf.TaskID == "" || rf.Status != "completed" || len(rf.WorkflowProgress) == 0 {
		t.Errorf("parsed %+v; want the probe's completed run with its rows", rf)
	}
	big := &countingReader{r: io.MultiReader(bytes.NewReader([]byte(`{"taskId":"`)), zeros{})}
	if _, err := ReadResultFile(big); !errors.Is(err, ErrResultFileTooLarge) {
		t.Errorf("over the cap: err = %v, want ErrResultFileTooLarge", err)
	}
	if big.n > MaxResultFileBytes+1 {
		t.Errorf("read %d bytes, want at most the cap + 1", big.n)
	}
}

// TestResultFileTerminal: only the statuses CC writes for an ended run.
func TestResultFileTerminal(t *testing.T) {
	t.Parallel()
	for st, want := range map[string]bool{"completed": true, "failed": true, "killed": true, "running": false, "stopped": false, "": false} {
		if got := (&ResultFile{Status: st}).Terminal(); got != want {
			t.Errorf("Terminal(%q) = %v, want %v", st, got, want)
		}
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
