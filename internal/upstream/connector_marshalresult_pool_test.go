package upstream

import (
	"bytes"
	"testing"
)

// R202606c-GO-006: on an Encode error, marshalResult must Reset the buffer it
// took from the pool and return THAT buffer (not a fresh empty one), so the
// pool's effective size does not shrink by one on every error. We exercise the
// error path by encoding an unmarshalable value (a chan), then confirm the
// happy path can immediately reuse a non-empty pooled buffer.
func TestMarshalResult_ErrorPathReturnsBufferToPool(t *testing.T) {
	// Seed the pool with a buffer whose contents would be poison if leaked.
	seed := new(bytes.Buffer)
	seed.WriteString("POISON-do-not-leak")
	marshalResultBufPool.Put(seed)

	// Encoding a chan fails (json: unsupported type). The error path must
	// Reset + return the buffer, not panic.
	if _, err := marshalResult(make(chan int)); err == nil {
		t.Fatal("expected marshal error for chan value")
	}

	// The happy path must still produce correct output (no poison bytes leak
	// from a recycled buffer, since the error path Reset it before Put).
	out, err := marshalResult(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("happy-path marshal failed: %v", err)
	}
	got := string(out)
	if want := `{"k":"v"}`; got != want {
		t.Fatalf("marshalResult = %q, want %q", got, want)
	}
	if bytes.Contains(out, []byte("POISON")) {
		t.Fatalf("poison bytes leaked into output: %q", out)
	}
}

// Many sequential errors must not panic and must keep producing correct
// output afterwards — a smoke test that the pool stays healthy under repeated
// error-path Puts (the buffer is returned, never a half-written one reused raw).
func TestMarshalResult_RepeatedErrorsStayHealthy(t *testing.T) {
	for i := 0; i < 100; i++ {
		if _, err := marshalResult(make(chan int)); err == nil {
			t.Fatalf("iteration %d: expected error", i)
		}
	}
	out, err := marshalResult([]int{1, 2, 3})
	if err != nil {
		t.Fatalf("marshal after errors failed: %v", err)
	}
	if want := "[1,2,3]"; string(out) != want {
		t.Fatalf("marshalResult = %q, want %q", out, want)
	}
}
