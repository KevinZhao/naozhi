"""
V1c: A1 的完整复现 — 证明 mid-turn 注入的 wrap 路径存在，只是对内容敏感。
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

WRAP_MARKERS = ["system-reminder", "new message", "while you were working"]

def run():
    s = Session("V1c", extra_args=["--replay-user-messages"])
    s.send("Run this bash: for i in 1 2 3 4 5; do echo $i; sleep 2; done. Then say DONE.")
    s.wait_seconds(4.0)
    # EXACT wording from A1 that worked
    s.send("Also please say HELLO at the very start of your reply.")

    s.wait_for_results(1, timeout=45)
    s.wait_seconds(3.0)
    summarize("V1c", s)

    thinks = s.thinking_texts()
    hello_in_thinking = any("HELLO" in t or "hello" in t.lower() for t in thinks)
    wrap_in_thinking = any(any(m.lower() in t.lower() for m in WRAP_MARKERS) for t in thinks)
    rs = s.results()
    reply = (rs[0].get("result") if rs else "") or ""
    hello_in_reply = "HELLO" in reply.upper()

    print(f"\n  [V1c] thinking mentions HELLO? {hello_in_thinking}")
    print(f"  [V1c] thinking wrap markers? {wrap_in_thinking}")
    print(f"  [V1c] HELLO in final reply? {hello_in_reply}")
    print(f"  [V1c] reply: {reply[:300]!r}")

    if thinks:
        print("\n  --- all thinking ---")
        for i, t in enumerate(thinks):
            print(f"  [thinking {i}] {t[:800]}")

    s.close()
    s.dump("/tmp/v1c.ndjson")
    return wrap_in_thinking, hello_in_reply

if __name__ == "__main__":
    w, r = run()
    # V1c success = injection reaches model AND is executed (same as A1)
    PASS = r  # HELLO in reply is strongest signal
    print(f"\n{'PASS' if PASS else 'FAIL'}: V1c  wrap_markers={w}  HELLO_in_reply={r}")
    sys.exit(0 if PASS else 1)
