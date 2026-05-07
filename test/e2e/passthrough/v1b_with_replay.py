"""
V1b: 重跑 V1 但加 --replay-user-messages。验证 replay 是否是让 CLI
把 mid-turn stdin user 消息放进 commandQueue 的必要条件。
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

WRAP_MARKERS = ["system-reminder", "new message", "while you were working"]

def run():
    s = Session("V1b", extra_args=["--replay-user-messages"])
    s.send("Run this bash: for i in 1 2 3 4 5; do echo $i; sleep 2; done. Then say DONE.")
    s.wait_seconds(4.0)
    s.send("Also please include the word INJECTED at the very start of your final reply.")

    s.wait_for_results(2, timeout=60)
    s.wait_seconds(3.0)
    summarize("V1b", s)

    thinks = s.thinking_texts()
    injected_in_thinking = any("INJECTED" in t or "injected" in t.lower() for t in thinks)
    wrap_in_thinking = any(any(m.lower() in t.lower() for m in WRAP_MARKERS) for t in thinks)
    rs = s.results()
    all_reply = " ".join((r.get("result") or "") for r in rs)
    injected_in_reply = "INJECTED" in all_reply.upper()

    print(f"\n  [V1b] result count: {len(rs)}")
    print(f"  [V1b] thinking refs injection? {injected_in_thinking}")
    print(f"  [V1b] thinking wrap markers? {wrap_in_thinking}")
    print(f"  [V1b] INJECTED in any reply? {injected_in_reply}")
    print(f"  [V1b] all replies: {all_reply[:400]!r}")

    if thinks:
        print("\n  --- last 2 thinking excerpts ---")
        for t in thinks[-2:]:
            print("  ---")
            print("  " + t[:1200].replace("\n", "\n  "))

    s.close()
    s.dump("/tmp/v1b.ndjson")
    return wrap_in_thinking, injected_in_thinking, injected_in_reply, len(rs)

if __name__ == "__main__":
    w, i, r, n = run()
    PASS = w and (i or r)
    print(f"\n{'PASS' if PASS else 'FAIL'}: V1b  wrap={w} inject_in_thinking={i} inject_in_reply={r} results={n}")
    sys.exit(0 if PASS else 1)
