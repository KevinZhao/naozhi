package ccprobe

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/ccmodels"
)

func TestSplitQuoted(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`"/Users/x/.toolbox/bin/claude" default-credential-export`,
			[]string{"/Users/x/.toolbox/bin/claude", "default-credential-export"}},
		{`"/Applications/My Tools/claude" export`,
			[]string{"/Applications/My Tools/claude", "export"}},
		{`claude   export`, []string{"claude", "export"}},
		{`""`, []string{""}},
	}
	for _, tc := range cases {
		got, err := splitQuoted(tc.in)
		if err != nil {
			t.Errorf("splitQuoted(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitQuoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := splitQuoted(`"/unterminated`); err == nil {
		t.Error("want an error for an unterminated quote")
	}
}

func TestParseCredentials_SkipsNoiseBeforeTheEnvelope(t *testing.T) {
	out := "Refreshing credentials...\n" +
		`{"Credentials":{"Version":1,"AccessKeyId":"AKIAEXAMPLE",` +
		`"SecretAccessKey":"s3cret","SessionToken":"tok",` +
		`"Expiration":"2026-09-21T23:59:59Z"}}` + "\n"
	creds, err := parseCredentials([]byte(out))
	if err != nil {
		t.Fatalf("parseCredentials: %v", err)
	}
	if creds.AccessKeyID != "AKIAEXAMPLE" || creds.SessionToken != "tok" {
		t.Errorf("got %+v", creds)
	}
	if !creds.CanExpire || creds.Expires.IsZero() {
		t.Errorf("expiration not carried: %+v", creds)
	}
}

func TestParseCredentials_RejectsOutputWithoutCredentials(t *testing.T) {
	for _, out := range []string{"", "not json", `{"Credentials":{}}`, `{"other":1}`} {
		if _, err := parseCredentials([]byte(out)); err == nil {
			t.Errorf("parseCredentials(%q) = nil error, want one", out)
		}
	}
}

func TestParseCredentials_ErrorNeverEchoesTheSecret(t *testing.T) {
	out := `{"Credentials":{"SecretAccessKey":"s3cret-do-not-print"}}`
	_, err := parseCredentials([]byte(out))
	if err == nil {
		t.Fatal("want an error when no access key id is present")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaked the secret: %v", err)
	}
}

func TestClassifyConverse(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		errType string
		body    string
		want    ccmodels.Status
	}{
		{"ok", http.StatusOK, "", "", ccmodels.StatusOK},
		{"access denied excludes", 403, "AccessDeniedException:endpoint",
			`{"message":"not authorized"}`, ccmodels.StatusDenied},
		{"unknown profile excludes", 404, "ResourceNotFoundException", "", ccmodels.StatusDenied},
		{"validation excludes", 400, "ValidationException",
			`{"message":"invocation of model ID ... isn't supported"}`, ccmodels.StatusDenied},
		{"throttling keeps", 429, "ThrottlingException", "", ccmodels.StatusUnknown},
		{"throttling by type alone keeps", 400, "ThrottlingException", "", ccmodels.StatusUnknown},
		{"server error is undecided", 503, "ServiceUnavailableException", "", ccmodels.StatusUnknown},
		{"unrecognised is undecided", 418, "", `{"__type":"WeirdError"}`, ccmodels.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyConverse(tc.code, tc.errType, []byte(tc.body))
			if got != tc.want {
				t.Errorf("status = %q, want %q (detail %q)", got, tc.want, detail)
			}
			if tc.want == ccmodels.StatusDenied && detail == "" {
				t.Error("an exclusion must carry a reason")
			}
		})
	}
}

func TestAwsErrorType_PrefersHeaderThenBody(t *testing.T) {
	if got := awsErrorType("AccessDeniedException:https://x", nil); got != "AccessDeniedException" {
		t.Errorf("header form = %q", got)
	}
	body := []byte(`{"__type":"com.amazon.coral.service#ThrottlingException"}`)
	if got := awsErrorType("", body); got != "ThrottlingException" {
		t.Errorf("body form = %q", got)
	}
	if got := awsErrorType("", []byte("not json")); got != "unknown error" {
		t.Errorf("fallback = %q", got)
	}
}

func TestClassifyCCResult(t *testing.T) {
	cases := []struct {
		name   string
		result ccResult
		want   ccmodels.Status
	}{
		{"success", ccResult{}, ccmodels.StatusOK},
		{"invalid identifier is the alias bug", ccResult{IsError: true,
			Result: "API Error (claude-haiku-4-5): 400 The provided model identifier is invalid.."},
			ccmodels.StatusBadAlias},
		{"denial", ccResult{IsError: true, Result: "AccessDeniedException: no"}, ccmodels.StatusDenied},
		{"anything else stays undecided", ccResult{IsError: true, Result: "429 too many requests"},
			ccmodels.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyCCResult(tc.result)
			if got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLastResult_ReadsWindowPastInterleavedNoise(t *testing.T) {
	out := "Warning: no stdin data received in 3s, proceeding without it.\n" +
		`{"type":"system","subtype":"init","model":"claude-opus-5[1m]"}` + "\n" +
		`{"type":"assistant","message":{}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,` +
		`"modelUsage":{"claude-opus-5":{"contextWindow":1000000}}}` + "\n"
	res, ok := lastResult([]byte(out))
	if !ok {
		t.Fatal("no result event found")
	}
	if res.IsError {
		t.Error("result misread as an error")
	}
	if res.window() != 1_000_000 {
		t.Errorf("window = %d, want 1000000", res.window())
	}
}

func TestLastResult_NoResultEvent(t *testing.T) {
	if _, ok := lastResult([]byte("crashed before any output\n")); ok {
		t.Error("want ok=false when cc produced no result event")
	}
}

// TestProbeSettings_IsolatesOneAliasAndDropsFallback pins why stage 2 is
// meaningful: a fallbackModel would let a misresolved alias answer anyway.
func TestProbeSettings_IsolatesOneAliasAndDropsFallback(t *testing.T) {
	base := []byte(`{
  "awsCredentialExport": "\"/x/claude\" default-credential-export",
  "env": {"AWS_REGION": "us-west-2"},
  "fallbackModel": ["claude-sonnet-5"],
  "availableModels": ["claude-opus-5", "claude-sonnet-5"],
  "modelOverrides": {"claude-opus-5": "p1", "claude-sonnet-5": "p2"}
}`)
	doc, err := ccmodels.ProbeSettings(base, ccmodels.Alias{Name: "claude-opus-5", Profile: "p1"})
	if err != nil {
		t.Fatalf("ProbeSettings: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, doc)
	}
	if _, ok := got["fallbackModel"]; ok {
		t.Error("fallbackModel survived; a bad alias would answer from the substitute")
	}
	if _, ok := got["awsCredentialExport"]; !ok {
		t.Error("credential export dropped; the probe would not authenticate")
	}
	var avail []string
	if err := json.Unmarshal(got["availableModels"], &avail); err != nil {
		t.Fatalf("availableModels: %v", err)
	}
	if !reflect.DeepEqual(avail, []string{"claude-opus-5"}) {
		t.Errorf("availableModels = %v, want only the probed alias", avail)
	}
}

func TestRun_SkipsCCStageAndKeepsUndecidedWithoutCredentials(t *testing.T) {
	p := &Prober{SkipCC: true}
	verdicts := p.Run(t.Context(), []ccmodels.Alias{
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
	})
	v, ok := verdicts["claude-opus-5"]
	if !ok {
		t.Fatal("no verdict recorded")
	}
	if v.Status != ccmodels.StatusUnknown {
		t.Errorf("status = %q, want unknown so BuildPlan keeps the alias", v.Status)
	}
}

func TestEstimateDuration_QuickIsCheaper(t *testing.T) {
	full, quick := EstimateDuration(13, false), EstimateDuration(13, true)
	if quick >= full {
		t.Errorf("quick %s should beat full %s", quick, full)
	}
	if quick <= 0 {
		t.Errorf("quick estimate = %s", quick)
	}
}

func TestDescribe_CarriesStatusWindowAndDetail(t *testing.T) {
	got := Describe(ccmodels.Verdict{
		Alias: "claude-opus-5[1m]", Status: ccmodels.StatusOK, Window: 1_000_000,
	})
	for _, want := range []string{"claude-opus-5[1m]", "ok", "1000k window"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
	if got := Describe(ccmodels.Verdict{Alias: "x", Status: ccmodels.StatusDenied, Detail: "nope"}); !strings.Contains(got, "nope") {
		t.Errorf("Describe() = %q, missing the detail", got)
	}
}
