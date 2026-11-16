package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withStallTimeout shortens fetchFile's stall window; not for parallel tests.
func withStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := stallTimeout
	stallTimeout = d
	t.Cleanup(func() { stallTimeout = orig })
}

// A download that takes far longer than the stall window but never pauses
// that long must finish. The old http.Client.Timeout (defaultTimeout) capped
// the whole body read at 60s, so a 25 MB binary on a slow link always failed
// at the 61st second with "Client.Timeout … while reading body". Here that
// timeout is shorter than the transfer, so a download held to it fails.
func TestFetchFile_SlowButMovingTransferCompletes(t *testing.T) {
	withStallTimeout(t, 300*time.Millisecond)
	origTimeout := defaultTimeout
	defaultTimeout = 300 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = origTimeout })
	chunk := bytes.Repeat([]byte("x"), 1024)
	const chunks = 10 // ~1s in all, more than three stall windows
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			w.Write(chunk) //nolint:errcheck
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	dest := filepath.Join(t.TempDir(), "bin")
	start := time.Now()
	if err := fetchFile(context.Background(), srv.URL+"/bin", dest, maxBinaryBytes); err != nil {
		t.Fatalf("fetchFile: %v", err)
	}
	if took := time.Since(start); took < 3*stallTimeout || took < 3*defaultTimeout {
		t.Fatalf("transfer took %s; the test needs it longer than the stall window and defaultTimeout to mean anything", took)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != chunks*len(chunk) {
		t.Fatalf("got %d bytes, want %d", len(got), chunks*len(chunk))
	}
}

// A transfer that stops sending is abandoned after the stall window, and the
// error says it stalled rather than a bare "context canceled".
func TestFetchFile_StalledTransferFails(t *testing.T) {
	withStallTimeout(t, 200*time.Millisecond)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial")) //nolint:errcheck
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	start := time.Now()
	err := fetchFile(context.Background(), srv.URL+"/bin", filepath.Join(t.TempDir(), "bin"), maxBinaryBytes)
	if err == nil {
		t.Fatal("fetchFile succeeded on a stalled transfer")
	}
	if !errors.Is(err, errStalled) || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v, want the stall named", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("gave up after %s, want about the stall window", took)
	}
}

// eofAfterCancel is a transport whose body, once the request is cancelled,
// ends in a clean io.EOF instead of the cancel error: what a real transport
// does now and then when the cancel races the end of the read.
type eofAfterCancel struct{}

func (eofAfterCancel) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
		Body: io.NopCloser(&cancelThenEOF{ctx: r.Context(), first: []byte("partial")})}, nil
}

type cancelThenEOF struct {
	ctx   context.Context
	first []byte
}

func (c *cancelThenEOF) Read(b []byte) (int, error) {
	if len(c.first) > 0 {
		n := copy(b, c.first)
		c.first = c.first[n:]
		return n, nil
	}
	<-c.ctx.Done()
	return 0, io.EOF
}

// A stalled transfer whose read still ends in a clean EOF after the
// watchdog cancelled it must fail as stalled, not pass a truncated file on
// (it reproduced about 1 in 100 runs of the test above on a real server).
func TestFetchFile_StalledThenCleanEOFFails(t *testing.T) {
	withStallTimeout(t, 100*time.Millisecond)
	prev := testHTTPTransport
	testHTTPTransport = eofAfterCancel{}
	t.Cleanup(func() { testHTTPTransport = prev })

	err := fetchFile(context.Background(), "https://example.invalid/bin", filepath.Join(t.TempDir(), "bin"), maxBinaryBytes)
	if err == nil {
		t.Fatal("fetchFile succeeded on a stalled transfer that ended in EOF")
	}
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want the stall named", err)
	}
}

// No response headers within the stall window counts as a stall too.
func TestFetchFile_NoHeadersFails(t *testing.T) {
	withStallTimeout(t, 200*time.Millisecond)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	err := fetchFile(context.Background(), srv.URL+"/bin", filepath.Join(t.TempDir(), "bin"), maxBinaryBytes)
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want a stall", err)
	}
}
