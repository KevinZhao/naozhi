package system

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/apiwatch"
)

func TestHandleAPIWatch(t *testing.T) {
	store := apiwatch.Open("")
	store.Record(apiwatch.Record{Kind: apiwatch.KindRetry, Key: "a", Status: 429})
	store.Record(apiwatch.Record{Kind: apiwatch.KindTurn, Key: "b", APIMS: 1000})
	h := New(Deps{APIWatch: store})

	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.HandleAPIWatch(w, httptest.NewRequest(http.MethodGet, "/api/system/apiwatch"+q, nil))
		return w
	}
	for _, q := range []string{"?hours=0", "?hours=169", "?hours=x"} {
		if w := get(q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400", q, w.Code)
		}
	}
	w := get("?hours=6&key=a")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var rep apiwatch.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Retries != 1 || rep.Summary.Turns != 0 || len(rep.Recent) != 1 || rep.Recent[0].Class != apiwatch.ClassRateLimit {
		t.Fatalf("report = %+v", rep)
	}

	// No store wired: an empty report, not an error.
	w = httptest.NewRecorder()
	New(Deps{}).HandleAPIWatch(w, httptest.NewRequest(http.MethodGet, "/api/system/apiwatch", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("nil store: code %d", w.Code)
	}
}
