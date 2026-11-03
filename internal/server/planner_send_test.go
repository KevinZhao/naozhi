package server

import (
	"errors"
	"testing"
)

// TestPrepareSend_RefusesPlannerNamespace: a dashboard or relayed send must
// not create planner:main as an ordinary session without its launch spec.
func TestPrepareSend_RefusesPlannerNamespace(t *testing.T) {
	e := &sendEngine{}
	for _, key := range []string{"planner:main", "planner:other"} {
		if _, _, err := e.prepareSend(sendParams{Key: key, Text: "hi"}); !errors.Is(err, errPlannerNamespace) {
			t.Errorf("prepareSend(%q) err = %v, want errPlannerNamespace", key, err)
		}
	}
}
