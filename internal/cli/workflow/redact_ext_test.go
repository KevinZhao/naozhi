package workflow_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// TestNoSecretReachesTheWire plants a key in every string a frame or a
// result file can put on the wire, one variant padded to straddle that
// field's cap, and checks the published JSON and result cache.
func TestNoSecretReachesTheWire(t *testing.T) {
	t.Parallel()
	// Bare sk- needs a 40-byte tail: a cut leaves a stub no redactor
	// downstream would catch, so no "sk-" may appear at all.
	key := "sk-" + strings.Repeat("Zq9X", 12)
	const stub = "sk-"
	for _, pad := range []int{0, 25, 55, 110, 190, 390} {
		v := strings.Repeat("x", pad) + " " + key
		q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
		lines := []string{
			fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":"wleak0001","task_type":"local_workflow","workflow_name":%s,"description":%s,"session_id":"s"}`, q(v), q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_progress","task_id":"wleak0001","description":%s,"summary":%s,"workflow_progress":[`+
				`{"type":"workflow_phase","index":1,"title":%s},`+
				`{"type":"workflow_agent","index":1,"label":%s,"phaseIndex":1,"agentId":"a1","model":%s,"state":%s,"startedAt":1,"lastToolName":%s,"lastToolSummary":%s,"error":%s},`+
				`{"type":"workflow_agent","index":2,"label":"b","phaseIndex":1,"agentId":"a2","state":"error","startedAt":1,"error":{"detail":%s}}]}`,
				q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_updated","task_id":"wleak0001","patch":{"status":%s}}`, q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"wleak0001","status":"completed","summary":%s}`, q(v)),
		}
		tr := workflow.New(nil)
		feed(t, tr, time.Now(), lines...)
		w := only(t, tr)
		if w.Agents[0].RawState == "" || w.Name == "" || w.NotifySummary == "" {
			t.Fatalf("pad %d: fixture did not reach every field: %+v", pad, *w)
		}
		rf, err := workflow.ParseResultFile([]byte(fmt.Sprintf(`{"taskId":"wleak0001","status":"completed","result":{"k":%s},"logs":[%s],"phases":[{"title":%s}],"workflowProgress":[{"type":"workflow_agent","index":3,"label":%s,"lastToolSummary":%s}]}`, q(v), q(v), q(v), q(v), q(v))))
		if err != nil {
			t.Fatal(err)
		}
		merged, _ := workflow.MergeResultFile(w, rf)
		for name, out := range map[string]any{
			"stream": w.Wire(w.Agents), "merged": merged.Wire(merged.Agents), "cache": workflow.NewResultCache(rf),
		} {
			b, _ := json.Marshal(out)
			if strings.Contains(string(b), stub) {
				t.Errorf("pad %d, %s: key stub on the wire: %s", pad, name, b)
			}
		}
	}
}
