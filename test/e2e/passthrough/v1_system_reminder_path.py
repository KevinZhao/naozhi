"""
V1: 验证 naozhi 走的 stream-json 注入路径 === CC TUI 的 wrap 路径。

当我们在 bash tool 执行期间注入一条非对抗性 user message，模型的 thinking
应该包含 CC 源码里定义的固定文本:
    "system-reminder" + "while you were working" 或相近片段。

这证明两边共用 wrapMessagesInSystemReminder + wrapCommandText 路径。
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

WRAP_MARKERS = [
    "system-reminder",
    "new message",
    "while you were working",
    "while I was",
]

def run():
    s = Session("V1")
    s.send("Run this bash: for i in 1 2 3 4 5; do echo $i; sleep 2; done. Then say DONE.")
    s.wait_seconds(4.0)
    s.send("Also please include the word INJECTED at the very start of your final reply.")

    # Wait for up to 2 results (in case injection goes to next turn)
    ok = s.wait_for_results(2, timeout=60)
    # Give extra grace for any straggler result
    s.wait_seconds(3.0)
    summarize("V1", s)

    thinks = s.thinking_texts()
    injected_in_thinking = any("INJECTED" in t or "injected" in t.lower() for t in thinks)
    wrap_in_thinking = any(
        any(m.lower() in t.lower() for m in WRAP_MARKERS) for t in thinks
    )
    results = s.results()
    final_text = (results[0].get("result") if results else "") or ""
    injected_in_reply = "INJECTED" in final_text.upper()

    print(f"\n  [V1] thinking references injection? {injected_in_thinking}")
    print(f"  [V1] thinking contains wrap markers? {wrap_in_thinking}")
    print(f"  [V1] final reply contains INJECTED? {injected_in_reply}")

    # Dump last thinking for inspection
    if thinks:
        print("\n  --- last thinking excerpt ---")
        print(thinks[-1][:1500])
        print("  --- end ---")

    s.close()
    s.dump("/tmp/v1.ndjson")
    return {
        "result_count": s.result_count(),
        "wrap_markers_in_thinking": wrap_in_thinking,
        "injected_in_thinking": injected_in_thinking,
        "injected_in_reply": injected_in_reply,
        "final_reply": final_text[:300],
    }

if __name__ == "__main__":
    r = run()
    # Pass = wrap markers visible AND (injection referenced OR executed)
    PASS = r["wrap_markers_in_thinking"] and (r["injected_in_thinking"] or r["injected_in_reply"])
    print(f"\n{'PASS' if PASS else 'FAIL'}: V1 — {r}")
    sys.exit(0 if PASS else 1)
