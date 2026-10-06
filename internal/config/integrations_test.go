package config

import (
	"strings"
	"testing"
)

func TestValidateIntegrations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, wantErr string
	}{
		{"ok", "integrations:\n  webhooks:\n    - url: https://h.example/x\n      events: [run.ended]\n      subsystems: [cron]\n      timeout: 5s\n", ""},
		{"relative url", "integrations:\n  webhooks:\n    - url: /hook\n", "absolute http(s)"},
		{"bad scheme", "integrations:\n  webhooks:\n    - url: ftp://h/x\n", "absolute http(s)"},
		{"bad event", "integrations:\n  webhooks:\n    - url: https://h/x\n      events: [turn.ended]\n", "unknown event"},
		{"bad subsystem", "integrations:\n  webhooks:\n    - url: https://h/x\n      subsystems: [session]\n", "unknown subsystem"},
		{"bad timeout", "integrations:\n  webhooks:\n    - url: https://h/x\n      timeout: soon\n", "invalid duration"},
		{"unexpanded secret", "integrations:\n  webhooks:\n    - url: https://h/x\n      secret: ${NOPE_UNSET_VAR}\n", "unexpanded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestIntegrations_EndpointsAndPlainHTTPDiag(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeCfg(t, "integrations:\n  webhooks:\n    - url: http://10.0.0.5/hook\n      secret: s\n      timeout: 3s\n    - url: http://127.0.0.1:9/ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	eps := cfg.WebhookEndpoints()
	if len(eps) != 2 || eps[0].Secret != "s" || eps[0].Timeout.Seconds() != 3 || eps[1].Timeout != 0 {
		t.Fatalf("endpoints = %+v", eps)
	}
	warned := 0
	for _, d := range cfg.integrationsDiags() {
		if strings.Contains(d.Field, "webhooks[0]") {
			warned++
		}
		if strings.Contains(d.Field, "webhooks[1]") {
			t.Fatal("loopback http must not warn")
		}
	}
	if warned != 1 {
		t.Fatalf("plain-http warnings = %d", warned)
	}
}
