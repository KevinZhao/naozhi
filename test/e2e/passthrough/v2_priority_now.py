"""
V2: priority:"now" 应该让 CLI 立即 abort 当前 turn。

发一个长任务（bash 30s），T=3s 发 priority:"now" 消息。
期望: 很快（< 5s）看到 result subtype=error_during_execution，
     然后紧接着一个新 turn 处理 priority:"now" 消息。
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

def run():
    s = Session("V2")
    s.send("Please run this bash very slowly: for i in $(seq 1 20); do echo tick=$i; sleep 2; done. Take your time.")
    s.wait_seconds(3.0)
    urgent_start = s.t0 + 3.0
    s.send("URGENT: stop whatever you're doing and just say the word PIVOT.", priority="now")

    # wait at most 20s for abort + pivot
    s.wait_for_results(2, timeout=40)

    rs = s.results()
    print(f"\n  [V2] got {len(rs)} results")
    for i, r in enumerate(rs):
        sub = r.get("subtype")
        stop = r.get("stop_reason")
        txt = (r.get("result") or "")[:200].replace("\n", " ")
        print(f"  [{i}] subtype={sub} stop={stop} text={txt!r}")

    summarize("V2", s)
    s.close()
    s.dump("/tmp/v2.ndjson")

    # Pass criteria:
    #   (a) first result has subtype='error_during_execution' OR stop_reason indicates interrupt
    #   (b) second result executes the pivot (contains "PIVOT")
    first_is_interrupt = bool(rs) and (
        rs[0].get("subtype") == "error_during_execution"
        or "interrupt" in (rs[0].get("stop_reason") or "").lower()
    )
    pivot_seen = any("PIVOT" in (r.get("result") or "").upper() for r in rs)
    print(f"\n  [V2] first result is interrupt? {first_is_interrupt}")
    print(f"  [V2] PIVOT executed somewhere? {pivot_seen}")
    return first_is_interrupt, pivot_seen, len(rs)

if __name__ == "__main__":
    first_abort, pivot, n = run()
    PASS = first_abort and pivot
    print(f"\n{'PASS' if PASS else 'FAIL'}: V2 — abort={first_abort} pivot={pivot} results={n}")
    sys.exit(0 if PASS else 1)
