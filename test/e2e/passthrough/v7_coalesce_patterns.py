"""
V7: 详细观察 CLI 合并模式。
   发 N 条，不同间隔，看合并规律。启用 replay 看到每条 uuid 与 result 的对应关系。
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session

def run(label, n, interval):
    s = Session(label, extra_args=["--replay-user-messages"], echo=False)
    uuids = []
    for i in range(n):
        u = s.send(f"Say '{i+1}' only.")
        uuids.append(u)
        if interval > 0:
            s.wait_seconds(interval)
    # Max wait
    s.wait_for_results(n, timeout=60)
    s.wait_seconds(2)

    results = s.results()
    replays = [e for e in s.user_events() if e.get("isReplay")]
    # Extract replay uuid → user events with that uuid, and batch structure
    batch_events = []
    for ev in replays:
        c = ev.get("message", {}).get("content", [])
        texts = [b.get("text","") for b in c if b.get("type")=="text"]
        batch_events.append((ev.get("uuid"), texts))

    print(f"  [{label}] n={n} interval={interval}s")
    print(f"    sent {len(uuids)} uuids")
    print(f"    got {len(replays)} replay events; {len(results)} results")
    for i, (u, ts) in enumerate(batch_events):
        print(f"    replay[{i}] uuid={u[:8]} n_texts={len(ts)} texts={ts!r}")
    for i, r in enumerate(results):
        print(f"    result[{i}] num_turns={r.get('num_turns')} text={(r.get('result') or '')[:60]!r}")
    s.close()
    s.dump(f"/tmp/{label}.ndjson")
    return len(replays), len(results)

if __name__ == "__main__":
    # V7a: tight burst — first one runs, rest get merged
    print("--- V7a: 5 msgs @ 50ms ---")
    rp_a, rs_a = run("v7a", 5, 0.05)

    # V7b: wait for each result (sequential)
    print("\n--- V7b: 5 sequential with 0.5s wait ---")
    s = Session("v7b", extra_args=["--replay-user-messages"], echo=False)
    uuids = []
    for i in range(5):
        u = s.send(f"Say '{i+1}' only.")
        uuids.append(u)
        # Wait for this to result
        target = s.result_count() + 1
        deadline_seq = __import__("time").time() + 30
        while __import__("time").time() < deadline_seq:
            if s.result_count() >= target:
                break
            __import__("time").sleep(0.1)
    s.wait_seconds(2)
    replays_b = [e for e in s.user_events() if e.get("isReplay")]
    print(f"    replays={len(replays_b)} results={s.result_count()}")
    for r in s.results():
        print(f"    result {(r.get('result') or '')[:40]!r}  num_turns={r.get('num_turns')}")
    s.close()
    s.dump("/tmp/v7b.ndjson")

    # V7c: very tight (0ms) — all in at once
    print("\n--- V7c: 5 msgs @ 0ms (all before first turn starts) ---")
    rp_c, rs_c = run("v7c", 5, 0.0)

    print("\n===== V7 verdict =====")
    print(f"V7a (50ms burst): replays={rp_a} results={rs_a}  (expect replays==5, results<5 for merge)")
    print(f"V7c (0ms burst):  replays={rp_c} results={rs_c}")
