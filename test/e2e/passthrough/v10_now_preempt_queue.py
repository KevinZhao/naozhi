"""
V10: priority:"now" 抢占时，已排队消息的命运。

msgA（长 bash）运行中，msgB（无 priority）排队，msgC 以 priority:"now" 抢占。
期望: A 被 abort；C 先跑并得到 C_DONE；B 不被丢弃——随后有 B 自己的 replay
     （晚于 C 的 result）与 result="B_DONE"。

用法: python3 v10_now_preempt_queue.py [--bash|--gen]
  --bash（默认）A 是阻塞 Bash 工具调用；--gen A 是纯生成长文。
"""
import sys, os, json, time
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

BASH_TASK = "Run this bash command exactly and then report its output: sleep 20 && echo A_DONE"
GEN_TASK = "Write a 1500-word essay about the history of tea. Do not use any tools."


def stdout_events(s: Session) -> list:
    out = []
    for (_, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            out.append(json.loads(line))
        except Exception:
            continue
    return out


def wait_for(s: Session, pred, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if any(pred(e) for e in stdout_events(s)):
            return True
        time.sleep(0.1)
    return False


def has_tool_use(e: dict) -> bool:
    if e.get("type") != "assistant":
        return False
    return any(c.get("type") == "tool_use" for c in e.get("message", {}).get("content", []))


def run(mode: str):
    s = Session("V10", extra_args=["--replay-user-messages"], echo=False)
    if mode == "gen":
        ua = s.send(GEN_TASK)
        wait_for(s, lambda e: e.get("type") == "user" and e.get("isReplay") and e.get("uuid") == ua, 60)
    else:
        ua = s.send(BASH_TASK)
        wait_for(s, has_tool_use, 60)
    time.sleep(2)
    ub = s.send("reply exactly B_DONE")
    time.sleep(1)
    uc = s.send("reply exactly C_DONE", priority="now")

    s.wait_for_results(3, timeout=120)
    s.wait_seconds(5)
    s.close(wait=30)
    s.dump(f"/tmp/v10_{mode}.ndjson")

    # Ordered timeline of replays (by uuid) and results.
    label = {ua: "A", ub: "B", uc: "C"}
    timeline = []
    for e in stdout_events(s):
        if e.get("type") == "user" and e.get("isReplay"):
            timeline.append(("replay", label.get(e.get("uuid"), "?")))
        elif e.get("type") == "result":
            timeline.append(("result", (e.get("result") or "").strip()[:40]))
            print(f"  result subtype={e.get('subtype')} is_error={e.get('is_error')} "
                  f"stop={e.get('stop_reason')} terminal_reason={e.get('terminal_reason')} "
                  f"text={(e.get('result') or '')[:60]!r}")
    print(f"  timeline: {timeline}")
    summarize("V10", s)

    c_result = next((i for i, (k, v) in enumerate(timeline) if k == "result" and "C_DONE" in v), None)
    b_replay = next((i for i, (k, v) in enumerate(timeline) if k == "replay" and v == "B"), None)
    b_done = any(k == "result" and v == "B_DONE" for k, v in timeline)
    b_after_c = c_result is not None and b_replay is not None and b_replay > c_result
    return b_after_c, b_done


if __name__ == "__main__":
    mode = "gen" if "--gen" in sys.argv else "bash"
    b_after_c, b_done = run(mode)
    PASS = b_after_c and b_done
    print(f"\n{'PASS' if PASS else 'FAIL'}: V10 ({mode}) — B replayed after C's result={b_after_c} B_DONE={b_done}")
    sys.exit(0 if PASS else 1)
