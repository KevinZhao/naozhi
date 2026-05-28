// Phase 5-prep / R-server-warnings-extract (2026-05-28):
// 启动期 plaintext 警告 helpers + 警告文案常量抽到独立文件。
// 纯物理切分、零行为变化。
//
// 这一组实现 Server.Start 启动时的安全 self-check：
//   - shouldWarnReverseNodePlaintext / shouldWarnNoTokenOpen — 决策矩阵
//   - isPlaintextPublicAddr — addr 是否非 loopback 且非 TLS 终结
//   - reverseNodePlaintextWarning / trustedProxyXFFReminder — 警告文案常量
//
// 这些 pure func + 常量与 Server lifecycle 主流程仅在 Start() 内一处
// 调用；通过同包可见性 caller 不需任何改动。
package server

import (
	"net"
)

// reverseNodePlaintextWarning is the message logged when /ws-node is exposed
// on a non-loopback plaintext HTTP bind with no trusted proxy in front. Named
// constant (not an inline literal) so tests can assert the exact journal text
// and a refactor that rewords one occurrence has a single source of truth to
// update. R176-SEC-MED.
const reverseNodePlaintextWarning = "reverse-node /ws-node endpoint served over plaintext HTTP with no trusted proxy: " +
	"remote-node tokens and cross-node session payloads may be sniffed by any " +
	"passive listener on the wire. A leaked token lets an attacker impersonate " +
	"the remote node and stream arbitrary session data into the primary. " +
	"Terminate TLS upstream and set server.trusted_proxy=true, or bind to " +
	"127.0.0.1 for local-only access."

// trustedProxyXFFReminder is the startup info-level note emitted whenever
// trusted_proxy=true. Pulled out so unit tests can pin the exact text and
// future ops doc references can grep one source of truth. R238-SEC-15 (#848).
const trustedProxyXFFReminder = "trusted_proxy=true: per-IP rate limiters, audit-log " +
	"client_ip fields, and same-origin gates trust the last X-Forwarded-For hop. " +
	"Ensure the upstream proxy (ALB/CloudFront/nginx) strips client-supplied XFF " +
	"headers before appending its own, or applies a hop-count limit — otherwise " +
	"a spoofed XFF can bypass per-IP rate limiting by attributing requests to a " +
	"victim's bucket. naozhi cannot verify the upstream contract from inside the " +
	"process; this reminder is one-shot at startup so the requirement is visible " +
	"in the boot journal."

// shouldWarnReverseNodePlaintext reports whether the /ws-node plaintext warning
// should fire at Server.Start.
//
// Decision matrix:
//
//	no reverse server, any addr, any proxy             → no warn (feature inactive)
//	reverse server,    loopback, any proxy             → no warn (traffic stays on host)
//	reverse server,    public,   trustedProxy=true     → no warn (TLS terminated upstream)
//	reverse server,    public,   trustedProxy=false    → WARN (R176-SEC-MED)
//
// Extracted from Server.Start so a unit test can exercise the matrix without
// binding ports or wiring the full reverse-node subsystem.
func shouldWarnReverseNodePlaintext(reverseServerEnabled bool, trustedProxy bool, addr string) bool {
	if !reverseServerEnabled {
		return false
	}
	if trustedProxy {
		return false
	}
	return isPlaintextPublicAddr(addr)
}

// shouldWarnNoTokenOpen reports whether the "no-auth API open to all callers"
// warning should fire at Server.Start.
//
// Decision matrix (dashboardToken == "" means no auth):
//
//	token set,  any addr, any proxy          → no warn (operator configured auth)
//	token "",   loopback, any proxy          → no warn (only accessible on host)
//	token "",   public,   trustedProxy=true  → no warn (upstream enforces auth)
//	token "",   public,   trustedProxy=false → WARN (R60-SEC-006 + R70-SEC-M1)
//
// Extracted from Server.Start so a unit test can assert the matrix without
// binding ports or mocking slog. R60-SEC-006.
func shouldWarnNoTokenOpen(dashboardToken, addr string, trustedProxy bool) bool {
	if dashboardToken != "" {
		return false
	}
	if !isPlaintextPublicAddr(addr) {
		return false
	}
	if trustedProxy {
		return false
	}
	return true
}

// isPlaintextPublicAddr reports whether addr is a non-loopback TCP listen
// address that would expose Bearer tokens and auth cookies over cleartext
// HTTP. Loopback (127.0.0.1 / ::1 / localhost) is considered safe because
// the traffic never leaves the host. Addresses we cannot parse are treated
// as public so the warning errs on the side of visibility.
func isPlaintextPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" form — no host, bound to all interfaces, public by default.
		return true
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}
